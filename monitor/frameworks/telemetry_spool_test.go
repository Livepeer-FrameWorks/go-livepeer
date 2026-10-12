//go:build linux || darwin

package frameworks

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	pb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto/ipc"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/telemetryspool"
	"github.com/stretchr/testify/require"
)

// The spool fsyncs every record and its directory under one lock that its
// delivery worker also holds while it reads records. On a busy disk that takes
// seconds, and send runs inside a segment's few-second budget: the rc6 stack
// gateways answered 503 for segments their orchestrator had transcoded in
// 80 ms. A spool whose lock is held must not hold up send.
func TestSendDoesNotWaitForAHeldSpool(t *testing.T) {
	dir := t.TempDir()
	held := filepath.Join(dir, "held.event")
	require.NoError(t, syscall.Mkfifo(held, 0o600))
	q, err := telemetryspool.Open(telemetryspool.Config{Directory: dir})
	require.NoError(t, err)

	// Pending reads each record under the spool's lock; reading the FIFO
	// blocks until a writer opens it, so the lock stays held like a stalled
	// fsync holds it.
	pendingDone := make(chan struct{})
	go func() {
		defer close(pendingDone)
		_, _ = q.Pending(10)
	}()
	// A writer that opens and closes the FIFO ends the read with EOF. On macOS
	// a writer that closes before the reader's open completes leaves the read
	// blocked, so the writer opens again until Pending returns.
	release := sync.OnceFunc(func() {
		for {
			if f, err := os.OpenFile(held, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = f.Close()
			}
			select {
			case <-pendingDone:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	t.Cleanup(release)
	time.Sleep(100 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	installSpool(t, ctx, q)

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		send(context.Background(), &pb.GatewayTelemetryEvent{Payload: &pb.GatewayTelemetryEvent_Transcode{Transcode: &pb.OrchestratorTranscodeOutcome{Success: true}}})
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("send waited for the spool's lock")
	}

	// Once the lock is free the writer persists the event. Waiting for it keeps
	// the writer from creating a file while the test directory is removed.
	release()
	require.NoError(t, os.Remove(held))
	require.Eventually(t, func() bool {
		rows, err := q.Pending(10)
		return err == nil && len(rows) == 1
	}, 5*time.Second, 10*time.Millisecond)
}
