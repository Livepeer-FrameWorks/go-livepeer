package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/eth"
	lpTypes "github.com/livepeer/go-livepeer/eth/types"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-livepeer/pm"
	"github.com/livepeer/go-livepeer/server"

	"github.com/golang/glog"
)

const orchestratorEndpointDiscoveryMaxBytes = 1 << 20

var orchestratorEndpointDiscoveryTimeout = 2 * time.Second

type ticketParamsValidator interface {
	ValidateTicketParams(ticketParams *pm.TicketParams) error
}

type DBOrchestratorPoolCache struct {
	store                 common.OrchestratorStore
	lpEth                 eth.LivepeerEthClient
	ticketParamsValidator ticketParamsValidator
	rm                    common.RoundsManager
	bcast                 common.Broadcaster
	orchBlacklist         []string
	discoveryTimeout      time.Duration
	ignoreCapacityCheck   bool
	useDiscoveryEndpoint  bool
	node                  *core.LivepeerNode
	network               string // stamped into the persisted discovery snapshot
	region                string // FRAMEWORKS_GATEWAY_REGION, for snapshot scoping

	// Discovery freshness, guarded by mu. usableOrchCount feeds the readiness
	// gate; refreshed reports whether at least one live refresh has completed.
	mu                    sync.RWMutex
	lastSuccessfulRefresh time.Time
	lastOrchCount         int
	usableOrchCount       int
	refreshed             bool
}

// UsableOrchCount returns the number of currently-selectable orchestrators as
// of the last successful refresh, and whether any refresh has completed yet.
// Callers (e.g. the /healthz readiness gate) should fall back to a live lookup
// when refreshed is false. It is cheap and lock-only — no DB access.
func (dbo *DBOrchestratorPoolCache) UsableOrchCount() (count int, refreshed bool) {
	dbo.mu.RLock()
	defer dbo.mu.RUnlock()
	return dbo.usableOrchCount, dbo.refreshed
}

// LastDiscoveryRefresh returns the time of the last successful non-empty
// refresh and the selectable orch count at that time, for observability.
func (dbo *DBOrchestratorPoolCache) LastDiscoveryRefresh() (time.Time, int) {
	dbo.mu.RLock()
	defer dbo.mu.RUnlock()
	return dbo.lastSuccessfulRefresh, dbo.lastOrchCount
}

// snapshotExpired reports whether the last successful refresh is older than the
// selection horizon. Used to decide whether an empty refresh may clear caps.
func (dbo *DBOrchestratorPoolCache) snapshotExpired(now time.Time) bool {
	dbo.mu.RLock()
	defer dbo.mu.RUnlock()
	if dbo.lastSuccessfulRefresh.IsZero() {
		return true
	}
	return now.Sub(dbo.lastSuccessfulRefresh) > discoverySnapshotMaxAge
}

type orchPollingInfo struct {
	level     int
	orchInfo  *net.OrchestratorInfo
	dbOrch    *common.DBOrch
	discovery json.RawMessage
}

func NewDBOrchestratorPoolCache(ctx context.Context, node *core.LivepeerNode, rm common.RoundsManager, orchBlacklist []string, discoveryTimeout time.Duration, liveAICapReportInterval time.Duration) (*DBOrchestratorPoolCache, error) {
	return DBOrchestratorPoolCacheConfig{
		Ctx:                     ctx,
		Node:                    node,
		RoundsManager:           rm,
		OrchBlacklist:           orchBlacklist,
		DiscoveryTimeout:        discoveryTimeout,
		LiveAICapReportInterval: liveAICapReportInterval,
	}.New()
}

type DBOrchestratorPoolCacheConfig struct {
	Ctx                     context.Context
	Node                    *core.LivepeerNode
	RoundsManager           common.RoundsManager
	OrchBlacklist           []string
	DiscoveryTimeout        time.Duration
	LiveAICapReportInterval time.Duration
	IgnoreCapacityCheck     bool
	UseDiscoveryEndpoint    bool
	// AsyncInitialDiscovery binds the HTTP listener immediately by running the
	// initial discovery crawl in the background (with self-healing retry)
	// instead of blocking startup until it completes.
	AsyncInitialDiscovery bool
	// Network is stamped into the persisted discovery snapshot so a snapshot
	// from another network is never hydrated.
	Network string
	// HydratedAt is the CapturedAt of a snapshot that was hydrated into the node
	// before this cache was built. It seeds lastSuccessfulRefresh so an early
	// empty live refresh treats the hydrated caps as fresh (and doesn't clear
	// them) until either a real refresh succeeds or the snapshot ages out.
	HydratedAt time.Time
	// HydratedOrchCount is the snapshot's selectable orchestrator count. It keeps
	// LastDiscoveryRefresh aligned with the readiness/selection count until live
	// discovery replaces it.
	HydratedOrchCount int
}

