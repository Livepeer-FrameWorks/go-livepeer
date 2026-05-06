package discovery

import (
	"container/heap"
	"context"
	"encoding/hex"
	"errors"
	"math/rand"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/go-livepeer/monitor/frameworks"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-livepeer/server"

	fwpb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto"
)

var getOrchestratorTimeoutLoop = 3 * time.Second
var maxGetOrchestratorCutoffTimeout = 6 * time.Second

// TODO remove this hack and use orchestratorPool.getOrchInfo
var serverGetOrchInfo = server.GetOrchestratorInfo

// OrchestratorPoolConfig groups options used to construct an orchestratorPool.
type OrchestratorPoolConfig struct {
	Broadcaster         common.Broadcaster
	URIs                []*url.URL
	Pred                func(*net.OrchestratorInfo) bool
	Score               float32
	OrchBlacklist       []string
	DiscoveryTimeout    time.Duration
	IgnoreCapacityCheck bool

	// Limits the number of additional nodes an orchestrator
	// can advertise within the GetOrchestratorInfo response.
	// Default 0.
	ExtraNodes int
}

type orchestratorPool struct {
	infos               []common.OrchestratorLocalInfo
	pred                func(info *net.OrchestratorInfo) bool
	bcast               common.Broadcaster
	orchBlacklist       []string
	discoveryTimeout    time.Duration
	ignoreCapacityCheck bool
	node                core.LivepeerNode
	extraNodes          int
	getOrchInfo         func(context.Context, common.Broadcaster, *url.URL, server.GetOrchestratorInfoParams) (*net.OrchestratorInfo, error)
}

func NewOrchestratorPool(bcast common.Broadcaster, uris []*url.URL, score float32, orchBlacklist []string, discoveryTimeout time.Duration) *orchestratorPool {
	pool, err := NewOrchestratorPoolWithConfig(OrchestratorPoolConfig{
		Broadcaster:      bcast,
		URIs:             uris,
		Score:            score,
		OrchBlacklist:    orchBlacklist,
		DiscoveryTimeout: discoveryTimeout,
		ExtraNodes:       bcast.ExtraNodes(),
	})
	if err != nil {
		glog.Error(err.Error())
		return &orchestratorPool{}
	}
	return pool
}

func NewOrchestratorPoolWithPred(bcast common.Broadcaster, addresses []*url.URL,
	pred func(*net.OrchestratorInfo) bool, score float32, orchBlacklist []string, discoveryTimeout time.Duration) *orchestratorPool {
	pool, err := NewOrchestratorPoolWithConfig(OrchestratorPoolConfig{
		Broadcaster:      bcast,
		URIs:             addresses,
		Pred:             pred,
		Score:            score,
		OrchBlacklist:    orchBlacklist,
		DiscoveryTimeout: discoveryTimeout,
		ExtraNodes:       bcast.ExtraNodes(),
	})
	if err != nil {
		glog.Error(err.Error())
		return &orchestratorPool{}
	}
	return pool
}

func NewOrchestratorPoolWithConfig(cfg OrchestratorPoolConfig) (*orchestratorPool, error) {
	if len(cfg.URIs) == 0 {
		return nil, errors.New("orchestrator pool config must contain at least one URI")
	}

	infos := make([]common.OrchestratorLocalInfo, 0, len(cfg.URIs))
	for _, uri := range cfg.URIs {
		infos = append(infos, common.OrchestratorLocalInfo{URL: uri, Score: cfg.Score})
	}

	return &orchestratorPool{
		infos:               infos,
		pred:                cfg.Pred,
		bcast:               cfg.Broadcaster,
		orchBlacklist:       cfg.OrchBlacklist,
		discoveryTimeout:    cfg.DiscoveryTimeout,
		ignoreCapacityCheck: cfg.IgnoreCapacityCheck,
		extraNodes:          cfg.ExtraNodes,
		getOrchInfo:         serverGetOrchInfo,
	}, nil
}

