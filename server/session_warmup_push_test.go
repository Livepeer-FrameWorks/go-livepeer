package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orchSetupCalls counts the /segment requests of a sessionSetupOrchHandler and
// records, per request, how long it waited for the session setup to finish.
type orchSetupCalls struct {
	atomic.Int32
	mu         sync.Mutex
	setupWaits []time.Duration
}

// setupWait is how long the i-th request (0-based) waited for the session
// setup to finish before its own work started.
func (c *orchSetupCalls) setupWait(t *testing.T, i int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	require.Greater(t, len(c.setupWaits), i, "request %d did not reach the orchestrator", i)
	return c.setupWaits[i]
}

// sessionSetupOrchHandler answers /segment the way an orchestrator answers a
// stream: the first request it gets starts setting the stream's session up,
// which takes setup, and every request is answered work after both its
// arrival and the end of that setup.
func sessionSetupOrchHandler(t *testing.T, setup, work time.Duration, calls *orchSetupCalls) http.HandlerFunc {
	tr := &net.TranscodeResult{Result: &net.TranscodeResult_Data{Data: &net.TranscodeData{
		Segments: []*net.TranscodedSegmentData{{Url: "https://orch/rendition.ts", Pixels: 100}},
	}}}
	buf, err := proto.Marshal(tr)
	require.NoError(t, err)
	var setupDone time.Time
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		now := time.Now()
		calls.mu.Lock()
		if setupDone.IsZero() {
			setupDone = now.Add(setup)
		}
		at := setupDone
		if now.After(at) {
			at = now
		}
		calls.setupWaits = append(calls.setupWaits, at.Sub(now))
		calls.mu.Unlock()
		at = at.Add(work)
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-time.After(time.Until(at)):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(buf)
	}
}

// coldSessionStream registers the stream "mani" with one orchestrator whose
// session setup takes setup and whose transcode takes work.
func coldSessionStream(t *testing.T, setup, work time.Duration) (*LivepeerServer, *orchSetupCalls) {
	s, cancel := setupServerWithCancel()
	ctx, stopOrch := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stopOrch()
		serverCleanup(s)
		cancel()
	})
	calls := &orchSetupCalls{}
	sess := StubBroadcastSession(stubTestTranscoder(ctx, sessionSetupOrchHandler(t, setup, work, calls)))
	sess.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	sess.Params.ManifestID = "mani"
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{sess}, nil, true))
	cxn.mid = "mani"
	cxn.params.ManifestID = "mani"
	slowRenditionDownload(t, 0)
	s.connectionLock.Lock()
	s.rtmpConnections["mani"] = cxn
	s.connectionLock.Unlock()
	return s, calls
}

// pushLiveSegment posts a 2 s live segment with a 3 s budget and returns the
// response status.
func pushLiveSegment(t *testing.T, s *LivepeerServer, path string) int {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(validPushSegment(t)))
	req.Header.Set("Content-Duration", "2000")
	req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, `{"workload":"live","deadlineMs":3000}`)
	w := httptest.NewRecorder()
	s.HandlePush(w, req)
	return w.Code
}

// postSessionWarmup posts the stream's first keyframe to its session warm-up
// endpoint and returns the response status.
func postSessionWarmup(t *testing.T, s *LivepeerServer, mid string) int {
	req := httptest.NewRequest(http.MethodPost, "/live/"+mid+"/session", bytes.NewReader(validPushSegment(t)))
	req.Header.Set("Content-Resolution", "1280x720")
	req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, `{"workload":"live"}`)
	w := httptest.NewRecorder()
	s.HandlePush(w, req)
	return w.Code
}

// Without a warm-up, a cold orchestrator's session setup lands in the first
// segment's budget and the segment misses it.
func TestPush_ColdSessionFirstSegmentMissesItsBudget(t *testing.T) {
	s, calls := coldSessionStream(t, 2500*time.Millisecond, 200*time.Millisecond)
	require.Equal(t, http.StatusServiceUnavailable, pushLiveSegment(t, s, "/live/mani/0.ts"))
	require.EqualValues(t, 1, calls.Load())
}

// A warm-up posted when the client read the stream's first keyframe, a second
// before segment 0 is complete, starts the orchestrator's session setup then;
// segment 0 pays only for what is left of it and makes its budget.
func TestPush_SessionWarmupLetsColdFirstSegmentMakeItsBudget(t *testing.T) {
	s, calls := coldSessionStream(t, 2500*time.Millisecond, 200*time.Millisecond)
	require.Equal(t, http.StatusAccepted, postSessionWarmup(t, s, "mani"))
	time.Sleep(time.Second)
	start := time.Now()
	require.Equal(t, http.StatusOK, pushLiveSegment(t, s, "/live/mani/0.ts"), "took=%s", time.Since(start))
	require.EqualValues(t, 2, calls.Load(), "the warm-up and segment 0 each reached the orchestrator once")
}

