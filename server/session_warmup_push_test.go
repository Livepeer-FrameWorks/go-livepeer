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
	"github.com/stretchr/testify/require"
)

// sessionSetupOrchHandler answers /segment the way an orchestrator answers a
// stream: the first request it gets starts setting the stream's session up,
// which takes setup, and every request is answered work after both its
// arrival and the end of that setup.
func sessionSetupOrchHandler(t *testing.T, setup, work time.Duration, calls *atomic.Int32) http.HandlerFunc {
	tr := &net.TranscodeResult{Result: &net.TranscodeResult_Data{Data: &net.TranscodeData{
		Segments: []*net.TranscodedSegmentData{{Url: "https://orch/rendition.ts", Pixels: 100}},
	}}}
	buf, err := proto.Marshal(tr)
	require.NoError(t, err)
	var mu sync.Mutex
	var setupDone time.Time
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		if setupDone.IsZero() {
			setupDone = time.Now().Add(setup)
		}
		at := setupDone
		mu.Unlock()
		if now := time.Now(); now.After(at) {
			at = now
		}
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
func coldSessionStream(t *testing.T, setup, work time.Duration) (*LivepeerServer, *atomic.Int32) {
	s, cancel := setupServerWithCancel()
	ctx, stopOrch := context.WithCancel(context.Background())
	t.Cleanup(func() {
		stopOrch()
		serverCleanup(s)
		cancel()
	})
	calls := &atomic.Int32{}
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
