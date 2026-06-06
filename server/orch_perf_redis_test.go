package server

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newTestPerfStore(t *testing.T) *orchHealthStore {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return &orchHealthStore{rdb: rdb, region: "eu"}
}

func TestRecordPerf_EWMAAndMetadata(t *testing.T) {
	store := newTestPerfStore(t)
	ep := "https://orch.example:8935"
	meta := perfMeta{serviceAddr: "0xservice", paymentRecipient: "0xrecipient", resolvedIP: "1.2.3.4"}

	// First observation seeds the EWMA with the raw rtt speed.
	store.recordPerf("vod", "240p", ep, meta, 4.0 /*xcode*/, 2.0 /*rtt*/, true)
	require.InDelta(t, 2.0, store.perfReader("vod", "240p").scores([]string{ep})[ep], 1e-9)

	// A faster sample pulls the rtt EWMA up (alpha 0.3): 0.3*6 + 0.7*2 = 3.2.
	store.recordPerf("vod", "240p", ep, meta, 8.0, 6.0, true)
	require.InDelta(t, 3.2, store.perfReader("vod", "240p").scores([]string{ep})[ep], 1e-9)

	// A failure is a zero-speed sample that drags the EWMA down: 0.3*0 + 0.7*3.2 = 2.24.
	store.recordPerf("vod", "240p", ep, meta, 1.0, 1.0, false)
	require.InDelta(t, 2.24, store.perfReader("vod", "240p").scores([]string{ep})[ep], 1e-9)

	// Metadata + counters recorded as attribution, not identity.
	got, err := store.rdb.HGetAll(context.Background(), perfKey("eu", "vod", "240p", ep)).Result()
	require.NoError(t, err)
	require.Equal(t, "0xservice", got["service_addr"])
	require.Equal(t, "0xrecipient", got["payment_recipient"])
	require.Equal(t, "1.2.3.4", got["resolved_ip"])
	require.Equal(t, "1", got["fails"])
	require.Equal(t, "3", got["samples"])
}

func TestPerfReader_FetchesNewEndpointWithinMemoWindow(t *testing.T) {
	store := newTestPerfStore(t)
	a := "https://a.example:8935"
	b := "https://b.example:8935"
	store.recordPerf("vod", "240p", a, perfMeta{}, 5.0, 5.0, true)
	store.recordPerf("vod", "240p", b, perfMeta{}, 9.0, 9.0, true)

	reader := store.perfReader("vod", "240p")
	// First call caches only A.
	require.InDelta(t, 5.0, reader.scores([]string{a})[a], 1e-9)
	// Second call within the same memo window introduces B — it must be fetched,
	// not treated as unknown just because it wasn't in the initial cache.
	second := reader.scores([]string{a, b})
	require.InDelta(t, 5.0, second[a], 1e-9)
	require.InDelta(t, 9.0, second[b], 1e-9)
}

func TestPerfReader_UnknownEndpointOmitted(t *testing.T) {
	store := newTestPerfStore(t)
	out := store.perfReader("vod", "240p").scores([]string{"https://never-seen.example:8935"})
	_, ok := out["https://never-seen.example:8935"]
	require.False(t, ok, "an endpoint with no history must be omitted")
}
