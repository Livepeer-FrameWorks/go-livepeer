package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/stretchr/testify/assert"
)

// healthzFakePool satisfies common.OrchestratorPool with a fixed GetInfos set
// and does NOT expose UsableOrchCount, so the handler falls back to GetInfos().
type healthzFakePool struct {
	infos []common.OrchestratorLocalInfo
}

func (p *healthzFakePool) GetInfos() []common.OrchestratorLocalInfo { return p.infos }
func (p *healthzFakePool) GetOrchestrators(context.Context, int, common.Suspender, common.CapabilityComparator, common.ScorePred) (common.OrchestratorDescriptors, error) {
	return nil, nil
}
func (p *healthzFakePool) Size() int                       { return len(p.infos) }
func (p *healthzFakePool) SizeWith(common.ScorePred) int   { return len(p.infos) }
func (p *healthzFakePool) Broadcaster() common.Broadcaster { return nil }

// healthzCachingPool also exposes a cached UsableOrchCount, which the handler
// prefers when refreshed.
type healthzCachingPool struct {
	healthzFakePool
	usable    int
	refreshed bool
}

func (p *healthzCachingPool) UsableOrchCount() (int, bool) { return p.usable, p.refreshed }

func serveHealthz(t *testing.T, pool common.OrchestratorPool) (int, string) {
	t.Helper()
	s := &LivepeerServer{LivepeerNode: &core.LivepeerNode{}}
	s.LivepeerNode.OrchestratorPool = pool
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	s.healthzHandler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestHealthzHandler_GatedOnUsableOrchestrators(t *testing.T) {
	assert := assert.New(t)

	// Nil pool -> not ready.
	code, body := serveHealthz(t, nil)
	assert.Equal(http.StatusServiceUnavailable, code)
	assert.JSONEq(`{"orchestrators":0}`, body)

	// Pool with no selectable orchestrators -> 503.
	code, body = serveHealthz(t, &healthzFakePool{})
	assert.Equal(http.StatusServiceUnavailable, code)
	assert.JSONEq(`{"orchestrators":0}`, body)

	// Pool with selectable orchestrators (no cached count) -> 200 via GetInfos().
	code, body = serveHealthz(t, &healthzFakePool{infos: []common.OrchestratorLocalInfo{{}, {}}})
	assert.Equal(http.StatusOK, code)
	assert.JSONEq(`{"orchestrators":2}`, body)

	// Cached count present and refreshed -> authoritative.
	code, body = serveHealthz(t, &healthzCachingPool{usable: 3, refreshed: true})
	assert.Equal(http.StatusOK, code)
	assert.JSONEq(`{"orchestrators":3}`, body)

	// Cached count refreshed but zero -> 503 even if GetInfos has stale entries.
	code, _ = serveHealthz(t, &healthzCachingPool{
		healthzFakePool: healthzFakePool{infos: []common.OrchestratorLocalInfo{{}}},
		usable:          0,
		refreshed:       true,
	})
	assert.Equal(http.StatusServiceUnavailable, code)

	// Not yet refreshed -> fall back to live GetInfos() (cold start / hydrated).
	code, body = serveHealthz(t, &healthzCachingPool{
		healthzFakePool: healthzFakePool{infos: []common.OrchestratorLocalInfo{{}}},
		refreshed:       false,
	})
	assert.Equal(http.StatusOK, code)
	assert.JSONEq(`{"orchestrators":1}`, body)
}