func (cfg DBOrchestratorPoolCacheConfig) New() (*DBOrchestratorPoolCache, error) {
	node := cfg.Node
	if node.Eth == nil {
		return nil, fmt.Errorf("could not create DBOrchestratorPoolCache: LivepeerEthClient is nil")
	}

	dbo := &DBOrchestratorPoolCache{
		store:                 node.Database,
		lpEth:                 node.Eth,
		ticketParamsValidator: node.Sender,
		rm:                    cfg.RoundsManager,
		bcast:                 core.NewBroadcaster(node),
		orchBlacklist:         cfg.OrchBlacklist,
		discoveryTimeout:      cfg.DiscoveryTimeout,
		ignoreCapacityCheck:   cfg.IgnoreCapacityCheck,
		useDiscoveryEndpoint:  cfg.UseDiscoveryEndpoint,
		node:                  node,
		network:               cfg.Network,
		region:                strings.TrimSpace(os.Getenv("FRAMEWORKS_GATEWAY_REGION")),
	}

	// If the node was hydrated from a snapshot before this cache was built, seed
	// the freshness state from the snapshot's capture time. This keeps the
	// hydrated caps from being wiped by the first empty live refresh (via
	// snapshotExpired) and surfaces a sensible LastDiscoveryRefresh until live
	// discovery replaces it. Safe without a lock: no goroutine has started yet.
	if !cfg.HydratedAt.IsZero() {
		dbo.lastSuccessfulRefresh = cfg.HydratedAt
		dbo.lastOrchCount = cfg.HydratedOrchCount
	}

	cacheOrchestrators := func() error {
		if err := dbo.cacheTranscoderPool(); err != nil {
			return err
		}

		if err := dbo.cacheOrchestratorStake(); err != nil {
			return err
		}

		if err := dbo.pollOrchestratorInfo(cfg.Ctx, cfg.LiveAICapReportInterval); err != nil {
			return err
		}
		return nil
	}

	if node.OrchestratorPool != nil || cfg.AsyncInitialDiscovery {
		// Don't block startup: bind the listener immediately and crawl in the
		// background. Retry the full chain with bounded backoff until it reaches
		// pollOrchestratorInfo and installs the refresh ticker — otherwise an
		// early discovery error (e.g. one bad eth RPC) would never reach the
		// ticker and permanently wedge a not-ready node.
		go func() {
			bo := backoff.NewExponentialBackOff()
			bo.MaxInterval = 2 * time.Minute
			bo.MaxElapsedTime = 0 // retry until success or ctx cancellation
			for {
				if cfg.Ctx.Err() != nil {
					return
				}
				if err := cacheOrchestrators(); err == nil {
					// Ticker is now running; periodic refresh self-heals from here.
					return
				} else {
					clog.Errorf(cfg.Ctx, "Initial orchestrator discovery failed, retrying: %v", err)
				}
				select {
				case <-cfg.Ctx.Done():
					return
				case <-time.After(bo.NextBackOff()):
				}
			}
		}()
	} else {
		// We don't have yet Orchestrator Pool, so we need to fetch it synchronously here
		return dbo, cacheOrchestrators()
	}

	return dbo, nil
}

func (dbo *DBOrchestratorPoolCache) getURLs() ([]*url.URL, error) {
	orchs, err := dbo.selectableOrchs()
	if err != nil || len(orchs) <= 0 {
		return nil, err
	}

	var uris []*url.URL
	for _, orch := range orchs {
		if uri, err := url.Parse(orch.ServiceURI); err == nil {
			uris = append(uris, uri)
		}
	}
	return uris, nil
}

func (dbo *DBOrchestratorPoolCache) GetInfos() []common.OrchestratorLocalInfo {
	uris, _ := dbo.getURLs()
	infos := make([]common.OrchestratorLocalInfo, 0, len(uris))
	for _, uri := range uris {
		infos = append(infos, common.OrchestratorLocalInfo{URL: uri, Score: common.Score_Untrusted})
	}
	return infos
}

