package discovery

import (
	"context"
	"errors"
	"math/big"
	"net/url"
	"sync"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/eth"
	lpTypes "github.com/livepeer/go-livepeer/eth/types"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-livepeer/pm"
	"github.com/livepeer/go-livepeer/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// failOnceEthClient fails the first TranscoderPool() call and then delegates to
// the embedded stub, so we can prove async initial discovery self-heals instead
// of permanently wedging after one early eth error.
type failOnceEthClient struct {
	*eth.StubClient
	mu    sync.Mutex
	calls int
}

func (f *failOnceEthClient) TranscoderPool() ([]*lpTypes.Transcoder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("simulated transient eth failure")
	}
	return f.StubClient.Orchestrators, nil
}

func (f *failOnceEthClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func sampleNetworkCapabilities() []*common.OrchNetworkCapabilities {
	return []*common.OrchNetworkCapabilities{
		{
			Address:      "0xabc",
			LocalAddress: "0xdef",
			OrchURI:      "https://orch-1.test:8935",
			Capabilities: &net.Capabilities{
				Bitstring:   []uint64{1, 2, 3},
				Mandatories: []uint64{4},
				Capacities:  map[uint32]uint32{7: 2},
				Version:     "1.0",
			},
			PriceInfo: &net.PriceInfo{PricePerUnit: 999, PixelsPerUnit: 1},
			CapabilitiesPrices: []*net.PriceInfo{
				{PricePerUnit: 5, PixelsPerUnit: 1, Capability: 7},
			},
			Hardware: []*net.HardwareInformation{
				{Pipeline: "live", ModelId: "m1"},
			},
		},
	}
}

func sampleDBOrchs() []*common.DBOrch {
	return []*common.DBOrch{
		common.NewDBOrch("0xabc", "https://orch-1.test:8935", 999, 0, 1000000, 500),
	}
}

func TestDiscoverySnapshot_JSONRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	snap := newDiscoverySnapshot("1", "arbitrum-one-mainnet", "0xGATEWAY", "eu-west",
		time.Unix(1_700_000_000, 0).UTC(), sampleDBOrchs(), sampleNetworkCapabilities())

	raw, err := marshalDiscoverySnapshot(snap)
	require.NoError(err)

	got, err := parseDiscoverySnapshot(raw)
	require.NoError(err)
	require.NotNil(got)

	assert.Equal(snap.SchemaVersion, got.SchemaVersion)
	assert.Equal(snap.ChainID, got.ChainID)
	assert.Equal(snap.Network, got.Network)
	assert.Equal(snap.BroadcasterAddr, got.BroadcasterAddr)
	assert.True(snap.CapturedAt.Equal(got.CapturedAt))
	require.Len(got.Orchestrators, 1)
	assert.Equal(snap.Orchestrators[0], got.Orchestrators[0])
	require.Len(got.Capabilities, 1)
	// Protobuf-backed caps round-trip through encoding/json.
	assert.Equal(snap.Capabilities[0].OrchURI, got.Capabilities[0].OrchURI)
	assert.Equal(snap.Capabilities[0].Capabilities.Bitstring, got.Capabilities[0].Capabilities.Bitstring)
	assert.Equal(snap.Capabilities[0].Capabilities.Capacities, got.Capabilities[0].Capabilities.Capacities)
	assert.Equal(snap.Capabilities[0].PriceInfo.PricePerUnit, got.Capabilities[0].PriceInfo.PricePerUnit)
	assert.Equal(snap.Capabilities[0].Hardware[0].ModelId, got.Capabilities[0].Hardware[0].ModelId)
}

