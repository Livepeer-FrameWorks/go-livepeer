package frameworks

import (
	"context"
	"sync"
	"time"
)

type segmentKey struct{}
type SegmentObservation struct {
	mu               sync.Mutex
	Address, URI, IP string
	Attempts         uint32
	Setup            time.Duration
	URIs             map[string]bool
}

func WithSegment(ctx context.Context) context.Context {
	return context.WithValue(ctx, segmentKey{}, &SegmentObservation{URIs: map[string]bool{}})
}
func BeginSegmentAttempt(ctx context.Context) {
	if s, _ := ctx.Value(segmentKey{}).(*SegmentObservation); s != nil {
		s.mu.Lock()
		s.Address, s.URI, s.IP = "", "", ""
		s.mu.Unlock()
	}
}
func SegmentSubmitted(ctx context.Context, uri string) {
	if s, _ := ctx.Value(segmentKey{}).(*SegmentObservation); s != nil {
		s.mu.Lock()
		s.Attempts++
		s.URIs[uri] = true
		s.mu.Unlock()
	}
}

// SelectedSegmentResult records the accepted response independently of cancelled hedges.
func SelectedSegmentResult(ctx context.Context, address, uri, ip string) {
	if s, _ := ctx.Value(segmentKey{}).(*SegmentObservation); s != nil {
		s.mu.Lock()
		s.Address, s.URI, s.IP = address, uri, ip
		s.mu.Unlock()
	}
}
func SegmentSelectionTime(ctx context.Context, elapsed time.Duration) {
	if s, _ := ctx.Value(segmentKey{}).(*SegmentObservation); s != nil {
		s.mu.Lock()
		if s.Attempts > 0 {
			s.Setup += elapsed
		}
		s.mu.Unlock()
	}
}
func SegmentSnapshot(ctx context.Context) (address, uri, ip string, attempts, fallbacks uint32, setup time.Duration) {
	if s, _ := ctx.Value(segmentKey{}).(*SegmentObservation); s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.URIs) > 1 {
			fallbacks = uint32(len(s.URIs) - 1)
		}
		return s.Address, s.URI, s.IP, s.Attempts, fallbacks, s.Setup
	}
	return
}
