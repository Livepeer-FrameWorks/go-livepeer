package server

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

var trustedProxyAllowlist = struct {
	sync.RWMutex
	prefixes []netip.Prefix
}{prefixes: []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}}

// SetTrustedProxyPrefixes replaces the peers allowed to supply the overwritten
// X-Forwarded-For value. Direct clients can never choose their source identity.
func SetTrustedProxyPrefixes(prefixes []netip.Prefix) {
	trustedProxyAllowlist.Lock()
	defer trustedProxyAllowlist.Unlock()
	trustedProxyAllowlist.prefixes = append([]netip.Prefix(nil), prefixes...)
}

func trustedProxy(addr netip.Addr) bool {
	trustedProxyAllowlist.RLock()
	defer trustedProxyAllowlist.RUnlock()
	for _, prefix := range trustedProxyAllowlist.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(remoteAddr)
}

func getRemoteAddr(r *http.Request) string {
	direct := remoteHost(r.RemoteAddr)
	peer, err := netip.ParseAddr(direct)
	if err != nil || !trustedProxy(peer.Unmap()) {
		return direct
	}

	forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	// The managed proxy overwrites this header, so a list indicates a broken
	// proxy invariant. Ignore it instead of guessing which hop is trustworthy.
	if forwarded == "" || strings.Contains(forwarded, ",") {
		return direct
	}
	forwarded = remoteHost(forwarded)
	addr, err := netip.ParseAddr(forwarded)
	if err != nil {
		return direct
	}
	return addr.Unmap().String()
}

func ingestRemoteIP(r *http.Request) (string, error) {
	value := getRemoteAddr(r)
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return "", fmt.Errorf("invalid ingest peer address %q: %w", value, err)
	}
	return addr.Unmap().String(), nil
}
