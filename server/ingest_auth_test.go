package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
	"github.com/stretchr/testify/require"
)

func canonicalTestProfile() ffmpeg.JsonProfile {
	return ffmpeg.JsonProfile{
		Name: "360p", Bitrate: 900000, Width: 544, Height: 352,
		FPS: 24000, FPSDen: 1000, Profile: "H264ConstrainedHigh", GOP: "0.0",
	}
}

func canonicalTestAuthResponse(manifestID string) ingestAuthResponse {
	return ingestAuthResponse{
		ManifestID: manifestID, TenantID: "tenant-1", StreamID: "stream-1",
		Profiles: []ffmpeg.JsonProfile{canonicalTestProfile()}, Workload: "vod",
		DeadlineMs: 12000, MinSpeed: 2.5, AuthorizedEdgeNodeID: "edge-1", SpecDigest: "spec-1",
	}
}

func setTestIngestHeader(t *testing.T, req *http.Request, token string) {
	t.Helper()
	config, err := json.Marshal(ingestJobConfig{Profiles: []ffmpeg.JsonProfile{canonicalTestProfile()}, JobToken: token})
	require.NoError(t, err)
	req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, string(config))
}

func closeTestIngestAuthIdleConnections(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { ingestAuthHTTPClient.CloseIdleConnections() })
}

func TestAuthenticateIngestStreamForwardsGatewayObservedContext(t *testing.T) {
	closeTestIngestAuthIdleConnections(t)
	var got ingestAuthRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		require.NoError(t, json.NewEncoder(w).Encode(canonicalTestAuthResponse("manifest-1")))
	}))
	defer server.Close()
	authURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	cfg := &ingestJobConfig{Profiles: []ffmpeg.JsonProfile{canonicalTestProfile()}, JobToken: "signed-token"}
	source := ingestSource{Width: 1920, Height: 1080, FPS: 29.97, Codec: "h264", PixelFormat: "yuv420p"}
	resp, err := authenticateIngestStream(authURL, "http://gateway/live/manifest-1/0.ts", cfg, source, "203.0.113.4")
	require.NoError(t, err)
	require.Equal(t, "manifest-1", resp.ManifestID)
	require.Equal(t, cfg.Profiles, got.Profiles)
	require.Equal(t, source, got.Source)
	require.Equal(t, "signed-token", got.JobToken)
	require.Equal(t, "203.0.113.4", got.RemoteIP)
}

func TestAuthenticateIngestStreamTimesOut(t *testing.T) {
	originalClient := ingestAuthHTTPClient
	ingestAuthHTTPClient = &http.Client{Timeout: 25 * time.Millisecond}
	t.Cleanup(func() { ingestAuthHTTPClient = originalClient })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	authURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	_, err = authenticateIngestStream(authURL, "http://gateway/live/manifest-1/0.ts", &ingestJobConfig{JobToken: "token"}, ingestSource{}, "203.0.113.4")
	require.Error(t, err)
	require.ErrorContains(t, err, "Client.Timeout")
}

