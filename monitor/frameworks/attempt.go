package frameworks

import (
	"context"
	"net"
	"sync"
)

type attemptKey struct{}
type Attempt struct {
	mu       sync.RWMutex
	IP       string
	TargetIP string
}

// WithAttempt isolates socket attribution from concurrent requests sharing a hostname.
func WithAttempt(ctx context.Context, targetIP string) context.Context {
	return context.WithValue(ctx, attemptKey{}, &Attempt{TargetIP: targetIP})
}
func SetRemoteIP(ctx context.Context, ip string) {
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil {
		return
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return
	}
	a.mu.Lock()
	a.IP = parsed.String()
	a.mu.Unlock()
}
func RemoteIP(ctx context.Context) string {
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.IP
}
func TargetIP(ctx context.Context) string {
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil {
		return ""
	}
	return a.TargetIP
}