// twoOrchStream registers the stream "mani" with two orchestrators; the
// session selector offers first first.
func twoOrchStream(t *testing.T, first, second http.HandlerFunc) (*LivepeerServer, *rtmpConnection, *BroadcastSession, *BroadcastSession) {
	s, cancel := setupServerWithCancel()
	ctx, stopOrch := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stopOrch()
		serverCleanup(s)
		cancel()
	})
	var sessions []*BroadcastSession
	for _, h := range []http.HandlerFunc{first, second} {
		sess := StubBroadcastSession(stubTestTranscoder(ctx, h))
		sess.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
		sess.Params.ManifestID = "mani"
		sessions = append(sessions, sess)
	}
	// LIFOSelector pops the last added session first.
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{sessions[1], sessions[0]}, nil, true))
	cxn.mid = "mani"
	cxn.params.ManifestID = "mani"
	slowRenditionDownload(t, 0)
	s.connectionLock.Lock()
	s.rtmpConnections["mani"] = cxn
	s.connectionLock.Unlock()
	return s, cxn, sessions[0], sessions[1]
}

// warmUpNow runs the stream's session warm-up to completion.
func warmUpNow(t *testing.T, cxn *rtmpConnection) {
	require.True(t, cxn.claimSessionUse(false))
	cxn.warmSession(context.Background(), &stream.HLSSegment{Data: validPushSegment(t), SeqNo: sessionWarmupSeqNo, Duration: 0.04})
}

// An orchestrator that refuses the warm-up keyframe with an error the segment
// path suspends it for is suspended by the warm-up too, another orchestrator
// is warmed, and the stream's first segment goes there.
func TestSessionWarmup_OrchestratorFaultMovesFirstSegmentElsewhere(t *testing.T) {
	var badCalls, goodCalls atomic.Int32
	s, cxn, bad, good := twoOrchStream(t,
		orchResultHandler(t, 0, "Invalid argument", &badCalls),
		orchResultHandler(t, 0, "", &goodCalls))
	warmUpNow(t, cxn)
	require.EqualValues(t, 1, badCalls.Load())

	pool := cxn.sessManager.trustedPool
	assert.NotContains(t, pool.sessMap, bad.Transcoder(), "the faulting orchestrator leaves the pool")
	assert.Greater(t, pool.sus.Suspended(bad.Transcoder()), 0, "the faulting orchestrator is suspended")
	assert.EqualValues(t, 1, goodCalls.Load(), "the warm-up moves on to the other orchestrator")
	if assert.Len(t, pool.lastSess, 1) {
		assert.Same(t, good, pool.lastSess[0], "the other orchestrator's session is the one the first segment reuses")
	}

	require.Equal(t, http.StatusOK, pushLiveSegment(t, s, "/live/mani/0.ts"))
	require.Equal(t, http.StatusOK, pushLiveSegment(t, s, "/live/mani/1.ts"))
	assert.EqualValues(t, 1, badCalls.Load(), "no segment is sent to the orchestrator whose warm-up failed")
	assert.EqualValues(t, 3, goodCalls.Load())
}

// A warm-up that only runs out of time says nothing about the orchestrator:
// its session is kept, nothing else is warmed, and the first segment still
// goes to it.
func TestSessionWarmup_TimeoutKeepsTheOrchestrator(t *testing.T) {
	old := sessionWarmupTimeout
	sessionWarmupTimeout = 600 * time.Millisecond
	t.Cleanup(func() { sessionWarmupTimeout = old })
	var slowCalls orchSetupCalls
	var otherCalls atomic.Int32
	setup := sessionSetupOrchHandler(t, 1500*time.Millisecond, 50*time.Millisecond, &slowCalls)
	s, cxn, slow, _ := twoOrchStream(t, setup, orchResultHandler(t, 0, "", &otherCalls))
	warmUpNow(t, cxn)
	require.EqualValues(t, 1, slowCalls.Load())

	pool := cxn.sessManager.trustedPool
	assert.Contains(t, pool.sessMap, slow.Transcoder(), "a timed-out warm-up keeps the orchestrator")
	assert.Zero(t, pool.sus.Suspended(slow.Transcoder()), "a timed-out warm-up does not suspend the orchestrator")
	assert.Zero(t, otherCalls.Load(), "a timed-out warm-up warms nothing else")

	require.Equal(t, http.StatusOK, pushLiveSegment(t, s, "/live/mani/0.ts"))
	assert.EqualValues(t, 2, slowCalls.Load(), "the first segment goes to the orchestrator the warm-up timed out on")
	assert.Zero(t, otherCalls.Load())
}

// Once a segment of the stream arrived, a failed warm-up warms no other
// orchestrator: the segment's own attempts choose where it goes.
func TestSessionWarmup_FaultAfterFirstSegmentWarmsNothingElse(t *testing.T) {
	var badCalls, goodCalls atomic.Int32
	_, cxn, bad, _ := twoOrchStream(t,
		orchResultHandler(t, 0, "Invalid argument", &badCalls),
		orchResultHandler(t, 0, "", &goodCalls))
	require.True(t, cxn.claimSessionUse(false))
	cxn.claimSessionUse(true)
	cxn.warmSession(context.Background(), &stream.HLSSegment{Data: validPushSegment(t), SeqNo: sessionWarmupSeqNo, Duration: 0.04})
	assert.EqualValues(t, 1, badCalls.Load())
	assert.Zero(t, goodCalls.Load())
	assert.Greater(t, cxn.sessManager.trustedPool.sus.Suspended(bad.Transcoder()), 0)
}
