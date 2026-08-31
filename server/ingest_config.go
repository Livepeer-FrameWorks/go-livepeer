package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/ffmpeg"
)

const (
	maxIngestProfiles   = 16
	maxIngestDeadlineMs = 60 * 60 * 1000
	maxIngestMinSpeed   = 100.0
	maxIngestJobToken   = 16 * 1024
	maxIngestAuthBody   = 1024 * 1024
)

var ingestConfigKeys = map[string]struct{}{
	"profiles": {}, "workload": {}, "deadlineMs": {}, "minSpeed": {}, "jobToken": {},
}

var ingestAuthResponseKeys = map[string]struct{}{
	"manifestID": {}, "tenantID": {}, "streamID": {}, "profiles": {}, "workload": {},
	"deadlineMs": {}, "minSpeed": {}, "authorizedEdgeNodeID": {}, "specDigest": {},
}

// ingestJobConfig is the complete client-controlled HTTP ingest contract. Keep
// this separate from ingestAuthResponse so a header can never populate fields
// that are authoritative only when returned by Foghorn.
type ingestJobConfig struct {
	Profiles   []ffmpeg.JsonProfile `json:"profiles,omitempty"`
	Workload   string               `json:"workload,omitempty"`
	DeadlineMs int                  `json:"deadlineMs,omitempty"`
	MinSpeed   float64              `json:"minSpeed,omitempty"`
	JobToken   string               `json:"jobToken"`
}

func parseIngestJobConfig(r *http.Request) (*ingestJobConfig, error) {
	raw := r.Header.Get(LIVERPEER_TRANSCODE_CONFIG_HEADER)
	if raw == "" {
		return nil, nil
	}

	var cfg ingestJobConfig
	if err := decodeStrictJSONObject([]byte(raw), ingestConfigKeys, &cfg); err != nil {
		return nil, err
	}
	if err := validateIngestControls(cfg.Profiles, cfg.Workload, cfg.DeadlineMs, cfg.MinSpeed); err != nil {
		return nil, err
	}
	if len(cfg.JobToken) > maxIngestJobToken {
		return nil, fmt.Errorf("jobToken exceeds %d bytes", maxIngestJobToken)
	}
	return &cfg, nil
}

func decodeStrictJSONObject(data []byte, allowed map[string]struct{}, dst interface{}) error {
	keys := json.NewDecoder(strings.NewReader(string(data)))
	first, err := keys.Token()
	if err != nil {
		return err
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return errors.New("expected JSON object")
	}
	seen := make(map[string]struct{}, len(allowed))
	for keys.More() {
		token, err := keys.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("expected JSON object key")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := keys.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := keys.Token(); err != nil {
		return err
	}
	if err := rejectTrailingJSON(keys); err != nil {
		return err
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return rejectTrailingJSON(dec)
}

func rejectTrailingJSON(dec *json.Decoder) error {
	var trailing interface{}
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func validateIngestControls(profiles []ffmpeg.JsonProfile, workload string, deadlineMs int, minSpeed float64) error {
	if len(profiles) > maxIngestProfiles {
		return fmt.Errorf("profiles exceeds maximum of %d", maxIngestProfiles)
	}
	if len(profiles) > 0 {
		if _, err := ffmpeg.ParseProfilesFromJsonProfileArray(profiles); err != nil {
			return fmt.Errorf("invalid profiles: %w", err)
		}
	}
	if workload != "" && workload != core.WorkloadLive && workload != core.WorkloadVOD {
		return fmt.Errorf("invalid workload %q", workload)
	}
	if deadlineMs < 0 || deadlineMs > maxIngestDeadlineMs {
		return fmt.Errorf("deadlineMs must be between 0 and %d", maxIngestDeadlineMs)
	}
	if math.IsNaN(minSpeed) || math.IsInf(minSpeed, 0) || minSpeed < 0 || minSpeed > maxIngestMinSpeed {
		return fmt.Errorf("minSpeed must be between 0 and %g", maxIngestMinSpeed)
	}
	return nil
}

func pixelFormatName(format ffmpeg.PixelFormat) string {
	switch format.RawValue {
	case ffmpeg.PixelFormatYUV420P:
		return "yuv420p"
	case ffmpeg.PixelFormatYUYV422:
		return "yuyv422"
	case ffmpeg.PixelFormatYUV422P:
		return "yuv422p"
	case ffmpeg.PixelFormatYUV444P:
		return "yuv444p"
	case ffmpeg.PixelFormatUYVY422:
		return "uyvy422"
	case ffmpeg.PixelFormatNV12:
		return "nv12"
	case ffmpeg.PixelFormatNV21:
		return "nv21"
	case ffmpeg.PixelFormatYUV420P10BE:
		return "yuv420p10be"
	case ffmpeg.PixelFormatYUV420P10LE:
		return "yuv420p10le"
	case ffmpeg.PixelFormatYUV422P10BE:
		return "yuv422p10be"
	case ffmpeg.PixelFormatYUV422P10LE:
		return "yuv422p10le"
	case ffmpeg.PixelFormatYUV444P10BE:
		return "yuv444p10be"
	case ffmpeg.PixelFormatYUV444P10LE:
		return "yuv444p10le"
	case ffmpeg.PixelFormatYUV420P12BE:
		return "yuv420p12be"
	case ffmpeg.PixelFormatYUV420P12LE:
		return "yuv420p12le"
	case ffmpeg.PixelFormatYUV422P12BE:
		return "yuv422p12be"
	case ffmpeg.PixelFormatYUV422P12LE:
		return "yuv422p12le"
	case ffmpeg.PixelFormatYUV444P12BE:
		return "yuv444p12be"
	case ffmpeg.PixelFormatYUV444P12LE:
		return "yuv444p12le"
	case ffmpeg.PixelFormatYUV420P16BE:
		return "yuv420p16be"
	case ffmpeg.PixelFormatYUV420P16LE:
		return "yuv420p16le"
	case ffmpeg.PixelFormatYUV422P16BE:
		return "yuv422p16be"
	case ffmpeg.PixelFormatYUV422P16LE:
		return "yuv422p16le"
	case ffmpeg.PixelFormatYUV444P16BE:
		return "yuv444p16be"
	case ffmpeg.PixelFormatYUV444P16LE:
		return "yuv444p16le"
	default:
		return "unknown"
	}
}