func (dbo *DBOrchestratorPoolCache) GetOrchestrators(ctx context.Context, numOrchestrators int, suspender common.Suspender, caps common.CapabilityComparator,
	scorePred common.ScorePred) (common.OrchestratorDescriptors, error) {

	uris, err := dbo.getURLs()
	if err != nil || len(uris) <= 0 {
		return nil, err
	}

	pred := func(info *net.OrchestratorInfo) bool {
		// Return early if no ETH address is specified
		if len(info.Address) == 0 {
			return false
		}

		if err := dbo.ticketParamsValidator.ValidateTicketParams(pmTicketParams(info.TicketParams)); err != nil {
			clog.V(common.DEBUG).Infof(ctx, "invalid ticket params orch=%v err=%q",
				info.GetTranscoder(),
				err,
			)
			return false
		}

		// check if O has a valid price
		price, err := common.RatPriceInfo(info.PriceInfo)
		if err != nil {
			clog.V(common.DEBUG).Infof(ctx, "invalid price info orch=%v err=%q", info.GetTranscoder(), err)
			return false
		}
		if price == nil {
			clog.V(common.DEBUG).Infof(ctx, "no price info received for orch=%v", info.GetTranscoder())
			return false
		}
		if price.Sign() < 0 {
			clog.V(common.DEBUG).Infof(ctx, "invalid price received for orch=%v price=%v", info.GetTranscoder(), price.RatString())
			return false
		}
		return true
	}

	orchPool, err := NewOrchestratorPoolWithConfig(OrchestratorPoolConfig{
		Broadcaster:         dbo.bcast,
		URIs:                uris,
		Pred:                pred,
		Score:               common.Score_Untrusted,
		OrchBlacklist:       dbo.orchBlacklist,
		DiscoveryTimeout:    dbo.discoveryTimeout,
		IgnoreCapacityCheck: dbo.ignoreCapacityCheck,
		ExtraNodes:          dbo.bcast.ExtraNodes(),
	})
	if err != nil {
		return nil, err
	}
	orchInfos, err := orchPool.GetOrchestrators(ctx, numOrchestrators, suspender, caps, scorePred)
	if err != nil || len(orchInfos) <= 0 {
		return nil, err
	}

	return orchInfos, nil
}

func (dbo *DBOrchestratorPoolCache) Size() int {
	count, _ := dbo.store.OrchCount(
		&common.DBOrchFilter{
			CurrentRound:   dbo.rm.LastInitializedRound(),
			UpdatedLastDay: true,
		},
	)
	return count
}

func (dbo *DBOrchestratorPoolCache) SizeWith(scorePred common.ScorePred) int {
	if scorePred(common.Score_Untrusted) {
		return dbo.Size()
	}
	return 0
}

func (dbo *DBOrchestratorPoolCache) cacheTranscoderPool() error {
	orchestrators, err := dbo.lpEth.TranscoderPool()
	if err != nil {
		return fmt.Errorf("Could not refresh DB list of orchestrators: %v", err)
	}

	for _, o := range orchestrators {
		if err := dbo.store.UpdateOrch(ethOrchToDBOrch(o)); err != nil {
			glog.Errorf("Unable to update orchestrator %v in DB: %v", o.Address.Hex(), err)
		}
	}

	return nil
}

func (dbo *DBOrchestratorPoolCache) cacheOrchestratorStake() error {
	orchs, err := dbo.store.SelectOrchs(
		&common.DBOrchFilter{
			CurrentRound: dbo.rm.LastInitializedRound(),
		},
	)
	if err != nil {
		return fmt.Errorf("could not retrieve orchestrators from DB: %v", err)
	}

	resc, errc := make(chan *common.DBOrch, len(orchs)), make(chan error, len(orchs))
	timeout := getOrchestratorTimeoutLoop // Needs to be same or longer than GRPCConnectTimeout in server/rpc.go
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	currentRound := dbo.rm.LastInitializedRound()

	getStake := func(o *common.DBOrch) {
		ep, err := dbo.lpEth.GetTranscoderEarningsPoolForRound(ethcommon.HexToAddress(o.EthereumAddr), currentRound)
		if err != nil {
			errc <- err
			return
		}

		stakeFp, err := common.BaseTokenAmountToFixed(ep.TotalStake)
		if err != nil {
			errc <- err
			return
		}
		o.Stake = stakeFp

		resc <- o
	}

	for _, o := range orchs {
		go getStake(o)
	}

	for i := 0; i < len(orchs); i++ {
		select {
		case res := <-resc:
			if err := dbo.store.UpdateOrch(res); err != nil {
				glog.Error("Error updating Orchestrator in DB: ", err)
			}
		case err := <-errc:
			glog.Errorln(err)
		case <-ctx.Done():
			glog.Info("Done fetching stake for orchestrators, context timeout")
			return nil
		}
	}

	return nil
}

