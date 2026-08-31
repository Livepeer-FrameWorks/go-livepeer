package core

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"syscall"

	"github.com/livepeer/go-livepeer/common"
)

var errInternalAddress = errors.New("connections to internal addresses are blocked")

var specialUsePrefixes = mustPrefixes(
	"0.0.0.0/8",
	"100.64.0.0/10",
	"192.0.0.0/24",
	"198.18.0.0/15",
	"240.0.0.0/4",
	"64:ff9b::/96",
)

var internalOSAllowlist struct {
	sync.RWMutex
	prefixes []netip.Prefix
}

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

// SetAllowedInternalOSPrefixes replaces the operator-controlled destination
// allowlist used by InternalBlockedHTTPClient.
func SetAllowedInternalOSPrefixes(prefixes []netip.Prefix) {
	internalOSAllowlist.Lock()
	defer internalOSAllowlist.Unlock()
	internalOSAllowlist.prefixes = append([]netip.Prefix(nil), prefixes...)
}

func internalOSAddressAllowed(addr netip.Addr) bool {
	internalOSAllowlist.RLock()
	defer internalOSAllowlist.RUnlock()
	for _, prefix := range internalOSAllowlist.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func rejectInternalDial(_ context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid resolved object-store address %q: %w", address, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("invalid resolved object-store host %q: %w", host, err)
	}
	addr = addr.Unmap()
	if internalOSAddressAllowed(addr) {
		return nil
	}
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return fmt.Errorf("%w: %s", errInternalAddress, address)
	}
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(addr) {
			return fmt.Errorf("%w: %s", errInternalAddress, address)
		}
	}
	return nil
}

var internalBlockedHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:           nil,
		DialContext:     (&net.Dialer{ControlContext: rejectInternalDial}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	},
	Timeout: common.HTTPTimeout / 2,
}

// InternalBlockedHTTPClient returns the shared client that rejects internal
// destinations after DNS resolution and on every redirect hop.
func InternalBlockedHTTPClient() *http.Client {
	return internalBlockedHTTPClient
}
