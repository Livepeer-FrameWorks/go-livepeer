// Package frameworks wraps the FrameWorks Decklog gateway-telemetry client so
// the gateway can emit per-orchestrator discovery / state / outcome events
// without forcing every callsite to know about gRPC, env wiring, GeoIP, or
// DNS-cache details. Telemetry is disabled when FRAMEWORKS_DECKLOG_GRPC_ADDR is
// unset; once Decklog is configured, cluster tenancy is required at startup.
//
// See docs/architecture/orchestrator-visibility.md (in the monorepo) for the
// full data model and pipeline.
package frameworks

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"

	"github.com/Livepeer-FrameWorks/monorepo/pkg/clients/decklog"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/geoip"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/logging"
	pb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto"

	"google.golang.org/protobuf/types/known/timestamppb"
)

// gatewayCtx is the (cluster, gateway) identity baked into every event by
// Init from FRAMEWORKS_* env vars. Stays empty when the env is missing,
// which makes Enabled() return false and every Emit* call a no-op.
type gatewayCtx struct {
	gatewayID            string
	gatewayRegion        string
	clusterID            string
	clusterOwnerTenantID string
}

var (
	mu       sync.RWMutex
	enabled  bool
	ctxIdent gatewayCtx
	client   *decklog.BatchedClient
	geoIP    *geoip.Reader
	resolver = newDNSCache(60 * time.Second)
	logger   logging.Logger
)

// Init wires the singleton from env. Safe to call once at gateway startup.
// FRAMEWORKS_DECKLOG_GRPC_ADDR controls whether this gateway is a FrameWorks
// telemetry producer; configured producers must also carry cluster ownership.
func Init(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()

	logger = logging.NewLogger()

	ident := gatewayCtx{
		gatewayID:            strings.TrimSpace(os.Getenv("FRAMEWORKS_GATEWAY_ID")),
		gatewayRegion:        strings.TrimSpace(os.Getenv("FRAMEWORKS_GATEWAY_REGION")),
		clusterID:            strings.TrimSpace(os.Getenv("FRAMEWORKS_CLUSTER_ID")),
		clusterOwnerTenantID: strings.TrimSpace(os.Getenv("FRAMEWORKS_CLUSTER_OWNER_TENANT_ID")),
	}
	addr := strings.TrimSpace(os.Getenv("FRAMEWORKS_DECKLOG_GRPC_ADDR"))
	tlsMode := strings.TrimSpace(os.Getenv("FRAMEWORKS_DECKLOG_TLS_MODE"))
	// Reuse the platform service token already injected for service-to-service
	// auth. FRAMEWORKS_DECKLOG_AUTH_TOKEN is an explicit override, not a
	// second required credential path.
	authToken := strings.TrimSpace(os.Getenv("FRAMEWORKS_DECKLOG_AUTH_TOKEN"))
	if authToken == "" {
		authToken = strings.TrimSpace(os.Getenv("SERVICE_TOKEN"))
	}

	cfg := decklog.BatchedClientConfig{
		Target:        addr,
		AllowInsecure: tlsMode != "mtls" && tlsMode != "tls",
		CACertFile:    strings.TrimSpace(os.Getenv("GRPC_TLS_CA_PATH")),
		Source:        "livepeer-gateway",
		ServiceToken:  authToken,
		Timeout:       5 * time.Second,
		Optional:      true,
	}
	c, err := decklog.NewBatchedClient(cfg, logger)
	if err != nil {
		return fmt.Errorf("frameworks telemetry: Decklog client init failed: %w", err)
	}
	client = c

	// GeoIP is best-effort metadata; telemetry still emits without it.
	if mmdbPath := strings.TrimSpace(os.Getenv("GEOIP_MMDB_PATH")); mmdbPath != "" {
		r, gerr := geoip.NewReader(mmdbPath)
		if gerr != nil {
			glog.Warningf("frameworks telemetry: GeoIP load failed (path=%s): %v; geo attachment will report geo_source=unknown", mmdbPath, gerr)
		} else {
			geoIP = r
			glog.Infof("frameworks telemetry: GeoIP loaded from %s", mmdbPath)
		}
	}

	ctxIdent = ident
	// Decklog requires cluster-owner tenancy on every gateway telemetry event.
	enabled = addr != "" && ident.clusterOwnerTenantID != ""
	if enabled {
		glog.Infof("frameworks telemetry: enabled (gateway=%s region=%s cluster=%s)", ident.gatewayID, ident.gatewayRegion, ident.clusterID)
	} else if addr != "" {
		return fmt.Errorf("frameworks telemetry: FRAMEWORKS_CLUSTER_OWNER_TENANT_ID is required when FRAMEWORKS_DECKLOG_GRPC_ADDR is set")
	}
	return nil
}

