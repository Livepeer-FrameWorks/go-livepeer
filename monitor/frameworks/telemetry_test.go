package frameworks

import (
	"context"
	pb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto/ipc"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/telemetryspool"
	"go.uber.org/goleak"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTelemetryCancellationClosesDecklogConnection(t *testing.T) {
	before := goleak.IgnoreCurrent()
	t.Setenv("FRAMEWORKS_DECKLOG_GRPC_ADDR", "127.0.0.1:1")
	t.Setenv("FRAMEWORKS_DECKLOG_TLS_MODE", "insecure")
	t.Setenv("FRAMEWORKS_GATEWAY_ID", "shutdown-test")
	t.Setenv("FRAMEWORKS_CLUSTER_ID", "test-cluster")
	t.Setenv("FRAMEWORKS_CLUSTER_OWNER_TENANT_ID", "11111111-1111-4111-8111-111111111111")
	t.Setenv("FRAMEWORKS_TELEMETRY_SPOOL_DIR", t.TempDir())
	t.Setenv("FRAMEWORKS_NETWORK_OBSERVER_URL", "")
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, Init(ctx))
	t.Cleanup(func() {
		t.Setenv("FRAMEWORKS_DECKLOG_GRPC_ADDR", "")
		require.NoError(t, Init(context.Background()))
	})
	cancel()
	require.Eventually(t, func() bool { return goleak.Find(before) == nil }, time.Second, 10*time.Millisecond)
}

// installSpool routes send through a spool writer for q until the test ends.
func installSpool(t *testing.T, ctx context.Context, q *telemetryspool.Queue) {
	t.Helper()
	w := startSpoolWriter(ctx, q.Enqueue)
	mu.Lock()
	oldSpool, oldWriter := spool, writer
	spool, writer = q, w
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); spool, writer = oldSpool, oldWriter; mu.Unlock() })
}

func TestAIOutcomeKeepsItsExistingIngestWriter(t *testing.T) {
	q, err := telemetryspool.Open(telemetryspool.Config{Directory: t.TempDir()})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	installSpool(t, ctx, q)
	send(context.Background(), &pb.GatewayTelemetryEvent{Payload: &pb.GatewayTelemetryEvent_Ai{Ai: &pb.OrchestratorAIOutcome{Success: true}}})
	var rows []telemetryspool.Record
	require.Eventually(t, func() bool {
		rows, err = q.Pending(1)
		return err == nil && len(rows) == 1
	}, 5*time.Second, 10*time.Millisecond)
	var event pb.GatewayTelemetryEvent
	require.NoError(t, proto.Unmarshal(rows[0].Payload, &event))
	require.Equal(t, int32(2), event.SchemaVersion, "the separate AI writer must receive this event instead of the transcode-only v3 projection")
	require.NotEmpty(t, event.EventId)
}

// Segments still in flight when the gateway shuts down emit their outcomes after
// the telemetry context ended; the spool keeps them for the next start.
func TestSendAfterShutdownStillSpools(t *testing.T) {
	q, err := telemetryspool.Open(telemetryspool.Config{Directory: t.TempDir()})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	installSpool(t, ctx, q)
	time.Sleep(100 * time.Millisecond)
	const events = 20
	for i := 0; i < events; i++ {
		send(context.Background(), &pb.GatewayTelemetryEvent{Payload: &pb.GatewayTelemetryEvent_Transcode{Transcode: &pb.OrchestratorTranscodeOutcome{SeqNo: uint64(i)}}})
	}
	require.Eventually(t, func() bool {
		rows, err := q.Pending(events)
		return err == nil && len(rows) == events
	}, 2*time.Second, 10*time.Millisecond)
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
