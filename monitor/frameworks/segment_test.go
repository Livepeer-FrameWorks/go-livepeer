package frameworks

import (
	"context"
	"testing"
	"time"
)

func TestFinalSegmentUsesSelectedResponseAndCountsHedges(t *testing.T) {
	ctx := WithSegment(context.Background())
	SegmentSelectionTime(ctx, time.Second)
	SegmentSubmitted(ctx, "https://slow.example")
	SegmentSubmitted(ctx, "https://winner.example")
	winner := WithAttempt(ctx, "")
	loser := WithAttempt(ctx, "")
	SetRemoteIP(winner, "192.0.2.2")
	SelectedSegmentResult(ctx, "winner", "https://winner.example", RemoteIP(winner))
	SetRemoteIP(loser, "192.0.2.1")
	address, _, ip, attempts, fallbacks, setup := SegmentSnapshot(ctx)
	if address != "winner" || ip != "192.0.2.2" || attempts != 2 || fallbacks != 1 || setup != 0 {
		t.Fatalf("bad final attribution: %s %s %d %d %s", address, ip, attempts, fallbacks, setup)
	}
	BeginSegmentAttempt(ctx)
	SegmentSelectionTime(ctx, 2*time.Second)
	_, uri, ip, _, _, setup := SegmentSnapshot(ctx)
	if uri != "" || ip != "" || setup != 2*time.Second {
		t.Fatal("retry retained previous attempt attribution or lost replacement timing")
	}
}
