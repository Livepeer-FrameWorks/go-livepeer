package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pushSegmentWithConfig posts a live segment of durMs with the given
// transcode configuration header and returns the response status and how
// long the push took.
func pushSegmentWithConfig(t *testing.T, s *LivepeerServer, path string, durMs int, config string) (int, time.Duration) {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(validPushSegment(t)))
	req.Header.Set("Content-Duration", strconv.Itoa(durMs))
	req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, config)
	w := httptest.NewRecorder()
	start := time.Now()
	s.HandlePush(w, req)
	return w.Code, time.Since(start)
}

// singleOrchConnection returns a connection whose stream has one orchestrator
// session served by handler. A warm session has already returned results and
// is on the stream's reuse list, so the next segment goes to it as a
// steady-state segment; a cold one still has to be set up.
func singleOrchConnection(t *testing.T, handler http.HandlerFunc, warm bool) (*rtmpConnection, *BroadcastSession) {
	ctx, stopOrch := context.WithCancel(context.Background())
	t.Cleanup(stopOrch)
	sess := StubBroadcastSession(stubTestTranscoder(ctx, handler))
	sess.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	bsm := bsmWithSessListExt([]*BroadcastSession{sess}, nil, true)
	if warm {
		sess.LatencyScore = 0.5
		sess.markAnswered()
		bsm.trustedPool.sel = &LIFOSelector{}
		bsm.trustedPool.lastSess = []*BroadcastSession{sess}
	}
	return budgetTestConnection(bsm), sess
}

// pushDeduped processes seg the way one push does, with a budget from
// requestDeadlineMs starting now.
func pushDeduped(cxn *rtmpConnection, seq uint64, requestDeadlineMs int) ([]string, error) {
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: seq, Duration: 2.0}
	budget := resolveSegmentBudget(cxn.params, requestDeadlineMs, seg.Duration, time.Now())
	return cxn.processSegmentDeduped(context.Background(), seg, &core.SegmentParameters{}, budget)
}

// An orchestrator that answers a steady-state 2 s segment in 2.6 s works,
// only slowly. Its submission runs until the segment's processing ends: the
// push gets 503 at respondBy (2.5 s), and the edge's re-POST joins the
// transcode and gets the result. One slow segment is one slow strike, not a
// suspension; the second consecutive one suspends the orchestrator.
func TestProcessSegment_SlowWorkingOrchestratorIsNotCutNorSuspended(t *testing.T) {
	var calls atomic.Int32
	cxn, sess := singleOrchConnection(t, coldOrchHandler(t, []time.Duration{2600 * time.Millisecond}, &calls), true)
	pool := cxn.sessManager.trustedPool

	_, err := pushDeduped(cxn, 5, 3000)
	require.ErrorIs(t, err, errSegmentBudgetExhausted)
	urls, err := pushDeduped(cxn, 5, 3000)
	require.NoError(t, err, "the re-POST gets the slow orchestrator's result")
	assert.Len(t, urls, 1)
	assert.EqualValues(t, 1, calls.Load(), "the re-POST joined the running transcode")
	assert.Contains(t, pool.sessMap, sess.Transcoder(), "a slow but working orchestrator stays")
	assert.Zero(t, pool.sus.Suspended(sess.Transcoder()), "one slow segment does not suspend")

	_, err = pushDeduped(cxn, 6, 3000)
	require.ErrorIs(t, err, errSegmentBudgetExhausted)
	_, err = pushDeduped(cxn, 6, 3000)
	require.NoError(t, err)
	assert.NotContains(t, pool.sessMap, sess.Transcoder(), "two consecutive slow segments suspend")
	assert.Greater(t, pool.sus.Suspended(sess.Transcoder()), 0)
}

// A cold session's first segment runs as long as its deadline allows: with an
// 8 s deadline, a 6.7 s session setup (past 80% of the deadline) is answered
// before respondBy, and the setup is not held against the orchestrator.
func TestProcessSegment_ColdFirstSegmentMakesItsExtendedDeadline(t *testing.T) {
	calls := &orchSetupCalls{}
	cxn, sess := singleOrchConnection(t, sessionSetupOrchHandler(t, 6700*time.Millisecond, 200*time.Millisecond, calls), false)
	slowRenditionDownload(t, 0)
	pool := cxn.sessManager.trustedPool

	urls, err := pushDeduped(cxn, 0, 8000)
	require.NoError(t, err)
	assert.Len(t, urls, 1)
	assert.EqualValues(t, 1, calls.Load())
	assert.Contains(t, pool.sessMap, sess.Transcoder())
	assert.Zero(t, pool.sus.Suspended(sess.Transcoder()))
}

