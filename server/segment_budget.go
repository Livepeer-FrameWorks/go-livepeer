package server

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/stream"
)

// Segment response budget.
//
// Every HTTP-ingest segment gets a budget B: the time the edge waits for this
// push before it gives up. The gateway spends B on upload, transcode and
// rendition download across every orchestrator it tries, and answers with a
// definitive 503 segmentResponseMargin before B runs out when it has no
// result. Processing runs detached from the HTTP request, so a re-POST of the
// same segment (or a concurrent duplicate) joins the in-flight transcode or
// gets the cached result instead of starting over.
//
// B comes from the stream's workload contract (see doc/reliability.md,
// "Segment budget and the deadlineMs contract"):
//   - deadlineMs in this request's Livepeer-Transcode-Configuration header,
//     capped by the stream's authoritative deadlineMs when one is set;
//   - otherwise the stream's deadlineMs (auth webhook response, or the first
//     request's header when no webhook is configured);
//   - otherwise segment duration + liveBudgetSlack for live, and
//     vodDefaultBudget for vod.
//
// A segment that a session still has to be set up for (see
// resolveSessionSetupBudget) gets sessionSetupAllowance on top. One
// orchestrator submission runs until processing ends; hedging, not a
// per-orchestrator cap, is how a second orchestrator gets the segment while
// the first is slow.
const (
	liveBudgetSlack       = 1000 * time.Millisecond
	vodDefaultBudget      = 30 * time.Second
	segmentResponseMargin = 500 * time.Millisecond
	// sessionSetupAllowance is the extra time a segment gets when the stream
	// first has to be registered and an orchestrator has to set its session
	// up (transcoder assignment, decoder, encoders) before the segment's own
	// transcode. It covers the slowest first segments observed on public
	// orchestrators: from the gateway receiving segment 0 to its answer, a
	// cold session took up to about 4.5 s where a 2 s segment's budget
	// answers by 2.5 s. Edges add the same allowance to the deadlineMs of a
	// session's first segment.
	sessionSetupAllowance = 2000 * time.Millisecond
	// hedgeSegDurShare is the latest point, as a share of segment duration,
	// at which a second orchestrator is started when the first has not
	// answered.
	hedgeSegDurShare = 0.6
	minHedgeDelay    = 250 * time.Millisecond
	// slowStrikeLimit consecutive round trips longer than the workload
	// tolerates (see recordRoundTrip) suspend an orchestrator.
	slowStrikeLimit = 2
)

var (
	// errSegmentBudgetExhausted is returned when B ran out before any
	// orchestrator produced a result. HandlePush answers 503.
	errSegmentBudgetExhausted = errors.New("segment budget exhausted")
	// errBadInput marks a segment that at least two distinct orchestrators
	// rejected with the same non-retryable error. HandlePush answers 422.
	errBadInput = errors.New("segment rejected by multiple orchestrators")
	// errHedgeLost is the cancellation cause of a submission whose segment was
	// already answered by another orchestrator.
	errHedgeLost = errors.New("segment answered by another orchestrator")
)

// badInputConsensus is the number of distinct orchestrators that must return
// the same non-retryable error before the segment itself is judged bad.
const badInputConsensus = 2

// maxHedgesPerSegment bounds hedging across all attempts of one segment, so
// hedging adds at most one extra paid submission per segment.
const maxHedgesPerSegment = 1

type segmentBudget struct {
	start time.Time
	// total is B: how long the edge waits for this push.
	total time.Duration
	// processing, when longer than total, is how long the segment's
	// processing keeps running for a re-POST to join after the push itself
	// was answered (see resolveSessionSetupBudget).
	processing time.Duration
	// hedges counts hedge submissions; shared by every copy of the budget.
	hedges *atomic.Int32
	// fetching, when set, is told whether processing holds an orchestrator's
	// transcode result and is fetching its renditions.
	fetching func(inHand bool)
}

// setFetching reports whether processing holds a transcode result whose
// renditions it is fetching.
func (b segmentBudget) setFetching(inHand bool) {
	if b.fetching != nil {
		b.fetching(inHand)
	}
}

// takeHedge reserves one hedge submission, reporting false once the segment
// has used maxHedgesPerSegment.
func (b segmentBudget) takeHedge() bool {
	if b.hedges == nil {
		return false
	}
	if b.hedges.Add(1) > maxHedgesPerSegment {
		b.hedges.Add(-1)
		return false
	}
	return true
}

type segmentBudgetKey struct{}

func withSegmentBudget(ctx context.Context, b segmentBudget) context.Context {
	return context.WithValue(ctx, segmentBudgetKey{}, b)
}

func segmentBudgetFromContext(ctx context.Context) (segmentBudget, bool) {
	b, ok := ctx.Value(segmentBudgetKey{}).(segmentBudget)
	return b, ok
}

// resolveSegmentBudget returns B for one segment. requestDeadlineMs is the
// deadlineMs of the request carrying the segment (0 when absent).
func resolveSegmentBudget(params *core.StreamParameters, requestDeadlineMs int, segDurSec float64, start time.Time) segmentBudget {
	return resolveBudget(params, requestDeadlineMs, segDurSec, start, 0)
}

