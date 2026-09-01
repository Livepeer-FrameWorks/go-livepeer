package server

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/stream"
	"github.com/stretchr/testify/assert"
)

func testSegmentFingerprint(label string) segmentFingerprint {
	return segmentFingerprint(sha256.Sum256([]byte(label)))
}

func TestSegCache_PutGetEviction(t *testing.T) {
	assert := assert.New(t)
	cxn := &rtmpConnection{}
	h := testSegmentFingerprint("seven")

	// Fill the cache to its bound.
	for seq := uint64(0); seq < segCacheMax; seq++ {
		cxn.putCachedSegURLs(seq, h, []string{"u"})
	}
	if _, ok := cxn.cachedSegURLs(0, h); !ok {
		t.Fatal("expected seq 0 cached before overflow")
	}

	// One more eviction past the bound drops the oldest (seq 0).
	cxn.putCachedSegURLs(segCacheMax, h, []string{"u"})
	if _, ok := cxn.cachedSegURLs(0, h); ok {
		t.Fatal("expected seq 0 evicted after overflow")
	}
	urls, ok := cxn.cachedSegURLs(segCacheMax, h)
	assert.True(ok)
	assert.Equal([]string{"u"}, urls)
}

func TestSegCache_UpdateInPlaceNoGrowth(t *testing.T) {
	cxn := &rtmpConnection{}
	h1 := testSegmentFingerprint("one")
	h2 := testSegmentFingerprint("two")
	cxn.putCachedSegURLs(5, h1, []string{"a"})
	cxn.putCachedSegURLs(5, h2, []string{"b"})
	urls, ok := cxn.cachedSegURLs(5, h2)
	if !ok || len(urls) != 1 || urls[0] != "b" {
		t.Fatalf("expected in-place update to b, got %v ok=%v", urls, ok)
	}
	if len(cxn.segCacheSeq) != 1 {
		t.Fatalf("expected no order growth on update, got %d", len(cxn.segCacheSeq))
	}
}

func TestSegCache_HashMismatchIsCacheMiss(t *testing.T) {
	cxn := &rtmpConnection{}
	h1 := testSegmentFingerprint("one")
	h2 := testSegmentFingerprint("two")
	cxn.putCachedSegURLs(9, h1, []string{"a"})
	// Same seq, different payload hash → must not return the stale result.
	if _, ok := cxn.cachedSegURLs(9, h2); ok {
		t.Fatal("expected cache miss for same seq with different payload hash")
	}
	// Same seq, matching hash → hit.
	if _, ok := cxn.cachedSegURLs(9, h1); !ok {
		t.Fatal("expected hit for matching seq and hash")
	}
}

func TestClaimSeq_ConflictOnDifferentPayload(t *testing.T) {
	cxn := &rtmpConnection{}
	h1 := testSegmentFingerprint("one")
	h2 := testSegmentFingerprint("two")
	// First push of seq 3 records its hash, no conflict.
	if err := cxn.claimSeq(3, h1); err != nil {
		t.Fatal("first claim should not conflict")
	}
	// Same seq, same payload (legit retry) → no conflict.
	if err := cxn.claimSeq(3, h1); err != nil {
		t.Fatal("same-payload retry should not conflict")
	}
	// Same seq, different payload (in-flight) → conflict.
	if err := cxn.claimSeq(3, h2); err != errSeqPayloadConflict {
		t.Fatal("different-payload same-seq should conflict (in-flight)")
	}
	// A completed seq with a different payload also conflicts.
	cxn2 := &rtmpConnection{}
	cxn2.putCachedSegURLs(4, h1, []string{"u"})
	if err := cxn2.claimSeq(4, h2); err != errSeqPayloadConflict {
		t.Fatal("different-payload same-seq should conflict (completed)")
	}
	if err := cxn2.claimSeq(4, h1); err != nil {
		t.Fatal("same-payload as completed should not conflict")
	}
}

