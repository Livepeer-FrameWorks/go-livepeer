package server

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-tools/drivers"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSegmentBudget(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	live := &core.StreamParameters{Workload: core.WorkloadLive}
	vod := &core.StreamParameters{Workload: core.WorkloadVOD}

	assert.Equal(3*time.Second, resolveSegmentBudget(live, 0, 2.0, now).total, "live falls back to segDur + 1s")
	assert.Equal(3*time.Second, resolveSegmentBudget(nil, 0, 2.0, now).total, "no workload is live")
	assert.Equal(30*time.Second, resolveSegmentBudget(vod, 0, 2.0, now).total)
	assert.Equal(2500*time.Millisecond, resolveSegmentBudget(live, 2500, 2.0, now).total, "request deadline wins")
	vod.DeadlineMs = 20000
	assert.Equal(20*time.Second, resolveSegmentBudget(vod, 0, 2.0, now).total, "stream deadline")
	assert.Equal(20*time.Second, resolveSegmentBudget(vod, 45000, 2.0, now).total, "stream deadline caps the request")
	assert.Equal(15*time.Second, resolveSegmentBudget(vod, 15000, 2.0, now).total)

	b := resolveSegmentBudget(live, 0, 2.0, now)
	assert.Equal(now.Add(3*time.Second), b.deadline())
	assert.Equal(now.Add(2500*time.Millisecond), b.respondBy())
	assert.Equal(2400*time.Millisecond, b.orchCap())
}

func TestSegmentBudget_OneHedgePerSegment(t *testing.T) {
	b := resolveSegmentBudget(nil, 0, 2.0, time.Now())
	copyOfB := b
	assert.True(t, b.takeHedge())
	assert.False(t, copyOfB.takeHedge(), "the hedge allowance is shared by every attempt of the segment")
	assert.False(t, segmentBudget{}.takeHedge(), "an unbudgeted segment never hedges")
}

func TestHedgeDelay(t *testing.T) {
	assert := assert.New(t)
	seg := &stream.HLSSegment{Duration: 2.0}
	assert.Equal(1200*time.Millisecond, hedgeDelay(seg, 0, false))
	assert.Equal(800*time.Millisecond, hedgeDelay(seg, 800*time.Millisecond, true))
	assert.Equal(1200*time.Millisecond, hedgeDelay(seg, 3*time.Second, true))
	assert.Equal(minHedgeDelay, hedgeDelay(seg, 10*time.Millisecond, true))
}

