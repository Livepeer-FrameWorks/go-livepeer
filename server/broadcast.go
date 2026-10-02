package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"

	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/go-livepeer/pm"
	"github.com/livepeer/go-tools/drivers"
	"github.com/livepeer/livepeer-data/pkg/data"
	"github.com/livepeer/livepeer-data/pkg/event"

	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/stream"
)

var refreshTimeout = 2500 * time.Millisecond
var maxDurationSec = common.MaxDuration.Seconds()

// Max threshold for # of broadcast sessions under which we will refresh the session list
var maxRefreshSessionsThreshold = 8.0

var recordSegmentsMaxTimeout = 1 * time.Minute

var BroadcastCfg = NewBroadcastConfig()
var MaxAttempts = 3

var MetadataQueue event.SimpleProducer
var MetadataPublishTimeout = 1 * time.Second

var getOrchestratorInfoRPC = GetOrchestratorInfo
var downloadSeg = core.DownloadData
var submitMultiSession = func(ctx context.Context, sess *BroadcastSession, seg *stream.HLSSegment, segPar *core.SegmentParameters,
	nonce uint64, resc chan *SubmitResult) {
	go submitSegment(ctx, sess, seg, segPar, nonce, resc)
}
var maxTranscodeAttempts = errors.New("hit max transcode attempts")

type BroadcastConfig struct {
	maxPricePerCapability map[core.Capability]map[string]*core.AutoConvertedPrice
	mu                    sync.RWMutex
}

func NewBroadcastConfig() *BroadcastConfig {
	maxPrices := make(map[core.Capability]map[string]*core.AutoConvertedPrice)
	models := make(map[string]*core.AutoConvertedPrice)
	maxPrices[core.Capability_Unused] = models
	return &BroadcastConfig{
		maxPricePerCapability: maxPrices,
	}
}

type SegFlightMetadata struct {
	startTime time.Time
	segDur    time.Duration
}

func (cfg *BroadcastConfig) MaxPrice() *big.Rat {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	//base price is capability that won't be set with specific price
	if cfg.maxPricePerCapability[core.Capability_Unused]["default"] == nil {
		return nil
	}
	return cfg.maxPricePerCapability[core.Capability_Unused]["default"].Value()
}

func (cfg *BroadcastConfig) SetMaxPrice(price *core.AutoConvertedPrice) {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	prevPrice := cfg.maxPricePerCapability[core.Capability_Unused]["default"]
	cfg.maxPricePerCapability[core.Capability_Unused]["default"] = price
	if prevPrice != nil {
		prevPrice.Stop()
	}
}

// GetCapabilitiesMaxPrice returns the max price for the given capabilities.
func (cfg *BroadcastConfig) GetCapabilitiesMaxPrice(caps common.CapabilityComparator) *big.Rat {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	if caps == nil {
		return cfg.MaxPrice()
	}
	netCaps := caps.ToNetCapabilities()
	if netCaps == nil || netCaps.Constraints == nil {
		return cfg.MaxPrice()
	}
	price := big.NewRat(0, 1)
	for capabilityInt, constraints := range netCaps.Constraints.PerCapability {
		for modelID := range constraints.Models {
			if capPrice := cfg.getCapabilityMaxPrice(core.Capability(capabilityInt), modelID); capPrice != nil {
				price = price.Add(price, capPrice)
			}
		}
	}

	// If no prices set per model, return maxPrice
	if price.Sign() == 0 {
		return cfg.MaxPrice()
	}

	return price
}

func (cfg *BroadcastConfig) getCapabilityMaxPrice(cap core.Capability, modelID string) *big.Rat {
	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	models, ok := cfg.maxPricePerCapability[cap]
	if !ok {
		// No price set for capability
		return nil
	}
	if price, modelOk := models[modelID]; modelOk && price != nil {
		return price.Value()
	}
	if defaultPrice, hasDefault := models["default"]; hasDefault && defaultPrice != nil {
		return defaultPrice.Value()
	}

	// No price set for the specific model or default
	return nil
}

func (cfg *BroadcastConfig) SetCapabilityMaxPrice(cap core.Capability, modelID string, newPrice *core.AutoConvertedPrice) {
	cfg.mu.Lock()
	defer cfg.mu.Unlock()
	if _, ok := cfg.maxPricePerCapability[cap]; !ok {
		cfg.maxPricePerCapability[cap] = make(map[string]*core.AutoConvertedPrice)
	}

	// Stop previous price subscription if it exists.
	if prevPrice, exists := cfg.maxPricePerCapability[cap][modelID]; exists && prevPrice != nil {
		prevPrice.Stop()
	}

	cfg.maxPricePerCapability[cap][modelID] = newPrice
}

type sessionsCreator func() ([]*BroadcastSession, error)
type sessionsCleanup func(sessionId string)
type SessionPool struct {
	mid core.ManifestID

	// Accessing or changing any of the below requires ownership of this mutex
	lock sync.Mutex

	sel      BroadcastSessionsSelector
	lastSess []*BroadcastSession
	sessMap  map[string]*BroadcastSession
	numOrchs int // how many orchs to request at once
	poolSize int

	refreshing bool // only allow one refresh in-flight
	finished   bool // set at stream end

	createSessions sessionsCreator
	cleanupSession sessionsCleanup
	sus            orchSuspender

	// latencyThreshold is the effective LatencyScore above which an
	// orchestrator is swapped. Zero means use SELECTOR_LATENCY_SCORE_THRESHOLD;
	// vod workloads relax it toward 1/MinSpeed so a single slow segment doesn't
	// trigger a swap.
	latencyThreshold float64
}

// latencyScoreThreshold returns the effective LatencyScore swap threshold for
// this pool, falling back to the package default when unset.
func (sp *SessionPool) latencyScoreThreshold() float64 {
	if sp.latencyThreshold > 0 {
		return sp.latencyThreshold
	}
	return SELECTOR_LATENCY_SCORE_THRESHOLD
}

// latencyThresholdForWorkload derives the LatencyScore swap threshold from the
// workload contract. LatencyScore is round-trip / segment-duration, so the
// reciprocal of the minimum sustained speed factor is the slowest round-trip a
// vod workload tolerates before swapping. live keeps the aggressive default.
func latencyThresholdForWorkload(params *core.StreamParameters) float64 {
	if params != nil && params.Workload == core.WorkloadVOD && params.MinSpeed > 0 {
		if thr := 1.0 / params.MinSpeed; thr > SELECTOR_LATENCY_SCORE_THRESHOLD {
			return thr
		}
	}
	return SELECTOR_LATENCY_SCORE_THRESHOLD
}

func NewSessionPool(mid core.ManifestID, poolSize, numOrchs int, sus orchSuspender, createSession sessionsCreator, cleanupSession sessionsCleanup,
	sel BroadcastSessionsSelector) *SessionPool {

	return &SessionPool{
		mid:            mid,
		numOrchs:       numOrchs,
		poolSize:       poolSize,
		sessMap:        make(map[string]*BroadcastSession),
		sel:            sel,
		createSessions: createSession,
		cleanupSession: cleanupSession,
		sus:            sus,
	}
}

func (sp *SessionPool) suspend(orch string) {
	poolSize := math.Max(1, float64(sp.poolSize))
	numOrchs := math.Max(1, float64(sp.numOrchs))
	penalty := int(math.Ceil(poolSize / numOrchs))
	sp.sus.suspend(orch, penalty)
}

// suspendedElsewhere reports whether sess's orchestrator carries a shared
// suspension while the pool still has other sessions to use. The caller holds
// sp.lock.
func (sp *SessionPool) suspendedElsewhere(sess *BroadcastSession) bool {
	if sp.sel.Size() == 0 && len(removeSessionFromList(sp.lastSess, sess)) == 0 {
		return false
	}
	return sp.sus.sharedSuspended(sess.Transcoder())
}

func (sp *SessionPool) unsuspend(orch string) {
	poolSize := math.Max(1, float64(sp.poolSize))
	numOrchs := math.Max(1, float64(sp.numOrchs))
	penalty := int(math.Ceil(poolSize / numOrchs))
	sp.sus.unsuspend(orch, penalty)
}

