package core

import (
	"testing"

	"github.com/livepeer/go-livepeer/eth"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestSetOffchainAddress_OffchainOrchestratorReportsConfiguredIdentity(t *testing.T) {
	n, _ := NewLivepeerNode(nil, "", nil)
	orch := NewOrchestrator(n, nil)
	require.Equal(t, ethcommon.Address{}, orch.Address(), "an offchain orchestrator has no identity by default")

	addr := ethcommon.HexToAddress("0x5eed0000000000000000000000000000000000c3")
	require.NoError(t, orch.SetOffchainAddress(addr))
	require.Equal(t, addr, orch.Address())
}

func TestSetOffchainAddress_OnchainOrchestratorKeepsItsAccount(t *testing.T) {
	account := ethcommon.HexToAddress("0x00000000000000000000000000000000000000aa")
	n, _ := NewLivepeerNode(&eth.StubClient{TranscoderAddress: account}, "", nil)
	orch := NewOrchestrator(n, nil)
	require.Error(t, orch.SetOffchainAddress(ethcommon.HexToAddress("0x5eed0000000000000000000000000000000000c3")))
	require.Equal(t, n.Eth.Account().Address, orch.Address())
}
