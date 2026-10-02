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
const (
	liveBudgetSlack       = 1000 * time.Millisecond
	vodDefaultBudget      = 30 * time.Second
	segmentResponseMargin = 500 * time.Millisecond
	// singleOrchBudgetShare caps one orchestrator's round trip so a stalled
	// orchestrator always leaves room for another attempt within B.
	singleOrchBudgetShare = 0.8
	// hedgeSegDurShare is the latest point, as a share of segment duration,
	// at which a second orchestrator is started when the first has not
	// answered.
	hedgeSegDurShare = 0.6
	minHedgeDelay    = 250 * time.Millisecond
	// slowStrikeLimit consecutive round trips longer than the segment
	// duration suspend an orchestrator.
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
	total time.Duration
	// hedges counts hedge submissions; shared by every copy of the budget.
	hedges *atomic.Int32
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
	var streamDeadlineMs int
	var workload string
	if params != nil {
		streamDeadlineMs = params.DeadlineMs
		workload = params.Workload
	}
	var total time.Duration
	switch {
	case requestDeadlineMs > 0:
		total = time.Duration(requestDeadlineMs) * time.Millisecond
		if streamDeadlineMs > 0 && requestDeadlineMs > streamDeadlineMs {
			total = time.Duration(streamDeadlineMs) * time.Millisecond
		}
	case streamDeadlineMs > 0:
		total = time.Duration(streamDeadlineMs) * time.Millisecond
	case workload == core.WorkloadVOD:
		total = vodDefaultBudget
	default:
		total = time.Duration(math.Max(segDurSec, 0)*float64(time.Second)) + liveBudgetSlack
	}
	return segmentBudget{start: start, total: total, hedges: new(atomic.Int32)}
}

// deadline is when processing stops.
func (b segmentBudget) deadline() time.Time { return b.start.Add(b.total) }

// respondBy is when HandlePush answers 503 if processing has no result yet.
func (b segmentBudget) respondBy() time.Time {
	margin := segmentResponseMargin
	if margin > b.total/2 {
		margin = b.total / 2
	}
	return b.start.Add(b.total - margin)
}

// orchCap is the longest a single orchestrator round trip may take.
func (b segmentBudget) orchCap() time.Duration {
	return time.Duration(float64(b.total) * singleOrchBudgetShare)
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