// A VOD segment's 30 s budget bounds its submission, not the 8 s HTTP timeout
// of an unbudgeted one: an orchestrator answering after 9 s delivers the
// segment and is not suspended for taking that long once.
func TestProcessSegment_VODSubmissionRunsPastHTTPTimeoutWithinBudget(t *testing.T) {
	var calls atomic.Int32
	cxn, sess := singleOrchConnection(t, coldOrchHandler(t, []time.Duration{9 * time.Second}, &calls), true)
	cxn.params.Workload = core.WorkloadVOD
	pool := cxn.sessManager.trustedPool

	urls, err := pushDeduped(cxn, 3, 0)
	require.NoError(t, err)
	assert.Len(t, urls, 1)
	assert.EqualValues(t, 1, calls.Load())
	assert.Contains(t, pool.sessMap, sess.Transcoder())
	assert.Zero(t, pool.sus.Suspended(sess.Transcoder()))
}

// Segment 0 of a stream pays for registering the stream and for the
// orchestrator setting its session up. It gets the session setup allowance
// on top of its budget, so a 3.5 s setup does not cost the segment, and the
// orchestrator is not suspended for it.
func TestPush_ColdFirstSegmentGetsSessionSetupAllowance(t *testing.T) {
	t.Run("no edge deadline", func(t *testing.T) {
		s, calls := coldSessionStream(t, 3500*time.Millisecond, 200*time.Millisecond)
		code, took := pushSegmentWithConfig(t, s, "/live/mani/0.ts", 2000, `{"workload":"live"}`)
		require.Equal(t, http.StatusOK, code, "took=%s", took)
		assert.EqualValues(t, 1, calls.Load())
		assertOrchestratorKept(t, s)
	})
	t.Run("edge deadline without the allowance", func(t *testing.T) {
		// The push waits only its steady deadline and gets 503 at 2.5 s;
		// processing goes on with the allowance, so the re-POST gets it.
		s, calls := coldSessionStream(t, 3500*time.Millisecond, 200*time.Millisecond)
		const config = `{"workload":"live","deadlineMs":3000}`
		code, took := pushSegmentWithConfig(t, s, "/live/mani/0.ts", 2000, config)
		require.Equal(t, http.StatusServiceUnavailable, code, "took=%s", took)
		code, took = pushSegmentWithConfig(t, s, "/live/mani/0.ts", 2000, config)
		require.Equal(t, http.StatusOK, code, "took=%s", took)
		assert.EqualValues(t, 1, calls.Load(), "the re-POST joined the running transcode")
		assertOrchestratorKept(t, s)
	})
	t.Run("edge deadline with the allowance", func(t *testing.T) {
		s, calls := coldSessionStream(t, 3500*time.Millisecond, 200*time.Millisecond)
		code, took := pushSegmentWithConfig(t, s, "/live/mani/0.ts", 2000, `{"workload":"live","deadlineMs":5000}`)
		require.Equal(t, http.StatusOK, code, "took=%s", took)
		assert.EqualValues(t, 1, calls.Load())
		assertOrchestratorKept(t, s)
	})
}

// assertOrchestratorKept checks that the only orchestrator of the stream
// "mani" is still in its pool and not suspended.
func assertOrchestratorKept(t *testing.T, s *LivepeerServer) {
	t.Helper()
	s.connectionLock.RLock()
	cxn := s.rtmpConnections["mani"]
	s.connectionLock.RUnlock()
	pool := cxn.sessManager.trustedPool
	pool.lock.Lock()
	defer pool.lock.Unlock()
	require.Len(t, pool.sessMap, 1, "the orchestrator is still in the pool")
	for orch := range pool.sessMap {
		assert.Zero(t, pool.sus.Suspended(orch), "the session setup is not an orchestrator fault")
	}
}
