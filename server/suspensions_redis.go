package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/core"
	"github.com/redis/go-redis/v9"
)

// orchSuspender is the suspension behavior a SessionPool needs from its backend.
// Both the in-memory *suspender and the durable *redisSuspender implement it.
type orchSuspender interface {
	Suspended(orch string) int
	suspend(orch string, penalty int)
	signalRefresh()
}

// Durable orchestrator health tuning. live excludes a bad orchestrator
// aggressively (long TTL); vod expires sooner so it can re-probe after a
// transient slowdown, while still not forgetting across a gateway restart
// because the suspension lives in Redis.
var (
	orchHealthLiveTTL   = 10 * time.Minute
	orchHealthVODTTL    = 2 * time.Minute
	orchHealthOpTimeout = 1 * time.Second
)

// orchHealthStore is the process-wide durable orchestrator health backend. When
// Redis is configured it shares suspension state across the regional gateway
// pool and survives gateway restarts; otherwise scoped() hands back in-memory
// suspenders identical to today's behavior, so single-gateway and self-hosted
// deployments need no Redis.
type orchHealthStore struct {
	rdb    *redis.Client
	region string
}

var (
	globalOrchHealth     *orchHealthStore
	globalOrchHealthOnce sync.Once
)

// sharedOrchHealthStore returns the process-wide health store, initializing it
// from the environment on first use.
func sharedOrchHealthStore() *orchHealthStore {
	globalOrchHealthOnce.Do(func() {
		globalOrchHealth = newOrchHealthStoreFromEnv()
	})
	return globalOrchHealth
}

// newOrchHealthStoreFromEnv builds the store from FrameWorks env conventions.
// Sentinel mode (FRAMEWORKS_ORCH_HEALTH_REDIS_SENTINEL_ADDRS set) takes
// precedence and survives a Redis master failover; otherwise the single-node
// FRAMEWORKS_ORCH_HEALTH_REDIS_URL is used. Any misconfiguration or connectivity
// failure falls back to in-memory so the gateway never fails to start because
// Redis is unavailable.
func newOrchHealthStoreFromEnv() *orchHealthStore {
	region := strings.TrimSpace(os.Getenv("FRAMEWORKS_GATEWAY_REGION"))

	rdb, err := newOrchHealthRedisFromEnv()
	if err != nil {
		glog.Errorf("orch health: %v — using in-memory suspension (not durable across restart)", err)
		return &orchHealthStore{region: region}
	}
	if rdb == nil {
		glog.Infof("orch health: no Redis configured — using in-memory suspension (not durable across restart)")
		return &orchHealthStore{region: region}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		glog.Errorf("orch health: Redis ping failed, falling back to in-memory: %v", err)
		_ = rdb.Close()
		return &orchHealthStore{region: region}
	}
	glog.Infof("orch health: durable Redis store enabled region=%s", region)
	return &orchHealthStore{rdb: rdb, region: region}
}

// newOrchHealthRedisFromEnv constructs the Redis client: a Sentinel failover
// client when sentinel addresses are configured, else a single-node client from
// the URL. Returns (nil, nil) when no Redis is configured at all.
func newOrchHealthRedisFromEnv() (*redis.Client, error) {
	if sentinels := strings.TrimSpace(os.Getenv("FRAMEWORKS_ORCH_HEALTH_REDIS_SENTINEL_ADDRS")); sentinels != "" {
		master := strings.TrimSpace(os.Getenv("FRAMEWORKS_ORCH_HEALTH_REDIS_MASTER_NAME"))
		if master == "" {
			return nil, fmt.Errorf("FRAMEWORKS_ORCH_HEALTH_REDIS_MASTER_NAME required with sentinel addrs")
		}
		addrs := strings.Split(sentinels, ",")
		for i := range addrs {
			addrs[i] = strings.TrimSpace(addrs[i])
		}
		password := strings.TrimSpace(os.Getenv("FRAMEWORKS_ORCH_HEALTH_REDIS_PASSWORD"))
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       master,
			SentinelAddrs:    addrs,
			Password:         password,
			SentinelPassword: password,
		}), nil
	}
	url := strings.TrimSpace(os.Getenv("FRAMEWORKS_ORCH_HEALTH_REDIS_URL"))
	if url == "" {
		return nil, nil
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid FRAMEWORKS_ORCH_HEALTH_REDIS_URL: %w", err)
	}
	return redis.NewClient(opt), nil
}

// scoped returns a suspender bound to a workload and capability key. The
// in-memory fallback ignores the scope, matching today's per-stream behavior.
func (s *orchHealthStore) scoped(workload, capKey string) orchSuspender {
	if s == nil || s.rdb == nil {
		return newSuspender()
	}
	if workload == "" {
		workload = core.WorkloadLive
	}
	return &redisSuspender{store: s, workload: workload, capKey: capKey}
}

// suspensionTTL is how long an orchestrator stays excluded for a workload.
func suspensionTTL(workload string) time.Duration {
	if workload == core.WorkloadVOD {
		return orchHealthVODTTL
	}
	return orchHealthLiveTTL
}

// redisSuspender is a durable suspender scoped to a region + workload +
// capability set, so the same orchestrator can be healthy for one workload and
// suspended for another. Penalty accumulates (INCR) and the key carries a
// workload TTL, giving escalating ordering plus automatic re-probe.
type redisSuspender struct {
	store    *orchHealthStore
	workload string
	capKey   string
}

func (r *redisSuspender) key(orch string) string {
	return fmt.Sprintf("orchhealth:%s:%s:%s:%s", r.store.region, r.workload, r.capKey, orch)
}

func (r *redisSuspender) Suspended(orch string) int {
	ctx, cancel := context.WithTimeout(context.Background(), orchHealthOpTimeout)
	defer cancel()
	n, err := r.store.rdb.Get(ctx, r.key(orch)).Int()
	if err == redis.Nil {
		return 0
	}
	if err != nil {
		glog.Errorf("orch health: Suspended read failed orch=%s: %v", orch, err)
		return 0
	}
	return n
}

func (r *redisSuspender) suspend(orch string, penalty int) {
	if penalty <= 0 {
		penalty = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), orchHealthOpTimeout)
	defer cancel()
	key := r.key(orch)
	pipe := r.store.rdb.TxPipeline()
	pipe.IncrBy(ctx, key, int64(penalty))
	pipe.Expire(ctx, key, suspensionTTL(r.workload))
	if _, err := pipe.Exec(ctx); err != nil {
		glog.Errorf("orch health: suspend failed orch=%s: %v", orch, err)
	}
}

// signalRefresh is a no-op for the durable store: suspensions expire by
// wall-clock TTL rather than the in-memory refresh-count model.
func (r *redisSuspender) signalRefresh() {}