func TestDiscoverySnapshot_EmptyParsesToNil(t *testing.T) {
	got, err := parseDiscoverySnapshot("")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestValidateDiscoverySnapshot(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	base := func() *common.DiscoverySnapshot {
		return newDiscoverySnapshot("1", "main", "0xAbC", "r", now, sampleDBOrchs(), sampleNetworkCapabilities())
	}

	// Valid (address compared case-insensitively).
	assert.NoError(t, validateDiscoverySnapshot(base(), "1", "main", "0xabc", now))

	// Schema mismatch.
	s := base()
	s.SchemaVersion = common.DiscoverySnapshotSchemaVersion + 1
	assert.Error(t, validateDiscoverySnapshot(s, "1", "main", "0xabc", now))

	// Chain mismatch.
	assert.Error(t, validateDiscoverySnapshot(base(), "2", "main", "0xabc", now))

	// Network mismatch.
	assert.Error(t, validateDiscoverySnapshot(base(), "1", "test", "0xabc", now))

	// Broadcaster mismatch.
	assert.Error(t, validateDiscoverySnapshot(base(), "1", "main", "0xother", now))

	// Expired.
	assert.Error(t, validateDiscoverySnapshot(base(), "1", "main", "0xabc", now.Add(discoverySnapshotMaxAge+time.Hour)))

	// Minor clock skew is accepted; clearly future-dated snapshots are rejected.
	assert.NoError(t, validateDiscoverySnapshot(base(), "1", "main", "0xabc", now.Add(-discoverySnapshotMaxFutureSkew/2)))
	assert.Error(t, validateDiscoverySnapshot(base(), "1", "main", "0xabc", now.Add(-discoverySnapshotMaxFutureSkew-time.Second)))

	// Nil.
	assert.Error(t, validateDiscoverySnapshot(nil, "1", "main", "0xabc", now))
}

func TestHydrateFromSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dbh, dbraw, err := common.TempDB(t)
	require.NoError(err)
	defer dbh.Close()
	defer dbraw.Close()
	require.NoError(dbh.SetChainID(big.NewInt(1)))

	node := &core.LivepeerNode{Database: dbh}

	const broadcaster = "0xGATEWAY"
	snap := newDiscoverySnapshot("1", "main", broadcaster, "r", time.Now(), sampleDBOrchs(), sampleNetworkCapabilities())
	raw, err := marshalDiscoverySnapshot(snap)
	require.NoError(err)
	require.NoError(dbh.UpdateNetworkCapabilitiesSnapshot(raw))

	// Replacement-host case: local DB empty -> caps restored AND rows seeded.
	got := HydrateFromSnapshot(node, "main", broadcaster, nil, time.Now())
	require.NotNil(got)
	assert.True(got.CapturedAt.Equal(snap.CapturedAt))
	assert.Len(node.GetNetworkCapabilities(), 1)
	orchs, err := dbh.SelectOrchs(&common.DBOrchFilter{UpdatedLastDay: true})
	require.NoError(err)
	assert.Len(orchs, 1, "snapshot rows should be seeded when local DB is empty")

	// Mismatched identity is ignored (no panic, returns nil).
	node2 := &core.LivepeerNode{Database: dbh}
	assert.Nil(HydrateFromSnapshot(node2, "main", "0xWRONG", nil, time.Now()))
	assert.Empty(node2.GetNetworkCapabilities())
}

func TestHydrateFromSnapshot_DoesNotOverwriteFresherLocalRows(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dbh, dbraw, err := common.TempDB(t)
	require.NoError(err)
	defer dbh.Close()
	defer dbraw.Close()
	require.NoError(dbh.SetChainID(big.NewInt(1)))

	// Local DB already has a (fresher) selectable row for the same address.
	require.NoError(dbh.UpdateOrch(common.NewDBOrch("0xabc", "https://local-fresh.test:8935", 111, 0, 1000000, 500)))

	node := &core.LivepeerNode{Database: dbh}
	const broadcaster = "0xGATEWAY"
	snap := newDiscoverySnapshot("1", "main", broadcaster, "r", time.Now(), sampleDBOrchs(), sampleNetworkCapabilities())
	raw, err := marshalDiscoverySnapshot(snap)
	require.NoError(err)
	require.NoError(dbh.UpdateNetworkCapabilitiesSnapshot(raw))

	require.NotNil(HydrateFromSnapshot(node, "main", broadcaster, nil, time.Now()))
	// Caps still restored.
	assert.Len(node.GetNetworkCapabilities(), 1)
	// But the local serviceURI is preserved, not overwritten by the snapshot.
	orchs, err := dbh.SelectOrchs(&common.DBOrchFilter{UpdatedLastDay: true})
	require.NoError(err)
	require.Len(orchs, 1)
	assert.Equal("https://local-fresh.test:8935", orchs[0].ServiceURI)
}

func validOrchInfo() func(ctx context.Context, bcast common.Broadcaster, orchestratorServer *url.URL, params server.GetOrchestratorInfoParams) (*net.OrchestratorInfo, error) {
	return func(ctx context.Context, bcast common.Broadcaster, orchestratorServer *url.URL, params server.GetOrchestratorInfoParams) (*net.OrchestratorInfo, error) {
		return &net.OrchestratorInfo{
			Address:      ethcommon.BytesToAddress([]byte(orchestratorServer.String())).Bytes(),
			Transcoder:   orchestratorServer.String(),
			PriceInfo:    &net.PriceInfo{PricePerUnit: 1, PixelsPerUnit: 1},
			TicketParams: &net.TicketParams{Recipient: ethcommon.BytesToAddress([]byte(orchestratorServer.String())).Bytes()},
		}, nil
	}
}

