package core

import (
	"math/big"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-livepeer/pm"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func TestPriceInfo_OffchainQuotesConfiguredPriceWithoutTickets(t *testing.T) {
	n, _ := NewLivepeerNode(nil, "", nil)
	n.SetBasePrice("default", NewFixedPrice(big.NewRat(3, 2)))
	n.QuoteOffchainPrice = true
	require.True(t, n.AutoAdjustPrice, "the node default leaves auto adjustment on")
	require.Nil(t, n.Recipient)
	require.Nil(t, n.Balances)
	orch := NewOrchestrator(n, nil)
	sender := ethcommon.Address{}

	// A session ID is passed while serving a segment; offchain there is no
	// session balance holding a fixed price.
	for _, manifestID := range []ManifestID{"", "session"} {
		price, err := orch.PriceInfo(sender, manifestID)
		require.NoError(t, err)
		require.Equal(t, &net.PriceInfo{PricePerUnit: 3, PixelsPerUnit: 2}, price)

		price, err = orch.PriceInfoForCaps(sender, manifestID, &net.Capabilities{})
		require.NoError(t, err)
		require.Equal(t, &net.PriceInfo{PricePerUnit: 3, PixelsPerUnit: 2}, price)

		params, err := orch.TicketParams(sender, price)
		require.NoError(t, err)
		require.Nil(t, params, "an offchain orchestrator issues no ticket params")
	}
}

func TestPriceInfo_OffchainWithoutQuoteFlagQuotesNothing(t *testing.T) {
	n, _ := NewLivepeerNode(nil, "", nil)
	// A base price set at runtime (CLI) does not make an offchain node quote.
	n.SetBasePrice("default", NewFixedPrice(big.NewRat(3, 2)))
	orch := NewOrchestrator(n, nil)

	price, err := orch.PriceInfo(ethcommon.Address{}, "session")
	require.NoError(t, err)
	require.Nil(t, price)
	price, err = orch.PriceInfoForCaps(ethcommon.Address{}, "session", &net.Capabilities{})
	require.NoError(t, err)
	require.Nil(t, price)
}

func TestPriceInfo_OnchainKeepsTicketOverheadAndSessionPrice(t *testing.T) {
	n, _ := NewLivepeerNode(nil, "", nil)
	n.SetBasePrice("default", NewFixedPrice(big.NewRat(1, 1)))
	n.Balances = NewAddressBalances(time.Minute)
	defer n.Balances.StopCleanup()
	recipient := new(pm.MockRecipient)
	recipient.On("TxCostMultiplier", mock.Anything).Return(big.NewRat(100, 1), nil)
	n.Recipient = recipient
	orch := NewOrchestrator(n, nil)
	sender := ethcommon.Address{}

	price, err := orch.PriceInfo(sender, "session")
	require.NoError(t, err)
	require.Zero(t, big.NewRat(101, 100).Cmp(big.NewRat(price.PricePerUnit, price.PixelsPerUnit)), "base price plus 1/txCostMultiplier redemption overhead")

	n.Balances.Credit(sender, "session", big.NewRat(0, 1))
	n.Balances.SetFixedPrice(sender, "session", big.NewRat(7, 5))
	price, err = orch.PriceInfo(sender, "session")
	require.NoError(t, err)
	require.Equal(t, &net.PriceInfo{PricePerUnit: 7, PixelsPerUnit: 5}, price, "the session's fixed price wins")
}
