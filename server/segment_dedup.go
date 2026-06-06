package server

import (
	"context"
	"errors"
	"hash/fnv"
	"strconv"

	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/stream"
)

// errSeqPayloadConflict is returned when a segment sequence number is re-pushed
// with different bytes than the in-flight/completed push for that sequence. It
// is non-retryable (HandlePush surfaces a 4xx).
var errSeqPayloadConflict = errors.New("segment sequence reused with a different payload")

// segCacheMax bounds the per-connection completed-segment cache. Segment
// sequence numbers are monotonic, so first-in-first-out eviction keeps the most
// recent segments; a retry of a segment older than the window re-transcodes,
// which is rare and harmless.
const segCacheMax = 32

// cachedSeg is a completed segment's rendition URLs together with a hash of the
// source bytes that produced them, so a same-seq retry carrying different bytes
// is not served the stale result.
type cachedSeg struct {
	hash uint64
	urls []string
}

// segPayloadHash is a fast, non-cryptographic fingerprint of the segment bytes.
// It only needs to distinguish a genuine retry (identical bytes) from a
// different payload reusing the same sequence number, not resist adversaries.
func segPayloadHash(data []byte) uint64 {
	h := fnv.New64a()
	h.Write(data)
	return h.Sum64()
}

// cachedSegURLs returns the rendition URLs of a previously-completed segment,
// but only when the cached entry was produced from the same payload bytes.
func (cxn *rtmpConnection) cachedSegURLs(seq uint64, hash uint64) ([]string, bool) {
	cxn.segCacheMu.Lock()
	defer cxn.segCacheMu.Unlock()
	c, ok := cxn.segCache[seq]
	if !ok || c.hash != hash {
		return nil, false
	}
	return c.urls, true
}

// putCachedSegURLs records the rendition URLs of a completed segment, evicting
// the oldest entry when the cache is full. It assumes a non-conflicting segment:
// processSegmentDeduped rejects a same-seq/different-payload push (claimSeq)
// before any transcode, so the only way an existing seq is re-recorded here is
// with the same payload hash (an in-place refresh).
func (cxn *rtmpConnection) putCachedSegURLs(seq uint64, hash uint64, urls []string) {
	cxn.segCacheMu.Lock()
	defer cxn.segCacheMu.Unlock()
	if cxn.segCache == nil {
		cxn.segCache = make(map[uint64]cachedSeg, segCacheMax)
	}
	if _, exists := cxn.segCache[seq]; exists {
		cxn.segCache[seq] = cachedSeg{hash: hash, urls: urls}
		return
	}
	if len(cxn.segCacheSeq) >= segCacheMax {
		oldest := cxn.segCacheSeq[0]
		cxn.segCacheSeq = cxn.segCacheSeq[1:]
		delete(cxn.segCache, oldest)
	}
	cxn.segCache[seq] = cachedSeg{hash: hash, urls: urls}
	cxn.segCacheSeq = append(cxn.segCacheSeq, seq)
}

// claimSeq records the first payload hash seen for a sequence number and reports
// a conflict when the same sequence is pushed with different bytes — whether the
// prior push is still in flight (segSeen) or already completed (segCache).
func (cxn *rtmpConnection) claimSeq(seq uint64, hash uint64) (conflict bool) {
	cxn.segCacheMu.Lock()
	defer cxn.segCacheMu.Unlock()
	if c, ok := cxn.segCache[seq]; ok {
		return c.hash != hash
	}
	if h, ok := cxn.segSeen[seq]; ok {
		return h != hash
	}
	if cxn.segSeen == nil {
		cxn.segSeen = make(map[uint64]uint64, segCacheMax)
	}
	if len(cxn.segSeenOrder) >= segCacheMax {
		oldest := cxn.segSeenOrder[0]
		cxn.segSeenOrder = cxn.segSeenOrder[1:]
		delete(cxn.segSeen, oldest)
	}
	cxn.segSeen[seq] = hash
	cxn.segSeenOrder = append(cxn.segSeenOrder, seq)
	return false
}

// processSegmentDeduped wraps processSegment so that duplicate or retried pushes
// of the same segment never start a second transcode. Concurrent in-flight
// pushes of the same (SeqNo, payload) are coalesced into one transcode via
// singleflight; a retry arriving after the original completed returns the cached
// rendition URLs. This is what keeps a same-keyNo re-POST from MistProcLivepeer
// safe instead of poisoning session state with a parallel transcode. A same-seq
// push carrying *different* bytes is a protocol anomaly and is rejected.
func (cxn *rtmpConnection) processSegmentDeduped(ctx context.Context, seg *stream.HLSSegment, segPar *core.SegmentParameters) ([]string, error) {
	hash := segPayloadHash(seg.Data)
	if cxn.claimSeq(seg.SeqNo, hash) {
		clog.Errorf(ctx, "Rejecting push: seqNo=%d reused with a different payload", seg.SeqNo)
		return nil, errSeqPayloadConflict
	}
	if urls, ok := cxn.cachedSegURLs(seg.SeqNo, hash); ok {
		clog.Infof(ctx, "Returning cached transcode for duplicate push seqNo=%d", seg.SeqNo)
		return urls, nil
	}
	key := strconv.FormatUint(seg.SeqNo, 10) + ":" + strconv.FormatUint(hash, 16)
	v, err, shared := cxn.segDedup.Do(key, func() (interface{}, error) {
		if urls, ok := cxn.cachedSegURLs(seg.SeqNo, hash); ok {
			return urls, nil
		}
		urls, err := processSegment(ctx, cxn, seg, segPar)
		// Only cache a real, non-empty transcode. An empty result (no
		// orchestrators available, or a view-only stream) must not be cached: a
		// later re-push of the same segment should re-attempt rather than be
		// served a stale "no renditions" answer.
		if err == nil && len(urls) > 0 {
			cxn.putCachedSegURLs(seg.SeqNo, hash, urls)
		}
		return urls, err
	})
	if shared {
		clog.Infof(ctx, "Joined in-flight transcode for duplicate push seqNo=%d", seg.SeqNo)
	}
	if err != nil {
		return nil, err
	}
	urls, _ := v.([]string)
	return urls, nil
}
