package frameworks

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"
)

// dnsCache resolves orchestrator hostnames to all A-record IPs and remembers
// which one was actually dialed. Per-cycle DNS for the same host is cached
// for ttl so we don't hammer the resolver during a discovery pass; the
// dialedIP record is updated on each dial so the per-IP `dialed` flag on
// emitted events reflects the most recent attempt.
//
// Multi-IP / round-robin / geo-anycast resolutions surface as multiple rows
// in `orchestrator_discovery_observed`: one with `dialed=true` carrying real
// latency + reachability, the others with `dialed=false` as observation
// context (geo-only). This is the behaviour the federation map's per-region
// table reads.
type dnsCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	resolved map[string]dnsEntry
	dialed   map[string]string // hostname → IP last dialed
}

type dnsEntry struct {
	ips []string
	exp time.Time
}

func newDNSCache(ttl time.Duration) *dnsCache {
	return &dnsCache{
		ttl:      ttl,
		resolved: map[string]dnsEntry{},
		dialed:   map[string]string{},
	}
}

func (c *dnsCache) lookup(host string) []string {
	if host == "" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		// Already an IP literal; no DNS needed.
		return []string{ip.String()}
	}
	c.mu.Lock()
	if entry, ok := c.resolved[host]; ok && time.Now().Before(entry.exp) {
		ips := append([]string(nil), entry.ips...)
		c.mu.Unlock()
		return ips
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		c.mu.Lock()
		c.resolved[host] = dnsEntry{ips: nil, exp: time.Now().Add(c.ttl)}
		c.mu.Unlock()
		return nil
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP.String())
	}
	sort.Strings(ips) // deterministic ordering across cycles
	c.mu.Lock()
	c.resolved[host] = dnsEntry{ips: ips, exp: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return ips
}

// dialedIP returns the IP last recorded as the dialed one for a hostname.
func (c *dnsCache) dialedIP(host string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ip, ok := c.dialed[host]; ok {
		return ip
	}
	return ""
}

func (c *dnsCache) dialedOrSingleIP(host string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ip, ok := c.dialed[host]; ok {
		return ip
	}
	if entry, ok := c.resolved[host]; ok && len(entry.ips) == 1 {
		return entry.ips[0]
	}
	return ""
}

// SetDialedIP records the IP the gateway actually dialed for this hostname,
// so subsequent telemetry for the same host knows which row should carry
// dialed=true.
func (c *dnsCache) SetDialedIP(host, ip string) {
	if host == "" || ip == "" {
		return
	}
	c.mu.Lock()
	c.dialed[host] = ip
	c.mu.Unlock()
}