func (o *orchestratorPool) GetInfos() []common.OrchestratorLocalInfo {
	return o.infos
}

func (o *orchestratorPool) GetOrchestrators(ctx context.Context, numOrchestrators int, suspender common.Suspender, caps common.CapabilityComparator,
	scorePred common.ScorePred) (common.OrchestratorDescriptors, error) {

	var seenMu sync.Mutex
	nodesPerOrch := o.extraNodes
	seen := make(map[string]bool, len(o.infos)*nodesPerOrch)
	linfos := make([]*common.OrchestratorLocalInfo, 0, len(o.infos))
	for i, _ := range o.infos {
		if scorePred(o.infos[i].Score) {
			linfos = append(linfos, &o.infos[i])
			seen[o.infos[i].URL.String()] = true
		}
	}

	numAvailableOrchs := len(linfos)
	maxOrchNodes := numAvailableOrchs * (nodesPerOrch + 1)
	numOrchestrators = min(maxOrchNodes, numOrchestrators)

	if numOrchestrators < 0 {
		return common.OrchestratorDescriptors{}, nil
	}

	// The following allows us to avoid capability check for jobs that only
	// depend on "legacy" features, since older orchestrators support these
	// features without capability discovery. This enables interop between
	// older orchestrators and newer orchestrators as long as the job only
	// requires the legacy feature set.
	//
	// When / if it's justified to completely break interop with older
	// orchestrators, then we can probably remove this check and work with
	// the assumption that all orchestrators support capability discovery.
	legacyCapsOnly := caps.LegacyOnly()

	isBlacklisted := func(info *net.OrchestratorInfo) bool {
		for _, blacklisted := range o.orchBlacklist {
			if strings.TrimPrefix(blacklisted, "0x") == strings.ToLower(hex.EncodeToString(info.Address)) {
				return true
			}
		}
		return false
	}

	isCompatible := func(info *net.OrchestratorInfo) bool {
		if o.pred != nil && !o.pred(info) {
			return false
		}
		// Legacy features already have support on the orchestrator.
		// Capabilities can be omitted in this case for older orchestrators.
		// Otherwise, capabilities are required to be present.
		if info.Capabilities == nil {
			if legacyCapsOnly {
				return true
			}
			return false
		}
		return caps.CompatibleWith(info.Capabilities)
	}
	// Pre-declare for recursion
	var getOrchInfo func(ctx context.Context, od common.OrchestratorDescriptor, level int, infoCh chan common.OrchestratorDescriptor, errCh chan error, allOrchInfoCh chan common.OrchestratorDescriptor)

	getOrchInfo = func(ctx context.Context, od common.OrchestratorDescriptor, level int, infoCh chan common.OrchestratorDescriptor, errCh chan error, allOrchInfoCh chan common.OrchestratorDescriptor) {
		start := time.Now()
		info, err := o.getOrchInfo(ctx, o.bcast, od.LocalInfo.URL, server.GetOrchestratorInfoParams{
			Caps:                caps.ToNetCapabilities(),
			IgnoreCapacityCheck: o.ignoreCapacityCheck,
		})
		latency := time.Since(start)
		clog.V(common.DEBUG).Infof(ctx, "Received GetOrchInfo RPC Response from uri=%v, latency=%v", od.LocalInfo.URL, latency)
		doingWork := info != nil && info.Transcoder != ""

		// FrameWorks gateway telemetry: emit per-attempt discovery observation
		// (success or failure) and, on success, per-instance state. This is
		// the result/error boundary of the per-orch RPC; emitting here means
		// failed dials are durable in periscope.orchestrator_discovery_samples
		// instead of being lost in the success-only `discovery_results` rollup.
		// No-op when running outside FrameWorks.
		if frameworks.Enabled() {
			emitFrameworksDiscovery(ctx, od, info, err, latency)
		}
		orchDescr := common.OrchestratorDescriptor{
			LocalInfo: &common.OrchestratorLocalInfo{
				URL:     od.LocalInfo.URL,
				Score:   od.LocalInfo.Score,
				Latency: &latency,
			},
			RemoteInfo: info,
		}
		if doingWork {
			allOrchInfoCh <- orchDescr
		}

		// discover newly advertised nodes. only recurse the first level for now.
		if level == 0 && info != nil && len(info.Nodes) > 0 {
			for i, inst := range info.Nodes {
				if i >= nodesPerOrch {
					break
				}
				seenMu.Lock()
				alreadySeen := seen[inst]
				if !alreadySeen {
					seen[inst] = true
				}
				seenMu.Unlock()
				if alreadySeen {
					continue
				}
				// haven't seen this one yet so lets continue
				u, err := url.Parse(inst)
				if err != nil {
					clog.Info(ctx, "Invalid node URL", "orch", od.LocalInfo.URL, "node", inst)
					continue
				}
				newOd := common.OrchestratorDescriptor{
					LocalInfo: &common.OrchestratorLocalInfo{URL: u, Score: od.LocalInfo.Score},
				}
				go getOrchInfo(ctx, newOd, level+1, infoCh, errCh, allOrchInfoCh)
			}
		}

		if err == nil && !isBlacklisted(info) && isCompatible(info) && doingWork {
			infoCh <- orchDescr
			return
		}

		clog.V(common.DEBUG).Infof(ctx, "Discovery unsuccessful for orchestrator %s, err=%v", od.LocalInfo.URL.String(), err)
		if err != nil && !errors.Is(err, context.Canceled) {
			if monitor.Enabled {
				monitor.LogDiscoveryError(ctx, od.LocalInfo.URL.String(), err.Error())
			}
		}
		errCh <- err
	}

	var ods common.OrchestratorDescriptors
	suspendedInfos := newSuspensionQueue()
	timedOut := false
	nbResp := 0
	odCh := make(chan common.OrchestratorDescriptor, maxOrchNodes)
	allOrchDescrCh := make(chan common.OrchestratorDescriptor, maxOrchNodes)
	errCh := make(chan error, maxOrchNodes)

	// Shuffle and create O descriptor
	for _, i := range rand.Perm(numAvailableOrchs) {
		if i >= maxOrchNodes {
			// prevents channel deadlocks when maxOrchNodes < numAvailableOrchs
			break
		}
		go getOrchInfo(ctx, common.OrchestratorDescriptor{linfos[i], nil}, 0, odCh, errCh, allOrchDescrCh)
	}

	// use a timer to time out the entire get info loop below
	cutoffTimer := time.NewTimer(maxGetOrchestratorCutoffTimeout)
	defer cutoffTimer.Stop()

	// try to wait for orchestrators until at least 1 is found (with the exponential backoff timeout)
	timeout := o.discoveryTimeout
	timer := time.NewTimer(timeout)

	// nbResp < maxOrchNodes : responses expected, whether successful or not
	// len(ods) < numOrchestrator: successful responses needed
	for nbResp < maxOrchNodes && len(ods) < numOrchestrators && !timedOut {
		select {
		case od := <-odCh:
			if penalty := suspender.Suspended(od.RemoteInfo.Transcoder); penalty == 0 {
				ods = append(ods, od)
			} else {
				heap.Push(suspendedInfos, &suspension{od.RemoteInfo, &od, penalty})
			}

			nbResp++
		case <-errCh:
			nbResp++
		case <-timer.C:
			if len(ods) > 0 {
				timedOut = true
			}

			// At this point we already waited timeout, so need to wait another timeout to make it the increased 2 * timeout
			timer.Reset(timeout)
			timeout *= 2
			if timeout > maxGetOrchestratorCutoffTimeout {
				timeout = maxGetOrchestratorCutoffTimeout
			}
			clog.V(common.DEBUG).Infof(ctx, "No orchestrators found, increasing discovery timeout to %s", timeout)
		case <-cutoffTimer.C:
			timedOut = true
		}
	}

	// Sort available orchestrators by LocalInfo.Latency ascending.
	sort.SliceStable(ods, func(i, j int) bool {
		li := ods[i].LocalInfo
		lj := ods[j].LocalInfo
		if li == nil || li.Latency == nil {
			// treat as "large" - sort to the end
			return false
		}
		if lj == nil || lj.Latency == nil {
			return true
		}
		return *li.Latency < *lj.Latency
	})

	// consider suspended orchestrators if we have an insufficient number of non-suspended ones
	if len(ods) < numOrchestrators {
		diff := numOrchestrators - len(ods)
		for i := 0; i < diff && suspendedInfos.Len() > 0; i++ {
			od := heap.Pop(suspendedInfos).(*suspension).od
			ods = append(ods, *od)
		}
	}

	if monitor.Enabled && len(ods) > 0 {
		var discoveryResults []map[string]string
		for _, o := range ods {
			discoveryResults = append(discoveryResults, map[string]string{
				"address":    hexutil.Encode(o.RemoteInfo.Address),
				"url":        o.RemoteInfo.Transcoder,
				"latency_ms": strconv.FormatInt(o.LocalInfo.Latency.Milliseconds(), 10),
			})
		}
		monitor.SendQueueEventAsync("discovery_results", discoveryResults)
	}
	clog.Infof(ctx, "Done fetching orch info orchs=%d/%d responses=%d/%d timedOut=%t",
		len(ods), numOrchestrators, nbResp, maxOrchNodes, timedOut)
	return ods, nil
}