func (dbo *DBOrchestratorPoolCache) pollOrchestratorInfo(ctx context.Context, liveAICapReportInterval time.Duration) error {
	if err := dbo.cacheOrchInfos(); err != nil {
		glog.Errorf("unable to poll orchestrator info: %v", err)
		return err
	}

	ticker := time.NewTicker(liveAICapReportInterval)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := dbo.cacheOrchInfos(); err != nil {
					glog.Errorf("unable to poll orchestrator info: %v", err)
				}
			}
		}
	}()

	return nil
}

func (dbo *DBOrchestratorPoolCache) cacheOrchInfos() error {
	//get list of orchestrators to poll info for.  If -orchAddr or -orchWebhookUrl is used it will
	//limit the set of orchestrators polled to those specified.
	var orchs []common.OrchestratorLocalInfo
	if dbo.node.OrchestratorPool != nil {
		orchs = dbo.node.OrchestratorPool.GetInfos()
		glog.Infof("Using orchestrator pool with %d orchestrators", len(orchs))
	}
	if len(orchs) == 0 {
		// Pool is nil, or it returned nothing yet (e.g. during async initial
		// discovery, before the DB cache pool is wired up / populated). Fall
		// back to the registered orchestrators in the DB so the crawl always
		// has targets rather than polling zero orchestrators.
		dbOrchs, err := dbo.store.SelectOrchs(
			&common.DBOrchFilter{
				CurrentRound: dbo.rm.LastInitializedRound(),
			},
		)
		if err != nil {
			return fmt.Errorf("could not retrieve orchestrators from DB: %v", err)
		}

		for _, o := range dbOrchs {
			url, err := parseURI(o.ServiceURI)
			if err != nil {
				continue
			}
			orchs = append(orchs, common.OrchestratorLocalInfo{URL: url})
		}

		glog.Infof("Using DB orchestrator pool with %d orchestrators", len(orchs))
	}

	nodesPerOrch := dbo.bcast.ExtraNodes()
	// Each base orchestrator can contribute itself plus up to nodesPerOrch first-level advertised nodes.
	maxOrchs := len(orchs) * (nodesPerOrch + 1)
	resc, errc := make(chan orchPollingInfo, maxOrchs), make(chan error, maxOrchs)
	timeout := getOrchestratorTimeoutLoop // Needs to be same or longer than GRPCConnectTimeout in server/rpc.go
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	getOrchInfoRPC := serverGetOrchInfo
	if pool, ok := dbo.node.OrchestratorPool.(*orchestratorPool); ok && pool.getOrchInfo != nil {
		getOrchInfoRPC = pool.getOrchInfo
	}

	getOrchInfo := func(orch common.OrchestratorLocalInfo, level int) {
		uri, err := parseURI(orch.URL.String())
		if err != nil {
			errc <- err
			return
		}
		// Do not connect if URI host is not set
		if uri.Host == "" {
			errc <- fmt.Errorf("skipping orch=%v, URI not set", orch.URL.String())
			return
		}
		start := time.Now()
		var discoveryCh chan json.RawMessage
		if dbo.useDiscoveryEndpoint {
			discoveryCh = make(chan json.RawMessage, 1)
			go func() {
				discovery, err := callOrchestratorDiscovery(ctx, uri)
				if err != nil {
					clog.V(common.DEBUG).Infof(ctx, "unable to fetch orchestrator endpoint discovery orch=%v err=%q", uri, err)
				}
				discoveryCh <- discovery
			}()
		}
		info, err := getOrchInfoRPC(ctx, dbo.bcast, uri, server.GetOrchestratorInfoParams{
			IgnoreCapacityCheck: dbo.ignoreCapacityCheck,
		})
		latency := time.Since(start)

		// Keep FrameWorks telemetry aligned with the regular orchestrator pool:
		// emit at the raw RPC result boundary so reachable orchs still show on
		// the map even if later DB-cache validation rejects the response.
		emitFrameworksDiscovery(ctx, common.OrchestratorDescriptor{
			LocalInfo: &common.OrchestratorLocalInfo{
				URL:   orch.URL,
				Score: orch.Score,
			},
		}, info, err, latency)
		if err != nil {
			errc <- err
			return
		}

		// Return early if no ETH address is specified
		if len(info.Address) == 0 {
			errc <- fmt.Errorf("missing ETH address orch=%v", info.GetTranscoder())
			return
		}

		price, err := common.RatPriceInfo(info.PriceInfo)
		if err != nil {
			errc <- fmt.Errorf("invalid price info orch=%v err=%q", info.GetTranscoder(), err)
			return
		}

		// PriceToFixed also checks if the input is nil, but this check tells us
		// which orch was missing price info
		if price == nil {
			errc <- fmt.Errorf("missing price info orch=%v", info.GetTranscoder())
			return
		}

		var dbOrch *common.DBOrch
		if info.GetTicketParams() != nil {
			dbOrch = &common.DBOrch{
				EthereumAddr: ethcommon.BytesToAddress(info.TicketParams.Recipient).Hex(),
			}

			dbOrch.PricePerPixel, err = common.PriceToFixed(price)
			if err != nil {
				errc <- err
				return
			}
		}

		var discovery json.RawMessage
		if discoveryCh != nil {
			select {
			case discovery = <-discoveryCh:
			case <-ctx.Done():
				clog.V(common.DEBUG).Infof(ctx, "skipping orchestrator endpoint discovery orch=%v err=%q", uri, ctx.Err())
			}
		}

		resc <- orchPollingInfo{
			level:     level,
			orchInfo:  info,
			dbOrch:    dbOrch,
			discovery: discovery,
		}
	}

	seen := make(map[string]bool, maxOrchs)
	numOrchs := 0
	startOrchLookup := func(orch common.OrchestratorLocalInfo, level int) {
		if orch.URL == nil {
			return
		}
		key := orch.URL.String()
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		numOrchs++
		go getOrchInfo(orch, level)
	}

	for _, orch := range orchs {
		startOrchLookup(orch, 0)
	}

	var orchNetworkCapabilities []*common.OrchNetworkCapabilities
	for i := 0; i < numOrchs; i++ {
		select {
		case res := <-resc:
			//add response to network capabilities
			orchNetworkCapabilities = append(orchNetworkCapabilities, orchInfoToOrchNetworkCapabilities(res))

			// discover newly advertised nodes. only recurse the first level.
			if res.level == 0 && len(res.orchInfo.GetNodes()) > 0 {
				for idx, inst := range res.orchInfo.GetNodes() {
					if idx >= nodesPerOrch {
						break
					}
					u, err := parseURI(inst)
					if err != nil {
						glog.Errorf("Invalid node URL orch=%v node=%v err=%q", res.orchInfo.GetTranscoder(), inst, err)
						continue
					}
					startOrchLookup(common.OrchestratorLocalInfo{URL: u, Score: common.Score_Untrusted}, res.level+1)
				}
			}

			//update db with response
			if res.dbOrch != nil {
				if err := dbo.store.UpdateOrch(res.dbOrch); err != nil {
					glog.Error("Error updating Orchestrator in DB: ", err)
				}
			}
		case err := <-errc:
			glog.Errorln(err)
		case <-ctx.Done():
			glog.Infof("Done fetching orch info for orchestrators, context timeout (fetched: %v out of %v)", i, numOrchs)
			i = numOrchs //exit loop
		}
	}

	now := time.Now()
	// Save network capabilities in LivepeerNode. Don't let a transient all-fail
	// refresh wipe hydrated/last-known caps (which would flip /healthz to 503):
	// only replace non-empty caps with an empty result once the last good
	// refresh is older than the selection horizon.
	if len(orchNetworkCapabilities) > 0 || dbo.snapshotExpired(now) {
		dbo.node.UpdateNetworkCapabilities(orchNetworkCapabilities)
	}

	// On a successful, non-empty refresh: record freshness/readiness counters
	// and persist a snapshot so a restart can hydrate before discovery completes.
	if len(orchNetworkCapabilities) > 0 {
		dbo.recordRefresh(now, orchNetworkCapabilities)
	}

	// Report AI container capacity metrics
	reportAICapacityFromNetworkCapabilities(orchNetworkCapabilities)

	return nil
}