// selectIdle takes a session other than exclude from the selector, skipping
// sessions that are no longer in the pool. The caller holds no pool lock.
func (sp *SessionPool) selectIdle(ctx context.Context, exclude *BroadcastSession) *BroadcastSession {
	sp.lock.Lock()
	defer sp.lock.Unlock()
	if sp.poolSize == 0 {
		return nil
	}
	var skipped []*BroadcastSession
	defer func() {
		for _, s := range skipped {
			sp.sel.Complete(s)
		}
	}()
	for sp.sel.Size() > 0 {
		sess := sp.sel.Select(ctx)
		if sess == nil {
			return nil
		}
		if sess == exclude || sess.Transcoder() == exclude.Transcoder() {
			skipped = append(skipped, sess)
			continue
		}
		if existing, ok := sp.sessMap[sess.Transcoder()]; !ok || existing != sess {
			continue
		}
		return sess
	}
	return nil
}

// release drops one in-flight segment from a session whose submission was
// cancelled and returns the session to the selector once it is idle. A
// session that is no longer in the pool is ignored.
func (sp *SessionPool) release(sess *BroadcastSession) {
	sp.lock.Lock()
	defer sp.lock.Unlock()
	if existing, ok := sp.sessMap[sess.Transcoder()]; !ok || existing != sess {
		return
	}
	if remaining, _ := sess.popSegInFlight(); remaining > 0 {
		return
	}
	sp.lastSess = removeSessionFromList(sp.lastSess, sess)
	sp.sel.Complete(sess)
}

// promote adds a session to the reuse list consulted by selectSessions.
func (sp *SessionPool) promote(sess *BroadcastSession) {
	sp.lock.Lock()
	defer sp.lock.Unlock()
	if existing, ok := sp.sessMap[sess.Transcoder()]; !ok || existing != sess {
		return
	}
	if !includesSession(sp.lastSess, sess) {
		sp.lastSess = append(sp.lastSess, sess)
	}
}

func (sp *SessionPool) refreshSessions(ctx context.Context) {
	// A pool sized from an empty orchestrator class (for example the trusted
	// pool when discovery marks every on-chain orchestrator untrusted) has
	// nothing to discover; querying it only produces a "no orchestrators" log.
	sp.lock.Lock()
	empty := sp.poolSize <= 0 || sp.numOrchs <= 0
	sp.lock.Unlock()
	if empty {
		return
	}
	started := time.Now()
	clog.V(common.DEBUG).Infof(ctx, "Starting session refresh")
	defer func() {
		sp.lock.Lock()
		clog.V(common.DEBUG).Infof(ctx, "Ending session refresh dur=%s orchs=%d", time.Since(started),
			sp.sel.Size())
		sp.lock.Unlock()
	}()
	sp.lock.Lock()
	if sp.finished || sp.refreshing {
		sp.lock.Unlock()
		return
	}
	sp.refreshing = true
	sp.lock.Unlock()

	sp.sus.signalRefresh()

	newBroadcastSessions, err := sp.createSessions()
	if err != nil {
		sp.lock.Lock()
		sp.refreshing = false
		sp.lock.Unlock()
		return
	}

	// if newBroadcastSessions is empty, exit without refreshing list
	if len(newBroadcastSessions) <= 0 {
		sp.lock.Lock()
		sp.refreshing = false
		sp.lock.Unlock()
		return
	}

	uniqueSessions := make([]*BroadcastSession, 0, len(newBroadcastSessions))
	sp.lock.Lock()
	defer sp.lock.Unlock()

	sp.refreshing = false
	if sp.finished {
		return
	}

	for _, sess := range newBroadcastSessions {
		if _, ok := sp.sessMap[sess.OrchestratorInfo.Transcoder]; ok {
			continue
		}
		uniqueSessions = append(uniqueSessions, sess)
		sp.sessMap[sess.OrchestratorInfo.Transcoder] = sess
	}

	sp.sel.Add(uniqueSessions)
}

func includesSession(sessions []*BroadcastSession, session *BroadcastSession) bool {
	for _, sess := range sessions {
		if sess == session {
			return true
		}
	}
	return false
}

func getOrchs(sessions []*BroadcastSession) []string {
	res := make([]string, len(sessions))
	for i, sess := range sessions {
		res[i] = sess.Transcoder()
	}
	return res
}

func removeSessionFromList(sessions []*BroadcastSession, sess *BroadcastSession) []*BroadcastSession {
	var res []*BroadcastSession
	for _, ls := range sessions {
		if ls != sess {
			res = append(res, ls)
		}
	}
	return res
}

func selectSession(ctx context.Context, sessions []*BroadcastSession, exclude []*BroadcastSession, durMult int, latencyThreshold float64) *BroadcastSession {
	for _, session := range sessions {
		// A session in the exclusion list is not selectable
		if includesSession(exclude, session) {
			continue
		}

		// A session without any segments in flight and that has a latency score that meets the selector
		// threshold is selectable
		if len(session.SegsInFlight) == 0 {
			if session.LatencyScore > 0 && session.LatencyScore <= latencyThreshold {
				clog.PublicInfof(ctx,
					"Reusing Orchestrator, reason=%v",
					fmt.Sprintf(
						"performance: no segments in flight, latency score of %v < %v",
						session.LatencyScore,
						durMult,
					),
				)

				return session
			}
			clog.PublicInfof(ctx,
				"Swapping Orchestrator, reason=%v",
				fmt.Sprintf(
					"performance: no segments in flight, latency score of %v < %v",
					session.LatencyScore,
					durMult,
				),
			)
		}

		// A session with segments in flight might be selectable under certain conditions
		if len(session.SegsInFlight) > 0 {
			// The 0th segment in the slice is the oldest segment in flight since segments are appended
			// to the slice as they are sent out
			oldestSegInFlight := session.SegsInFlight[0]
			timeInFlight := time.Since(oldestSegInFlight.startTime)
			// durMult can be tuned by the caller to tighten/relax the maximum in flight time for the oldest segment
			maxTimeInFlight := time.Duration(durMult) * oldestSegInFlight.segDur

			// We're more lenient for segments <= 1s in length, since we've found the overheads to make this quite an aggressive target
			// to consistently meet, so instead set a floor of 1.5s
			if maxTimeInFlight <= 1*time.Second {
				maxTimeInFlight = 1500 * time.Millisecond
			}

			if timeInFlight < maxTimeInFlight {
				clog.PublicInfof(ctx,
					"Reusing orchestrator reason=%v",
					fmt.Sprintf(
						"performance: segments in flight, latency score of %v < %v",
						session.LatencyScore,
						durMult,
					),
				)

				return session
			}
			clog.PublicInfof(ctx,
				"Swapping Orchestrator, reason=%v",
				fmt.Sprintf(
					"performance: no segments in flight, latency score of %v < %v",
					session.LatencyScore,
					durMult,
				),
			)
		}
	}
	return nil
}

