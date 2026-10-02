// Package ethrpc dials Ethereum JSON-RPC endpoints without exposing the
// endpoint's credentials in errors.
//
// Hosted RPC providers put the API key in the URL path or query (for example
// https://host/v2/<key>). go-ethereum's HTTP transport surfaces failures as
// *url.Error values that embed the full request URL, and those errors reach
// every log line that prints an RPC error. Dial hands go-ethereum a redacted
// URL and swaps the real one in at the transport, so error strings only ever
// carry the redacted form.
package ethrpc

import (
	"context"
	"net/http"
	"net/url"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

const redactedPath = "/redacted"

// Redact returns rawURL with user info, path, query and fragment removed. A
// URL that does not parse is replaced entirely.
func Redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "<redacted>"
	}
	r := url.URL{Scheme: u.Scheme, Host: u.Host}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		r.Path = redactedPath
	}
	return r.String()
}

// Dial returns a JSON-RPC client for rawURL. HTTP(S) endpoints are dialed with
// a redacted URL and a transport that sends each request to the real URL.
// Other schemes (ws, wss, IPC) are dialed unchanged.
func Dial(ctx context.Context, rawURL string) (*rpc.Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return rpc.DialContext(ctx, rawURL)
	}
	client := &http.Client{Transport: &secretURLTransport{target: u, base: http.DefaultTransport}}
	return rpc.DialOptions(ctx, Redact(rawURL), rpc.WithHTTPClient(client))
}

// DialEthClient is Dial wrapped in an ethclient.Client.
func DialEthClient(ctx context.Context, rawURL string) (*ethclient.Client, error) {
	c, err := Dial(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(c), nil
}

// secretURLTransport rewrites every outgoing request to the configured target
// URL. http.Client wraps transport failures in a *url.Error carrying the URL it
// was given, which is the redacted one.
type secretURLTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *secretURLTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	target := *t.target
	out.URL = &target
	out.Host = ""
	if target.User != nil {
		if _, set := out.Header["Authorization"]; !set {
			password, _ := target.User.Password()
			out.SetBasicAuth(target.User.Username(), password)
		}
		out.URL.User = nil
	}
	return t.base.RoundTrip(out)
}