func TestCacheOrchInfos_FallsBackToDBWhenPoolEmpty(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldGet := serverGetOrchInfo
	defer func() { serverGetOrchInfo = oldGet }()
	inner := validOrchInfo()
	var mu sync.Mutex
	polls := 0
	serverGetOrchInfo = func(ctx context.Context, bcast common.Broadcaster, orchestratorServer *url.URL, params server.GetOrchestratorInfoParams) (*net.OrchestratorInfo, error) {
		mu.Lock()
		polls++
		mu.Unlock()
		return inner(ctx, bcast, orchestratorServer, params)
	}

	dbh, dbraw, err := common.TempDB(t)
	require.NoError(err)
	defer dbh.Close()
	defer dbraw.Close()

	// Pre-populate selectable DB rows.
	for _, o := range sampleDBOrchs() {
		require.NoError(dbh.UpdateOrch(o))
	}

	node := &core.LivepeerNode{
		Database: dbh,
		Eth:      &eth.StubClient{TotalStake: big.NewInt(0)},
		Sender:   &pm.MockSender{},
		// A non-nil but EMPTY pool: GetInfos() returns nothing.
		OrchestratorPool: &orchestratorPool{},
	}

	dbo := &DBOrchestratorPoolCache{
		store: dbh,
		rm:    &stubRoundsManager{},
		node:  node,
		bcast: core.NewBroadcaster(node),
	}

	require.NoError(dbo.cacheOrchInfos())

	// The empty pool yields no GetInfos() targets; without the DB fallback the
	// crawl would poll zero orchestrators. It must instead poll the registered
	// DB rows and complete a usable refresh.
	mu.Lock()
	got := polls
	mu.Unlock()
	assert.GreaterOrEqual(got, 1, "DB fallback should poll registered orchestrators despite an empty pool")
	_, refreshed := dbo.UsableOrchCount()
	assert.True(refreshed)
}

func TestCacheOrchInfos_DoesNotClearCapsOnTransientEmptyRefresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dbh, dbraw, err := common.TempDB(t)
	require.NoError(err)
	defer dbh.Close()
	defer dbraw.Close()

	node := &core.LivepeerNode{
		Database: dbh,
		Eth:      &eth.StubClient{TotalStake: big.NewInt(0)},
		Sender:   &pm.MockSender{},
	}
	// Hydrated/last-known caps present.
	require.NoError(node.UpdateNetworkCapabilities(sampleNetworkCapabilities()))

	dbo := &DBOrchestratorPoolCache{
		store: dbh,
		rm:    &stubRoundsManager{},
		node:  node,
		bcast: core.NewBroadcaster(node),
	}

	// DB has no orchestrators -> the refresh yields zero caps. With a recent
	// successful refresh, the guard must preserve the existing caps.
	dbo.mu.Lock()
	dbo.lastSuccessfulRefresh = time.Now()
	dbo.mu.Unlock()
	require.NoError(dbo.cacheOrchInfos())
	assert.Len(node.GetNetworkCapabilities(), 1, "transient empty refresh must not clear caps")

	// Once the last refresh is older than the selection horizon, an empty
	// refresh is allowed to clear caps so we don't serve forever-stale data.
	dbo.mu.Lock()
	dbo.lastSuccessfulRefresh = time.Now().Add(-2 * discoverySnapshotMaxAge)
	dbo.mu.Unlock()
	require.NoError(dbo.cacheOrchInfos())
	assert.Empty(node.GetNetworkCapabilities(), "expired snapshot allows clearing caps")
}

func TestCacheOrchInfos_PersistsSnapshotOnNonEmptyRefresh(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldGet := serverGetOrchInfo
	defer func() { serverGetOrchInfo = oldGet }()
	serverGetOrchInfo = validOrchInfo()

	dbh, dbraw, err := common.TempDB(t)
	require.NoError(err)
	defer dbh.Close()
	defer dbraw.Close()
	require.NoError(dbh.SetChainID(big.NewInt(1)))

	for _, o := range sampleDBOrchs() {
		require.NoError(dbh.UpdateOrch(o))
	}

	node := &core.LivepeerNode{
		Database: dbh,
		Eth:      &eth.StubClient{TotalStake: big.NewInt(0)},
		Sender:   &pm.MockSender{},
	}
	dbo := &DBOrchestratorPoolCache{
		store:   dbh,
		rm:      &stubRoundsManager{},
		node:    node,
		network: "main",
		bcast:   core.NewBroadcaster(node),
	}

	// No snapshot before the refresh.
	raw, err := dbh.SelectNetworkCapabilitiesSnapshot()
	require.NoError(err)
	require.Empty(raw)

	require.NoError(dbo.cacheOrchInfos())

	raw, err = dbh.SelectNetworkCapabilitiesSnapshot()
	require.NoError(err)
	require.NotEmpty(raw, "a non-empty refresh should persist a snapshot")

	snap, err := parseDiscoverySnapshot(raw)
	require.NoError(err)
	require.NotNil(snap)
	assert.Equal(common.DiscoverySnapshotSchemaVersion, snap.SchemaVersion)
	assert.Equal("1", snap.ChainID)
	assert.Equal("main", snap.Network)
	assert.NotEmpty(snap.Capabilities)
	// Snapshot rows are the selection-filtered set (getURLs), so a hydrate
	// restores exactly what serving will select.
	assert.NotEmpty(snap.Orchestrators)
	selectable, err := dbo.selectableOrchs()
	require.NoError(err)
	assert.Len(snap.Orchestrators, len(selectable))
}

