package server

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/monitor/frameworks"
	"github.com/livepeer/lpms/stream"
	"github.com/redis/go-redis/v9"
)

// Durable performance tuning. The EWMA is intentionally sticky (long TTL) — unlike
// suspension, a proven-fast orchestrator instance should stay proven across
// restarts and idle periods. The in-process memo keeps the hot selection path
// off Redis.
var (
	orchPerfEWMAAlpha = 0.3
	orchPerfTTL       = 48 * time.Hour
	orchPerfMemoTTL   = 10 * time.Second
)

// Performance and suspension are keyed by the concrete instance ENDPOINT we
// POSTed to (the resolved Transcoder URL), NOT by a wallet address. A single
// on-chain service can run many instances behind throwaway signer wallets that
// all redeem to one payment recipient; keying perf by either wallet would smear
// distinct instances into one score. The service address and payment recipient
// are kept only as metadata for attribution.

// orchPerfReader returns the durable per-instance performance score (the
// end-to-end round-trip-speed EWMA, higher = faster) for the given candidate
// endpoints. A missing entry is omitted so the caller can apply an exploration
// default.
type orchPerfReader interface {
	scores(endpoints []string) map[string]float64
}

// orchPerfEWMAScript atomically folds one observation into the endpoint's EWMAs.
// First observation seeds the EWMA with the raw value; thereafter it is
// alpha*sample + (1-alpha)*prev. xcode_ewma is transcode-only throughput
// (observability); rtt_ewma is the end-to-end speed selection ranks on. The
// service_addr / payment_recipient / resolved_ip metadata attribute the endpoint
// without being part of its identity. samples/fails/last_at are observability.
var orchPerfEWMAScript = redis.NewScript(`
local key = KEYS[1]
local alpha = tonumber(ARGV[1])
local xc = tonumber(ARGV[2])
local rtt = tonumber(ARGV[3])
local ok = tonumber(ARGV[4])
local ttl = tonumber(ARGV[5])
local now = tonumber(ARGV[6])
local curXc = tonumber(redis.call('HGET', key, 'xcode_ewma'))
if curXc == nil then
  redis.call('HSET', key, 'xcode_ewma', xc, 'rtt_ewma', rtt)
else
  local curRtt = tonumber(redis.call('HGET', key, 'rtt_ewma')) or rtt
  redis.call('HSET', key, 'xcode_ewma', alpha*xc + (1-alpha)*curXc, 'rtt_ewma', alpha*rtt + (1-alpha)*curRtt)
end
redis.call('HSET', key, 'service_addr', ARGV[7], 'payment_recipient', ARGV[8], 'resolved_ip', ARGV[9])
redis.call('HINCRBY', key, 'samples', 1)
if ok == 0 then redis.call('HINCRBY', key, 'fails', 1) end
redis.call('HSET', key, 'last_at', now)
redis.call('EXPIRE', key, ttl)
return 1
`)

func perfKey(region, workload, capKey, endpoint string) string {
	return fmt.Sprintf("orchperf:%s:%s:%s:%s", region, workload, capKey, endpoint)
}

// perfMeta carries the attribution identities recorded alongside an endpoint's
// performance: the on-chain service address and payment recipient (both wallet
// identities) and the resolved IP. None of these are the endpoint's identity.
type perfMeta struct {
	serviceAddr      string
	paymentRecipient string
	resolvedIP       string
}

// recordPerf folds one segment's observed speed factors into the durable EWMA
// for (region, workload, capKey, endpoint). A failed segment is recorded as a
// zero-speed sample so a flaky-but-unsuspended instance sinks in ranking;
// suspension still hard-removes separately. No-op when Redis is unconfigured.
func (s *orchHealthStore) recordPerf(workload, capKey, endpoint string, meta perfMeta, xcodeSpeed, rttSpeed float64, ok bool) {
	if s == nil || s.rdb == nil || endpoint == "" {
		return
	}
	if workload == "" {
		workload = core.WorkloadLive
	}
	okv := 1
	if !ok {
		okv = 0
		xcodeSpeed = 0
		rttSpeed = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchHealthOpTimeout)
	defer cancel()
	key := perfKey(s.region, workload, capKey, endpoint)
	if err := orchPerfEWMAScript.Run(ctx, s.rdb, []string{key},
		orchPerfEWMAAlpha, xcodeSpeed, rttSpeed, okv, int(orchPerfTTL.Seconds()), time.Now().Unix(),
		meta.serviceAddr, meta.paymentRecipient, meta.resolvedIP).Err(); err != nil {
		glog.Errorf("orch perf: record failed endpoint=%s: %v", endpoint, err)
	}
}

