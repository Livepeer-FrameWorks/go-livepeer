package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/monitor"
	"github.com/livepeer/lpms/ffmpeg"
)

const LIVERPEER_TRANSCODE_CONFIG_HEADER = "Livepeer-Transcode-Configuration"

type authWebhookRequest struct {
	URL               string               `json:"url"`
	Profiles          []ffmpeg.JsonProfile `json:"profiles,omitempty"`
	ContentResolution string               `json:"contentResolution,omitempty"`
}

type ingestSource struct {
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	FPS         float64 `json:"fps"`
	Codec       string  `json:"codec"`
	PixelFormat string  `json:"pixelFormat"`
}

type ingestAuthRequest struct {
	URL      string               `json:"url"`
	Profiles []ffmpeg.JsonProfile `json:"profiles"`
	Source   ingestSource         `json:"source"`
	JobToken string               `json:"jobToken"`
	RemoteIP string               `json:"remoteIP"`
}

// ingestAuthResponse deliberately contains no storage, session, preset or
// reinitialization fields. Foghorn is authoritative for this complete contract.
type ingestAuthResponse struct {
	ManifestID           string               `json:"manifestID"`
	TenantID             string               `json:"tenantID"`
	StreamID             string               `json:"streamID"`
	Profiles             []ffmpeg.JsonProfile `json:"profiles"`
	Workload             string               `json:"workload"`
	DeadlineMs           int                  `json:"deadlineMs"`
	MinSpeed             float64              `json:"minSpeed"`
	AuthorizedEdgeNodeID string               `json:"authorizedEdgeNodeID"`
	SpecDigest           string               `json:"specDigest"`
}

func authenticateIngestStream(authURL *url.URL, incomingRequestURL string, cfg *ingestJobConfig, source ingestSource, remoteIP string) (*ingestAuthResponse, error) {
	if authURL == nil {
		return nil, errors.New("ingest auth webhook is not configured")
	}
	if cfg == nil || cfg.JobToken == "" {
		return nil, errors.New("missing jobToken")
	}
	remoteAddr, err := netip.ParseAddr(remoteIP)
	if err != nil {
		return nil, fmt.Errorf("invalid remoteIP: %w", err)
	}
	remoteIP = remoteAddr.Unmap().String()
	started := time.Now()

	profiles := cfg.Profiles
	if profiles == nil {
		profiles = []ffmpeg.JsonProfile{}
	}
	req := ingestAuthRequest{
		URL:      incomingRequestURL,
		Profiles: profiles,
		Source:   source,
		JobToken: cfg.JobToken,
		RemoteIP: remoteIP,
	}
	jsonValue, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(authURL.String(), "application/json", bytes.NewBuffer(jsonValue))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rbody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxIngestAuthBody+1))
		if readErr != nil {
			return nil, readErr
		}
		return nil, fmt.Errorf("status=%d error=%s", resp.StatusCode, string(rbody))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIngestAuthBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxIngestAuthBody {
		return nil, fmt.Errorf("ingest auth response exceeds %d bytes", maxIngestAuthBody)
	}
	if len(body) == 0 {
		return nil, errors.New("empty ingest auth response")
	}
	var authResp ingestAuthResponse
	if err := decodeStrictJSONObject(body, ingestAuthResponseKeys, &authResp); err != nil {
		return nil, fmt.Errorf("invalid ingest auth response: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("invalid ingest auth response: %w", err)
	}
	for key := range ingestAuthResponseKeys {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("invalid ingest auth response: missing field %q", key)
		}
	}
	if authResp.ManifestID == "" || authResp.TenantID == "" || authResp.StreamID == "" ||
		authResp.Workload == "" || authResp.AuthorizedEdgeNodeID == "" || authResp.SpecDigest == "" {
		return nil, errors.New("incomplete ingest auth identity")
	}
	if len(authResp.Profiles) == 0 {
		return nil, errors.New("empty canonical profiles")
	}
	if err := validateIngestControls(authResp.Profiles, authResp.Workload, authResp.DeadlineMs, authResp.MinSpeed); err != nil {
		return nil, fmt.Errorf("invalid canonical ingest controls: %w", err)
	}

	took := time.Since(started)
	glog.Infof("Stream authentication for authURL=%s url=%s dur=%s", authURL, incomingRequestURL, took)
	if monitor.Enabled {
		monitor.AuthWebhookFinished(took)
	}
	return &authResp, nil
}

