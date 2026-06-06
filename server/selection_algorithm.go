package server

import (
	"context"
	"math"
	"math/big"
	"math/rand"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/go-livepeer/clog"
)

var random = rand.New(rand.NewSource(time.Now().UnixNano()))

type ProbabilitySelectionAlgorithm struct {
	MinPerfScore float64
	MinStake     int64

	StakeWeight float64
	PriceWeight float64
	RandWeight  float64
	PerfWeight  float64

	PriceExpFactor         float64
	IgnoreMaxPriceIfNeeded bool
}

func (sa ProbabilitySelectionAlgorithm) Select(ctx context.Context, addrs []ethcommon.Address, stakes map[ethcommon.Address]int64, maxPrice *big.Rat, prices map[ethcommon.Address]*big.Rat, perfScores map[ethcommon.Address]float64) ethcommon.Address {
	filtered := sa.filter(ctx, addrs, maxPrice, prices, perfScores)
	filtered = sa.filterByMinStake(ctx, filtered, stakes)
	probabilities := sa.calculateProbabilities(filtered, stakes, prices, perfScores)
	return selectBy(probabilities)
}

// filterByMinStake drops orchestrators below the configured minimum stake as an
// eligibility gate, but falls back to all candidates if none qualify so a strict
// floor can never empty the pool. Off by default (MinStake <= 0).
func (sa ProbabilitySelectionAlgorithm) filterByMinStake(ctx context.Context, addrs []ethcommon.Address, stakes map[ethcommon.Address]int64) []ethcommon.Address {
	if sa.MinStake <= 0 {
		return addrs
	}
	var res []ethcommon.Address
	for _, addr := range addrs {
		if stakes[addr] >= sa.MinStake {
			res = append(res, addr)
		}
	}
	if len(res) == 0 {
		clog.Warningf(ctx, "No Orchestrators passed min stake filter, not using the filter, numAddrs=%d, minStake=%d", len(addrs), sa.MinStake)
		return addrs
	}
	return res
}

func (sa ProbabilitySelectionAlgorithm) filter(ctx context.Context, addrs []ethcommon.Address, maxPrice *big.Rat, prices map[ethcommon.Address]*big.Rat, perfScores map[ethcommon.Address]float64) []ethcommon.Address {
	filteredByPerfScore := sa.filterByPerfScore(ctx, addrs, perfScores)
	return sa.filterByMaxPrice(ctx, filteredByPerfScore, maxPrice, prices)
}

func (sa ProbabilitySelectionAlgorithm) filterByPerfScore(ctx context.Context, addrs []ethcommon.Address, scores map[ethcommon.Address]float64) []ethcommon.Address {
	if sa.MinPerfScore <= 0 || len(scores) == 0 {
		// Performance Score filter not defined, return all Orchestrators
		return addrs
	}

	var res []ethcommon.Address
	for _, addr := range addrs {
		if scores[addr] >= sa.MinPerfScore {
			res = append(res, addr)
		}
	}

	if len(res) == 0 {
		// If no orchestrators pass the perf filter, return all Orchestrators.
		// That may mean some issues with the PerfScore service.
		clog.Warningf(ctx, "No Orchestrators passed min performance score filter, not using the filter, numAddrs=%d, minPerfScore=%v, scores=%v, addrs=%v", len(addrs), sa.MinPerfScore, scores, addrs)
		return addrs
	}
	return res
}

func (sa ProbabilitySelectionAlgorithm) filterByMaxPrice(ctx context.Context, addrs []ethcommon.Address, maxPrice *big.Rat, prices map[ethcommon.Address]*big.Rat) []ethcommon.Address {
	res := filterByMaxPrice(ctx, addrs, maxPrice, prices)
	if len(res) == 0 && sa.IgnoreMaxPriceIfNeeded {
		// If no orchestrators pass the filter, return all Orchestrators
		// It means that no orchestrators are below the max price
		clog.Warningf(ctx, "No Orchestrators passed max price filter, not using the filter, numAddrs=%d, maxPrice=%v, prices=%v, addrs=%v", len(addrs), maxPrice, prices, addrs)
		return addrs
	}
	return res
}