// Enabled returns whether telemetry is wired. Used to skip expensive
// per-event work (DNS resolution, geo lookup) in non-FW deployments.
func Enabled() bool {
	mu.RLock()
	defer mu.RUnlock()
	return enabled
}

// SetDialedIP records the remote IP the gateway connected to for a given
// orchestrator hostname.
func SetDialedIP(host, ip string) {
	if !Enabled() {
		return
	}
	resolver.SetDialedIP(host, ip)
}

func ResolvedIPForURL(rawURL string) string {
	if !Enabled() {
		return ""
	}
	host := hostFromURL(rawURL)
	if host == "" {
		return ""
	}
	return resolver.dialedOrSingleIP(host)
}

// EmitDiscoveryObserved emits one row per resolved IP for a single
// GetOrchestratorInfo attempt. The dialed IP carries actual latency +
// reachability; sibling A-record IPs come along as observation context with
// dialed=false, so the per-(gateway, orch_addr, resolved_ip) vantage view
// is preserved end-to-end.
//
// orchURL is the URL the gateway dialed (used for hostname resolution).
// orchAddr is the orchestrator's eth address (hex). dialedLatency and
// reachable describe the actual dial result; failureKind is the
// classification (dns/tcp/rpc/timeout/...) when reachable=false.
func EmitDiscoveryObserved(
	ctx context.Context,
	orchAddr, orchURL, advertisedNodeURL string,
	dialedLatency time.Duration,
	reachable, compatible bool,
	score float32,
	failureReason, failureKind string,
) {
	if !Enabled() {
		return
	}
	host := hostFromURL(orchURL)
	if host == "" {
		return
	}
	dialedIP := resolver.dialedIP(host)
	resolvedIPs := resolver.lookup(host)
	if len(resolvedIPs) == 0 {
		// DNS failed; emit a single row with empty IP so the failure is durable.
		resolvedIPs = []string{""}
	}

	now := time.Now()
	for _, ip := range resolvedIPs {
		isDialed := ip == dialedIP || (dialedIP == "" && len(resolvedIPs) == 1)
		evt := &pb.GatewayTelemetryEvent{
			Timestamp:            timestamppb.New(now),
			Payload:              nil, // set below
			GatewayId:            ctxIdent.gatewayID,
			GatewayRegion:        ctxIdent.gatewayRegion,
			ClusterId:            ctxIdent.clusterID,
			ClusterOwnerTenantId: ctxIdent.clusterOwnerTenantID,
		}
		obs := &pb.OrchestratorDiscoveryObserved{
			OrchAddr:           orchAddr,
			OrchUrl:            orchURL,
			AdvertisedNodeUrl:  advertisedNodeURL,
			DiscoveryLatencyMs: 0,
			Reachable:          false,
			Compatible:         compatible,
			Score:              score,
			Vantage:            buildVantage(ip, isDialed, now),
		}
		if isDialed {
			obs.DiscoveryLatencyMs = uint32(dialedLatency.Milliseconds())
			obs.Reachable = reachable
			obs.FailureReason = failureReason
			obs.FailureKind = failureKind
		}
		evt.Payload = &pb.GatewayTelemetryEvent_Discovery{Discovery: obs}
		send(ctx, evt)
	}
}

// EmitStateUpdate emits a per-instance state event after a successful
// GetOrchestratorInfo response. Pricing/capabilities/hardware are
// per-instance: an orch's eth address fronts N load-balanced instances and
// each can run independent config (usually consistent in practice, NOT
// guaranteed). The receiving table keys on resolved_ip so divergence is
// preserved.
func EmitStateUpdate(ctx context.Context, orchAddr, orchURL string, info *pb.OrchestratorStateUpdate) {
	if !Enabled() || info == nil {
		return
	}
	host := hostFromURL(orchURL)
	dialedIP := ""
	if host != "" {
		dialedIP = resolver.dialedOrSingleIP(host)
	}
	now := time.Now()
	info.Vantage = buildVantage(dialedIP, true, now)

	send(ctx, &pb.GatewayTelemetryEvent{
		Timestamp:            timestamppb.New(now),
		Payload:              &pb.GatewayTelemetryEvent_State{State: info},
		GatewayId:            ctxIdent.gatewayID,
		GatewayRegion:        ctxIdent.gatewayRegion,
		ClusterId:            ctxIdent.clusterID,
		ClusterOwnerTenantId: ctxIdent.clusterOwnerTenantID,
	})
}

