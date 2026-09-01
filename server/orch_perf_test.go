package server

import (
	"context"
	"math/big"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/go-livepeer/pm"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type fakeEndpointPerf map[string]float64

func (f fakeEndpointPerf) scores(endpoints []string) map[string]float64 {
	out := map[string]float64{}
	for _, e := range endpoints {
		if v, ok := f[e]; ok {
			out[e] = v
		}
	}
	return out
}

// TestSelectUnknownSession_PrefersFasterInstanceSameRecipient verifies that perf
// is attributed per instance endpoint, not per wallet: two instances that redeem
// to the SAME payment recipient but expose different endpoints must be scored
// separately, and selection must route to the faster endpoint.
func TestSelectUnknownSession_PrefersFasterInstanceSameRecipient(t *testing.T) {
	recipient := pm.RandAddress()
	mk := func(url string) *BroadcastSession {
		s := StubBroadcastSession(url)
		s.OrchestratorInfo.TicketParams.Recipient = recipient.Bytes()
		return s
	}
	slow := mk("https://slow.example:8935")
	fast := mk("https://fast.example:8935")

	sel := &Selector{
		stakeRdr:           newStubStakeReader(),
		selectionAlgorithm: stubSelectionAlgorithm{},
		perfReader:         fakeEndpointPerf{"https://fast.example:8935": 5.0, "https://slow.example:8935": 1.0},
	}
	// slow first: without the per-endpoint tiebreak the old code returned the
	// first session matching the (shared) recipient address.
	sel.sessions = []*BroadcastSession{slow, fast}

	got := sel.selectUnknownSession(context.Background())
	require.NotNil(t, got)
	require.Equal(t, "https://fast.example:8935", got.Transcoder(),
		"should route to the faster instance despite a shared redeem address")
}

// spySelectionAlgorithm captures the candidate addresses the selector hands to
// the probability algorithm, so a test can assert the grouping identity directly
// rather than inferring it from the routed session.
type spySelectionAlgorithm struct{ gotAddrs []ethcommon.Address }

func (sa *spySelectionAlgorithm) Select(ctx context.Context, addrs []ethcommon.Address, stakes map[ethcommon.Address]int64, maxPrice *big.Rat, prices map[ethcommon.Address]*big.Rat, perfScores map[ethcommon.Address]float64) ethcommon.Address {
	sa.gotAddrs = addrs
	if len(addrs) == 0 {
		return ethcommon.Address{}
	}
	return addrs[0]
}

// TestSelectUnknownSession_GroupsByServiceAddress verifies the candidate-level
// identity model: two instances under the SAME on-chain service address but with
// DIFFERENT payment recipients are presented to the probability algorithm as ONE
// candidate (the service address, not the two recipients), and selection still
// routes to the faster instance endpoint.
func TestSelectUnknownSession_GroupsByServiceAddress(t *testing.T) {
	service := pm.RandAddress()
	mk := func(url string) *BroadcastSession {
		s := StubBroadcastSession(url)
		s.OrchestratorInfo.Address = service.Bytes()
		// distinct recipients — must NOT split the service into two candidates
		s.OrchestratorInfo.TicketParams.Recipient = pm.RandAddress().Bytes()
		return s
	}
	slow := mk("https://slow.example:8935")
	fast := mk("https://fast.example:8935")

	spy := &spySelectionAlgorithm{}
	sel := &Selector{
		stakeRdr:           newStubStakeReader(),
		selectionAlgorithm: spy,
		perfReader:         fakeEndpointPerf{"https://fast.example:8935": 5.0, "https://slow.example:8935": 1.0},
	}
	sel.sessions = []*BroadcastSession{slow, fast}

	got := sel.selectUnknownSession(context.Background())
	require.NotNil(t, got)
	// The probability universe collapsed both instances to the single service
	// address — proving grouping is by service, not recipient.
	require.Equal(t, []ethcommon.Address{ethcommon.BytesToAddress(service.Bytes())}, spy.gotAddrs)
	require.Equal(t, "https://fast.example:8935", got.Transcoder(),
		"faster instance under the grouped service wins")
}

func TestProbabilitySelection_PerfWeightFavorsFastOrch(t *testing.T) {
	fast := ethcommon.HexToAddress("0x1111111111111111111111111111111111111111")
	slow := ethcommon.HexToAddress("0x2222222222222222222222222222222222222222")
	addrs := []ethcommon.Address{fast, slow}

	// slow orchestrator has far more stake; fast orchestrator has the better
	// observed end-to-end speed EWMA. With perf-dominant weights the fast,
	// low-stake orchestrator must out-rank the slow, high-stake one.
	stakes := map[ethcommon.Address]int64{fast: 1, slow: 100}
	prices := map[ethcommon.Address]*big.Rat{fast: big.NewRat(1, 1), slow: big.NewRat(1, 1)}
	perf := map[ethcommon.Address]float64{fast: 5.0, slow: 1.0}

	sa := ProbabilitySelectionAlgorithm{
		StakeWeight: 0.2, PriceWeight: 0.1, PerfWeight: 0.5, RandWeight: 0.2,
		PriceExpFactor: 100,
	}
	probs := sa.calculateProbabilities(addrs, stakes, prices, perf)
	require.Greater(t, probs[fast], probs[slow], "fast low-stake orch should outrank slow high-stake orch")
}

func TestProbabilitySelection_UnknownPerfGetsExploration(t *testing.T) {
	known := ethcommon.HexToAddress("0x1111111111111111111111111111111111111111")
	unknown := ethcommon.HexToAddress("0x2222222222222222222222222222222222222222")
	addrs := []ethcommon.Address{known, unknown}
	stakes := map[ethcommon.Address]int64{known: 1, unknown: 1}
	prices := map[ethcommon.Address]*big.Rat{known: big.NewRat(1, 1), unknown: big.NewRat(1, 1)}
	// Only `known` has an observed score; `unknown` should still get non-trivial
	// probability mass (exploration default + rand), not be starved.
	perf := map[ethcommon.Address]float64{known: 3.0}

	sa := ProbabilitySelectionAlgorithm{StakeWeight: 0.2, PerfWeight: 0.5, RandWeight: 0.3, PriceExpFactor: 100}
	probs := sa.calculateProbabilities(addrs, stakes, prices, perf)
	require.Greater(t, probs[unknown], 0.1, "unknown orch must retain exploration probability")
}

func TestFilterByMinStake(t *testing.T) {
	a := ethcommon.HexToAddress("0x1111111111111111111111111111111111111111")
	b := ethcommon.HexToAddress("0x2222222222222222222222222222222222222222")
	addrs := []ethcommon.Address{a, b}
	stakes := map[ethcommon.Address]int64{a: 100, b: 10}

	sa := ProbabilitySelectionAlgorithm{MinStake: 50}
	got := sa.filterByMinStake(context.Background(), addrs, stakes)
	require.Equal(t, []ethcommon.Address{a}, got, "below-min orchestrator should be filtered out")

	// Empty-pool fallback: when none qualify, return all rather than nothing.
	saHigh := ProbabilitySelectionAlgorithm{MinStake: 1000}
	gotAll := saHigh.filterByMinStake(context.Background(), addrs, stakes)
	require.ElementsMatch(t, addrs, gotAll, "must fall back to all candidates when none meet the floor")

	// Off by default.
	saOff := ProbabilitySelectionAlgorithm{}
	require.Equal(t, addrs, saOff.filterByMinStake(context.Background(), addrs, stakes))
}

func TestSubsetScores(t *testing.T) {
	a := "https://orch-a.example:8935"
	b := "https://orch-b.example:8935"
	src := map[string]float64{a: 2.0}
	out := subsetScores(src, []string{a, b})
	require.Equal(t, 2.0, out[a])
	_, ok := out[b]
	require.False(t, ok, "missing endpoint must be omitted, not defaulted")
}

func TestOrchPerfQueueIsBounded(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })
	store := &orchHealthStore{rdb: rdb, perfQueue: make(chan perfObservation, 1)}
	// Mark initialization complete without starting workers so saturation is deterministic.
	store.perfQueueOnce.Do(func() {})
	observation := perfObservation{endpoint: "https://orch.example:8935"}
	require.True(t, store.enqueuePerf(observation))
	require.False(t, store.enqueuePerf(observation))
}
