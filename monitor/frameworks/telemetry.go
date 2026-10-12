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
	"github.com/Livepeer-FrameWorks/monorepo/pkg/telemetryspool"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang/glog"

	"github.com/Livepeer-FrameWorks/monorepo/pkg/clients/decklog"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/geoip"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/logging"
	pb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto/ipc"

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
	mu          sync.RWMutex
	enabled     bool
	ctxIdent    gatewayCtx
	client      *decklog.BatchedClient
	geoIP       *geoip.Reader
	resolver    = newDNSCache(60 * time.Second)
	logger      logging.Logger
	spool       *telemetryspool.Queue
	writer      *spoolWriter
	networkID   string
	environment string
)

// Init wires the singleton from env. Safe to call once at gateway startup.
// FRAMEWORKS_DECKLOG_GRPC_ADDR controls whether this gateway is a FrameWorks
// telemetry producer; configured producers must also carry cluster ownership.
func Init(ctx context.Context) error {
	mu.Lock()
	defer mu.Unlock()

	logger = logging.NewLogger()
	if err := initCandidates(ctx); err != nil {
		return err
	}

	ident := gatewayCtx{
		gatewayID:            strings.TrimSpace(os.Getenv("FRAMEWORKS_GATEWAY_ID")),
		gatewayRegion:        strings.TrimSpace(os.Getenv("FRAMEWORKS_GATEWAY_REGION")),
		clusterID:            strings.TrimSpace(os.Getenv("FRAMEWORKS_CLUSTER_ID")),
		clusterOwnerTenantID: strings.TrimSpace(os.Getenv("FRAMEWORKS_CLUSTER_OWNER_TENANT_ID")),
	}
	addr := strings.TrimSpace(os.Getenv("FRAMEWORKS_DECKLOG_GRPC_ADDR"))
	if addr != "" && (ident.gatewayID == "" || ident.clusterID == "") {
		return fmt.Errorf("frameworks telemetry requires stable gateway and cluster IDs")
	}
	tlsMode := strings.TrimSpace(os.Getenv("FRAMEWORKS_DECKLOG_TLS_MODE"))
	allowInsecure := true
	if addr != "" {
		var err error
		allowInsecure, err = decklogAllowsInsecure(tlsMode)
		if err != nil {
			return err
		}
	}
	// Reuse the platform service token already injected for service-to-service
	// auth. FRAMEWORKS_DECKLOG_AUTH_TOKEN is an explicit override, not a
	// second required credential path.
	authToken := strings.TrimSpace(os.Getenv("FRAMEWORKS_DECKLOG_AUTH_TOKEN"))
	if authToken == "" {
		authToken = strings.TrimSpace(os.Getenv("SERVICE_TOKEN"))
	}

	cfg := decklog.BatchedClientConfig{
		Target:        addr,
		AllowInsecure: allowInsecure,
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
	if addr != "" {
		go func() {
			<-ctx.Done()
			if err := c.Close(); err != nil {
				glog.Warningf("frameworks telemetry shutdown: %v", err)
			}
		}()
	}

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
		directory := strings.TrimSpace(os.Getenv("FRAMEWORKS_TELEMETRY_SPOOL_DIR"))
		if directory == "" {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil {
				return fmt.Errorf("find persistent telemetry directory: %w", homeErr)
			}
			directory = filepath.Join(home, ".lpData", "frameworks-telemetry", ident.gatewayID)
		}
		maxBytes := int64(64 << 20)
		if raw := os.Getenv("FRAMEWORKS_TELEMETRY_SPOOL_MAX_BYTES"); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value <= 0 {
				return fmt.Errorf("invalid telemetry spool bound")
			}
			maxBytes = value
		}
		spool, err = telemetryspool.Open(telemetryspool.Config{Directory: directory, MaxBytes: maxBytes})
		if err != nil {
			return fmt.Errorf("open telemetry spool: %w", err)
		}
		networkID = os.Getenv("FRAMEWORKS_NETWORK_ID")
		if networkID == "" {
			networkID = "42161"
		}
		environment = os.Getenv("FRAMEWORKS_ENVIRONMENT")
		if environment == "" {
			environment = "production"
		}
		q := spool
		w := startSpoolWriter(ctx, q.Enqueue)
		writer = w
		go q.Run(ctx, func(ctx context.Context, raw []byte) error {
			var event pb.GatewayTelemetryEvent
			if err := proto.Unmarshal(raw, &event); err != nil {
				return err
			}
			sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return c.SendGatewayTelemetryContext(sendCtx, &event)
		}, func(err error) { glog.Warningf("frameworks telemetry delivery pending: %v", err) })
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				count, _, dropped := q.Stats()
				dropped += w.dropped.Load()
				send(ctx, &pb.GatewayTelemetryEvent{Timestamp: timestamppb.Now(), GatewayId: ident.gatewayID, GatewayRegion: ident.gatewayRegion, ClusterId: ident.clusterID, ClusterOwnerTenantId: ident.clusterOwnerTenantID, Payload: &pb.GatewayTelemetryEvent_ObserverReport{ObserverReport: &pb.OrchestratorObserverReport{ReportKind: "heartbeat", CadenceSeconds: 60, QueueRecords: uint32(count), DroppedRecords: dropped, Healthy: true}}})
			}
		}()
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

// ResolvedIPForURL cannot attribute a new request using a previous socket.
func ResolvedIPForURL(rawURL string) string { return "" }

func EmitFilterDecision(ctx context.Context, address, uri, stage, reason string) {
	if !Enabled() {
		return
	}
	EmitDecision(ctx, &pb.OrchestratorCandidateDecision{OrchAddr: address, OrchUrl: uri, Stage: stage, Disposition: "rejected", Reason: reason, Workload: "transcoding", RuleVersion: "gateway-v1"})
}

