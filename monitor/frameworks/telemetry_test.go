package frameworks

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostFromURL(t *testing.T) {
	tests := map[string]string{
		"https://orch.example:8935/path": "orch.example",
		"orch.example:8935/path":         "orch.example",
		"https://[2001:db8::1]:8935/x":   "2001:db8::1",
		"[2001:db8::1]:8935":             "2001:db8::1",
		"":                               "",
	}
	for input, expected := range tests {
		require.Equal(t, expected, hostFromURL(input), input)
	}
}

func TestDecklogAllowsInsecure(t *testing.T) {
	for _, mode := range []string{"", "disabled", "insecure"} {
		allow, err := decklogAllowsInsecure(mode)
		require.NoError(t, err)
		require.True(t, allow)
	}
	for _, mode := range []string{"tls", "mtls", "MTLS"} {
		allow, err := decklogAllowsInsecure(mode)
		require.NoError(t, err)
		require.False(t, allow)
	}
	_, err := decklogAllowsInsecure("typo")
	require.Error(t, err)
}
