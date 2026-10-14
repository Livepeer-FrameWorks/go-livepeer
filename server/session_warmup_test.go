package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSessionWarmupPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/live/mani/session":     true,
		"/live/a-b_c/session":    true,
		"/live/mani/0.ts":        false,
		"/live/session":          false,
		"/live//session":         false,
		"/live/a/b/session":      false,
		"/live/mani/session.ts":  false,
		"/recordings/m/session":  false,
		"/live/mani/sessionx":    false,
		"/live/mani/notsession":  false,
		"/live/mani/session/":    false,
		"/live/mani/0/session.m": false,
	} {
		assert.Equal(t, want, isSessionWarmupPath(path), path)
	}
}

func TestSessionWarmupDuration(t *testing.T) {
	assert.Equal(t, 40*time.Millisecond, sessionWarmupDuration("40", 25))
	assert.Equal(t, 40*time.Millisecond, sessionWarmupDuration("", 25))
	assert.Equal(t, defaultSessionWarmupDuration, sessionWarmupDuration("bogus", 0))
	assert.Equal(t, defaultSessionWarmupDuration, sessionWarmupDuration("-5", 0))
	assert.Equal(t, maxSessionWarmupDuration, sessionWarmupDuration("60000", 25))
}

// A stream is warmed at most once: a repeated warm-up, or one that arrives
// after a segment, reaches no orchestrator.
func TestSessionWarmup_OncePerStream(t *testing.T) {
	s, calls := coldSessionStream(t, 0, 50*time.Millisecond)
	require.Equal(t, http.StatusAccepted, postSessionWarmup(t, s, "mani"))
	require.Equal(t, http.StatusAccepted, postSessionWarmup(t, s, "mani"))
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.EqualValues(t, 1, calls.Load(), "a repeated warm-up must not reach the orchestrator again")

	require.Equal(t, http.StatusOK, pushLiveSegment(t, s, "/live/mani/0.ts"))
	require.Equal(t, http.StatusAccepted, postSessionWarmup(t, s, "mani"))
	time.Sleep(200 * time.Millisecond)
	require.EqualValues(t, 2, calls.Load())
}

func TestSessionWarmup_AfterFirstSegmentDoesNothing(t *testing.T) {
	s, calls := coldSessionStream(t, 0, 50*time.Millisecond)
	require.Equal(t, http.StatusOK, pushLiveSegment(t, s, "/live/mani/0.ts"))
	require.Equal(t, http.StatusAccepted, postSessionWarmup(t, s, "mani"))
	time.Sleep(200 * time.Millisecond)
	require.EqualValues(t, 1, calls.Load())
}

// The warmed session is the one the first segment uses, and it counts as
// answered, so the first segment's round trip is a normal transcode sample.
func TestSessionWarmup_FirstSegmentReusesTheWarmedSession(t *testing.T) {
	s, calls := coldSessionStream(t, 300*time.Millisecond, 50*time.Millisecond)
	cxn := s.rtmpConnections["mani"]
	require.Equal(t, http.StatusAccepted, postSessionWarmup(t, s, "mani"))
	require.Eventually(t, func() bool {
		cxn.sessManager.sessLock.Lock()
		defer cxn.sessManager.sessLock.Unlock()
		pool := cxn.sessManager.trustedPool
		pool.lock.Lock()
		defer pool.lock.Unlock()
		return len(pool.lastSess) == 1 && !pool.lastSess[0].awaitingFirstResult() && len(pool.lastSess[0].SegsInFlight) == 0
	}, 3*time.Second, 10*time.Millisecond, "the warmed session stays on the reuse list, answered and idle")
	start := time.Now()
	require.Equal(t, http.StatusOK, pushLiveSegment(t, s, "/live/mani/0.ts"))
	assert.Less(t, time.Since(start), 300*time.Millisecond, "segment 0 no longer pays for the setup")
	assert.EqualValues(t, 2, calls.Load())
}

func TestSessionWarmup_RejectsBadRequests(t *testing.T) {
	s, calls := coldSessionStream(t, 0, 0)

	req := httptest.NewRequest(http.MethodGet, "/live/mani/session", nil)
	w := httptest.NewRecorder()
	s.HandlePush(w, req)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/live/mani/session", bytes.NewReader(make([]byte, maxSessionWarmupBytes+1)))
	w = httptest.NewRecorder()
	s.HandlePush(w, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/live/mani/session", bytes.NewReader([]byte("not media")))
	w = httptest.NewRecorder()
	s.HandlePush(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)

	req = httptest.NewRequest(http.MethodPost, "/live/mani/session", bytes.NewReader(validPushSegment(t)))
	req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, `{"unknown":1}`)
	w = httptest.NewRecorder()
	s.HandlePush(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, calls.Load())
	assert.True(t, s.rtmpConnections["mani"].claimSessionUse(false), "a refused warm-up leaves the stream unwarmed")
}