func (sp *SessionPool) selectSessions(ctx context.Context, sessionsNum int) []*BroadcastSession {
	sp.lock.Lock()
	defer sp.lock.Unlock()
	if sp.poolSize == 0 {
		return nil
	}

	checkSessions := func(m *SessionPool) bool {
		numSess := m.sel.Size()
		refreshThreshold := int(math.Min(maxRefreshSessionsThreshold, math.Ceil(float64(m.numOrchs)/2.0)))
		clog.Infof(ctx, "Checking if the session refresh is needed, numSess=%v, refreshThreshold=%v", numSess, refreshThreshold)
		if numSess < refreshThreshold {
			go m.refreshSessions(ctx)
		}
		return (numSess > 0 || len(sp.lastSess) > 0)
	}
	var selectedSessions []*BroadcastSession

	for checkSessions(sp) {
		var sess *BroadcastSession

		// Re-use last session if oldest segment is in-flight for < segDur
		gotFromLast := false
		sess = selectSession(ctx, sp.lastSess, selectedSessions, 1, sp.latencyScoreThreshold())
		if sess == nil {
			// Or try a new session from the available ones
			sess = sp.sel.Select(ctx)
		} else {
			gotFromLast = true
		}

		if sess == nil {
			// If no new sessions are available, re-use last session when oldest segment is in-flight for < 2 * segDur
			sess = selectSession(ctx, sp.lastSess, selectedSessions, 2, sp.latencyScoreThreshold())
			if sess != nil {
				gotFromLast = true
				clog.V(common.DEBUG).Infof(ctx, "No sessions in the selector for manifestID=%v re-using orch=%v with acceptable in-flight time",
					sp.mid, sess.Transcoder())
			}
		}

		// No session found, return nil
		if sess == nil {
			break
		}

		/*
			Don't select sessions no longer in the map.

			Retry if the first selected session has been removed from
			the map.  This may occur if the session is removed while
			still in the list.  To avoid a runtime search of the
			session list under lock, simply fixup the session list at
			selection time by retrying the selection.
		*/

		if _, ok := sp.sessMap[sess.Transcoder()]; ok && sp.suspendedElsewhere(sess) {
			// Another stream or gateway suspended this orchestrator in the
			// shared store; drop it while other sessions remain.
			clog.Infof(ctx, "Removing orch=%v suspended by another stream or gateway", sess.Transcoder())
			sp.cleanupSession(sess.PMSessionID)
			delete(sp.sessMap, sess.Transcoder())
			sess.SegsInFlight = nil
			sp.lastSess = removeSessionFromList(sp.lastSess, sess)
			continue
		}
		if _, ok := sp.sessMap[sess.Transcoder()]; ok {
			selectedSessions = append(selectedSessions, sess)

			if len(selectedSessions) == sessionsNum {
				break
			}
		} else {
			if gotFromLast {
				// Last session got removed from map (possibly due to a failure) so stop tracking its in-flight segments
				sess.SegsInFlight = nil
				sp.lastSess = removeSessionFromList(sp.lastSess, sess)
				clog.V(common.DEBUG).Infof(ctx, "Removing orch=%v from manifestID=%s session list", sess.Transcoder(), sp.mid)
				clog.PublicInfof(ctx, "Removing orch=%v from manifestID=%s session list", sess.Transcoder(), sp.mid)
				if monitor.Enabled {
					monitor.OrchestratorSwapped(ctx)
				}
			}
		}
	}
	if len(selectedSessions) == 0 {
		// No session found, return nil
		sp.lastSess = nil
	} else {
		for _, ls := range sp.lastSess {
			if !includesSession(selectedSessions, ls) {
				clog.V(common.DEBUG).Infof(ctx, "Swapping from orch=%v to orch=%+v for manifestID=%s", ls.Transcoder(),
					getOrchs(selectedSessions), sp.mid)
				clog.PublicInfof(ctx, "Swapping from orch=%v to orch=%+v for manifestID=%s", ls.Transcoder(),
					getOrchs(selectedSessions), sp.mid)
				if monitor.Enabled {
					monitor.OrchestratorSwapped(ctx)
				}
			}
		}
		sp.lastSess = append([]*BroadcastSession{}, selectedSessions...)
	}
	return selectedSessions
}

func (sp *SessionPool) removeSession(session *BroadcastSession) {
	sp.lock.Lock()
	defer sp.lock.Unlock()

	sp.cleanupSession(session.PMSessionID)
	delete(sp.sessMap, session.Transcoder())
}

func (sp *SessionPool) cleanup() {
	sp.lock.Lock()
	defer sp.lock.Unlock()
	sp.finished = true
	sp.lastSess = nil
	sp.sel.Clear()
	sp.sessMap = make(map[string]*BroadcastSession) // prevent segfaults
}

func (sp *SessionPool) completeSession(sess *BroadcastSession) {
	sp.lock.Lock()
	defer sp.lock.Unlock()
	if existingSess, ok := sp.sessMap[sess.Transcoder()]; ok {
		if existingSess != sess {
			// that means that sess object was removed from pool and then same
			// Orchestrator was added to the pool again
			return
		}
		sess.lock.Lock()
		defer sess.lock.Unlock()
		if len(sess.SegsInFlight) == 1 {
			sess.SegsInFlight = nil
		} else if len(sess.SegsInFlight) > 1 {
			sess.SegsInFlight = sess.SegsInFlight[1:]
			// skip returning this session back to the selector
			// we will return it later in transcodeSegment() once all in-flight segs downloaded
			return
		}

		// If the latency score meets the selector threshold, we skip giving the session back to the selector
		// because we consider it for re-use in selectSession()
		if sess.LatencyScore > 0 && sess.LatencyScore <= sp.latencyScoreThreshold() {
			return
		}

		sp.sel.Complete(sess)
	}
}

type BroadcastSessionsManager struct {
	mid core.ManifestID

	// Accessing or changing any of the below requires ownership of this mutex
	sessLock sync.Mutex

	finished bool // set at stream end

	trustedPool   *SessionPool
	untrustedPool *SessionPool

	// perf reads round-trip estimates from the shared performance store; nil
	// without Redis.
	perf *orchPerf
}

func NewSessionManager(ctx context.Context, node *core.LivepeerNode, params *core.StreamParameters) *BroadcastSessionsManager {
	if node.Capabilities != nil {
		params.Capabilities.SetMinVersionConstraint(node.Capabilities.MinVersionConstraint())
	}
	var trustedPoolSize, untrustedPoolSize float64
	if node.OrchestratorPool != nil {
		trustedPoolSize = float64(node.OrchestratorPool.SizeWith(common.ScoreAtLeast(common.Score_Trusted)))
		untrustedPoolSize = float64(node.OrchestratorPool.SizeWith(common.ScoreEqualTo(common.Score_Untrusted)))
	}
	maxInflight := common.HTTPTimeout.Seconds() / SegLen.Seconds()
	trustedNumOrchs := int(math.Min(trustedPoolSize, maxInflight*2))
	untrustedNumOrchs := int(untrustedPoolSize)
	// Durable, regionally-shared orchestrator health when configured; in-memory
	// otherwise. Scoped by workload + capability so a bad orchestrator for vod
	// doesn't exclude it for live and vice versa.
	healthStore := sharedOrchHealthStore()
	capKey := common.ProfilesNames(params.Profiles)
	susTrusted := healthStore.scoped(params.Workload, capKey)
	susUntrusted := healthStore.scoped(params.Workload, capKey)
	cleanupSession := func(sessionID string) {
		// Offchain gateways have no payment sender.
		if node.Sender != nil {
			node.Sender.CleanupSession(sessionID)
		}
	}
	createSessionsTrusted := func() ([]*BroadcastSession, error) {
		return selectOrchestrator(ctx, node, params, trustedNumOrchs, susTrusted, common.ScoreAtLeast(common.Score_Trusted), cleanupSession)
	}
	createSessionsUntrusted := func() ([]*BroadcastSession, error) {
		return selectOrchestrator(ctx, node, params, untrustedNumOrchs, susUntrusted, common.ScoreEqualTo(common.Score_Untrusted), cleanupSession)
	}
	var stakeRdr stakeReader
	if node.Eth != nil {
		stakeRdr = &storeStakeReader{store: node.Database}
	}
	// Durable performance reader for this workload+capability bucket; shared by
	// both pools so the in-process memo is reused. nil when no Redis store.
	perfReader := healthStore.perfReader(params.Workload, capKey)
	trustedSel := NewMinLSSelector(stakeRdr, 1.0, node.SelectionAlgorithm, node.OrchPerfScore, params.Capabilities)
	untrustedSel := NewMinLSSelector(stakeRdr, 1.0, node.SelectionAlgorithm, node.OrchPerfScore, params.Capabilities)
	trustedSel.perfReader = perfReader
	untrustedSel.perfReader = perfReader
	bsm := &BroadcastSessionsManager{
		mid:           params.ManifestID,
		trustedPool:   NewSessionPool(params.ManifestID, int(trustedPoolSize), trustedNumOrchs, susTrusted, createSessionsTrusted, cleanupSession, trustedSel),
		untrustedPool: NewSessionPool(params.ManifestID, int(untrustedPoolSize), untrustedNumOrchs, susUntrusted, createSessionsUntrusted, cleanupSession, untrustedSel),
	}
	if p, ok := perfReader.(*orchPerf); ok {
		bsm.perf = p
	}
	latencyThreshold := latencyThresholdForWorkload(params)
	bsm.trustedPool.latencyThreshold = latencyThreshold
	bsm.untrustedPool.latencyThreshold = latencyThreshold
	bsm.trustedPool.refreshSessions(ctx)
	bsm.untrustedPool.refreshSessions(ctx)
	return bsm
}

func (bsm *BroadcastSessionsManager) suspendAndRemoveOrch(sess *BroadcastSession) {
	if sess.OrchestratorScore == common.Score_Untrusted {
		bsm.untrustedPool.suspend(sess.OrchestratorInfo.GetTranscoder())
		bsm.untrustedPool.removeSession(sess)
	} else {
		bsm.trustedPool.suspend(sess.OrchestratorInfo.GetTranscoder())
		bsm.trustedPool.removeSession(sess)
	}
}