// resolveSessionSetupBudget returns the budget of a segment that pays for
// setting up the orchestrator session it goes to: segment 0 of a stream, or a
// segment arriving while the stream has no orchestrator session that has
// returned a result (sessionSetupPending). Such a segment gets B +
// sessionSetupAllowance:
//   - without a request deadlineMs, B is the stream's (or default) budget plus
//     the allowance;
//   - a request deadlineMs is how long the edge waits, and an edge that knows
//     the segment starts a session already includes the allowance in it, so
//     the stream's deadlineMs caps it only after adding the allowance;
//   - processing runs at least until the stream's budget plus the allowance
//     even when the request's own deadline ends earlier, so the edge's
//     re-POST of a segment it sent with its steady deadline joins the
//     transcode instead of finding it cancelled.
func resolveSessionSetupBudget(params *core.StreamParameters, requestDeadlineMs int, segDurSec float64, start time.Time) segmentBudget {
	return resolveBudget(params, requestDeadlineMs, segDurSec, start, sessionSetupAllowance)
}

func resolveBudget(params *core.StreamParameters, requestDeadlineMs int, segDurSec float64, start time.Time, allowance time.Duration) segmentBudget {
	var streamDeadlineMs int
	var workload string
	if params != nil {
		streamDeadlineMs = params.DeadlineMs
		workload = params.Workload
	}
	streamDeadline := time.Duration(streamDeadlineMs) * time.Millisecond
	// own is the budget the gateway derives without a request deadline.
	var own time.Duration
	switch {
	case streamDeadlineMs > 0:
		own = streamDeadline
	case workload == core.WorkloadVOD:
		own = vodDefaultBudget
	default:
		own = time.Duration(math.Max(segDurSec, 0)*float64(time.Second)) + liveBudgetSlack
	}
	own += allowance
	total := own
	if requestDeadlineMs > 0 {
		total = time.Duration(requestDeadlineMs) * time.Millisecond
		if streamDeadlineMs > 0 && total > streamDeadline+allowance {
			total = streamDeadline + allowance
		}
	}
	processing := total
	if allowance > 0 && own > processing {
		processing = own
	}
	return segmentBudget{start: start, total: total, processing: processing, hedges: new(atomic.Int32)}
}

// deadline is when the edge's wait for this push ends.
func (b segmentBudget) deadline() time.Time { return b.start.Add(b.total) }

// processingDeadline is when processing stops: the deadline, or later for a
// segment whose processing runs past the edge's wait.
func (b segmentBudget) processingDeadline() time.Time {
	if b.processing > b.total {
		return b.start.Add(b.processing)
	}
	return b.deadline()
}

// respondBy is when HandlePush answers 503 if processing has no result yet.
// segmentResponseMargin before the deadline leaves the edge room to re-POST
// and join the work. When processing already holds a transcode result and is
// fetching renditions, a re-POST cannot get it sooner, so HandlePush waits
// for it until deadline instead.
func (b segmentBudget) respondBy() time.Time {
	margin := segmentResponseMargin
	if margin > b.total/2 {
		margin = b.total / 2
	}
	return b.start.Add(b.total - margin)
}

// budgetedMaxAttempts is how many orchestrators a segment with budget b may
// try: one per segment duration that fits in the budget, counting segments
// shorter than a second as one second, and never fewer than MaxAttempts. The
// budget still ends processing first when attempts are slow.
func budgetedMaxAttempts(b segmentBudget, segDurSec float64) int {
	segDur := time.Duration(segDurSec * float64(time.Second))
	if segDur < time.Second {
		segDur = time.Second
	}
	n := int(math.Ceil(float64(b.total) / float64(segDur)))
	if n < MaxAttempts {
		return MaxAttempts
	}
	return n
}

// hedgeDelay is how long the gateway waits for the first orchestrator's result
// before also submitting the segment to a second one: the earlier of
// hedgeSegDurShare of the segment duration and the orchestrator's estimated
// 90th-percentile round trip, never below minHedgeDelay.
func hedgeDelay(seg *stream.HLSSegment, p90 time.Duration, haveP90 bool) time.Duration {
	d := time.Duration(seg.Duration * hedgeSegDurShare * float64(time.Second))
	if haveP90 && p90 > 0 && p90 < d {
		d = p90
	}
	if d < minHedgeDelay {
		d = minHedgeDelay
	}
	return d
}

// orchError is an error returned by, or attributed to, one remote
// orchestrator. The gateway suspends that orchestrator and retries the
// segment elsewhere; the error never describes the segment by itself.
type orchError struct {
	orch string
	err  error
}

func (e *orchError) Error() string { return e.err.Error() }
func (e *orchError) Unwrap() error { return e.err }

// badInputTracker counts, per non-retryable error message, the distinct
// orchestrators that returned it for one segment.
type badInputTracker map[string]map[string]struct{}

// observe records err and reports whether it reached badInputConsensus.
func (t badInputTracker) observe(err error) bool {
	var oe *orchError
	if !errors.As(err, &oe) || !isNonRetryableError(oe.err) {
		return false
	}
	msg := oe.err.Error()
	if t[msg] == nil {
		t[msg] = map[string]struct{}{}
	}
	t[msg][oe.orch] = struct{}{}
	return len(t[msg]) >= badInputConsensus
}

// orchs returns the orchestrators that agreed on msg.
func (t badInputTracker) orchs(msg string) []string {
	out := make([]string, 0, len(t[msg]))
	for o := range t[msg] {
		out = append(out, o)
	}
	return out
}
