package starter

import (
	"math/big"
	"testing"

	"github.com/livepeer/go-livepeer/core"
	"github.com/stretchr/testify/require"
)

func offchainOrchestratorConfig(pricePerUnit *string, pixelsPerUnit string) LivepeerConfig {
	cfg := DefaultLivepeerConfig()
	orchestrator := true
	cfg.Orchestrator = &orchestrator
	cfg.PricePerUnit = pricePerUnit
	cfg.PixelsPerUnit = &pixelsPerUnit
	return cfg
}

func TestOffchainOrchestratorQuotesExplicitPricePerUnit(t *testing.T) {
	n, err := core.NewLivepeerNode(nil, t.TempDir(), nil)
	require.NoError(t, err)
	price := "1500"
	setOffchainOrchestratorPrices(n, offchainOrchestratorConfig(&price, "1000"))

	require.True(t, n.QuoteOffchainPrice)
	require.Nil(t, n.Recipient)
	require.Zero(t, big.NewRat(3, 2).Cmp(n.GetBasePrice("default")), "price per pixel is pricePerUnit/pixelsPerUnit")
}

func TestOffchainOrchestratorWithoutPricePerUnitQuotesNothing(t *testing.T) {
	n, err := core.NewLivepeerNode(nil, t.TempDir(), nil)
	require.NoError(t, err)
	setOffchainOrchestratorPrices(n, offchainOrchestratorConfig(nil, "1000"))

	require.False(t, n.QuoteOffchainPrice)
	require.Nil(t, n.GetBasePrice("default"))
}

func TestOffchainOrchestratorRefusesCurrencyWithoutPriceFeed(t *testing.T) {
	n, err := core.NewLivepeerNode(nil, t.TempDir(), nil)
	require.NoError(t, err)
	price := "0.50USD"
	require.PanicsWithError(t, "Error converting price: PriceFeedWatcher is not initialized", func() {
		setOffchainOrchestratorPrices(n, offchainOrchestratorConfig(&price, "1"))
	})
	require.False(t, n.QuoteOffchainPrice)
}

func TestOnchainOrchestratorStillRequiresPricePerUnit(t *testing.T) {
	n, err := core.NewLivepeerNode(nil, t.TempDir(), nil)
	require.NoError(t, err)
	require.PanicsWithError(t, "-pricePerUnit must be set", func() {
		setOrchestratorBasePrices(n, offchainOrchestratorConfig(nil, "1"), true)
	})
	price := "1500"
	setOrchestratorBasePrices(n, offchainOrchestratorConfig(&price, "1000"), true)
	require.Zero(t, big.NewRat(3, 2).Cmp(n.GetBasePrice("default")))
	require.False(t, n.QuoteOffchainPrice, "an onchain orchestrator quotes through its ticket recipient")
}