// selectableOrchs returns the orchestrators serving discovery would currently
// select — the same filter getURLs uses. The persisted snapshot and the
// usable-orch readiness count are both built from this so they never claim more
// than selection can actually use.
func (dbo *DBOrchestratorPoolCache) selectableOrchs() ([]*common.DBOrch, error) {
	return dbo.store.SelectOrchs(
		&common.DBOrchFilter{
			CurrentRound:   dbo.rm.LastInitializedRound(),
			UpdatedLastDay: true,
		},
	)
}

// recordRefresh updates freshness/readiness counters and best-effort persists a
// snapshot after a successful non-empty discovery refresh.
func (dbo *DBOrchestratorPoolCache) recordRefresh(now time.Time, caps []*common.OrchNetworkCapabilities) {
	selectable, err := dbo.selectableOrchs()
	if err != nil {
		glog.Errorf("discovery: could not read selectable orchestrators for snapshot: %v", err)
		selectable = nil
	}

	dbo.mu.Lock()
	dbo.lastSuccessfulRefresh = now
	dbo.lastOrchCount = len(selectable)
	dbo.usableOrchCount = len(selectable)
	dbo.refreshed = true
	dbo.mu.Unlock()

	if monitor.Enabled {
		monitor.DiscoveryRefresh(now, len(selectable))
	}

	dbo.persistSnapshot(now, selectable, caps)
}