func (bsm *BroadcastSessionsManager) poolFor(sess *BroadcastSession) *SessionPool {
	if sess.OrchestratorScore == common.Score_Untrusted {
		return bsm.untrustedPool
	}
	return bsm.trustedPool
}

// unsuspendOrch lifts the suspension suspendAndRemoveOrch placed on orch. The
// session itself stays removed; discovery re-adds the orchestrator on the next
// refresh.
func (bsm *BroadcastSessionsManager) unsuspendOrch(orch string) {
	for _, pool := range []*SessionPool{bsm.trustedPool, bsm.untrustedPool} {
		pool.unsuspend(orch)
	}
}

// sessionWaitInterval is the pause between discovery refreshes while a
// segment waits for its first orchestrator session.
var sessionWaitInterval = 250 * time.Millisecond

// waitForSessions refreshes discovery and retries selection until a session
// is available or ctx ends. A refresh already in flight is waited out by
// polling selection.
func (bsm *BroadcastSessionsManager) waitForSessions(ctx context.Context) []*BroadcastSession {
	waitStart := time.Now()
	for {
		refreshed := make(chan struct{})
		go func() {
			defer close(refreshed)
			bsm.trustedPool.refreshSessions(ctx)
			bsm.untrustedPool.refreshSessions(ctx)
		}()
		select {
		case <-refreshed:
		case <-ctx.Done():
			return nil
		}
		if sessions := bsm.selectSessions(ctx); len(sessions) > 0 {
			clog.Infof(ctx, "Orchestrator session available after waiting %s", time.Since(waitStart))
			return sessions
		}
		select {
		case <-time.After(sessionWaitInterval):
		case <-ctx.Done():
			return nil
		}
	}
}

// selectHedgeSession returns a session other than exclude that has no
// segments in flight, or nil when none is available.
func (bsm *BroadcastSessionsManager) selectHedgeSession(ctx context.Context, exclude *BroadcastSession) *BroadcastSession {
	bsm.sessLock.Lock()
	defer bsm.sessLock.Unlock()
	if sess := bsm.untrustedPool.selectIdle(ctx, exclude); sess != nil {
		return sess
	}
	return bsm.trustedPool.selectIdle(ctx, exclude)
}

// releaseSession hands back a session whose submission was cancelled because
// another orchestrator answered the segment first.
func (bsm *BroadcastSessionsManager) releaseSession(sess *BroadcastSession) {
	bsm.sessLock.Lock()
	defer bsm.sessLock.Unlock()
	bsm.poolFor(sess).release(sess)
}

// promoteSession makes a hedge session that answered a segment eligible for
// reuse on the next segment, like a session chosen by selectSessions.
func (bsm *BroadcastSessionsManager) promoteSession(sess *BroadcastSession) {
	bsm.sessLock.Lock()
	defer bsm.sessLock.Unlock()
	bsm.poolFor(sess).promote(sess)
}

// roundTripP90 is the orchestrator's estimated 90th-percentile round trip from
// the shared performance store.
func (bsm *BroadcastSessionsManager) roundTripP90(sess *BroadcastSession) (time.Duration, bool) {
	if bsm.perf == nil {
		return 0, false
	}
	return bsm.perf.roundTripP90(sess.Transcoder())
}

// recordRoundTrip suspends an orchestrator after slowStrikeLimit consecutive
// segments whose round trip exceeded the segment duration. The round-trip
// sample itself already lowered the orchestrator's shared performance score.
func (bsm *BroadcastSessionsManager) recordRoundTrip(ctx context.Context, sess *BroadcastSession, seg *stream.HLSSegment, roundTrip time.Duration) {
	segDur := time.Duration(seg.Duration * float64(time.Second))
	if segDur <= 0 {
		return
	}
	sess.lock.Lock()
	if roundTrip <= segDur {
		sess.slowStrikes = 0
		sess.lock.Unlock()
		return
	}
	sess.slowStrikes++
	strikes := sess.slowStrikes
	sess.lock.Unlock()
	if strikes < slowStrikeLimit {
		return
	}
	clog.Warningf(ctx, "Suspending orch=%s: %d consecutive round trips longer than the segment duration roundTrip=%s segDur=%s",
		sess.Transcoder(), strikes, roundTrip, segDur)
	bsm.suspendAndRemoveOrch(sess)
}

func (bs *BroadcastSession) pushSegInFlight(seg *stream.HLSSegment) {
	bs.lock.Lock()
	bs.SegsInFlight = append(bs.SegsInFlight,
		SegFlightMetadata{
			startTime: time.Now(),
			segDur:    time.Duration(seg.Duration * float64(time.Second)),
		})
	bs.lock.Unlock()
}

// Pop a SegFlightMetadata from a session's SegsInFlight
// Returns the end length of a session's SegsInFlight and the popped SegFlightMetadata
func (bs *BroadcastSession) popSegInFlight() (int, SegFlightMetadata) {
	bs.lock.Lock()
	defer bs.lock.Unlock()

	if len(bs.SegsInFlight) == 0 {
		return 0, SegFlightMetadata{}
	}

	sm := bs.SegsInFlight[0]
	bs.SegsInFlight = bs.SegsInFlight[1:]
	return len(bs.SegsInFlight), sm
}

// selects number of sessions to use according to current algorithm
func (bsm *BroadcastSessionsManager) selectSessions(ctx context.Context) []*BroadcastSession {
	bsm.sessLock.Lock()
	defer bsm.sessLock.Unlock()

	// Default to selecting from untrusted pool
	sessions := bsm.untrustedPool.selectSessions(ctx, 1)
	if len(sessions) == 0 {
		sessions = bsm.trustedPool.selectSessions(ctx, 1)
	}

	return sessions
}

func (bsm *BroadcastSessionsManager) cleanup(ctx context.Context) {
	// send tear down signals to each orchestrator session to free resources
	for _, sess := range bsm.untrustedPool.sessMap {
		bsm.completeSession(ctx, sess, true)
	}
	for _, sess := range bsm.trustedPool.sessMap {
		bsm.completeSession(ctx, sess, true)
	}

	bsm.sessLock.Lock()
	defer bsm.sessLock.Unlock()
	bsm.finished = true

	bsm.trustedPool.cleanup()
	bsm.untrustedPool.cleanup()
}

// the caller needs to ensure bsm.sessLock is acquired before calling this.
func (bsm *BroadcastSessionsManager) completeSessionUnsafe(ctx context.Context, sess *BroadcastSession, tearDown bool) {
	if tearDown {
		go func() {
			if err := EndTranscodingSession(ctx, sess); err != nil {
				clog.Errorf(ctx, "Error completing transcoding session: %q", err)
			}
		}()
	}
	if sess.OrchestratorScore == common.Score_Untrusted {
		bsm.untrustedPool.completeSession(sess)
	} else if sess.OrchestratorScore == common.Score_Trusted {
		bsm.trustedPool.completeSession(sess)
	} else {
		panic("shouldn't happen")
	}
}

func (bsm *BroadcastSessionsManager) completeSession(ctx context.Context, sess *BroadcastSession, tearDown bool) {
	bsm.sessLock.Lock()
	defer bsm.sessLock.Unlock()
	bsm.completeSessionUnsafe(ctx, sess, tearDown)
}

func (bsm *BroadcastSessionsManager) hasOrchestratorPool() bool {
	if bsm == nil {
		return false
	}
	bsm.sessLock.Lock()
	defer bsm.sessLock.Unlock()
	return bsm.trustedPool.poolSize > 0 ||
		bsm.untrustedPool.poolSize > 0 ||
		bsm.trustedPool.numOrchs > 0 ||
		bsm.untrustedPool.numOrchs > 0
}

