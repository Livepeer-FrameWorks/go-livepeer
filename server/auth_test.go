package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/livepeer/lpms/ffmpeg"
	"github.com/stretchr/testify/require"
)

func TestNoAuthIfNoAuthURLPassed(t *testing.T) {
	_, err := authenticateStream(nil, "", nil, "")
	require.NoError(t, err)
}

func TestAuthFailsIfAuthServerDoesNotExist(t *testing.T) {
	badURL, err := url.Parse("http://1.2.3.4.5.6.7.8:1234/nope")
	require.NoError(t, err)

	_, err = authenticateStream(badURL, "", nil, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no such host")
}

func TestAuthFailsIfServerReturnsErrorCode(t *testing.T) {
	s, serverURL := stubAuthServer(t, http.StatusBadRequest, "")
	defer s.Close()

	_, err := authenticateStream(serverURL, "", nil, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status=400")
}

func TestAuthSucceedsIfServerReturnsEmptyBody(t *testing.T) {
	s, serverURL := stubAuthServer(t, http.StatusOK, "")
	defer s.Close()

	_, err := authenticateStream(serverURL, "", nil, "")
	require.NoError(t, err)
}

func TestAuthFailsIfServerReturnsInvalidJSON(t *testing.T) {
	s, serverURL := stubAuthServer(t, http.StatusOK, `{"this": "does not have a closing brace"`)
	defer s.Close()

	_, err := authenticateStream(serverURL, "", nil, "")
	require.EqualError(t, err, "unexpected end of JSON input")
}

func TestAuthFailsIfManifestIDEmpty(t *testing.T) {
	s, serverURL := stubAuthServer(t, http.StatusOK, `{"streamID": "123"}`)
	defer s.Close()

	_, err := authenticateStream(serverURL, "", nil, "")
	require.EqualError(t, err, "empty manifest id not allowed")
}

func TestAuthSucceeds(t *testing.T) {
	s, serverURL := stubAuthServer(t, http.StatusOK, `{"manifestID": "123", "streamID": "456"}`)
	defer s.Close()

	resp, err := authenticateStream(serverURL, "https://some-url.com/test", nil, "")
	require.NoError(t, err)
	require.Equal(t, "123", resp.ManifestID)
	require.Equal(t, "456", resp.StreamID)
}

func TestAuthForwardsTranscodeContext(t *testing.T) {
	var got authWebhookRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.Write([]byte(`{"manifestID":"manifest-1","streamID":"stream-1","profiles":[{"name":"360p","bitrate":900000,"width":544,"height":352,"fps":24000,"fpsDen":1000,"profile":"H264ConstrainedHigh","gop":"0.0"}]}`))
	}))
	defer ts.Close()
	serverURL, err := url.Parse(ts.URL)
	require.NoError(t, err)

	profiles := []ffmpeg.JsonProfile{{
		Name:    "360p",
		Bitrate: 900000,
		Width:   544,
		Height:  352,
		FPS:     24000,
		FPSDen:  1000,
		Profile: "H264ConstrainedHigh",
		GOP:     "0.0",
	}}
	resp, err := authenticateStream(serverURL, "https://gateway/live/manifest-1/0.ts", &authWebhookResponse{Profiles: profiles}, "2718x1750")
	require.NoError(t, err)
	require.Equal(t, "manifest-1", resp.ManifestID)
	require.Equal(t, "https://gateway/live/manifest-1/0.ts", got.URL)
	require.Equal(t, "2718x1750", got.ContentResolution)
	require.Equal(t, profiles, got.Profiles)
}

func TestAILiveAuthSucceeds(t *testing.T) {
	s, serverURL := stubAuthServer(t, http.StatusOK, `{}`)
	defer s.Close()

	resp, err := authenticateAIStream(serverURL, "", AIAuthRequest{
		Stream: "stream",
	})
	require.NoError(t, err)
	require.Equal(t, AIAuthResponse{}, *resp)
}