func TestSegmentRequestFingerprint_BindsProcessingControls(t *testing.T) {
	seg := &stream.HLSSegment{Data: []byte("media"), Name: "42.ts", Duration: 2, SeqNo: 42}
	base := segmentRequestFingerprint(seg, &core.SegmentParameters{})

	tests := []struct {
		name   string
		seg    *stream.HLSSegment
		params *core.SegmentParameters
	}{
		{name: "duration", seg: &stream.HLSSegment{Data: []byte("media"), Name: "42.ts", Duration: 3, SeqNo: 42}, params: &core.SegmentParameters{}},
		{name: "name", seg: &stream.HLSSegment{Data: []byte("media"), Name: "42.mp4", Duration: 2, SeqNo: 42}, params: &core.SegmentParameters{}},
		{name: "zero frame", seg: &stream.HLSSegment{Data: []byte("media"), Name: "42.ts", Duration: 2, SeqNo: 42, IsZeroFrame: true}, params: &core.SegmentParameters{}},
		{name: "clip", seg: seg, params: &core.SegmentParameters{Clip: &core.SegmentClip{From: time.Second, To: 2 * time.Second}}},
		{name: "reinit", seg: seg, params: &core.SegmentParameters{ForceSessionReinit: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := segmentRequestFingerprint(tc.seg, tc.params); got == base {
				t.Fatal("processing-control change must produce a distinct fingerprint")
			}
		})
	}
}

func TestClaimSeq_RejectsReplayOutsideWindow(t *testing.T) {
	cxn := &rtmpConnection{}
	for seq := uint64(1); seq <= segCacheMax+1; seq++ {
		if err := cxn.claimSeq(seq, testSegmentFingerprint(string(rune(seq)))); err != nil {
			t.Fatalf("claim seq %d: %v", seq, err)
		}
	}
	if err := cxn.claimSeq(1, testSegmentFingerprint("one")); err != errSeqOutsideReplayWindow {
		t.Fatalf("expected replay-window rejection, got %v", err)
	}
}

func TestLatencyThresholdForWorkload(t *testing.T) {
	assert := assert.New(t)
	// live (or empty) keeps the aggressive default
	assert.Equal(SELECTOR_LATENCY_SCORE_THRESHOLD, latencyThresholdForWorkload(&core.StreamParameters{Workload: core.WorkloadLive, MinSpeed: 0.5}))
	assert.Equal(SELECTOR_LATENCY_SCORE_THRESHOLD, latencyThresholdForWorkload(&core.StreamParameters{}))
	// vod relaxes toward 1/MinSpeed when that is slower than the default
	assert.InDelta(2.0, latencyThresholdForWorkload(&core.StreamParameters{Workload: core.WorkloadVOD, MinSpeed: 0.5}), 1e-9)
	// vod with a fast MinSpeed never tightens below the default
	assert.Equal(SELECTOR_LATENCY_SCORE_THRESHOLD, latencyThresholdForWorkload(&core.StreamParameters{Workload: core.WorkloadVOD, MinSpeed: 2.0}))
	// vod without MinSpeed keeps the default
	assert.Equal(SELECTOR_LATENCY_SCORE_THRESHOLD, latencyThresholdForWorkload(&core.StreamParameters{Workload: core.WorkloadVOD}))
}

func TestOrchHealthStore_ScopedFallbackWhenNoRedis(t *testing.T) {
	store := &orchHealthStore{region: "eu"}
	sus := store.scoped(core.WorkloadVOD, "240p,360p")
	if _, ok := sus.(*suspender); !ok {
		t.Fatalf("expected in-memory *suspender fallback, got %T", sus)
	}
}

func TestSuspensionTTL(t *testing.T) {
	if suspensionTTL(core.WorkloadVOD) != orchHealthVODTTL {
		t.Fatal("vod should use vod TTL")
	}
	if suspensionTTL(core.WorkloadLive) != orchHealthLiveTTL {
		t.Fatal("live should use live TTL")
	}
	if suspensionTTL("") != orchHealthLiveTTL {
		t.Fatal("empty workload should default to live TTL")
	}
}

func TestRedisSuspenderKey(t *testing.T) {
	r := &redisSuspender{store: &orchHealthStore{region: "eu-west"}, workload: core.WorkloadVOD, capKey: "240p,360p"}
	got := r.key("https://orch.example:8935")
	want := "orchhealth:eu-west:vod:240p,360p:https://orch.example:8935"
	if got != want {
		t.Fatalf("key mismatch:\n got=%s\nwant=%s", got, want)
	}
}