// A new stream whose first discovery came back empty waits for discovery
// within the segment budget instead of failing the segment at once.
func TestProcessSegment_WaitsForDiscoveryWithinBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	o := StubBroadcastSession(stubTestTranscoder(ctx, orchResultHandler(t, 0, "", &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}

	var discoveries atomic.Int32
	create := func() ([]*BroadcastSession, error) {
		// Orchestrators are not ready for the first two discovery rounds.
		if discoveries.Add(1) <= 2 {
			return nil, nil
		}
		return []*BroadcastSession{o}, nil
	}
	pool := NewSessionPool("test", 1, 1, newSuspender(), create, func(string) {}, &LIFOSelector{})
	bsm := &BroadcastSessionsManager{
		trustedPool:   pool,
		untrustedPool: NewSessionPool("test", 0, 0, newSuspender(), create, func(string) {}, &LIFOSelector{}),
	}
	pool.refreshSessions(ctx) // the empty refresh NewSessionManager does at stream start
	cxn := budgetTestConnection(bsm)

	seg := &stream.HLSSegment{Data: []byte("dummy"), Duration: 2.0}
	budget := resolveSegmentBudget(cxn.params, 0, seg.Duration, time.Now())
	pctx, pcancel := context.WithDeadline(ctx, budget.deadline())
	defer pcancel()
	urls, err := processSegment(withSegmentBudget(pctx, budget), cxn, seg, nil)
	require.NoError(t, err)
	assert.Len(t, urls, 1)
	assert.EqualValues(t, 1, calls.Load())
}

// Without any orchestrator the wait ends with the budget, not before it.
func TestProcessSegment_NoOrchestratorWaitsOutBudget(t *testing.T) {
	create := func() ([]*BroadcastSession, error) { return nil, nil }
	pool := NewSessionPool("test", 1, 1, newSuspender(), create, func(string) {}, &LIFOSelector{})
	bsm := &BroadcastSessionsManager{
		trustedPool:   pool,
		untrustedPool: NewSessionPool("test", 0, 0, newSuspender(), create, func(string) {}, &LIFOSelector{}),
	}
	cxn := budgetTestConnection(bsm)
	seg := &stream.HLSSegment{Data: []byte("dummy"), Duration: 0.5}
	start := time.Now()
	budget := resolveSegmentBudget(cxn.params, 0, seg.Duration, start)
	pctx, pcancel := context.WithDeadline(context.Background(), budget.deadline())
	defer pcancel()
	_, err := processSegment(withSegmentBudget(pctx, budget), cxn, seg, nil)
	assert.ErrorIs(t, err, errNoOrchs)
	assert.Equal(t, 503, pushErrorStatus(err))
	assert.GreaterOrEqual(t, time.Since(start), budget.total-50*time.Millisecond)
}

// An offchain gateway has no payment sender; dropping a failed orchestrator
// must not dereference it.
func TestNewSessionManager_OffchainRemoveSessionWithoutSender(t *testing.T) {
	n, _ := core.NewLivepeerNode(nil, "", nil)
	require.Nil(t, n.Sender)
	mid := core.RandomManifestID()
	bsm := NewSessionManager(context.TODO(), n, &core.StreamParameters{ManifestID: mid, OS: drivers.NewMemoryDriver(nil).NewSession(string(mid))})
	sess := StubBroadcastSession("https://orch")
	bsm.trustedPool.sessMap[sess.Transcoder()] = sess
	assert.NotPanics(t, func() { bsm.suspendAndRemoveOrch(context.Background(), sess, errors.New("test")) })
}

// A panic while processing a segment fails that segment instead of the
// gateway process.
func TestProcessSegmentDeduped_RecoversPanic(t *testing.T) {
	cxn := budgetTestConnection(bsmWithSessListExt(nil, nil, true))
	cxn.pl = nil // processSegment dereferences the playlist
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 9, Duration: 2.0}
	_, err := cxn.processSegmentDeduped(context.Background(), seg, &core.SegmentParameters{}, resolveSegmentBudget(cxn.params, 0, seg.Duration, time.Now()))
	assert.ErrorIs(t, err, errSegmentPanic)
	assert.Equal(t, 500, pushErrorStatus(err))
}

// A re-POST of a segment joins the transcode started by an earlier request even
// after that request was abandoned, and gets the result.
func TestProcessSegmentDeduped_SurvivesLeaderCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	o := StubBroadcastSession(stubTestTranscoder(ctx, orchResultHandler(t, 400*time.Millisecond, "", &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{o}, nil, true))
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 5, Duration: 2.0}

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := cxn.processSegmentDeduped(leaderCtx, seg, &core.SegmentParameters{}, resolveSegmentBudget(cxn.params, 0, seg.Duration, time.Now()))
		leaderErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	leaderCancel()
	require.ErrorIs(t, <-leaderErr, context.Canceled)

	// Each push carries its own segment, as HandlePush builds one per request.
	repost := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 5, Duration: 2.0}
	urls, err := cxn.processSegmentDeduped(context.Background(), repost, &core.SegmentParameters{}, resolveSegmentBudget(cxn.params, 0, repost.Duration, time.Now()))
	require.NoError(t, err)
	assert.Len(t, urls, 1)
	assert.EqualValues(t, 1, calls.Load(), "the re-POST must join, not transcode again")
}