func TestProfileEqualityWithNoProfiles(t *testing.T) {
	a := authWebhookResponse{}
	b := authWebhookResponse{}

	require.True(t, a.areProfilesEqual(b))
}

func TestProfileEqualityFailsWhenProfilesDiffer(t *testing.T) {
	a := authWebhookResponse{
		Profiles: []ffmpeg.JsonProfile{
			{
				Name:    "Name 1",
				Profile: "Profile 1",
				GOP:     "intra",
				Bitrate: 10000,
				Width:   1024,
				Height:  768,
				FPS:     1,
				Encoder: "encoder-1",
				FPSDen:  1,
			},
		},
	}
	b := authWebhookResponse{
		Profiles: []ffmpeg.JsonProfile{
			{
				Name:    "Name DIFFERENT",
				Profile: "Profile 1",
				GOP:     "intra",
				Bitrate: 10000,
				Width:   1024,
				Height:  768,
				FPS:     1,
				Encoder: "encoder-1",
				FPSDen:  1,
			},
		},
	}

	require.False(t, a.areProfilesEqual(b))
}

func TestProfileEqualityFailsWhenNumProfilesDiffer(t *testing.T) {
	a := authWebhookResponse{
		Profiles: []ffmpeg.JsonProfile{
			{
				Name:    "Name 1",
				Profile: "Profile 1",
				GOP:     "intra",
				Bitrate: 10000,
				Width:   1024,
				Height:  768,
				FPS:     1,
				Encoder: "encoder-1",
				FPSDen:  1,
			},
			{
				Name:    "Name 2",
				Profile: "Profile 2",
				GOP:     "",
				Bitrate: 20000,
				Width:   1,
				Height:  1,
				FPS:     1000,
				Encoder: "encoder-2",
				FPSDen:  900,
			},
		},
	}
	b := authWebhookResponse{
		Profiles: []ffmpeg.JsonProfile{
			{
				Name:    "Name 1",
				Profile: "Profile 1",
				GOP:     "intra",
				Bitrate: 10000,
				Width:   1024,
				Height:  768,
				FPS:     1,
				Encoder: "encoder-1",
				FPSDen:  1,
			},
		},
	}

	require.False(t, a.areProfilesEqual(b))
}

func TestProfileEqualityWithMultipleProfiles(t *testing.T) {
	a := authWebhookResponse{
		Profiles: []ffmpeg.JsonProfile{
			{
				Name:    "Name 1",
				Profile: "Profile 1",
				GOP:     "intra",
				Bitrate: 10000,
				Width:   1024,
				Height:  768,
				FPS:     1,
				Encoder: "encoder-1",
				FPSDen:  1,
			},
			{
				Name:    "Name 2",
				Profile: "Profile 2",
				GOP:     "",
				Bitrate: 20000,
				Width:   1,
				Height:  1,
				FPS:     1000,
				Encoder: "encoder-2",
				FPSDen:  900,
			},
		},
	}
	b := authWebhookResponse{
		Profiles: []ffmpeg.JsonProfile{
			{
				Name:    "Name 1",
				Profile: "Profile 1",
				GOP:     "intra",
				Bitrate: 10000,
				Width:   1024,
				Height:  768,
				FPS:     1,
				Encoder: "encoder-1",
				FPSDen:  1,
			},
			{
				Name:    "Name 2",
				Profile: "Profile 2",
				GOP:     "",
				Bitrate: 20000,
				Width:   1,
				Height:  1,
				FPS:     1000,
				Encoder: "encoder-2",
				FPSDen:  900,
			},
		},
	}

	require.True(t, a.areProfilesEqual(b))
}

func stubAuthServer(t *testing.T, respCode int, respBody string) (*httptest.Server, *url.URL) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(respCode)
				if len(respBody) > 0 {
					fmt.Fprintln(w, respBody)
				}
			},
		),
	)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	return server, serverURL
}
