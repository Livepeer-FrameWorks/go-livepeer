package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/lpms/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSegmentBudget(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	live := &core.StreamParameters{Workload: core.WorkloadLive}
	vod := &core.StreamParameters{Workload: core.WorkloadVOD}

	assert.Equal(3*time.Second, resolveSegmentBudget(live, 0, 2.0, now).total, "live falls back to segDur + 1s")
	assert.Equal(3*time.Second, resolveSegmentBudget(nil, 0, 2.0, now).total, "no workload is live")
	assert.Equal(30*time.Second, resolveSegmentBudget(vod, 0, 2.0, now).total)
	assert.Equal(2500*time.Millisecond, resolveSegmentBudget(live, 2500, 2.0, now).total, "request deadline wins")
	vod.DeadlineMs = 20000
	assert.Equal(20*time.Second, resolveSegmentBudget(vod, 0, 2.0, now).total, "stream deadline")
	assert.Equal(20*time.Second, resolveSegmentBudget(vod, 45000, 2.0, now).total, "stream deadline caps the request")
	assert.Equal(15*time.Second, resolveSegmentBudget(vod, 15000, 2.0, now).total)

	b := resolveSegmentBudget(live, 0, 2.0, now)
	assert.Equal(now.Add(3*time.Second), b.deadline())
	assert.Equal(now.Add(2500*time.Millisecond), b.respondBy())
	assert.Equal(2400*time.Millisecond, b.orchCap())
}

func TestSegmentBudget_OneHedgePerSegment(t *testing.T) {
	b := resolveSegmentBudget(nil, 0, 2.0, time.Now())
	copyOfB := b
	assert.True(t, b.takeHedge())
	assert.False(t, copyOfB.takeHedge(), "the hedge allowance is shared by every attempt of the segment")
	assert.False(t, segmentBudget{}.takeHedge(), "an unbudgeted segment never hedges")
}

func TestHedgeDelay(t *testing.T) {
	assert := assert.New(t)
	seg := &stream.HLSSegment{Duration: 2.0}
	assert.Equal(1200*time.Millisecond, hedgeDelay(seg, 0, false))
	assert.Equal(800*time.Millisecond, hedgeDelay(seg, 800*time.Millisecond, true))
	assert.Equal(1200*time.Millisecond, hedgeDelay(seg, 3*time.Second, true))
	assert.Equal(minHedgeDelay, hedgeDelay(seg, 10*time.Millisecond, true))
}

// A re-POST of a segment joins the transcode started by an earlier request even
// after that request was abandoned, and gets the result.
func TestProcessSegmentDeduped_SurvivesLeaderCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	o := StubBroadcastSession(stubTestTranscoder(ctx, orchResultHandler(t, 400*time.Millisecond, "", &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{o}, nil, true))
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 5, Duration: 2.0}

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := cxn.processSegmentDeduped(leaderCtx, seg, &core.SegmentParameters{}, resolveSegmentBudget(cxn.params, 0, seg.Duration, time.Now()))
		leaderErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	leaderCancel()
	require.ErrorIs(t, <-leaderErr, context.Canceled)

	// Each push carries its own segment, as HandlePush builds one per request.
	repost := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 5, Duration: 2.0}
	urls, err := cxn.processSegmentDeduped(context.Background(), repost, &core.SegmentParameters{}, resolveSegmentBudget(cxn.params, 0, repost.Duration, time.Now()))
	require.NoError(t, err)
	assert.Len(t, urls, 1)
	assert.EqualValues(t, 1, calls.Load(), "the re-POST must join, not transcode again")
}

// Without a result by respondBy the caller gets a definitive budget error ahead
// of the edge's deadline.
func TestProcessSegmentDeduped_RespondsBeforeDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	o := StubBroadcastSession(stubTestTranscoder(ctx, orchResultHandler(t, 10*time.Second, "", &calls)))
	o.Params.Profiles = []ffmpeg.VideoProfile{ffmpeg.P144p30fps16x9}
	cxn := budgetTestConnection(bsmWithSessListExt([]*BroadcastSession{o}, nil, true))
	seg := &stream.HLSSegment{Data: []byte("dummy"), SeqNo: 6, Duration: 1.0}

	start := time.Now()
	budget := resolveSegmentBudget(cxn.params, 0, seg.Duration, start)
	_, err := cxn.processSegmentDeduped(context.Background(), seg, &core.SegmentParameters{}, budget)
	took := time.Since(start)
	require.ErrorIs(t, err, errSegmentBudgetExhausted)
	assert.InDelta(t, float64(budget.total-segmentResponseMargin), float64(took), float64(150*time.Millisecond))
}