func TestAuthenticateIngestStreamFailsClosed(t *testing.T) {
	closeTestIngestAuthIdleConnections(t)
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "unknown storage field", body: `{"manifestID":"m","objectStore":"s3+http://127.0.0.1/b"}`},
		{name: "wrong field casing", body: `{"ManifestID":"m"}`},
		{name: "duplicate field", body: `{"manifestID":"m","manifestID":"other"}`},
		{name: "incomplete identity", body: `{"manifestID":"m","profiles":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			authURL, err := url.Parse(server.URL)
			require.NoError(t, err)
			_, err = authenticateIngestStream(authURL, "http://gateway/live/m/0.ts", &ingestJobConfig{JobToken: "token"}, ingestSource{}, "203.0.113.4")
			require.Error(t, err)
		})
	}
}

func TestHandlePushRejectsForbiddenHeaderBeforeWebhook(t *testing.T) {
	var hits atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer webhook.Close()
	oldURL := AuthWebhookURL
	AuthWebhookURL = mustParseUrl(t, webhook.URL)
	defer func() { AuthWebhookURL = oldURL }()

	req := httptest.NewRequest(http.MethodPost, "/live/stream/0.ts", bytes.NewReader(validPushSegment(t)))
	req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, `{"jobToken":"token","objectStore":"s3+http://127.0.0.1/b"}`)
	w := httptest.NewRecorder()
	(&LivepeerServer{}).HandlePush(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Zero(t, hits.Load())
}

func TestHandlePushFailsClosedWithoutTokenOrAuthResponse(t *testing.T) {
	closeTestIngestAuthIdleConnections(t)
	var hits atomic.Int32
	webhook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer webhook.Close()
	oldURL := AuthWebhookURL
	AuthWebhookURL = mustParseUrl(t, webhook.URL)
	defer func() { AuthWebhookURL = oldURL }()

	s, cancel := setupServerWithCancel()
	defer serverCleanup(s)
	defer cancel()

	missingToken := httptest.NewRequest(http.MethodPost, "/live/stream/0.ts", bytes.NewReader(validPushSegment(t)))
	w := httptest.NewRecorder()
	s.HandlePush(w, missingToken)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Zero(t, hits.Load())

	emptyResponse := httptest.NewRequest(http.MethodPost, "/live/stream/0.ts", bytes.NewReader(validPushSegment(t)))
	setTestIngestHeader(t, emptyResponse, "token")
	w = httptest.NewRecorder()
	s.HandlePush(w, emptyResponse)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, int32(1), hits.Load())
	require.Empty(t, s.rtmpConnections)
}

func TestHandlePushBindsTokenAndSourceIPForConnection(t *testing.T) {
	closeTestIngestAuthIdleConnections(t)
	var got ingestAuthRequest
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		require.NoError(t, json.NewEncoder(w).Encode(canonicalTestAuthResponse("bound")))
	}))
	defer webhook.Close()
	oldURL := AuthWebhookURL
	AuthWebhookURL = mustParseUrl(t, webhook.URL)
	defer func() { AuthWebhookURL = oldURL }()

	s, cancel := setupServerWithCancel()
	defer serverCleanup(s)
	defer cancel()

	push := func(token, remoteIP string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/live/bound/0.ts", bytes.NewReader(validPushSegment(t)))
		req.RemoteAddr = remoteIP + ":4000"
		observed := canonicalTestProfile()
		observed.Name = "client-observation"
		observed.Bitrate = 1000000
		config, err := json.Marshal(ingestJobConfig{Profiles: []ffmpeg.JsonProfile{observed}, JobToken: token})
		require.NoError(t, err)
		req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, string(config))
		w := httptest.NewRecorder()
		s.HandlePush(w, req)
		return w
	}

	first := push("token-a", "203.0.113.4")
	require.NotEqual(t, http.StatusForbidden, first.Code)
	require.Equal(t, "token-a", got.JobToken)
	require.Equal(t, "203.0.113.4", got.RemoteIP)
	cxn := s.rtmpConnections["bound"]
	require.NotNil(t, cxn)
	require.Equal(t, "360p", cxn.params.Profiles[0].Name)
	require.Equal(t, 12000, cxn.params.DeadlineMs)
	require.Equal(t, "edge-1", cxn.params.AuthorizedEdgeNodeID)
	require.Equal(t, "spec-1", cxn.params.SpecDigest)

	require.Equal(t, http.StatusForbidden, push("token-b", "203.0.113.4").Code)
	require.Equal(t, http.StatusForbidden, push("token-a", "203.0.113.5").Code)
}

func TestGetRemoteAddrTrustsOnlyConfiguredProxy(t *testing.T) {
	trustedProxyAllowlist.RLock()
	old := append([]netip.Prefix(nil), trustedProxyAllowlist.prefixes...)
	trustedProxyAllowlist.RUnlock()
	defer SetTrustedProxyPrefixes(old)
	SetTrustedProxyPrefixes([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("X-Forwarded-For", "198.51.100.8")
	req.RemoteAddr = "127.0.0.1:4000"
	require.Equal(t, "198.51.100.8", getRemoteAddr(req))

	req.RemoteAddr = "203.0.113.9:4000"
	require.Equal(t, "203.0.113.9", getRemoteAddr(req))

	req.RemoteAddr = "127.0.0.1:4000"
	req.Header.Set("X-Forwarded-For", "198.51.100.8, 10.0.0.1")
	require.Equal(t, "127.0.0.1", getRemoteAddr(req))
}