// EmitTranscodeOutcome emits one transcode result/error event. streamTenantID
// is the FrameWorks per-session tenant from StreamParameters.TenantID.
func EmitTranscodeOutcome(ctx context.Context, streamTenantID string, payload *pb.OrchestratorTranscodeOutcome) {
	if !Enabled() || payload == nil {
		return
	}
	if strings.TrimSpace(streamTenantID) == "" {
		glog.Warningf("frameworks telemetry: dropping transcode outcome without stream tenant (orch=%s session=%s)", payload.GetOrchAddr(), payload.GetSessionId())
		return
	}
	now := time.Now()
	send(ctx, &pb.GatewayTelemetryEvent{
		Timestamp:            timestamppb.New(now),
		Payload:              &pb.GatewayTelemetryEvent_Transcode{Transcode: payload},
		GatewayId:            ctxIdent.gatewayID,
		GatewayRegion:        ctxIdent.gatewayRegion,
		ClusterId:            ctxIdent.clusterID,
		ClusterOwnerTenantId: ctxIdent.clusterOwnerTenantID,
		StreamTenantId:       streamTenantID,
	})
}

// EmitAIOutcome emits one AI result/error event.
func EmitAIOutcome(ctx context.Context, streamTenantID string, payload *pb.OrchestratorAIOutcome) {
	if !Enabled() || payload == nil {
		return
	}
	if strings.TrimSpace(streamTenantID) == "" {
		glog.Warningf("frameworks telemetry: dropping AI outcome without stream tenant (orch=%s session=%s)", payload.GetOrchAddr(), payload.GetSessionId())
		return
	}
	now := time.Now()
	send(ctx, &pb.GatewayTelemetryEvent{
		Timestamp:            timestamppb.New(now),
		Payload:              &pb.GatewayTelemetryEvent_Ai{Ai: payload},
		GatewayId:            ctxIdent.gatewayID,
		GatewayRegion:        ctxIdent.gatewayRegion,
		ClusterId:            ctxIdent.clusterID,
		ClusterOwnerTenantId: ctxIdent.clusterOwnerTenantID,
		StreamTenantId:       streamTenantID,
	})
}

// send is the single fire-and-log path. Errors are logged at debug level;
// data-plane processing should not block on telemetry.
func send(ctx context.Context, evt *pb.GatewayTelemetryEvent) {
	mu.RLock()
	c := client
	mu.RUnlock()
	if c == nil {
		return
	}
	if err := c.SendGatewayTelemetry(evt); err != nil {
		// Use V(common.DEBUG) equivalent; these failures are noisy under
		// transient network blips.
		glog.V(2).Infof("frameworks telemetry: send failed: %v", err)
	}
}

func buildVantage(resolvedIP string, dialed bool, now time.Time) *pb.OrchestratorVantageGeo {
	v := &pb.OrchestratorVantageGeo{
		ResolvedIp:    resolvedIP,
		Dialed:        dialed,
		GeoSource:     "unknown",
		GeoResolvedAt: timestamppb.New(now),
	}
	mu.RLock()
	r := geoIP
	mu.RUnlock()
	if r == nil || resolvedIP == "" {
		return v
	}
	rec := r.Lookup(resolvedIP)
	if rec == nil {
		return v
	}
	v.Latitude = rec.Latitude
	v.Longitude = rec.Longitude
	v.City = rec.City
	v.CountryCode = rec.CountryCode
	v.GeoSource = "mmdb"
	return v
}

func hostFromURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	// Cheap host extraction without pulling net/url for every event in a hot
	// loop. Format: scheme://host[:port]/...
	s := rawURL
	if i := strings.Index(s, "://"); i != -1 {
		s = s[i+3:]
	}
	if i := strings.Index(s, "/"); i != -1 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i != -1 {
		// Strip port, but keep IPv6 brackets intact.
		if !strings.Contains(s, "]") {
			s = s[:i]
		}
	}
	return s
}