func selectOrchestrator(ctx context.Context, n *core.LivepeerNode, params *core.StreamParameters, count int, sus common.Suspender,
	scorePred common.ScorePred, cleanupSession sessionsCleanup) ([]*BroadcastSession, error) {

	if n.OrchestratorPool == nil {
		clog.Infof(ctx, "No orchestrators specified; not transcoding")
		return nil, errDiscovery
	}

	ods, err := n.OrchestratorPool.GetOrchestrators(ctx, count, sus, params.Capabilities, scorePred)

	if len(ods) <= 0 {
		clog.InfofErr(ctx, "No orchestrators found; not transcoding", err)
		return nil, errNoOrchs
	}
	if err != nil {
		return nil, err
	}

	var sessions []*BroadcastSession

	for _, od := range ods {
		var (
			sessionID    string
			balance      Balance
			ticketParams *pm.TicketParams
		)

		if od.RemoteInfo.AuthToken == nil {
			clog.Errorf(ctx, "Missing auth token orch=%v", od.RemoteInfo.Transcoder)
			continue
		}

		if n.Sender != nil {
			if od.RemoteInfo.TicketParams == nil {
				clog.Errorf(ctx, "Missing ticket params orch=%v", od.RemoteInfo.Transcoder)
				continue
			}

			ticketParams = pmTicketParams(od.RemoteInfo.TicketParams)
			sessionID = n.Sender.StartSession(*ticketParams)

			if n.Balances != nil {
				balance = core.NewBalance(ticketParams.Recipient, core.ManifestID(od.RemoteInfo.AuthToken.SessionId), n.Balances)
			}
		}

		var orchOS drivers.OSSession
		if len(od.RemoteInfo.Storage) > 0 {
			orchOS = drivers.NewSessionWithHTTPClient(
				core.FromNetOsInfo(od.RemoteInfo.Storage[0]), core.InternalBlockedHTTPClient())
		}

		bcastOS := params.OS
		if bcastOS.IsExternal() {
			// Give each O its own OS session to prevent front running uploads
			pfx := fmt.Sprintf("%v/%v", params.ManifestID, od.RemoteInfo.AuthToken.SessionId)
			bcastOS = bcastOS.OS().NewSession(pfx)
		}

		var oScore float32
		if od.LocalInfo != nil {
			oScore = od.LocalInfo.Score
		}
		var initialLatency time.Duration
		if od.LocalInfo != nil && od.LocalInfo.Latency != nil {
			initialLatency = *od.LocalInfo.Latency
		}
		session := &BroadcastSession{
			Broadcaster:       core.NewBroadcaster(n),
			Params:            params,
			OrchestratorInfo:  od.RemoteInfo,
			OrchestratorOS:    orchOS,
			BroadcasterOS:     bcastOS,
			Sender:            n.Sender,
			CleanupSession:    cleanupSession,
			PMSessionID:       sessionID,
			Balances:          n.Balances,
			Balance:           balance,
			lock:              &sync.RWMutex{},
			OrchestratorScore: oScore,
			InitialPrice:      od.RemoteInfo.PriceInfo,
			InitialLatency:    initialLatency,
		}

		sessions = append(sessions, session)
	}
	return sessions, nil
}

func processSegment(ctx context.Context, cxn *rtmpConnection, seg *stream.HLSSegment, segPar *core.SegmentParameters) ([]string, error) {

	rtmpStrm := cxn.stream
	nonce := cxn.nonce
	cpl := cxn.pl
	mid := cxn.mid
	vProfile := cxn.profile

	if seg.Duration > maxDurationSec || seg.Duration < 0 {
		clog.Errorf(ctx, "Invalid duration seqNo=%d dur=%v", seg.SeqNo, seg.Duration)
		return nil, fmt.Errorf("invalid duration %v", seg.Duration)
	}

	clog.V(common.DEBUG).Infof(ctx, "Processing segment dur=%v bytes=%v", seg.Duration, len(seg.Data))
	if segPar != nil && segPar.ForceSessionReinit {
		clog.V(common.DEBUG).Infof(ctx, "Requesting HW Session Reinitialization for seg.SeqNo=%v", seg.SeqNo)
	}
	if monitor.Enabled {
		monitor.SegmentEmerged(ctx, nonce, seg.SeqNo, len(BroadcastJobVideoProfiles), seg.Duration)
	}
	atomic.AddUint64(&cxn.sourceBytes, uint64(len(seg.Data)))

	seg.Name = "" // hijack seg.Name to convey the uploaded URI
	ext, err := common.ProfileFormatExtension(vProfile.Format)
	if err != nil {
		clog.Errorf(ctx, "Unknown format extension err=%s", err)
		return nil, err
	}
	name := fmt.Sprintf("%s/%d%s", vProfile.Name, seg.SeqNo, ext)
	ros := cpl.GetRecordOSSession()
	segDurMs := getSegDurMsString(seg)

	hasZeroVideoFrame := seg.IsZeroFrame
	if ros != nil && !hasZeroVideoFrame {
		go func() {
			ctx, cancel := clog.WithTimeout(context.Background(), ctx, recordSegmentsMaxTimeout)
			defer cancel()
			now := time.Now()
			fields := &drivers.FileProperties{
				Metadata: map[string]string{"duration": segDurMs},
			}
			uri, err := drivers.SaveRetried(ctx, ros, name, seg.Data, fields, 3)
			took := time.Since(now)
			if err != nil {
				clog.Errorf(ctx, "Error saving name=%s bytes=%d to record store err=%q",
					name, len(seg.Data), err)
			} else {
				cpl.InsertHLSSegmentJSON(vProfile, seg.SeqNo, uri, seg.Duration)
				clog.Infof(ctx, "Successfully saved name=%s bytes=%d to record store took=%s",
					name, len(seg.Data), took)
				cpl.FlushRecord()
			}
			if monitor.Enabled {
				monitor.RecordingSegmentSaved(took, err)
			}
		}()
	}
	uri, err := cpl.GetOSSession().SaveData(ctx, name, bytes.NewReader(seg.Data), nil, 0)
	if err != nil {
		clog.Errorf(ctx, "Error saving segment err=%q", err)
		if monitor.Enabled {
			monitor.SegmentUploadFailed(ctx, nonce, seg.SeqNo, monitor.SegmentUploadErrorUnknown, err, true, "")
		}
		return nil, err
	}
	if cpl.GetOSSession().IsExternal() {
		seg.Name = uri // hijack seg.Name to convey the uploaded URI
	}
	err = cpl.InsertHLSSegment(vProfile, seg.SeqNo, uri, seg.Duration)
	if monitor.Enabled {
		monitor.SourceSegmentAppeared(ctx, nonce, seg.SeqNo, string(mid), vProfile.Name, ros != nil)
	}
	if err != nil {
		clog.Errorf(ctx, "Error inserting segment err=%q", err)
		if monitor.Enabled {
			monitor.SegmentUploadFailed(ctx, nonce, seg.SeqNo, monitor.SegmentUploadErrorDuplicateSegment, err, false, "")
		}
	}

	if hasZeroVideoFrame {
		var urls []string
		for _, profile := range cxn.params.Profiles {
			ext, err := common.ProfileFormatExtension(profile.Format)
			if err != nil {
				clog.Errorf(ctx, "Error getting extension for profile=%v with segment err=%q",
					profile.Format, err)
				return nil, err
			}
			name := fmt.Sprintf("%s/%d%s", profile.Name, seg.SeqNo, ext)
			uri, err := cpl.GetOSSession().SaveData(ctx, name, bytes.NewReader(seg.Data), nil, 0)
			if err != nil {
				clog.Errorf(ctx, "Error saving segment err=%q", err)
				if monitor.Enabled {
					monitor.SegmentUploadFailed(ctx, nonce, seg.SeqNo, monitor.SegmentUploadErrorUnknown, err, true, "")
				}
				return nil, err
			}
			urls = append(urls, uri)
			err = cpl.InsertHLSSegment(&profile, seg.SeqNo, uri, seg.Duration)
			if err != nil {
				clog.Errorf(ctx, "Error inserting segment err=%q", err)
				if monitor.Enabled {
					monitor.SegmentUploadFailed(ctx, nonce, seg.SeqNo, monitor.SegmentUploadErrorDuplicateSegment, err, false, "")
				}
			}
		}
		return urls, nil
	}

	var (
		startTime = time.Now()
		attempts  []data.TranscodeAttemptInfo
		urls      []string
	)
	if cxn.params != nil && len(cxn.params.Profiles) == 0 {
		return []string{}, nil
	}
	_, budgeted := segmentBudgetFromContext(ctx)
	badInput := badInputTracker{}
	badInputReached := false
	for len(attempts) < MaxAttempts {
		// Each attempt runs on a different orchestrator: a failing one is
		// suspended and removed before the next selection.
		var info *data.TranscodeAttemptInfo
		urls, info, err = transcodeSegment(ctx, cxn, seg, name, segPar)
		attempts = append(attempts, *info)
		if err == nil {
			break
		}

		if errors.Is(err, errNoOrchs) || errors.Is(err, errDiscovery) {
			clog.Warningf(ctx, "Not retrying current segment because no orchestrator session is available err=%q", err)
			break
		}
		if shouldStopStream(err) {
			clog.Warningf(ctx, "Stopping current stream due to err=%q", err)
			rtmpStrm.Close()
			break
		}
		if badInput.observe(err) {
			// Distinct orchestrators agree the segment cannot be transcoded, so
			// the input is at fault and their suspensions are lifted.
			var oe *orchError
			errors.As(err, &oe)
			for _, orch := range badInput.orchs(oe.err.Error()) {
				cxn.sessManager.unsuspendOrch(orch)
			}
			clog.Warningf(ctx, "Not retrying current segment: %d orchestrators returned the same non-retryable error err=%q", badInputConsensus, err)
			if monitor.Enabled {
				monitor.SegmentTranscodeFailed(ctx, monitor.SegmentTranscodeErrorNonRetryable, nonce, seg.SeqNo, err, true)
			}
			err = fmt.Errorf("%w: %w", errBadInput, err)
			badInputReached = true
			break
		}
		var oe *orchError
		if !errors.As(err, &oe) && isNonRetryableError(err) {
			clog.Warningf(ctx, "Not retrying current segment due to non-retryable error err=%q", err)
			if monitor.Enabled {
				monitor.SegmentTranscodeFailed(ctx, monitor.SegmentTranscodeErrorNonRetryable, nonce, seg.SeqNo, err, true)
			}
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			if budgeted && errors.Is(ctxErr, context.DeadlineExceeded) {
				err = fmt.Errorf("%w: %w", errSegmentBudgetExhausted, err)
			} else {
				err = ctxErr
			}
			clog.Warningf(ctx, "Not retrying current segment due to context cancellation err=%q", err)
			if monitor.Enabled {
				monitor.SegmentTranscodeFailed(ctx, monitor.SegmentTranscodeErrorCtxCancelled, nonce, seg.SeqNo, err, true)
			}
			break
		}
		// orchestrator-scoped or recoverable error, retry on another orchestrator
	}

	if MetadataQueue != nil {
		success := err == nil && len(urls) > 0
		streamID := string(mid)
		if cxn.params != nil && cxn.params.ExternalStreamID != "" {
			streamID = cxn.params.ExternalStreamID
		}
		key := newTranscodeEventKey(mid, streamID)
		evt := newTranscodeEvent(streamID, seg, startTime, success, attempts)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), MetadataPublishTimeout)
			defer cancel()
			if err := MetadataQueue.Publish(ctx, key, evt, false); err != nil {
				clog.Errorf(ctx, "Error publishing stream transcode event: err=%q key=%q event=%+v", err, key, evt)
			}
		}()
	}
	if len(attempts) == MaxAttempts && err != nil && !badInputReached && !errors.Is(err, errSegmentBudgetExhausted) {
		err = fmt.Errorf("%w: %w", maxTranscodeAttempts, err)
		if monitor.Enabled {
			monitor.SegmentTranscodeFailed(ctx, monitor.SegmentTranscodeErrorMaxAttempts, nonce, seg.SeqNo, err, true)
		}
	}
	return urls, err
}

