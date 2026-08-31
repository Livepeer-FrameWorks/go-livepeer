package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseIngestJobConfig(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/live/stream/0.ts", nil)
	req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, `{"profiles":[],"workload":"vod","deadlineMs":12000,"minSpeed":2.5,"jobToken":"signed-token"}`)

	cfg, err := parseIngestJobConfig(req)
	require.NoError(t, err)
	require.Equal(t, "vod", cfg.Workload)
	require.Equal(t, 12000, cfg.DeadlineMs)
	require.Equal(t, 2.5, cfg.MinSpeed)
	require.Equal(t, "signed-token", cfg.JobToken)
}

func TestParseIngestJobConfigRejectsForbiddenKeys(t *testing.T) {
	for _, key := range []string{
		"objectStore", "recordObjectStore", "manifestID", "sessionID", "streamID",
		"presets", "verificationFreq", "timeoutMultiplier", "clip", "forceSessionReinit",
	} {
		t.Run(key, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/live/stream/0.ts", nil)
			req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, `{"jobToken":"token","`+key+`":"value"}`)
			_, err := parseIngestJobConfig(req)
			require.Error(t, err)
			require.Contains(t, err.Error(), "unknown field")
		})
	}
}

func TestParseIngestJobConfigRejectsBoundsAndTrailingJSON(t *testing.T) {
	values := []string{
		`{"workload":"batch","jobToken":"token"}`,
		`{"deadlineMs":3600001,"jobToken":"token"}`,
		`{"minSpeed":101,"jobToken":"token"}`,
		`{"jobToken":"token"} {}`,
		`{"JobToken":"token"}`,
		`{"jobToken":"one","jobToken":"two"}`,
		`{"jobToken":"` + strings.Repeat("x", maxIngestJobToken+1) + `"}`,
	}
	for _, value := range values {
		req := httptest.NewRequest(http.MethodPost, "/live/stream/0.ts", nil)
		req.Header.Set(LIVERPEER_TRANSCODE_CONFIG_HEADER, value)
		_, err := parseIngestJobConfig(req)
		require.Error(t, err, value)
	}
}
