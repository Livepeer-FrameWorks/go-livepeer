package frameworks

import (
	"context"
	pb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto/ipc"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/telemetryspool"
	"google.golang.org/protobuf/proto"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIOutcomeKeepsItsExistingIngestWriter(t *testing.T) {
	q, err := telemetryspool.Open(telemetryspool.Config{Directory: t.TempDir()})
	require.NoError(t, err)
	mu.Lock()
	oldSpool := spool
	spool = q
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); spool = oldSpool; mu.Unlock() })
	send(context.Background(), &pb.GatewayTelemetryEvent{Payload: &pb.GatewayTelemetryEvent_Ai{Ai: &pb.OrchestratorAIOutcome{Success: true}}})
	rows, err := q.Pending(1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var event pb.GatewayTelemetryEvent
	require.NoError(t, proto.Unmarshal(rows[0].Payload, &event))
	require.Equal(t, int32(2), event.SchemaVersion, "the separate AI writer must receive this event instead of the transcode-only v3 projection")
	require.NotEmpty(t, event.EventId)
}

func TestHostFromURL(t *testing.T) {
	tests := map[string]string{
		"https://orch.example:8935/path": "orch.example",
		"orch.example:8935/path":         "orch.example",
		"https://[2001:db8::1]:8935/x":   "2001:db8::1",
		"[2001:db8::1]:8935":             "2001:db8::1",
		"":                               "",
	}
	for input, expected := range tests {
		require.Equal(t, expected, hostFromURL(input), input)
	}
}

func TestDecklogAllowsInsecure(t *testing.T) {
	for _, mode := range []string{"", "disabled", "insecure"} {
		allow, err := decklogAllowsInsecure(mode)
		require.NoError(t, err)
		require.True(t, allow)
	}
	for _, mode := range []string{"tls", "mtls", "MTLS"} {
		allow, err := decklogAllowsInsecure(mode)
		require.NoError(t, err)
		require.False(t, allow)
	}
	_, err := decklogAllowsInsecure("typo")
	require.Error(t, err)
}
