package discovery

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/core"
)

// discoverySnapshotMaxAge bounds how old a persisted snapshot may be and still
// be hydrated. It is aligned with the selection horizon (SelectOrchs uses
// UpdatedLastDay), so caps and rows survive at least as long as serving would
// select them. Earlier staleness is surfaced as a freshness metric, not by
// dropping the snapshot.
const (
	discoverySnapshotMaxAge        = 24 * time.Hour
	discoverySnapshotMaxFutureSkew = 5 * time.Minute
)

// newDiscoverySnapshot builds an identity-stamped snapshot envelope from a
// successful discovery refresh.
func newDiscoverySnapshot(chainID, network, broadcasterAddr, region string, capturedAt time.Time,
	orchs []*common.DBOrch, caps []*common.OrchNetworkCapabilities) *common.DiscoverySnapshot {
	return &common.DiscoverySnapshot{
		SchemaVersion:   common.DiscoverySnapshotSchemaVersion,
		ChainID:         chainID,
		Network:         network,
		BroadcasterAddr: broadcasterAddr,
		Region:          region,
		CapturedAt:      capturedAt,
		Orchestrators:   orchs,
		Capabilities:    caps,
	}
}

func marshalDiscoverySnapshot(snap *common.DiscoverySnapshot) (string, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// parseDiscoverySnapshot decodes a persisted snapshot. An empty string yields
// (nil, nil) so "no snapshot" is not an error.
func parseDiscoverySnapshot(raw string) (*common.DiscoverySnapshot, error) {
	if raw == "" {
		return nil, nil
	}
	var snap common.DiscoverySnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, fmt.Errorf("could not parse discovery snapshot: %w", err)
	}
	return &snap, nil
}

// validateDiscoverySnapshot rejects a snapshot from a different schema version,
// chain, network, gateway, or region, or one older than discoverySnapshotMaxAge. This
// is what stops a snapshot meant for another network/gateway (e.g. via a shared
// Redis mirror) from ever being applied.
func validateDiscoverySnapshot(snap *common.DiscoverySnapshot, chainID, network, broadcasterAddr, region string, now time.Time) error {
	if snap == nil {
		return fmt.Errorf("no snapshot")
	}
	if snap.SchemaVersion != common.DiscoverySnapshotSchemaVersion {
		return fmt.Errorf("snapshot schema version mismatch: have %d want %d", snap.SchemaVersion, common.DiscoverySnapshotSchemaVersion)
	}
	if snap.ChainID != chainID {
		return fmt.Errorf("snapshot chainID mismatch: have %q want %q", snap.ChainID, chainID)
	}
	if snap.Network != network {
		return fmt.Errorf("snapshot network mismatch: have %q want %q", snap.Network, network)
	}
	if !strings.EqualFold(snap.BroadcasterAddr, broadcasterAddr) {
		return fmt.Errorf("snapshot broadcaster mismatch: have %q want %q", snap.BroadcasterAddr, broadcasterAddr)
	}
	if snap.Region != region {
		return fmt.Errorf("snapshot region mismatch: have %q want %q", snap.Region, region)
	}
	if age := now.Sub(snap.CapturedAt); age > discoverySnapshotMaxAge {
		return fmt.Errorf("snapshot too old: captured %s ago", age.Round(time.Second))
	} else if age < -discoverySnapshotMaxFutureSkew {
		return fmt.Errorf("snapshot captured in the future: %s", (-age).Round(time.Second))
	}
	return nil
}

// HydrateFromSnapshot loads the persisted discovery snapshot and, if it is valid
// for this chain/network/gateway and not expired, restores in-memory
// capabilities immediately and — only when the local DB has no selectable rows
// (a replacement host or wiped datadir) — writes the snapshot's orchestrator
// rows so serving selection can use them before live discovery completes.
//
// It is best-effort: any problem is logged and the function returns nil so
// startup never fails on an absent/stale snapshot. Returns the applied snapshot
// (whose CapturedAt the caller seeds into the pool cache so an early empty
// refresh doesn't immediately clear the hydrated caps), or nil if none applied.
func HydrateFromSnapshot(n *core.LivepeerNode, network, broadcasterAddr, region string, currentRound *big.Int, now time.Time) *common.DiscoverySnapshot {
	if n == nil || n.Database == nil {
		return nil
	}
	db := n.Database

	raw, err := db.SelectNetworkCapabilitiesSnapshot()
	if err != nil {
		glog.Errorf("discovery: could not load snapshot: %v", err)
		return nil
	}
	snap, err := parseDiscoverySnapshot(raw)
	if err != nil {
		glog.Errorf("discovery: %v", err)
		return nil
	}
	if snap == nil {
		return nil
	}

	var chainID string
	if id, err := db.ChainID(); err == nil && id != nil {
		chainID = id.String()
	}
	if err := validateDiscoverySnapshot(snap, chainID, network, broadcasterAddr, region, now); err != nil {
		glog.Infof("discovery: ignoring persisted snapshot: %v", err)
		return nil
	}

	// Restore capabilities for capability-aware routing + the readiness gate.
	n.UpdateNetworkCapabilities(snap.Capabilities)

	// Only seed selection rows when the local DB has nothing selectable yet
	// (replacement host / wiped datadir). On a same-node restart the local
	// orchestrators table is authoritative and fresher, so leave it untouched.
	selectable, err := db.SelectOrchs(&common.DBOrchFilter{
		CurrentRound:   currentRound,
		UpdatedLastDay: true,
	})
	if err != nil {
		glog.Errorf("discovery: could not check local orchestrators: %v", err)
	} else if len(selectable) == 0 {
		for _, o := range snap.Orchestrators {
			if err := db.UpdateOrch(o); err != nil {
				glog.Errorf("discovery: could not hydrate orchestrator %s: %v", o.EthereumAddr, err)
			}
		}
		glog.Infof("discovery: hydrated %d orchestrator rows from snapshot (captured %s)",
			len(snap.Orchestrators), snap.CapturedAt.Format(time.RFC3339))
	}

	glog.Infof("discovery: restored %d orchestrator capabilities from snapshot", len(snap.Capabilities))
	return snap
}