// EmitDiscoveryObserved records the actual RPC socket. DNS answers belong to
// independent observer discovery and cannot attribute an RPC to an instance.
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
	dialedIP := RemoteIP(ctx)
	resolvedIPs := []string{dialedIP}

	now := time.Now()
	for _, ip := range resolvedIPs {
		isDialed := dialedIP != "" && ip == dialedIP
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
			CompatibilityKnown: false,
			SamplingSource:     "gateway",
			ObserverHealthy:    true,
			TargetIp:           TargetIP(ctx),
			AttemptId:          uuid.NewString(),
			Score:              score,
			Vantage:            buildVantage(ip, isDialed, now),
		}
		if isDialed || ip == "" {
			obs.DiscoveryLatencyMs = uint32(dialedLatency.Milliseconds())
			obs.Reachable = reachable
			obs.FailureReason = failureKind
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
	dialedIP := RemoteIP(ctx)
	now := time.Now()
	info.Vantage = buildVantage(dialedIP, dialedIP != "", now)

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

// spoolWriterQueue bounds the events waiting for the spool: about a minute of a
// busy gateway's events.
const spoolWriterQueue = 4096

// spoolWriter persists events on its own goroutine. Events are emitted on
// segment and discovery paths that run within a segment's few-second budget,
// while a spool write fsyncs the record and its directory under a lock the
// delivery worker shares; on a busy disk that takes seconds. A full queue
// drops the event and counts it in the heartbeat's dropped records. Once the
// context ends (gateway shutdown) the goroutine drains what was queued and
// exits; segments still in flight emit after that, and offer writes those
// directly so the spool keeps them for the next start.
type spoolWriter struct {
	events  chan spoolEvent
	dropped atomic.Uint64
	write   func(spoolEvent)
	// mu orders offer's queueing against the goroutine's exit: after stopped
	// is set nothing is queued, so the final drain sees every queued event.
	mu      sync.Mutex
	stopped bool
}

type spoolEvent struct {
	id  string
	raw []byte
}

func startSpoolWriter(ctx context.Context, enqueue func(id string, raw []byte) error) *spoolWriter {
	w := &spoolWriter{events: make(chan spoolEvent, spoolWriterQueue)}
	w.write = func(e spoolEvent) {
		if err := enqueue(e.id, e.raw); err != nil {
			glog.Warningf("frameworks telemetry spool enqueue failed: %v", err)
		}
	}
	go func() {
		for {
			select {
			case e := <-w.events:
				w.write(e)
			case <-ctx.Done():
				w.mu.Lock()
				w.stopped = true
				w.mu.Unlock()
				for {
					select {
					case e := <-w.events:
						w.write(e)
					default:
						return
					}
				}
			}
		}
	}()
	return w
}

func (w *spoolWriter) offer(e spoolEvent) {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		w.write(e)
		return
	}
	select {
	case w.events <- e:
	default:
		if w.dropped.Add(1) == 1 {
			glog.Warningf("frameworks telemetry: spool queue full; dropping events")
		}
	}
	w.mu.Unlock()
}

// send hands the event to the spool writer, which persists it before the
// network worker attempts delivery. It never waits for the disk.
func send(ctx context.Context, evt *pb.GatewayTelemetryEvent) {
	mu.RLock()
	w := writer
	network, env := networkID, environment
	mu.RUnlock()
	if w == nil {
		return
	}
	evt.EventId = uuid.NewString()
	evt.SchemaVersion = 3
	// AI outcomes use their separate writer; the network projection records transcoding.
	if evt.GetAi() != nil {
		evt.SchemaVersion = 2
	}
	evt.NetworkId = network
	evt.Environment = env
	evt.ProducerKind = "gateway"
	raw, err := proto.Marshal(evt)
	if err != nil {
		glog.Warningf("frameworks telemetry: marshal event failed: %v", err)
		return
	}
	w.offer(spoolEvent{id: evt.EventId, raw: raw})
}
func EmitDecision(ctx context.Context, payload *pb.OrchestratorCandidateDecision) {
	if !Enabled() {
		return
	}
	send(ctx, &pb.GatewayTelemetryEvent{Timestamp: timestamppb.Now(), GatewayId: ctxIdent.gatewayID, GatewayRegion: ctxIdent.gatewayRegion, ClusterId: ctxIdent.clusterID, ClusterOwnerTenantId: ctxIdent.clusterOwnerTenantID, Payload: &pb.GatewayTelemetryEvent_Decision{Decision: payload}})
}
func EmitSegmentResult(ctx context.Context, tenantID string, payload *pb.OrchestratorSegmentResult) {
	if !Enabled() || tenantID == "" {
		return
	}
	send(ctx, &pb.GatewayTelemetryEvent{Timestamp: timestamppb.Now(), GatewayId: ctxIdent.gatewayID, GatewayRegion: ctxIdent.gatewayRegion, ClusterId: ctxIdent.clusterID, ClusterOwnerTenantId: ctxIdent.clusterOwnerTenantID, StreamTenantId: tenantID, Payload: &pb.GatewayTelemetryEvent_SegmentResult{SegmentResult: payload}})
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
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if !strings.Contains(rawURL, "://") {
		rawURL = "//" + rawURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func decklogAllowsInsecure(mode string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "disabled", "insecure":
		return true, nil
	case "tls", "mtls":
		return false, nil
	default:
		return false, fmt.Errorf("frameworks telemetry: unsupported FRAMEWORKS_DECKLOG_TLS_MODE %q", mode)
	}
}