// perfReader returns a scoped reader for selection, or nil when Redis is
// unconfigured (selection then keeps its existing stake/price/rand behavior).
func (s *orchHealthStore) perfReader(workload, capKey string) orchPerfReader {
	if s == nil || s.rdb == nil {
		return nil
	}
	if workload == "" {
		workload = core.WorkloadLive
	}
	return &orchPerf{store: s, workload: workload, capKey: capKey}
}

// orchPerf is a workload+capability-scoped performance reader with a short
// in-process memo so repeated selections for the same stream don't hit Redis on
// every segment.
type orchPerf struct {
	store    *orchHealthStore
	workload string
	capKey   string

	mu sync.Mutex
	// cache holds observed scores; fetched records every endpoint queried this
	// window (hit or miss) so a known-miss isn't re-queried each segment. Both are
	// reset together when the window expires.
	cache   map[string]float64
	fetched map[string]bool
	cacheAt time.Time
}

func (p *orchPerf) key(endpoint string) string {
	return perfKey(p.store.region, p.workload, p.capKey, endpoint)
}

func (p *orchPerf) scores(endpoints []string) map[string]float64 {
	if p == nil || p.store == nil || p.store.rdb == nil || len(endpoints) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// Expire the whole window, then fetch only endpoints not already queried this
	// window. This way a pool refresh that introduces a new endpoint mid-window
	// still gets its durable score instead of being treated as unknown.
	if p.fetched == nil || time.Since(p.cacheAt) >= orchPerfMemoTTL {
		p.cache = map[string]float64{}
		p.fetched = map[string]bool{}
		p.cacheAt = time.Now()
	}
	var missing []string
	for _, ep := range endpoints {
		if !p.fetched[ep] {
			missing = append(missing, ep)
		}
	}
	if len(missing) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), orchHealthOpTimeout)
		defer cancel()
		pipe := p.store.rdb.Pipeline()
		cmds := make(map[string]*redis.StringCmd, len(missing))
		for _, ep := range missing {
			// Rank on end-to-end round-trip speed — the wall time the client waits,
			// which is the failure mode the contract addresses. Transcode-only speed
			// is observability only.
			cmds[ep] = pipe.HGet(ctx, p.key(ep), "rtt_ewma")
		}
		// redis.Nil is expected for instances with no history yet; only log real errors.
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			glog.Errorf("orch perf: scores read failed: %v", err)
		}
		for ep, cmd := range cmds {
			p.fetched[ep] = true
			if v, err := cmd.Float64(); err == nil {
				p.cache[ep] = v
			}
		}
	}
	return subsetScores(p.cache, endpoints)
}

// recordOrchPerf folds one completed segment into the durable per-instance
// performance EWMA, asynchronously so a slow Redis write never delays the
// segment response. It is a throughput signal, so it only records once the
// instance was actually reached (upload completed); pre-upload failures (no
// round-trip) are skipped. A post-upload failure is recorded as ok=false.
// Suspension still hard-removes failing instances independently.
func recordOrchPerf(sess *BroadcastSession, seg *stream.HLSSegment, uploadDur, transcodeDur time.Duration, result *ReceivedTranscodeResult, retErr error) {
	if sess == nil || sess.OrchestratorInfo == nil || sess.Params == nil || uploadDur <= 0 {
		return
	}
	endpoint := sess.OrchestratorInfo.GetTranscoder()
	if endpoint == "" {
		return
	}
	store := sharedOrchHealthStore()
	if store == nil || store.rdb == nil {
		return
	}
	segMs := seg.Duration * 1000.0
	if segMs <= 0 {
		return
	}
	xcodeSpeed := segMs / math.Max(float64(transcodeDur.Milliseconds()), 1)
	rttSpeed := segMs / math.Max(float64((uploadDur+transcodeDur).Milliseconds()), 1)
	ok := retErr == nil && result != nil

	meta := perfMeta{
		serviceAddr: ethcommon.BytesToAddress(sess.OrchestratorInfo.Address).Hex(),
		resolvedIP:  frameworks.ResolvedIPForURL(endpoint),
	}
	if tp := sess.OrchestratorInfo.GetTicketParams(); tp != nil {
		meta.paymentRecipient = ethcommon.BytesToAddress(tp.Recipient).Hex()
	}
	workload := sess.Params.Workload
	capKey := common.ProfilesNames(sess.Params.Profiles)
	go store.recordPerf(workload, capKey, endpoint, meta, xcodeSpeed, rttSpeed, ok)
}

func subsetScores(src map[string]float64, keys []string) map[string]float64 {
	out := make(map[string]float64, len(keys))
	for _, k := range keys {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	return out
}