// Without a result by respondBy the caller gets a definitive budget error ahead
// of the edge's deadline.
func TestProcessSegmentDeduped_RespondsBeforeDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	o := StubBroadcastSession(stubTestTranscoder(ctx, orchResultHandler(t, 10*time.Second, "", &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{o}, nil, true))
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 6, Duration: 1.0}

	start := time.Now()
	budget := resolveSegmentBudget(cxn.params, 0, seg.Duration, start)
	_, err := cxn.processSegmentDeduped(context.Background(), seg, &core.SegmentParameters{}, budget)
	took := time.Since(start)
	require.ErrorIs(t, err, errSegmentBudgetExhausted)
	assert.InDelta(t, float64(budget.total-segmentResponseMargin), float64(took), float64(150*time.Millisecond))
}

// coldOrchHandler answers /segment with a successful transcode after the
// delay of its call: delays[i] for the i-th call, the last delay after that.
func coldOrchHandler(t *testing.T, delays []time.Duration, calls *atomic.Int32) http.HandlerFunc {
	tr := &net.TranscodeResult{Result: &net.TranscodeResult_Data{Data: &net.TranscodeData{
		Segments: []*net.TranscodedSegmentData{{Url: "https://orch/rendition.ts", Pixels: 100}},
	}}}
	buf, err := proto.Marshal(tr)
	require.NoError(t, err)
	return func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if n >= len(delays) {
			n = len(delays) - 1
		}
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-time.After(delays[n]):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(buf)
	}
}