// Call a webhook URL, passing the request URL and Mist-provided transcode
// context. The webhook is the single authority that accepts or rejects profiles.
func authenticateStream(authURL *url.URL, incomingRequestURL string, transcodeConfig *authWebhookResponse, contentResolution string) (*authWebhookResponse, error) {
	if authURL == nil {
		return nil, nil
	}
	started := time.Now()

	req := authWebhookRequest{
		URL:               incomingRequestURL,
		ContentResolution: contentResolution,
	}
	if transcodeConfig != nil && len(transcodeConfig.Profiles) > 0 {
		req.Profiles = transcodeConfig.Profiles
	}
	jsonValue, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(authURL.String(), "application/json", bytes.NewBuffer(jsonValue))
	if err != nil {
		return nil, err
	}

	rbody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status=%d error=%s", resp.StatusCode, string(rbody))
	}
	if len(rbody) == 0 {
		return nil, nil
	}

	var authResp authWebhookResponse
	if err = json.Unmarshal(rbody, &authResp); err != nil {
		return nil, err
	}
	if authResp.ManifestID == "" {
		return nil, errors.New("empty manifest id not allowed")
	}

	took := time.Since(started)
	glog.Infof("Stream authentication for authURL=%s url=%s dur=%s", authURL, incomingRequestURL, took)
	if monitor.Enabled {
		monitor.AuthWebhookFinished(took)
	}

	return &authResp, nil
}

// Compare two sets of profiles. Since there's no deep equality method in Go,
// we marshal to JSON and compare the resulting strings
func (a authWebhookResponse) areProfilesEqual(b authWebhookResponse) bool {
	// Return quickly in simple cases without trying to marshal JSON
	if len(a.Profiles) != len(b.Profiles) {
		return false
	}
	if len(a.Profiles) == 0 {
		return true
	}

	profilesA, err := json.Marshal(a.Profiles)
	if err != nil {
		return false
	}

	profilesB, err := json.Marshal(b.Profiles)
	if err != nil {
		return false
	}

	return string(profilesA) == string(profilesB)
}

type AIAuthRequest struct {
	// Stream name or stream key
	Stream    string `json:"stream"`
	StreamKey string `json:"stream_key"`

	// Stream type, eg RTMP or WHIP
	Type string `json:"type"`

	// Query parameters that came with the stream, if any
	QueryParams string `json:"query_params,omitempty"`

	// Gateway host
	GatewayHost string `json:"gateway_host"`
	WhepURL     string `json:"whep_url"`
	StatusURL   string `json:"status_url"`
	UpdateURL   string `json:"update_url"`
}

// Contains the configuration parameters for this AI job
type AIAuthResponse struct {
	// Where to send the output video
	RTMPOutputURL string `json:"rtmp_output_url"`

	// Name of the pipeline to run
	Pipeline string `json:"pipeline"`

	// ID of the pipeline to run
	PipelineID string `json:"pipeline_id"`

	// ID of the stream
	StreamID string `json:"stream_id"`

	// Parameters for the pipeline
	PipelineParams json.RawMessage        `json:"pipeline_parameters"`
	paramsMap      map[string]interface{} // unmarshaled params
}

func authenticateAIStream(authURL *url.URL, apiKey string, req AIAuthRequest) (*AIAuthResponse, error) {
	req.StreamKey = req.Stream
	if authURL == nil {
		return nil, fmt.Errorf("No auth URL configured")
	}
	started := time.Now()

	jsonValue, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	request, err := http.NewRequest("POST", authURL.String(), bytes.NewBuffer(jsonValue))
	if err != nil {
		return nil, err
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", apiKey)
	request.Header.Set("Authorization", apiKey)

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}

	rbody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("status=%d error=%s", resp.StatusCode, string(rbody))
	}

	took := time.Since(started)
	glog.Infof("AI Stream authentication for authURL=%s stream=%s dur=%s", authURL, req.Stream, took)
	if monitor.Enabled {
		monitor.AuthWebhookFinished(took)
	}

	var authResp AIAuthResponse
	if err := json.Unmarshal(rbody, &authResp); err != nil {
		return nil, err
	}
	if len(authResp.PipelineParams) > 0 {
		if err := json.Unmarshal([]byte(authResp.PipelineParams), &authResp.paramsMap); err != nil {
			return nil, err
		}
	}

	return &authResp, nil
}