// persistSnapshot writes an identity-stamped snapshot to the local DB. It is
// best-effort: failures are logged, never fatal, and never overwrite a good
// snapshot with an empty one (callers only invoke it on a non-empty refresh).
func (dbo *DBOrchestratorPoolCache) persistSnapshot(now time.Time, orchs []*common.DBOrch, caps []*common.OrchNetworkCapabilities) {
	db := dbo.node.Database
	if db == nil {
		return
	}
	var chainID string
	if id, err := db.ChainID(); err != nil {
		glog.Errorf("discovery: skipping snapshot persist, could not read chainID: %v", err)
		return
	} else if id != nil {
		chainID = id.String()
	}
	var broadcaster string
	if dbo.bcast != nil {
		broadcaster = dbo.bcast.Address().Hex()
	}
	// Identity is the safety boundary for hydration: a snapshot with an unknown
	// chain/network/gateway can never be trusted on load, so don't write one.
	// Skip (and log) rather than persisting a snapshot that validation will
	// silently reject later.
	if chainID == "" || dbo.network == "" || broadcaster == "" {
		glog.Warningf("discovery: skipping snapshot persist, incomplete identity (chain=%q network=%q broadcaster=%q)", chainID, dbo.network, broadcaster)
		return
	}
	snap := newDiscoverySnapshot(chainID, dbo.network, broadcaster, dbo.region, now, orchs, caps)
	raw, err := marshalDiscoverySnapshot(snap)
	if err != nil {
		glog.Errorf("discovery: could not marshal snapshot: %v", err)
		return
	}
	if err := db.UpdateNetworkCapabilitiesSnapshot(raw); err != nil {
		glog.Errorf("discovery: could not persist snapshot: %v", err)
	}
}

func reportAICapacityFromNetworkCapabilities(orchNetworkCapabilities []*common.OrchNetworkCapabilities) {
	if !monitor.Enabled {
		return
	}
	// Build structured capacity data
	modelCapacities := make(map[string]*monitor.ModelAICapacities)

	for _, orchCap := range orchNetworkCapabilities {
		for _, price := range orchCap.CapabilitiesPrices {
			if price.Capability != uint32(core.Capability_LiveVideoToVideo) {
				continue
			}
			pricePerUnit := price.PricePerUnit
			pixelsPerUnit := price.PixelsPerUnit
			pricePerPixel := big.NewRat(pricePerUnit, pixelsPerUnit)
			monitor.LiveAIPricePerPixel(orchCap.OrchURI, pricePerPixel)
		}

		models := getModelCapsFromNetCapabilities(orchCap.Capabilities)

		for modelID, model := range models {
			if _, exists := modelCapacities[modelID]; !exists {
				modelCapacities[modelID] = &monitor.ModelAICapacities{
					ModelID:       modelID,
					Orchestrators: make(map[string]monitor.AIContainerCapacity),
				}
			}

			capacity := monitor.AIContainerCapacity{
				Idle:  int(model.Capacity),
				InUse: int(model.CapacityInUse),
			}
			modelCapacities[modelID].Orchestrators[orchCap.OrchURI] = capacity
		}
	}

	monitor.ReportAIContainerCapacity(modelCapacities)
}