// slowRenditionDownload makes fetching a rendition from the orchestrator take
// d for the rest of the test.
func slowRenditionDownload(t *testing.T, d time.Duration) {
	old := downloadSeg
	t.Cleanup(func() { downloadSeg = old })
	downloadSeg = func(ctx context.Context, url string) ([]byte, error) {
		select {
		case <-time.After(d):
			return []byte("rendition"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// A cold session's first segment: the orchestrator answers late because it
// sets the session up first, and fetching the renditions runs past respondBy
// but ends within the budget. The push gets the renditions, not a 503 for a
// result the gateway already holds.
func TestProcessSegmentDeduped_ColdFirstSegmentWaitsForRenditionsInHand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	o := StubBroadcastSession(stubTestTranscoder(ctx, coldOrchHandler(t, []time.Duration{1200 * time.Millisecond}, &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	o.BroadcasterOS = &stubOSSession{host: "https://gateway-storage"}
	slowRenditionDownload(t, 500*time.Millisecond)
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{o}, nil, true))
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 0, Duration: 1.0}

	start := time.Now()
	budget := resolveSegmentBudget(cxn.params, 0, seg.Duration, start)
	urls, err := cxn.processSegmentDeduped(context.Background(), seg, &core.SegmentParameters{}, budget)
	took := time.Since(start)
	require.NoError(t, err, "took=%s respondBy=%s", took, budget.respondBy().Sub(start))
	assert.Len(t, urls, 1)
	assert.Greater(t, took, budget.respondBy().Sub(start), "the renditions arrived after respondBy")
	assert.Less(t, took, budget.total, "the answer still lands within the budget")
	assert.EqualValues(t, 1, calls.Load())
}

// Holding for renditions in hand never outlasts the budget: when fetching
// them runs past the deadline the push gets the budget error at the deadline.
func TestProcessSegmentDeduped_RenditionsInHandHoldEndsAtDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	o := StubBroadcastSession(stubTestTranscoder(ctx, coldOrchHandler(t, []time.Duration{1200 * time.Millisecond}, &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	o.BroadcasterOS = &stubOSSession{host: "https://gateway-storage"}
	slowRenditionDownload(t, 5*time.Second)
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{o}, nil, true))
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 0, Duration: 1.0}

	start := time.Now()
	budget := resolveSegmentBudget(cxn.params, 0, seg.Duration, start)
	_, err := cxn.processSegmentDeduped(context.Background(), seg, &core.SegmentParameters{}, budget)
	took := time.Since(start)
	require.ErrorIs(t, err, errSegmentBudgetExhausted)
	assert.InDelta(t, float64(budget.total), float64(took), float64(150*time.Millisecond))
	// Processing ends with its own deadline; let it finish before the test
	// restores the rendition downloader it is still using.
	require.Eventually(t, func() bool {
		cxn.segCacheMu.Lock()
		defer cxn.segCacheMu.Unlock()
		return len(cxn.segFetching) == 0
	}, 2*time.Second, 10*time.Millisecond)
}

// The round trips of segments submitted before a session's first result
// include the orchestrator setting the session up. They must neither suspend
// the orchestrator nor make the session look too slow to reuse, or the
// gateway trades a warmed-up session for another cold one.
func TestProcessSegment_SessionSetupRoundTripIsNotSlowness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	// Both answers take longer than the 2s segment; the first includes setup.
	o := StubBroadcastSession(stubTestTranscoder(ctx, coldOrchHandler(t, []time.Duration{2200 * time.Millisecond, 2100 * time.Millisecond, 100 * time.Millisecond}, &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	bsm := bsmWithSessListExt([]*BroadcastSession{o}, nil, true)
	cxn := budgetTestConnection(bsm)

	process := func(seq uint64) {
		seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: seq, Duration: 2.0}
		budget := resolveSegmentBudget(cxn.params, 0, seg.Duration, time.Now())
		pctx, pcancel := context.WithDeadline(ctx, budget.deadline())
		defer pcancel()
		urls, err := processSegment(withSegmentBudget(pctx, budget), cxn, seg, nil)
		require.NoError(t, err)
		require.Len(t, urls, 1)
	}

	process(0)
	o.lock.RLock()
	score := o.LatencyScore
	o.lock.RUnlock()
	assert.LessOrEqual(t, score, SELECTOR_LATENCY_SCORE_THRESHOLD, "a setup round trip must not mark the session for swapping")
	assert.Same(t, o, selectSession(ctx, []*BroadcastSession{o}, nil, 1, SELECTOR_LATENCY_SCORE_THRESHOLD), "the warmed-up session serves the next segment")

	process(1)
	assert.Contains(t, bsm.trustedPool.sessMap, o.Transcoder(), "one slow segment after setup must not remove the orchestrator")
	assert.Zero(t, bsm.trustedPool.sus.Suspended(o.Transcoder()), "one slow segment after setup must not suspend the orchestrator")
}

// useOrchHealthStore makes store the process-wide health store for the test.
func useOrchHealthStore(t *testing.T, store *orchHealthStore) {
	globalOrchHealthOnce.Do(func() {})
	old := globalOrchHealth
	globalOrchHealth = store
	t.Cleanup(func() { globalOrchHealth = old })
}

// The first result of a session includes the orchestrator setting the session
// up; it must not lower that orchestrator's shared performance score.
func TestSubmitSegment_SessionSetupRoundTripIsNotPerformance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newTestPerfStore(t)
	useOrchHealthStore(t, store)
	var calls atomic.Int32
	sess := StubBroadcastSession(stubTestTranscoder(ctx, coldOrchHandler(t, []time.Duration{0}, &calls)))
	sess.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	sess.Params.Workload = core.WorkloadLive
	key := perfKey("eu", core.WorkloadLive, "P144p30fps16x9", sess.Transcoder())
	samples := func() string {
		v, _ := store.rdb.HGet(context.Background(), key, "samples").Result()
		return v
	}
	seg := &stream.HLSSegment{Data: []byte("dummy"), Duration: 2.0}

	_, err := SubmitSegment(ctx, sess.Clone(), seg, nil, 0, false)
	require.NoError(t, err)
	time.Sleep(300 * time.Millisecond)
	assert.Empty(t, samples(), "the session's first round trip is not a performance sample")

	sess.markAnswered()
	_, err = SubmitSegment(ctx, sess.Clone(), seg, nil, 0, false)
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return samples() == "1" }, 2*time.Second, 20*time.Millisecond)
}
