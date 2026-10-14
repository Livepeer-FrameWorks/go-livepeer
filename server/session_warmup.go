package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/stream"
)

// Session warm-up.
//
// An orchestrator sets a stream's session up (transcoder assignment, decoder,
// encoders) when the session's first segment arrives, which on a cold public
// orchestrator takes longer than a live segment's whole budget. An HTTP-ingest
// client posts the stream's first keyframe, as MPEG-TS, to
// POST /live/<manifestID>/session as soon as it has it, with the same
// Livepeer-Transcode-Configuration header as its segments. The gateway
// authorizes and registers the stream as a segment would, answers 202, and in
// the background submits the keyframe to the orchestrator session the stream's
// first segment will use, discarding the result. The setup then runs while the
// client is still filling segment 0, and segment 0's budget pays only for the
// part of the setup that is left.
//
// A stream is warmed at most once: a warm-up for a stream whose sessions a
// warm-up or a segment already used is answered 202 without doing anything.
const (
	sessionWarmupSuffix = "/session"
	// maxSessionWarmupBytes bounds the keyframe a warm-up carries.
	maxSessionWarmupBytes = 8 << 20
	// defaultSessionWarmupDuration is the duration a warm-up keyframe is
	// submitted with when the request has none and the frame rate is unknown.
	defaultSessionWarmupDuration = 40 * time.Millisecond
	// maxSessionWarmupDuration caps the duration a warm-up keyframe is
	// submitted (and paid) with.
	maxSessionWarmupDuration = time.Second
	// sessionWarmupSeqNo numbers the warm-up keyframe so that it never shares
	// a sequence number, and so an orchestrator-side file name, with a
	// segment of the stream.
	sessionWarmupSeqNo = math.MaxInt64
	// sessionWarmupDisposition marks the attempt of a warm-up in FrameWorks
	// telemetry: its round trip is session setup, not transcode speed.
	sessionWarmupDisposition = "session_warmup"
)

// sessionWarmupTimeout bounds the background work of one warm-up: discovery
// and the orchestrator's answer.
var sessionWarmupTimeout = common.HTTPTimeout

// isSessionWarmupPath reports whether p is /live/<manifestID>/session.
func isSessionWarmupPath(p string) bool {
	rest, ok := strings.CutPrefix(p, "/live/")
	if !ok {
		return false
	}
	mid, ok := strings.CutSuffix(rest, sessionWarmupSuffix)
	return ok && mid != "" && !strings.Contains(mid, "/")
}

type sessionWarmupKey struct{}

func withSessionWarmup(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionWarmupKey{}, true)
}

func isSessionWarmup(ctx context.Context) bool {
	v, _ := ctx.Value(sessionWarmupKey{}).(bool)
	return v
}