func getModelCapsFromNetCapabilities(caps *net.Capabilities) map[string]*net.Capabilities_CapabilityConstraints_ModelConstraint {
	if caps == nil || caps.Constraints == nil || caps.Constraints.PerCapability == nil {
		return nil
	}
	liveAI, ok := caps.Constraints.PerCapability[uint32(core.Capability_LiveVideoToVideo)]
	if !ok {
		return nil
	}

	return liveAI.Models
}

func (dbo *DBOrchestratorPoolCache) Broadcaster() common.Broadcaster {
	return dbo.bcast
}

func parseURI(addr string) (*url.URL, error) {
	if !strings.HasPrefix(addr, "http") {
		addr = "https://" + addr
	}
	uri, err := url.ParseRequestURI(addr)
	if err != nil {
		return nil, fmt.Errorf("Could not parse orchestrator URI: %v", err)
	}
	return uri, nil
}

func ethOrchToDBOrch(orch *lpTypes.Transcoder) *common.DBOrch {
	if orch == nil {
		return nil
	}

	dbo := &common.DBOrch{
		ServiceURI:        orch.ServiceURI,
		EthereumAddr:      orch.Address.String(),
		ActivationRound:   common.ToInt64(orch.ActivationRound),
		DeactivationRound: common.ToInt64(orch.DeactivationRound),
	}

	return dbo
}

func pmTicketParams(params *net.TicketParams) *pm.TicketParams {
	if params == nil {
		return nil
	}

	return &pm.TicketParams{
		Recipient:         ethcommon.BytesToAddress(params.Recipient),
		FaceValue:         new(big.Int).SetBytes(params.FaceValue),
		WinProb:           new(big.Int).SetBytes(params.WinProb),
		RecipientRandHash: ethcommon.BytesToHash(params.RecipientRandHash),
		Seed:              new(big.Int).SetBytes(params.Seed),
		ExpirationBlock:   new(big.Int).SetBytes(params.ExpirationBlock),
		ExpirationParams: &pm.TicketExpirationParams{
			CreationRound:          params.ExpirationParams.GetCreationRound(),
			CreationRoundBlockHash: ethcommon.BytesToHash(params.ExpirationParams.GetCreationRoundBlockHash()),
		},
	}
}

func orchInfoToOrchNetworkCapabilities(res orchPollingInfo) *common.OrchNetworkCapabilities {
	var orch common.OrchNetworkCapabilities

	// add orch operating information if available
	info := res.orchInfo
	if info != nil {
		orch.LocalAddress = ethcommon.BytesToAddress(info.GetAddress()).Hex()
		orch.OrchURI = info.GetTranscoder()
		orch.Capabilities = info.GetCapabilities()
		orch.PriceInfo = info.GetPriceInfo()
		orch.Hardware = info.GetHardware()
		orch.CapabilitiesPrices = info.GetCapabilitiesPrices()
		if info.GetTicketParams() != nil {
			orch.Address = string(ethcommon.BytesToAddress(info.TicketParams.Recipient).Hex())
		}
	}
	orch.Discovery = res.discovery

	return &orch
}

func callOrchestratorDiscovery(ctx context.Context, orchURI *url.URL) (json.RawMessage, error) {
	if orchURI == nil {
		return nil, fmt.Errorf("missing orchestrator URI")
	}
	if orchURI.Host == "" {
		return nil, fmt.Errorf("missing host in orchestrator URI %q", orchURI.String())
	}

	discoveryURL := orchURI.JoinPath("discovery")
	reqCtx, cancel := context.WithTimeout(ctx, orchestratorEndpointDiscoveryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, discoveryURL.String(), nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: orchestratorEndpointDiscoveryTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("endpoint discovery returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, orchestratorEndpointDiscoveryMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > orchestratorEndpointDiscoveryMaxBytes {
		return nil, fmt.Errorf("endpoint discovery response exceeds %d bytes", orchestratorEndpointDiscoveryMaxBytes)
	}

	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid endpoint discovery JSON")
	}

	return json.RawMessage(body), nil
}
