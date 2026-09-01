package server

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strconv"

	"github.com/livepeer/go-livepeer/clog"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/lpms/stream"
)

// errSeqPayloadConflict is returned when a segment sequence number is re-pushed
// with different bytes than the in-flight/completed push for that sequence. It
// is non-retryable (HandlePush surfaces a 4xx).
var errSeqPayloadConflict = errors.New("segment sequence reused with a different request")

// errSeqOutsideReplayWindow is returned when a sequence number has fallen out
// of the bounded replay window. Reprocessing it would turn an old retry into a
// fresh paid transcode.
var errSeqOutsideReplayWindow = errors.New("segment sequence is outside the replay window")

// segCacheMax bounds the per-connection completed-segment cache. Segment
// sequence numbers are monotonic, so first-in-first-out eviction keeps the most
// recent segments. Requests older than the retained window are rejected rather
// than retranscoded.
const segCacheMax = 32

type segmentFingerprint [sha256.Size]byte

// cachedSeg is a completed segment's rendition URLs together with the request
// fingerprint that produced them, so a conflicting same-seq retry is not served
// a stale result.
type cachedSeg struct {
	hash segmentFingerprint
	urls []string
}

// segmentRequestFingerprint binds deduplication to everything that changes the
// transcode result. It is deliberately cryptographic because the HTTP-ingest
// peer is authenticated but not trusted to preserve request identity.
func segmentRequestFingerprint(seg *stream.HLSSegment, segPar *core.SegmentParameters) segmentFingerprint {
	h := sha256.New()
	writeBytes := func(v []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(v)))
		h.Write(size[:])
		h.Write(v)
	}
	writeUint64 := func(v uint64) {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}

	writeBytes(seg.Data)
	writeBytes([]byte(seg.Name))
	writeUint64(math.Float64bits(seg.Duration))
	if seg.IsZeroFrame {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	if segPar == nil {
		h.Write([]byte{0})
	} else {
		h.Write([]byte{1})
		if segPar.Clip == nil {
			h.Write([]byte{0})
		} else {
			h.Write([]byte{1})
			writeUint64(uint64(segPar.Clip.From))
			writeUint64(uint64(segPar.Clip.To))
		}
		if segPar.ForceSessionReinit {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}

	var fingerprint segmentFingerprint
	copy(fingerprint[:], h.Sum(nil))
	return fingerprint
}

// cachedSegURLs returns the rendition URLs of a previously-completed segment,
// but only when the cached entry was produced from the same request.
func (cxn *rtmpConnection) cachedSegURLs(seq uint64, hash segmentFingerprint) ([]string, bool) {
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
// processSegmentDeduped rejects a same-seq/different-request push (claimSeq)
// before any transcode, so the only way an existing seq is re-recorded here is
// with the same request fingerprint (an in-place refresh).
func (cxn *rtmpConnection) putCachedSegURLs(seq uint64, hash segmentFingerprint, urls []string) {
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

// claimSeq records the first fingerprint seen for a sequence number and reports
// a conflict when the same sequence is pushed with different media or controls — whether the
// prior push is still in flight (segSeen) or already completed (segCache).
func (cxn *rtmpConnection) claimSeq(seq uint64, hash segmentFingerprint) error {
	cxn.segCacheMu.Lock()
	defer cxn.segCacheMu.Unlock()
	if c, ok := cxn.segCache[seq]; ok {
		if c.hash != hash {
			return errSeqPayloadConflict
		}
		return nil
	}
	if h, ok := cxn.segSeen[seq]; ok {
		if h != hash {
			return errSeqPayloadConflict
		}
		return nil
	}
	if cxn.segReplayFloorSet && seq <= cxn.segReplayFloor {
		return errSeqOutsideReplayWindow
	}
	if cxn.segSeen == nil {
		cxn.segSeen = make(map[uint64]segmentFingerprint, segCacheMax)
	}
	if len(cxn.segSeenOrder) >= segCacheMax {
		oldest := cxn.segSeenOrder[0]
		cxn.segSeenOrder = cxn.segSeenOrder[1:]
		delete(cxn.segSeen, oldest)
		if !cxn.segReplayFloorSet || oldest > cxn.segReplayFloor {
			cxn.segReplayFloor = oldest
			cxn.segReplayFloorSet = true
		}
	}
	cxn.segSeen[seq] = hash
	cxn.segSeenOrder = append(cxn.segSeenOrder, seq)
	return nil
}

// processSegmentDeduped wraps processSegment so that duplicate or retried pushes
// of the same segment never start a second transcode. Concurrent in-flight
// pushes of the same (SeqNo, request fingerprint) are coalesced into one transcode via
// singleflight; a retry arriving after the original completed returns the cached
// rendition URLs. This is what keeps a same-keyNo re-POST from MistProcLivepeer
// safe instead of poisoning session state with a parallel transcode. Conflicting
// reuse of the same sequence number is rejected.
func (cxn *rtmpConnection) processSegmentDeduped(ctx context.Context, seg *stream.HLSSegment, segPar *core.SegmentParameters) ([]string, error) {
	hash := segmentRequestFingerprint(seg, segPar)
	if err := cxn.claimSeq(seg.SeqNo, hash); err != nil {
		clog.Errorf(ctx, "Rejecting push: invalid sequence reuse seqNo=%d err=%q", seg.SeqNo, err)
		return nil, err
	}
	if urls, ok := cxn.cachedSegURLs(seg.SeqNo, hash); ok {
		clog.Infof(ctx, "Returning cached transcode for duplicate push seqNo=%d", seg.SeqNo)
		return urls, nil
	}
	key := strconv.FormatUint(seg.SeqNo, 10) + ":" + hex.EncodeToString(hash[:])
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
