package server

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/eth"
)

// AutoDepositConfig configures the auto-deposit goroutine.
type AutoDepositConfig struct {
	// MinDeposit triggers a deposit when the TicketBroker deposit falls below this (wei).
	MinDeposit *big.Int
	// GasReserve is kept in the wallet for future transactions; everything else is deposited (wei).
	GasReserve *big.Int
	// Interval between deposit checks.
	Interval time.Duration
}

// RunAutoDeposit polls the gateway's TicketBroker deposit and native ETH balance.
// When the on-chain deposit drops below cfg.MinDeposit AND the wallet has a
// significant balance (> GasReserve + meaningful amount), it deposits
// balance - GasReserve into the TicketBroker.
func RunAutoDeposit(ctx context.Context, ethClient eth.LivepeerEthClient, backend eth.Backend, cfg AutoDepositConfig) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	addr := ethClient.Account().Address
	glog.Infof("Auto-deposit started for %s (minDeposit=%s gasReserve=%s interval=%s)",
		addr.Hex(), formatETH(cfg.MinDeposit), formatETH(cfg.GasReserve), cfg.Interval)

	for {
		select {
		case <-ctx.Done():
			glog.Info("Auto-deposit stopped")
			return
		case <-ticker.C:
			checkAndDeposit(ctx, ethClient, backend, addr, cfg)
		}
	}
}

func checkAndDeposit(ctx context.Context, ethClient eth.LivepeerEthClient, backend eth.Backend, addr common.Address, cfg AutoDepositConfig) {
	info, err := ethClient.GetSenderInfo(addr)
	if err != nil {
		glog.Errorf("Auto-deposit: failed to get sender info: %v", err)
		return
	}

	if info.Deposit.Cmp(cfg.MinDeposit) >= 0 {
		glog.V(6).Infof("Auto-deposit: deposit %s >= min %s, skipping",
			formatETH(info.Deposit), formatETH(cfg.MinDeposit))
		return
	}

	glog.Infof("Auto-deposit: deposit %s < min %s, checking wallet balance",
		formatETH(info.Deposit), formatETH(cfg.MinDeposit))

	balance, err := backend.BalanceAt(ctx, addr, nil)
	if err != nil {
		glog.Errorf("Auto-deposit: failed to get balance: %v", err)
		return
	}

	// Deposit amount = balance - gasReserve
	depositAmount := new(big.Int).Sub(balance, cfg.GasReserve)

	// Only deposit if the amount is meaningful (at least 0.02 ETH / ~$34)
	minMeaningful := new(big.Int).SetUint64(2e16) // 0.02 ETH
	if depositAmount.Cmp(minMeaningful) <= 0 {
		glog.Warningf("Auto-deposit: wallet balance %s too low after gas reserve %s (would deposit %s)",
			formatETH(balance), formatETH(cfg.GasReserve), formatETH(depositAmount))
		return
	}

	glog.Infof("Auto-deposit: depositing %s into TicketBroker (balance=%s reserve=%s)",
		formatETH(depositAmount), formatETH(balance), formatETH(cfg.GasReserve))

	tx, err := ethClient.FundDeposit(depositAmount)
	if err != nil {
		glog.Errorf("Auto-deposit: FundDeposit failed: %v", err)
		return
	}

	glog.Infof("Auto-deposit: FundDeposit tx=%s amount=%s", tx.Hash().Hex(), formatETH(depositAmount))
}

func formatETH(wei *big.Int) string {
	if wei == nil {
		return "0"
	}
	return eth.FormatUnits(wei, "ETH")
}