// A warm-up is authorized like a segment: the auth webhook registers a new
// stream bound to the warm-up's job token and source address, and requests
// with another token are refused.
func TestSessionWarmup_AuthorizesLikeASegment(t *testing.T) {
	closeTestIngestAuthIdleConnections(t)
	var hits atomic.Int32
	var got ingestAuthRequest
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		require.NoError(t, json.NewEncoder(w).Encode(canonicalTestAuthResponse("warm")))
	}))
	defer webhook.Close()
	oldURL := AuthWebhookURL
	AuthWebhookURL = mustParseUrl(t, webhook.URL)
	defer func() { AuthWebhookURL = oldURL }()

	s, cancel := setupServerWithCancel()
	defer serverCleanup(s)
	defer cancel()

	warmup := func(token, remoteIP string) int {
		req := httptest.NewRequest(http.MethodPost, "/live/warm/session", bytes.NewReader(validPushSegment(t)))
		req.RemoteAddr = remoteIP + ":4000"
		if token != "" {
			setTestIngestHeader(t, req, token)
		}
		w := httptest.NewRecorder()
		s.HandlePush(w, req)
		return w.Code
	}

	require.Equal(t, http.StatusForbidden, warmup("", "203.0.113.4"))
	require.Zero(t, hits.Load(), "a warm-up without a job token never reaches the webhook")

	require.Equal(t, http.StatusAccepted, warmup("token-a", "203.0.113.4"))
	require.EqualValues(t, 1, hits.Load())
	require.Equal(t, "token-a", got.JobToken)
	require.Equal(t, "203.0.113.4", got.RemoteIP)
	cxn, _, exists := s.lookupHTTPIngest("warm")
	require.True(t, exists, "the warm-up registered the stream")
	require.Equal(t, 12000, cxn.params.DeadlineMs)

	require.Equal(t, http.StatusForbidden, warmup("token-b", "203.0.113.4"))
	require.Equal(t, http.StatusForbidden, warmup("token-a", "203.0.113.5"))
	require.Equal(t, http.StatusAccepted, warmup("token-a", "203.0.113.4"), "a repeated warm-up is accepted")
	require.EqualValues(t, 1, hits.Load(), "a registered stream is not authorized again")
}

// The warm-up's attempt is marked in FrameWorks telemetry so its setup round
// trip is not read as the orchestrator's transcode speed.
func TestSessionWarmup_ContextMarksTheAttempt(t *testing.T) {
	assert.False(t, isSessionWarmup(context.Background()))
	assert.True(t, isSessionWarmup(withSessionWarmup(context.Background())))
}

// One keyframe carries no frame rate, so the stream a warm-up registers takes
// its source frame rate from the client, not from probing the frame: the auth
// webhook checks the job's profiles against it.
func TestSessionWarmup_RegistersTheClientsFrameRate(t *testing.T) {
	closeTestIngestAuthIdleConnections(t)
	var got ingestAuthRequest
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		require.NoError(t, json.NewEncoder(w).Encode(canonicalTestAuthResponse("fps")))
	}))
	defer webhook.Close()
	oldURL := AuthWebhookURL
	AuthWebhookURL = mustParseUrl(t, webhook.URL)
	defer func() { AuthWebhookURL = oldURL }()

	s, cancel := setupServerWithCancel()
	defer serverCleanup(s)
	defer cancel()

	req := httptest.NewRequest(http.MethodPost, "/live/fps/session", bytes.NewReader(validPushSegment(t)))
	req.RemoteAddr = "203.0.113.4:4000"
	req.Header.Set("Content-Frame-Rate", "15")
	setTestIngestHeader(t, req, "token-a")
	w := httptest.NewRecorder()
	s.HandlePush(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)
	assert.InDelta(t, 15.0, got.Source.FPS, 0.001)
}

func TestParseSessionWarmupFrameRate(t *testing.T) {
	for v, want := range map[string]float32{"15": 15, "29.970": 29.97, " 25 ": 25, "1000": 1000} {
		fps, ok := parseSessionWarmupFrameRate(v)
		assert.True(t, ok, v)
		assert.InDelta(t, want, fps, 0.001, v)
	}
	for _, v := range []string{"", "0", "-1", "1001", "NaN", "Inf", "fast"} {
		_, ok := parseSessionWarmupFrameRate(v)
		assert.False(t, ok, v)
	}
}