func transcodeSegment(ctx context.Context, cxn *rtmpConnection, seg *stream.HLSSegment, name string,
	segPar *core.SegmentParameters) ([]string, *data.TranscodeAttemptInfo, error) {

	var urls []string
	info := &data.TranscodeAttemptInfo{}
	var err error

	defer func(startTime time.Time) {
		info.LatencyMs = time.Since(startTime).Milliseconds()
		if err != nil {
			errStr := err.Error()
			info.Error = &errStr
		}
	}(time.Now())

	nonce := cxn.nonce
	sessions := cxn.sessManager.selectSessions(ctx)
	if _, budgeted := segmentBudgetFromContext(ctx); budgeted && len(sessions) == 0 && cxn.sessManager.hasOrchestratorPool() {
		// Discovery can come back empty while orchestrators start up or are
		// briefly unreachable; the segment budget, not the first empty pool,
		// decides when the gateway gives up.
		sessions = cxn.sessManager.waitForSessions(ctx)
	}
	// Return early under a few circumstances:
	// View-only (non-transcoded) streams or no sessions available
	if len(sessions) == 0 {
		if !cxn.sessManager.hasOrchestratorPool() {
			clog.Infof(ctx, "No sessions available because no orchestrators were configured; not transcoding")
			return nil, info, nil
		}
		err = errNoOrchs
		if monitor.Enabled {
			monitor.SegmentTranscodeFailed(ctx, monitor.SegmentTranscodeErrorNoOrchestrators, nonce, seg.SeqNo, err, true)
		}
		clog.Infof(ctx, "No sessions available for segment")
		return nil, info, err
	}
	info.Orchestrator = data.OrchestratorMetadata{
		TranscoderUri: sessions[0].Transcoder(),
		Address:       sessions[0].Address(),
	}

	clog.Infof(ctx, "Trying to transcode segment using sessions=%d", len(sessions))
	if monitor.Enabled {
		monitor.TranscodeTry(ctx, nonce, seg.SeqNo)
	}
	sess, segUsed, res, roundTrip, err := submitHedged(ctx, cxn, sessions[0], seg, name, segPar)
	if err != nil {
		return nil, info, err
	}
	info.Orchestrator = data.OrchestratorMetadata{
		TranscoderUri: sess.Transcoder(),
		Address:       sess.Address(),
	}
	urls, err = downloadResults(ctx, cxn, segUsed, sess, res)
	if err == nil {
		cxn.sessManager.recordRoundTrip(ctx, sess, seg, roundTrip)
	}
	return urls, info, err
}

// submission is one in-flight SubmitSegment call of a segment attempt.
type submission struct {
	sess    *BroadcastSession
	seg     *stream.HLSSegment
	start   time.Time
	cancel  context.CancelCauseFunc
	settled bool // its result has been handled
}

// submitHedged submits seg to primary and, when the segment has a budget and
// primary has not answered within hedgeDelay, also to one more orchestrator.
// The first successful result wins and the other submission is cancelled.
// Every orchestrator that returns an error is suspended and removed, and its
// error is returned as an *orchError unless another submission succeeds.
// At most one hedge is started per attempt, so a hedged segment costs one
// extra payment.
func submitHedged(ctx context.Context, cxn *rtmpConnection, primary *BroadcastSession, seg *stream.HLSSegment, name string,
	segPar *core.SegmentParameters) (*BroadcastSession, *stream.HLSSegment, *ReceivedTranscodeResult, time.Duration, error) {

	bsm := cxn.sessManager
	budget, budgeted := segmentBudgetFromContext(ctx)
	resc := make(chan *SubmitResult, 2)
	var subs []*submission

	start := func(sess *BroadcastSession) error {
		segOut, err := prepareForTranscoding(ctx, cxn, sess, seg, name)
		if err != nil {
			return &orchError{orch: sess.Transcoder(), err: err}
		}
		sess.pushSegInFlight(segOut)
		sctx, cancel := context.WithCancelCause(ctx)
		if budgeted {
			var cancelTimeout context.CancelFunc
			sctx, cancelTimeout = context.WithTimeout(sctx, budget.orchCap())
			parentCancel := cancel
			cancel = func(cause error) { parentCancel(cause); cancelTimeout() }
		}
		subs = append(subs, &submission{sess: sess, seg: segOut, start: time.Now(), cancel: cancel})
		submitMultiSession(sctx, sess, segOut, segPar, cxn.nonce, resc)
		return nil
	}
	find := func(sess *BroadcastSession) *submission {
		for _, s := range subs {
			if s.sess == sess {
				return s
			}
		}
		return nil
	}

	if err := start(primary); err != nil {
		return nil, nil, nil, 0, err
	}

	var hedgeC <-chan time.Time
	if budgeted {
		p90, ok := bsm.roundTripP90(primary)
		t := time.NewTimer(hedgeDelay(seg, p90, ok))
		defer t.Stop()
		hedgeC = t.C
	}
	hedged := false
	inflight := 1
	var lastErr error
	for inflight > 0 {
		select {
		case r := <-resc:
			inflight--
			sub := find(r.Session)
			sub.settled = true
			if r.Err == nil && r.TranscodeResult != nil {
				for _, other := range subs {
					if !other.settled {
						other.settled = true
						other.cancel(errHedgeLost)
						bsm.releaseSession(other.sess)
					}
				}
				sub.cancel(nil)
				if hedged {
					won := sub.sess != primary
					if won {
						bsm.promoteSession(sub.sess)
					}
					clog.Infof(ctx, "Hedged segment answered by orch=%s hedgeWon=%v", sub.sess.Transcoder(), won)
					if monitor.Enabled {
						monitor.SegmentHedged(won)
					}
				}
				return sub.sess, sub.seg, r.TranscodeResult, time.Since(sub.start), nil
			}
			sub.cancel(nil)
			err := r.Err
			if err == nil {
				err = errors.New("empty response")
			}
			if ctx.Err() != nil {
				// The segment's own context ended (budget spent or caller
				// gone); that says nothing about this orchestrator.
				bsm.releaseSession(r.Session)
				lastErr = err
				continue
			}
			bsm.suspendAndRemoveOrch(r.Session)
			lastErr = &orchError{orch: r.Session.Transcoder(), err: err}
		case <-hedgeC:
			hedgeC = nil
			if !budget.takeHedge() {
				clog.V(common.DEBUG).Infof(ctx, "Segment already hedged once; not hedging orch=%s", primary.Transcoder())
				continue
			}
			hedge := bsm.selectHedgeSession(ctx, primary)
			if hedge == nil {
				clog.V(common.DEBUG).Infof(ctx, "No hedge orchestrator available orch=%s", primary.Transcoder())
				continue
			}
			if err := start(hedge); err != nil {
				// prepareForTranscoding already suspended and removed it.
				clog.Warningf(ctx, "Could not start hedge orch=%s err=%q", hedge.Transcoder(), err)
				continue
			}
			hedged = true
			inflight++
			clog.Infof(ctx, "Hedging segment: orch=%s has not answered, also submitting to orch=%s", primary.Transcoder(), hedge.Transcoder())
		}
	}
	if hedged && monitor.Enabled {
		monitor.SegmentHedged(false)
	}
	return nil, nil, nil, 0, lastErr
}