func (o *orchestratorPool) Size() int {
	return len(o.infos)
}

func (o *orchestratorPool) SizeWith(scorePred common.ScorePred) int {
	var size int
	for _, info := range o.infos {
		if scorePred(info.Score) {
			size++
		}
	}
	return size
}

func (o *orchestratorPool) Broadcaster() common.Broadcaster {
	return o.bcast
}

func (o *orchestratorPool) pollOrchestratorInfo(ctx context.Context) {

}

// emitFrameworksDiscovery sends per-attempt discovery + per-instance state
// events to FrameWorks Decklog. Called from getOrchInfo at the result/error
// boundary so failed dials are durable. The state emission is per-instance
// (resolved IP) — pricing/capabilities/hardware can legitimately differ
// across instances of the same orch's load-balanced pool, so the receiving
// table keys per IP, not per orch_addr. See
// docs/architecture/orchestrator-visibility.md (in the monorepo).
func emitFrameworksDiscovery(ctx context.Context, od common.OrchestratorDescriptor, info *net.OrchestratorInfo, err error, latency time.Duration) {
	if od.LocalInfo == nil || od.LocalInfo.URL == nil {
		return
	}
	urlStr := od.LocalInfo.URL.String()

	// Orch eth address resolution, in priority order:
	//   1. The info from THIS attempt (success path).
	//   2. od.RemoteInfo, the cached info from the most recent prior
	//      successful discovery in this pool — covers transient failures
	//      (e.g., orch was up last cycle, blipped this cycle). The eth
	//      address is stable across restarts so the previous one is
	//      authoritative.
	//   3. URL-derived synthetic id "url:<host>" — only used when this is
	//      the first time we've ever seen the orch and it failed. Ingest keeps
	//      those rows under the stable URL key; once this gateway has learned
	//      an eth address, later failures use the eth-address key.
	// This means once an orch has succeeded once, every subsequent failure
	// attributes correctly against its hex address — no more orphan buckets.
	orchAddr := ""
	switch {
	case info != nil && len(info.Address) > 0:
		orchAddr = hexutil.Encode(info.Address)
	case od.RemoteInfo != nil && len(od.RemoteInfo.Address) > 0:
		orchAddr = hexutil.Encode(od.RemoteInfo.Address)
	case od.LocalInfo != nil && od.LocalInfo.URL != nil:
		orchAddr = "url:" + od.LocalInfo.URL.Host
	}

	reachable := err == nil && info != nil && info.Transcoder != ""
	failureKind := ""
	failureReason := ""
	if err != nil {
		failureReason = err.Error()
		failureKind = classifyDiscoveryError(err)
	} else if !reachable {
		failureKind = "no_transcoder"
		failureReason = "orchestrator returned empty transcoder URI"
	}

	frameworks.EmitDiscoveryObserved(
		ctx,
		orchAddr,
		urlStr,
		"", // advertisedNodeURL: filled at the top-level loop, not per-attempt
		latency,
		reachable,
		true, // compatibility is decided downstream of getOrchInfo; default true here, the failure-on-incompat path doesn't reach this site
		od.LocalInfo.Score,
		failureReason,
		failureKind,
	)

	// Successful response → per-instance state event (price/capabilities/
	// hardware as observed from THIS instance behind the orch's DNS).
	if reachable {
		state := &fwpb.OrchestratorStateUpdate{
			OrchAddr:           orchAddr,
			CanonicalUrl:       info.Transcoder,
			AdvertisedNodeUrls: append([]string(nil), info.Nodes...),
			Source:             "gateway_pool",
		}
		if info.Capabilities != nil {
			// Upstream caps is bitmask + version; we don't have the human-friendly
			// names handy here. Encode the raw bitmask as a single-element
			// "caps_v<version>:<hex>" string so downstream consumers can decode it.
			state.Capabilities = capabilitiesToStrings(info.Capabilities)
		}
		if info.PriceInfo != nil {
			state.PricePerUnit = info.PriceInfo.PricePerUnit
			state.PixelsPerUnit = info.PriceInfo.PixelsPerUnit
		}
		if len(info.CapabilitiesPrices) > 0 {
			state.CapabilityPriceEntries = capabilityPriceEntries(info.CapabilitiesPrices)
		}
		if len(info.Hardware) > 0 {
			state.Hardware = hardwareSummary(info.Hardware)
		}
		frameworks.EmitStateUpdate(ctx, orchAddr, urlStr, state)
	}
}