// Regression for the hydrate-before-discovery startup path: when New() is built
// with a HydratedAt from the snapshot, the first (empty) crawl must NOT clear
// the caps that hydration restored into the node, because the seeded
// lastSuccessfulRefresh marks them fresh.
func TestNew_SeedsFreshnessFromHydratedSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	dbh, dbraw, err := common.TempDB(t)
	require.NoError(err)
	defer dbh.Close()
	defer dbraw.Close()

	node := &core.LivepeerNode{
		Database: dbh,
		Eth:      &eth.StubClient{TotalStake: big.NewInt(0)}, // no orchestrators -> empty crawl
		Sender:   &pm.MockSender{},
	}
	// Simulate what HydrateFromSnapshot did before the cache is built.
	require.NoError(node.UpdateNetworkCapabilities(sampleNetworkCapabilities()))
	hydratedAt := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Synchronous build (AsyncInitialDiscovery false) so the empty crawl has run
	// by the time New() returns — deterministic, no goroutine timing.
	pool, err := DBOrchestratorPoolCacheConfig{
		Ctx:                     ctx,
		Node:                    node,
		RoundsManager:           &stubRoundsManager{},
		DiscoveryTimeout:        500 * time.Millisecond,
		LiveAICapReportInterval: time.Minute,
		HydratedAt:              hydratedAt,
		HydratedOrchCount:       len(sampleDBOrchs()),
	}.New()
	require.NoError(err)

	// The empty live refresh must have preserved the hydrated caps.
	assert.Len(node.GetNetworkCapabilities(), 1, "empty crawl must not clear hydrated caps")

	// Freshness reflects the snapshot capture time until live discovery replaces it.
	last, count := pool.LastDiscoveryRefresh()
	assert.True(last.Equal(hydratedAt))
	assert.Equal(len(sampleDBOrchs()), count)
}

func TestAsyncInitialDiscoverySelfHeals(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	oldGet := serverGetOrchInfo
	defer func() { serverGetOrchInfo = oldGet }()
	serverGetOrchInfo = validOrchInfo()

	dbh, dbraw, err := common.TempDB(t)
	require.NoError(err)
	defer dbh.Close()
	defer dbraw.Close()

	orchestrators := StubOrchestrators([]string{"https://127.0.0.1:8936"})

	sender := &pm.MockSender{}
	sender.On("ValidateTicketParams", mock.Anything).Return(nil)

	ethc := &failOnceEthClient{StubClient: &eth.StubClient{
		Orchestrators: orchestrators,
		TotalStake:    new(big.Int).Mul(big.NewInt(5000), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)),
	}}
	node := &core.LivepeerNode{Database: dbh, Eth: ethc, Sender: sender}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := DBOrchestratorPoolCacheConfig{
		Ctx:                     ctx,
		Node:                    node,
		RoundsManager:           &stubRoundsManager{},
		DiscoveryTimeout:        500 * time.Millisecond,
		LiveAICapReportInterval: time.Minute,
		AsyncInitialDiscovery:   true,
	}.New()
	require.NoError(err) // must NOT block on the failing initial crawl

	// Before any refresh, readiness is not yet established.
	if _, refreshed := pool.UsableOrchCount(); refreshed {
		t.Fatal("expected not-refreshed before background discovery completes")
	}

	// The first TranscoderPool() fails; the background loop retries with backoff
	// and self-heals — readiness must eventually be established rather than the
	// node staying permanently not-ready.
	require.Eventually(func() bool {
		_, refreshed := pool.UsableOrchCount()
		return refreshed
	}, 10*time.Second, 20*time.Millisecond, "discovery should self-heal after the transient failure")

	count, _ := pool.UsableOrchCount()
	assert.GreaterOrEqual(count, 1)
	assert.GreaterOrEqual(ethc.callCount(), 2, "TranscoderPool should have been retried")
}