type SubmitResult struct {
	Session         *BroadcastSession
	TranscodeResult *ReceivedTranscodeResult
	Err             error
}

func submitSegment(ctx context.Context, sess *BroadcastSession, seg *stream.HLSSegment, segPar *core.SegmentParameters,
	nonce uint64, resc chan *SubmitResult) {

	res, err := SubmitSegment(ctx, sess.Clone(), seg, segPar, nonce, false)
	resc <- &SubmitResult{
		Session:         sess,
		TranscodeResult: res,
		Err:             err,
	}
}

func prepareForTranscoding(ctx context.Context, cxn *rtmpConnection, sess *BroadcastSession, seg *stream.HLSSegment,
	name string) (*stream.HLSSegment, error) {

	// storage the orchestrator prefers
	res := seg
	sess.lock.RLock()
	ios := sess.OrchestratorOS
	sess.lock.RUnlock()
	if ios != nil {
		// XXX handle case when orch expects direct upload
		uri, err := ios.SaveData(ctx, name, bytes.NewReader(seg.Data), nil, 0)
		if err != nil {
			clog.Errorf(ctx, "Error saving segment to OS manifestID=%v nonce=%d seqNo=%d err=%q", cxn.mid, cxn.nonce, seg.SeqNo, err)
			if monitor.Enabled {
				monitor.SegmentUploadFailed(ctx, cxn.nonce, seg.SeqNo, monitor.SegmentUploadErrorOS, err, false, "")
			}
			cxn.sessManager.suspendAndRemoveOrch(sess)
			return nil, err
		}
		segCopy := *seg
		res = &segCopy
		res.Name = uri // hijack seg.Name to convey the uploaded URI
	}

	if err := refreshSessionIfNeeded(ctx, sess, false); err != nil {
		clog.Errorf(ctx, "Error refreshing session manifestID=%s orch=%v err=%q", cxn.mid, sess.Transcoder(), err)
		cxn.sessManager.suspendAndRemoveOrch(sess)
		return nil, err
	}

	return res, nil
}

func downloadResults(ctx context.Context, cxn *rtmpConnection, seg *stream.HLSSegment, sess *BroadcastSession, res *ReceivedTranscodeResult) ([]string, error) {

	nonce := cxn.nonce
	// download transcoded segments from the transcoder
	gotErr := false // only send one error msg per segment list
	var errCode monitor.SegmentTranscodeError
	errFunc := func(subType monitor.SegmentTranscodeError, url string, err error) {
		clog.Errorf(ctx, "%v error with segment nonce=%d seqNo=%d: %v (URL: %v)", subType, nonce, seg.SeqNo, err, url)
		if monitor.Enabled && !gotErr {
			monitor.SegmentTranscodeFailed(ctx, subType, nonce, seg.SeqNo, err, false)
			gotErr = true
			errCode = subType
		}
	}
	cpl := cxn.pl

	var dlErr error
	n := len(res.Segments)
	segURLs := make([]string, len(res.Segments))
	segLock := &sync.Mutex{}
	cond := sync.NewCond(segLock)
	var recordWG sync.WaitGroup

	dlFunc := func(url string, i int) {
		defer func() {
			cond.L.Lock()
			n--
			if n == 0 {
				cond.Signal()
			}
			cond.L.Unlock()
		}()

		bos := sess.BroadcasterOS
		profile := sess.Params.Profiles[i]

		bros := cpl.GetRecordOSSession()
		var data []byte
		// Download segment data when it needs to be uploaded to the broadcaster's
		// own object store or recording store.
		if bros != nil || bos != nil && !bos.IsOwn(url) {
			d, err := downloadSeg(ctx, url)
			if err != nil {
				errFunc(monitor.SegmentTranscodeErrorDownload, url, err)
				segLock.Lock()
				dlErr = err
				segLock.Unlock()
				cxn.sessManager.suspendAndRemoveOrch(sess)
				return
			}

			data = d
			atomic.AddUint64(&cxn.transcodedBytes, uint64(len(data)))
		}

		if bros != nil {
			go func() {
				ctx, cancel := clog.WithTimeout(context.Background(), ctx, recordSegmentsMaxTimeout)
				defer cancel()
				ext, _ := common.ProfileFormatExtension(profile.Format)
				name := fmt.Sprintf("%s/%d%s", profile.Name, seg.SeqNo, ext)
				segDurMs := getSegDurMsString(seg)
				now := time.Now()
				fields := &drivers.FileProperties{
					Metadata: map[string]string{"duration": segDurMs},
				}
				uri, err := drivers.SaveRetried(ctx, bros, name, data, fields, 3)
				took := time.Since(now)
				if err != nil {
					clog.Errorf(ctx, "Error saving nonce=%d manifestID=%s name=%s to record store err=%q", nonce, cxn.mid, name, err)
				} else {
					cpl.InsertHLSSegmentJSON(&profile, seg.SeqNo, uri, seg.Duration)
					clog.Infof(ctx, "Successfully saved nonce=%d manifestID=%s name=%s size=%d bytes to record store took=%s",
						nonce, cxn.mid, name, len(data), took)
				}
				recordWG.Done()
				if monitor.Enabled {
					monitor.RecordingSegmentSaved(took, err)
				}
			}()
		}

		if bos != nil && !bos.IsOwn(url) {
			ext, err := common.ProfileFormatExtension(profile.Format)
			if err != nil {
				errFunc(monitor.SegmentTranscodeErrorSaveData, url, err)
				return
			}
			name := fmt.Sprintf("%s/%d%s", profile.Name, seg.SeqNo, ext)
			newURL, err := bos.SaveData(ctx, name, bytes.NewReader(data), nil, 0)
			if err != nil {
				switch err.Error() {
				case "Session ended":
					errFunc(monitor.SegmentTranscodeErrorSessionEnded, url, err)
				default:
					errFunc(monitor.SegmentTranscodeErrorSaveData, url, err)
				}
				return
			}
			url = newURL
		}

		segLock.Lock()
		segURLs[i] = url
		segLock.Unlock()
	}

	dlStart := time.Now()
	if cpl.GetRecordOSSession() != nil && len(res.Segments) > 0 {
		recordWG.Add(len(res.Segments))
	}
	for i, v := range res.Segments {
		go dlFunc(v.Url, i)
	}
	if cpl.GetRecordOSSession() != nil && len(res.Segments) > 0 {
		go func() {
			recordWG.Wait()
			cpl.FlushRecord()
		}()
	}

	cond.L.Lock()
	for n != 0 {
		cond.Wait()
	}
	cond.L.Unlock()
	if dlErr != nil {
		return nil, dlErr
	}
	updateSession(sess, res)
	cxn.sessManager.completeSession(ctx, sess, false)

	downloadDur := time.Since(dlStart)
	if monitor.Enabled {
		monitor.SegmentDownloaded(ctx, nonce, seg.SeqNo, downloadDur)
	}

	for i, url := range segURLs {
		err := cpl.InsertHLSSegment(&sess.Params.Profiles[i], seg.SeqNo, url, seg.Duration)
		if err != nil {
			// InsertHLSSegment only returns ErrSegmentAlreadyExists error
			// Right now InsertHLSSegment call is atomic regarding transcoded segments - we either inserting
			// all the transcoded segments or none, so we shouldn't hit this error
			// But report in case that InsertHLSSegment changed or something wrong is going on in other parts of workflow
			clog.Errorf(ctx, "Playlist insertion error nonce=%d manifestID=%s seqNo=%d err=%q", nonce, cxn.mid, seg.SeqNo, err)
			if monitor.Enabled {
				monitor.SegmentTranscodeFailed(ctx, monitor.SegmentTranscodeErrorDuplicateSegment, nonce, seg.SeqNo, err, false)
			}
		}
	}

	if monitor.Enabled {
		monitor.SegmentFullyTranscoded(ctx, nonce, seg.SeqNo, common.ProfilesNames(sess.Params.Profiles), errCode, sess.OrchestratorInfo)
	}

	clog.V(common.DEBUG).Infof(ctx, "Successfully processed segment")
	return segURLs, nil
}