func filterByMaxPrice(ctx context.Context, addrs []ethcommon.Address, maxPrice *big.Rat, prices map[ethcommon.Address]*big.Rat) []ethcommon.Address {
	if maxPrice == nil || len(prices) == 0 {
		// Max price filter not defined, return all Orchestrators
		return addrs
	}

	var res []ethcommon.Address
	for _, addr := range addrs {
		price := prices[addr]
		if price != nil && price.Cmp(maxPrice) <= 0 {
			res = append(res, addr)
		} else {
			if price == nil {
				price = big.NewRat(-1, 1)
			}
			clog.Warningf(ctx, "Orchestrator %s is above max price %v, price=%v", addr, maxPrice.FloatString(3), price.FloatString(3))
		}
	}
	return res
}

func (sa ProbabilitySelectionAlgorithm) calculateProbabilities(addrs []ethcommon.Address, stakes map[ethcommon.Address]int64, prices map[ethcommon.Address]*big.Rat, perfScores map[ethcommon.Address]float64) map[ethcommon.Address]float64 {
	pricesNorm := map[ethcommon.Address]float64{}
	for _, addr := range addrs {
		p, _ := prices[addr].Float64()
		pricesNorm[addr] = math.Exp(-1 * p / sa.PriceExpFactor)
	}

	// Performance values with an exploration default: an orchestrator with no
	// observed score yet is assigned the mean of the observed ones, so it sits
	// mid-pack (RandWeight still surfaces it) instead of being starved or unfairly
	// preferred. With no observations at all the term degenerates to uniform.
	perfVals := map[ethcommon.Address]float64{}
	var perfObserved float64
	var perfKnown int
	for _, addr := range addrs {
		if v, ok := perfScores[addr]; ok && v > 0 {
			perfObserved += v
			perfKnown++
		}
	}
	perfDefault := 0.0
	if perfKnown > 0 {
		perfDefault = perfObserved / float64(perfKnown)
	}

	var priceSum, stakeSum, perfSum float64
	for _, addr := range addrs {
		priceSum += pricesNorm[addr]
		stakeSum += float64(stakes[addr])
		v, ok := perfScores[addr]
		if !ok || v <= 0 {
			v = perfDefault
		}
		perfVals[addr] = v
		perfSum += v
	}

	probs := map[ethcommon.Address]float64{}
	for _, addr := range addrs {
		priceProb := 1.0
		if priceSum != 0 {
			priceProb = pricesNorm[addr] / priceSum
		}
		stakeProb := 1.0
		if stakeSum != 0 {
			stakeProb = float64(stakes[addr]) / stakeSum
		}
		perfProb := 1.0 / float64(len(addrs))
		if perfSum != 0 {
			perfProb = perfVals[addr] / perfSum
		}
		randProb := 1.0 / float64(len(addrs))

		probs[addr] = sa.PriceWeight*priceProb + sa.StakeWeight*stakeProb + sa.PerfWeight*perfProb + sa.RandWeight*randProb
	}

	return probs
}

func selectBy(probabilities map[ethcommon.Address]float64) ethcommon.Address {
	if len(probabilities) == 0 {
		return ethcommon.Address{}
	}

	var addrs []ethcommon.Address
	var cumProbs []float64
	var cumProb float64
	for addr, prob := range probabilities {
		addrs = append(addrs, addr)
		cumProb += prob
		cumProbs = append(cumProbs, cumProb)
	}

	r := random.Float64()
	for i, cumProb := range cumProbs {
		if r <= cumProb {
			return addrs[i]
		}
	}

	// return any Orchestrator is none was found with the probabilities
	// should not happen, but just to be on the safe side if we encounter some super corner case with the float
	// number precision
	return addrs[0]
}

// LiveSelectionAlgorithm is the Selection Algorithm used for Realtime Video AI
type LiveSelectionAlgorithm struct{}

func (sa LiveSelectionAlgorithm) Select(ctx context.Context, addrs []ethcommon.Address, stakes map[ethcommon.Address]int64, maxPrice *big.Rat, prices map[ethcommon.Address]*big.Rat, perfScores map[ethcommon.Address]float64) ethcommon.Address {
	filtered := filterByMaxPrice(ctx, addrs, maxPrice, prices)
	if len(filtered) == 0 {
		return ethcommon.Address{}
	}
	// Return the first address that satisfies the max price filter
	return filtered[0]
}
