package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectInternalDial(t *testing.T) {
	SetAllowedInternalOSPrefixes(nil)
	t.Cleanup(func() { SetAllowedInternalOSPrefixes(nil) })

	for _, tc := range []struct {
		address string
		blocked bool
	}{
		{address: "127.0.0.1:9000", blocked: true},
		{address: "[::1]:9000", blocked: true},
		{address: "10.0.0.1:9000", blocked: true},
		{address: "172.16.0.1:9000", blocked: true},
		{address: "192.168.0.1:9000", blocked: true},
		{address: "169.254.169.254:80", blocked: true},
		{address: "[fe80::1]:9000", blocked: true},
		{address: "100.64.0.1:9000", blocked: true},
		{address: "198.18.0.1:9000", blocked: true},
		{address: "224.0.0.1:9000", blocked: true},
		{address: "[64:ff9b::127.0.0.1]:9000", blocked: true},
		{address: "[64:ff9b:1::127.0.0.1]:9000", blocked: true},
		{address: "8.8.8.8:443", blocked: false},
		{address: "[2606:4700:4700::1111]:443", blocked: false},
	} {
		err := rejectInternalDial(context.Background(), "tcp", tc.address, nil)
		if tc.blocked {
			require.ErrorIs(t, err, errInternalAddress, tc.address)
		} else {
			require.NoError(t, err, tc.address)
		}
	}

	SetAllowedInternalOSPrefixes([]netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")})
	require.NoError(t, rejectInternalDial(context.Background(), "tcp", "10.20.1.2:9000", nil))
	require.ErrorIs(t, rejectInternalDial(context.Background(), "tcp", "10.21.1.2:9000", nil), errInternalAddress)
}

func TestInternalBlockedHTTPClientRejectsRedirectToPrivate(t *testing.T) {
	var protectedHits atomic.Int32
	protected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		protectedHits.Add(1)
		_, _ = w.Write([]byte("protected"))
	}))
	defer protected.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, protected.URL, http.StatusFound)
	})
	baseURL := newNonLoopbackHTTPServer(t, mux)
	parsed, err := url.Parse(baseURL)
	require.NoError(t, err)
	addr, err := netip.ParseAddr(parsed.Hostname())
	require.NoError(t, err)
	SetAllowedInternalOSPrefixes([]netip.Prefix{netip.PrefixFrom(addr, addr.BitLen())})
	t.Cleanup(func() {
		SetAllowedInternalOSPrefixes(nil)
		InternalBlockedHTTPClient().CloseIdleConnections()
	})

	_, err = InternalBlockedHTTPClient().Get(baseURL + "/redirect")
	require.ErrorIs(t, err, errInternalAddress)
	require.Zero(t, protectedHits.Load())
}