var sessionErrStrings = []string{"dial tcp", "unexpected EOF", core.ErrOrchBusy.Error(), core.ErrOrchCap.Error()}

var sessionErrRegex = common.GenErrRegex(sessionErrStrings)

func shouldStopSession(err error) bool {
	return sessionErrRegex.MatchString(err.Error())
}

// Return an updated copy of the given session using the received transcode result
func updateSession(sess *BroadcastSession, res *ReceivedTranscodeResult) {
	sess.lock.Lock()
	defer sess.lock.Unlock()
	sess.LatencyScore = res.LatencyScore

	if res.Info == nil {
		// Return early if we do not need to update OrchestratorInfo
		return
	}

	oInfo := res.Info
	oldInfo := sess.OrchestratorInfo
	sess.OrchestratorInfo = oInfo

	if len(oInfo.Storage) > 0 {
		sess.OrchestratorOS = drivers.NewSessionWithHTTPClient(
			core.FromNetOsInfo(oInfo.Storage[0]), core.InternalBlockedHTTPClient())
	}

	if sess.Sender != nil && oInfo.TicketParams != nil {
		// Note: We do not validate the ticket params included in the OrchestratorInfo
		// message here. Instead, we store the ticket params with the current BroadcastSession
		// and the next time this BroadcastSession is used, the ticket params will be validated
		// during ticket creation in genPayment(). If ticket params validation during ticket
		// creation fails, then this BroadcastSession will be removed
		oldSession := sess.PMSessionID
		sess.PMSessionID = sess.Sender.StartSession(*pmTicketParams(oInfo.TicketParams))
		sess.CleanupSession(oldSession)

		// Session ID changed so we need to make sure the balance tracks the new session ID
		if oldInfo.AuthToken.SessionId != oInfo.AuthToken.SessionId {
			sess.Balance = core.NewBalance(ethcommon.BytesToAddress(sess.OrchestratorInfo.TicketParams.Recipient),
				core.ManifestID(sess.OrchestratorInfo.AuthToken.SessionId), sess.Balances)
		}
	}
}

func clearSessionBalance(sess *BroadcastSession, id core.ManifestID) {
	sess.lock.Lock()
	defer sess.lock.Unlock()
	if sess.Balances != nil && sess.OrchestratorInfo != nil && sess.OrchestratorInfo.TicketParams != nil {
		sess.Balance = core.NewBalance(ethcommon.BytesToAddress(sess.OrchestratorInfo.TicketParams.Recipient), id, sess.Balances)
	}
}

func refreshSessionIfNeeded(ctx context.Context, sess *BroadcastSession, ignoreCapacityCheck bool) error {
	shouldRefresh, err := shouldRefreshSession(ctx, sess)
	if err != nil {
		return err
	}
	if shouldRefresh {
		return refreshSession(ctx, sess, ignoreCapacityCheck)
	}
	return nil
}

func refreshSession(ctx context.Context, sess *BroadcastSession, ignoreCapacityCheck bool) error {
	uri, err := url.Parse(sess.Transcoder())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	oInfo, err := getOrchestratorInfoRPC(ctx, sess.Broadcaster, uri, GetOrchestratorInfoParams{
		Caps:                sess.Params.Capabilities.ToNetCapabilities(),
		IgnoreCapacityCheck: ignoreCapacityCheck,
	})
	if err != nil {
		return err
	}

	// Create dummy result
	sess.lock.RLock()
	res := &ReceivedTranscodeResult{
		LatencyScore: sess.LatencyScore,
		Info:         oInfo,
	}
	sess.lock.RUnlock()

	updateSession(sess, res)
	return nil
}

func shouldRefreshSession(ctx context.Context, sess *BroadcastSession) (bool, error) {
	sess.lock.RLock()
	OrchestratorInfo := sess.OrchestratorInfo
	sess.lock.RUnlock()
	if OrchestratorInfo.AuthToken == nil {
		return false, errors.New("missing auth token")
	}

	// Refresh auth token if we are within the last 10% of the token's valid period
	authTokenExpireBuffer := 0.1
	refreshPoint := OrchestratorInfo.AuthToken.Expiration - int64(authTokenValidPeriod.Seconds()*authTokenExpireBuffer)
	if time.Now().After(time.Unix(refreshPoint, 0)) {
		clog.V(common.VERBOSE).Infof(ctx, "Auth token expired, refreshing for orch=%v", OrchestratorInfo.Transcoder)

		return true, nil
	}

	if sess.Sender != nil {
		if err := sess.Sender.ValidateTicketParams(pmTicketParams(OrchestratorInfo.TicketParams)); err != nil {
			if err != pm.ErrTicketParamsExpired {
				return false, err
			}

			clog.V(common.VERBOSE).Infof(ctx, "Ticket params expired, refreshing for orch=%v", OrchestratorInfo.Transcoder)

			return true, nil
		}
	}

	return false, nil
}

func newTranscodeEventKey(mid core.ManifestID, streamID string) string {
	shardKey := string(mid[0])
	return fmt.Sprintf("stream_health.transcode.%s.%s", shardKey, streamID)
}

func newTranscodeEvent(streamID string, seg *stream.HLSSegment, startTime time.Time, success bool, attempts []data.TranscodeAttemptInfo) *data.TranscodeEvent {
	segMeta := data.SegmentMetadata{
		Name:     seg.Name,
		SeqNo:    seg.SeqNo,
		Duration: seg.Duration,
		ByteSize: len(seg.Data),
	}
	return data.NewTranscodeEvent(monitor.NodeID, streamID, segMeta, startTime, success, attempts)
}

func getSegDurMsString(seg *stream.HLSSegment) string {
	return strconv.Itoa(int(seg.Duration * 1000))
}

func nonRetryableErrMapInit() map[string]bool {
	errs := make(map[string]bool)
	for _, v := range ffmpeg.NonRetryableErrs {
		errs[v] = true
	}
	return errs
}

var NonRetryableErrMap = nonRetryableErrMapInit()

func isNonRetryableError(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if NonRetryableErrMap[e.Error()] {
			return true
		}
	}
	if errors.Is(err, errSeqPayloadConflict) || errors.Is(err, errSeqOutsideReplayWindow) {
		return true
	}
	return false
}

// isBadInputError reports errors that describe the pushed segment itself
// rather than an orchestrator: conflicting or out-of-window sequence reuse, and
// a segment that badInputConsensus orchestrators rejected the same way.
func isBadInputError(err error) bool {
	return errors.Is(err, errBadInput) || errors.Is(err, errSeqPayloadConflict) || errors.Is(err, errSeqOutsideReplayWindow)
}
