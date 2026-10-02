package ethrpc

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const secret = "s3cr3t-api-key"

func TestRedact(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("https://rpc.example.com/redacted", Redact("https://rpc.example.com/v2/"+secret))
	assert.Equal("https://rpc.example.com/redacted", Redact("https://rpc.example.com/?key="+secret))
	assert.Equal("https://rpc.example.com/redacted", Redact("https://user:"+secret+"@rpc.example.com"))
	assert.Equal("https://rpc.example.com", Redact("https://rpc.example.com"))
	assert.Equal("<redacted>", Redact("::not a url "+secret))
}

func TestDial_ErrorsDoNotContainSecret(t *testing.T) {
	// A closed listener produces a transport-level error, which http.Client
	// wraps in a *url.Error that embeds the request URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	rawURL := "http://" + addr + "/v2/" + secret + "?apikey=" + secret
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Control: go-ethereum's own dialer embeds the full URL in the error.
	plain, err := ethclient.DialContext(ctx, rawURL)
	require.NoError(t, err)
	_, err = plain.SuggestGasPrice(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), secret)

	c, err := DialEthClient(ctx, rawURL)
	require.NoError(t, err)
	_, err = c.SuggestGasPrice(ctx)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
	assert.Contains(t, err.Error(), addr)
}

func TestDial_RequestsReachRealURL(t *testing.T) {
	var gotPath, gotQuery, gotUser, gotPass string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotUser, gotPass, _ = r.BasicAuth()
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x2a"}`))
	}))
	defer ts.Close()

	rawURL := strings.Replace(ts.URL, "http://", "http://alice:"+secret+"@", 1) + "/v2/" + secret + "?k=" + secret
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := DialEthClient(ctx, rawURL)
	require.NoError(t, err)
	price, err := c.SuggestGasPrice(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 42, price.Int64())
	assert.Equal(t, "/v2/"+secret, gotPath)
	assert.Equal(t, "k="+secret, gotQuery)
	assert.Equal(t, "alice", gotUser)
	assert.Equal(t, secret, gotPass)
}