// HandleSessionWarmup handles POST /live/<manifestID>/session; see the
// package comment on session warm-up above.
func (s *LivepeerServer) HandleSessionWarmup(w http.ResponseWriter, r *http.Request) {
	errorOut := func(status int, format string, params ...interface{}) {
		msg := fmt.Sprintf(format, params...)
		glog.Errorf("%s statusCode=%d", msg, status)
		http.Error(w, msg, status)
	}
	if r.Method != http.MethodPost {
		errorOut(http.StatusMethodNotAllowed, "session warm-up wrong method=%s url=%s", r.Method, r.URL)
		return
	}
	ingestConfig, err := parseIngestJobConfig(r)
	if err != nil {
		errorOut(http.StatusBadRequest, "failed to parse transcode config header: %q", err)
		return
	}
	remoteAddr, err := ingestRemoteIP(r)
	if AuthWebhookURL != nil && (err != nil || ingestConfig == nil || ingestConfig.JobToken == "") {
		errorOut(http.StatusForbidden, "Missing or invalid ingest authorization")
		return
	}
	body, err := common.ReadAtMost(r.Body, maxSessionWarmupBytes)
	r.Body.Close()
	if err != nil {
		errorOut(http.StatusRequestEntityTooLarge, "session warm-up body unreadable or larger than %d bytes: %s", maxSessionWarmupBytes, err)
		return
	}
	reqURL := &url.URL{Scheme: "http", Host: r.Host, Path: r.URL.Path}
	mid := core.ManifestID(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/live/"), sessionWarmupSuffix))
	ctx := clog.AddManifestID(r.Context(), string(mid))
	ctx = clog.AddVal(ctx, clog.ClientIP, remoteAddr)

	status, mediaFormat, err := ffmpeg.GetCodecInfoBytes(body)
	if err != nil || status != ffmpeg.CodecStatusOk || mediaFormat.Vcodec == "" {
		errorOut(http.StatusUnprocessableEntity, "Invalid session warm-up media url=%s status=%d vcodec=%q err=%v", reqURL, status, mediaFormat.Vcodec, err)
		return
	}
	// One frame carries no frame rate; probing it reports a guess. The stream
	// registers with the source frame rate the auth webhook checks the
	// profiles against, so it comes from the client, which knows it.
	if fps, ok := parseSessionWarmupFrameRate(r.Header.Get(sessionWarmupFrameRateHeader)); ok {
		mediaFormat.FPS = fps
	}
	vcodecVal, ok := ffmpeg.FfmpegNameToVideoCodec[mediaFormat.Vcodec]
	if !ok {
		errorOut(http.StatusUnprocessableEntity, "Unknown input stream codec=%s", mediaFormat.Vcodec)
		return
	}

	cxn, mid, exists := s.lookupHTTPIngest(mid)
	if exists {
		if !ingestAuthorizationMatches(cxn, ingestConfig, remoteAddr) {
			errorOut(http.StatusForbidden, "Ingest authorization changed for active stream")
			return
		}
	} else {
		var code int
		cxn, mid, code, err = s.registerHTTPIngest(ctx, reqURL, mid, ingestConfig, remoteAddr, mediaFormat, &vcodecVal,
			common.ProfileExtensionFormat(".ts"), &core.SegmentParameters{})
		if err != nil {
			errorOut(code, "%s", err.Error())
			return
		}
	}
	s.connectionLock.Lock()
	cxn.lastUsed = time.Now()
	s.connectionLock.Unlock()
	ctx = clog.AddManifestID(ctx, string(mid))
	ctx = clog.AddNonce(ctx, cxn.nonce)

	if !cxn.claimSessionUse(false) {
		clog.V(common.DEBUG).Infof(ctx, "Session warm-up skipped: the stream's sessions are already in use")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	seg := &stream.HLSSegment{
		Data:     body,
		SeqNo:    sessionWarmupSeqNo,
		Duration: sessionWarmupDuration(r.Header.Get("Content-Duration"), mediaFormat.FPS).Seconds(),
	}
	clog.Infof(ctx, "Warming up the stream's orchestrator session bytes=%d resolution=%dx%d", len(body), mediaFormat.Width, mediaFormat.Height)
	go cxn.warmSession(clog.Clone(context.Background(), ctx), seg)
	w.WriteHeader(http.StatusAccepted)
}

// sessionWarmupFrameRateHeader carries the source frame rate of a warm-up in
// frames per second.
const sessionWarmupFrameRateHeader = "Content-Frame-Rate"

// parseSessionWarmupFrameRate parses a frame rate header value, accepting
// rates above 0 and up to 1000 frames per second.
func parseSessionWarmupFrameRate(v string) (float32, bool) {
	fps, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || !(fps > 0 && fps <= 1000) {
		return 0, false
	}
	return float32(fps), true
}

// sessionWarmupDuration is the duration a warm-up keyframe is submitted with:
// the request's Content-Duration in milliseconds, else one frame at fps.
func sessionWarmupDuration(header string, fps float32) time.Duration {
	d := defaultSessionWarmupDuration
	if ms, err := strconv.Atoi(header); err == nil && ms > 0 {
		d = time.Duration(ms) * time.Millisecond
	} else if fps > 0 {
		d = time.Duration(float64(time.Second) / float64(fps))
	}
	if d > maxSessionWarmupDuration {
		d = maxSessionWarmupDuration
	}
	return d
}

// warmSession submits seg to the orchestrator session the stream's next
// segment will use and discards the result, so that orchestrator sets the
// session up before the stream's first segment arrives. The session stays
// first on the reuse list, marked as answered, so the first segment uses it
// and its round trip counts as the orchestrator's transcode speed.
//
// A failed warm-up is handled as a failed segment attempt is: unless the
// warm-up ran out of time, its orchestrator is suspended and removed, so the
// stream's first segment is not sent to it. The keyframe then goes to one more
// session that is already available, within the same sessionWarmupTimeout and
// only while no segment of the stream has arrived.
func (cxn *rtmpConnection) warmSession(ctx context.Context, seg *stream.HLSSegment) {
	ctx, cancel := context.WithTimeout(ctx, sessionWarmupTimeout)
	defer cancel()
	start := time.Now()
	for attempt := 1; attempt <= maxSessionWarmupAttempts; attempt++ {
		if attempt > 1 && cxn.segmentArrived() {
			// The segment's own attempts choose its orchestrator; a second
			// warm-up would only pay for a keyframe nothing waits on.
			clog.Infof(ctx, "Session warm-up not moved to another orchestrator: a segment of the stream arrived")
			return
		}
		if !cxn.warmOneSession(ctx, seg, start, attempt > 1) {
			return
		}
	}
}

// maxSessionWarmupAttempts bounds the orchestrators one warm-up submits its
// keyframe to.
const maxSessionWarmupAttempts = 2

// warmOneSession submits seg to one orchestrator session and reports whether
// that orchestrator failed it and was suspended, so that another session may
// be warmed. A retry takes only a session already available.
func (cxn *rtmpConnection) warmOneSession(ctx context.Context, seg *stream.HLSSegment, start time.Time, retry bool) bool {
	bsm := cxn.sessManager
	sessions := bsm.selectSessions(ctx)
	if len(sessions) == 0 && !retry && bsm.hasOrchestratorPool() {
		sessions = bsm.waitForSessions(ctx)
	}
	if len(sessions) == 0 {
		clog.Infof(ctx, "Session warm-up found no orchestrator session took=%s", time.Since(start))
		return false
	}
	sess := sessions[0]
	ext, err := common.ProfileFormatExtension(cxn.profile.Format)
	if err != nil {
		ext = ".ts"
	}
	segOut, err := prepareForTranscoding(ctx, cxn, sess, seg, fmt.Sprintf("%s/session%s", cxn.profile.Name, ext))
	if err != nil {
		// prepareForTranscoding suspended and removed the orchestrator.
		clog.Warningf(ctx, "Session warm-up could not prepare orch=%s err=%q", sess.Transcoder(), err)
		return ctx.Err() == nil
	}
	// A segment that arrives while the orchestrator still sets the session up
	// joins this session instead of moving to another, cold, orchestrator.
	sess.pushInFlight(sessionWarmupTimeout)
	res, err := SubmitSegment(withSessionWarmup(ctx), sess.Clone(), segOut, nil, cxn.nonce, false)
	if err != nil {
		clog.Warningf(ctx, "Session warm-up failed orch=%s took=%s err=%q", sess.Transcoder(), time.Since(start), err)
		if ctx.Err() != nil {
			// The warm-up ran out of time, which says nothing about the
			// orchestrator: the session setup it started may still finish.
			bsm.releaseSession(sess)
			return false
		}
		// As for a segment attempt, an error the orchestrator returned,
		// non-retryable ones included, is the orchestrator's: one keyframe
		// cannot show that the input is at fault.
		bsm.suspendAndRemoveOrch(ctx, sess, err)
		return true
	}
	sess.markAnswered()
	// The round trip was session setup; it must not make the session look too
	// slow to reuse for the stream's first segment.
	if thr := bsm.poolFor(sess).latencyScoreThreshold(); res.LatencyScore > thr {
		res.LatencyScore = thr
	}
	updateSession(sess, res)
	bsm.completeSession(ctx, sess, false)
	clog.Infof(ctx, "Session warm-up done orch=%s took=%s", sess.Transcoder(), time.Since(start))
	return false
}
