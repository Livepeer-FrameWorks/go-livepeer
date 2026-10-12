package discovery

import (
	"context"
	"errors"
	"net/url"
	"testing"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/livepeer/go-livepeer/common"
	"github.com/livepeer/go-livepeer/net"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDiscoveryAdmissionClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"sender reserve", status.Error(codes.Unknown, "insufficient sender reserve"), "observer_payment"},
		{"signature", status.Error(codes.Unknown, "orchestrator req sig check failed"), "authentication"},
		{"authentication", status.Error(codes.Unauthenticated, "denied"), "authentication"},
		{"permission", status.Error(codes.PermissionDenied, "denied"), "authentication"},
		{"capacity text", status.Error(codes.Unknown, "orchestrator is at capacity"), "capacity"},
		{"capacity code", status.Error(codes.ResourceExhausted, "busy"), "capacity"},
		{"cancelled rpc", status.Error(codes.Canceled, "context canceled"), "canceled"},
		{"cancelled local", context.Canceled, "canceled"},
		{"timeout", context.DeadlineExceeded, "timeout"},
		{"connection", errors.New("connection refused"), "tcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyDiscoveryError(tc.err); got != tc.want {
				t.Fatalf("classification=%q, want %q", got, tc.want)
			}
		})
	}
}

// Both discovery paths dial from URL-only descriptors, so a failed call to an
// orchestrator that answered before must still name its address.
func TestDiscoveryFailureKeepsLearnedOrchestratorIdentity(t *testing.T) {
	learned, _ := url.Parse("https://orch-learned.test:8935")
	unknown, _ := url.Parse("https://orch-unknown.test:8935")
	descriptor := func(u *url.URL) common.OrchestratorDescriptor {
		return common.OrchestratorDescriptor{LocalInfo: &common.OrchestratorLocalInfo{URL: u}}
	}
	address := ethcommon.HexToAddress("0x00000000000000000000000000000000000000ab")
	want := hexutil.Encode(address.Bytes())

	if got := discoveryOrchAddr(descriptor(learned), &net.OrchestratorInfo{Address: address.Bytes(), Transcoder: learned.String()}); got != want {
		t.Fatalf("success attributed to %q, want %q", got, want)
	}
	if got := discoveryOrchAddr(descriptor(learned), nil); got != want {
		t.Fatalf("failure after a success attributed to %q, want %q", got, want)
	}
	if got := knownOrchAddr(descriptor(learned), nil); got != want {
		t.Fatalf("admission decision after a success attributed to %q, want %q", got, want)
	}
	if got := discoveryOrchAddr(descriptor(unknown), nil); got != "url:orch-unknown.test:8935" {
		t.Fatalf("failure of a never-answered orchestrator attributed to %q", got)
	}
	if got := knownOrchAddr(descriptor(unknown), nil); got != "" {
		t.Fatalf("admission decision of a never-answered orchestrator attributed to %q", got)
	}
}