// classifyDiscoveryError maps a getOrchInfo error to a low-cardinality
// failure_kind for periscope rollups. Coarse on purpose — fine-grained
// classification is the caller's job via failure_reason.
func classifyDiscoveryError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "context deadline exceeded"), strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "dns"):
		return "dns"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "connect:"):
		return "tcp"
	case strings.Contains(msg, "rpc"), strings.Contains(msg, "grpc"):
		return "rpc"
	case strings.Contains(msg, "context canceled"):
		return "canceled"
	default:
		return "other"
	}
}

func capabilitiesToStrings(caps *net.Capabilities) []string {
	if caps == nil {
		return nil
	}
	out := make([]string, 0, len(caps.Bitstring))
	for _, b := range caps.Bitstring {
		out = append(out, strconv.FormatUint(b, 16))
	}
	return out
}

func capabilityPriceEntries(prices []*net.PriceInfo) []*fwpb.OrchestratorCapabilityPriceEntry {
	if len(prices) == 0 {
		return nil
	}
	out := make([]*fwpb.OrchestratorCapabilityPriceEntry, 0, len(prices))
	for i, p := range prices {
		if p == nil {
			continue
		}
		// Upstream PriceInfo doesn't carry a capability id at this layer.
		// Preserve position so consumers can correlate against the orch's
		// capabilities array; capability name stays empty until upstream
		// surfaces it.
		out = append(out, &fwpb.OrchestratorCapabilityPriceEntry{
			Position:      uint32(i),
			PricePerUnit:  p.PricePerUnit,
			PixelsPerUnit: p.PixelsPerUnit,
		})
	}
	return out
}

func hardwareSummary(hw []*net.HardwareInformation) string {
	if len(hw) == 0 {
		return ""
	}
	parts := make([]string, 0, len(hw))
	for _, h := range hw {
		if h == nil {
			continue
		}
		parts = append(parts, h.Pipeline+"="+h.ModelId)
	}
	return strings.Join(parts, ",")
}
