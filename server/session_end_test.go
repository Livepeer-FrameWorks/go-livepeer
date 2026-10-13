package server

import (
	"context"
	"errors"
	gonet "net"
	"sync"
	"testing"
	"time"

	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/lpms/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// endRecorder records the sessions a pool asks its orchestrators to end.
type endRecorder struct {
	mu    sync.Mutex
	ended []*BroadcastSession
}

func (r *endRecorder) end(sess *BroadcastSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, sess)
}

func (r *endRecorder) sessions() []*BroadcastSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*BroadcastSession(nil), r.ended...)
}

func endTestPool(rec *endRecorder, sus orchSuspender, sessions ...*BroadcastSession) *SessionPool {
	sel := NewMinLSSelector(nil, 1.0, nil, nil, nil)
	sel.Add(sessions)
	pool := NewSessionPool("test", len(sessions), len(sessions), sus,
		func() ([]*BroadcastSession, error) { return nil, nil }, func(string) {}, sel)
	pool.endSession = rec.end
	for _, sess := range sessions {
		pool.sessMap[sess.Transcoder()] = sess
		pool.inSel[sess] = true
	}
	return pool
}

// A late result on a session that already left the pool must not remove the
// session the pool now holds for the same orchestrator.
func TestRemoveSession_StaleSessionKeepsCurrentSession(t *testing.T) {
	assert := assert.New(t)
	old := StubBroadcastSession("https://orch")
	pool := endTestPool(&endRecorder{}, newSuspender(), old)

	pool.removeSession(old)
	assert.Empty(pool.sessMap)

	current := StubBroadcastSession("https://orch")
	pool.sessMap[current.Transcoder()] = current

	pool.removeSession(old)
	assert.Same(current, pool.sessMap["https://orch"])
}

// Suspending an orchestrator ends its session on the orchestrator, so the
// session stops holding one of its MaxSessions slots.
func TestSuspendAndRemoveOrch_EndsOrchestratorSession(t *testing.T) {
	assert := assert.New(t)
	rec := &endRecorder{}
	bad := StubBroadcastSession("https://bad")
	good := StubBroadcastSession("https://good")
	pool := endTestPool(rec, newSuspender(), bad, good)
	bsm := &BroadcastSessionsManager{trustedPool: pool, untrustedPool: endTestPool(rec, newSuspender())}

	bsm.suspendAndRemoveOrch(context.Background(), bad, errors.New("Invalid argument"))
	assert.Equal([]*BroadcastSession{bad}, rec.sessions())

	// A second failure on the same, already removed session ends nothing new,
	// and never the orchestrator's replacement session.
	replacement := StubBroadcastSession("https://bad")
	pool.sessMap[replacement.Transcoder()] = replacement
	bsm.suspendAndRemoveOrch(context.Background(), bad, errors.New("Invalid argument"))
	assert.Equal([]*BroadcastSession{bad}, rec.sessions())
	assert.Same(replacement, pool.sessMap["https://bad"])
}

// The slow round-trip suspension ends the session it removes.
func TestRecordRoundTrip_SuspensionEndsOrchestratorSession(t *testing.T) {
	rec := &endRecorder{}
	slow := StubBroadcastSession("https://slow")
	pool := endTestPool(rec, newSuspender(), slow, StubBroadcastSession("https://fast"))
	bsm := &BroadcastSessionsManager{trustedPool: pool, untrustedPool: endTestPool(rec, newSuspender())}

	seg := &stream.HLSSegment{Duration: 1.0}
	for i := 0; i < slowStrikeLimit; i++ {
		bsm.recordRoundTrip(context.Background(), slow, seg, 10*time.Second)
	}
	assert.Equal(t, []*BroadcastSession{slow}, rec.sessions())
	assert.NotContains(t, pool.sessMap, "https://slow")
}

type sharedSuspendedStub struct {
	orchSuspender
	orch string
}

func (s *sharedSuspendedStub) sharedSuspended(orch string) bool { return orch == s.orch }

// A session dropped because another stream or gateway suspended its
// orchestrator is ended on that orchestrator.
func TestSelectSessions_SharedSuspensionDropEndsOrchestratorSession(t *testing.T) {
	assert := assert.New(t)
	rec := &endRecorder{}
	bad := StubBroadcastSession("https://bad")
	good := StubBroadcastSession("https://good")
	pool := endTestPool(rec, &sharedSuspendedStub{orchSuspender: newSuspender(), orch: "https://bad"}, bad, good)

	for i := 0; i < 2; i++ {
		sessions := pool.selectSessions(context.Background(), 1)
		assert.Equal([]*BroadcastSession{good}, sessions)
		pool.completeSession(good)
	}
	assert.NotContains(pool.sessMap, "https://bad")
	assert.Equal([]*BroadcastSession{bad}, rec.sessions())
}

// blockingEndOrch is an orchestrator whose EndTranscodingSession never answers.
type blockingEndOrch struct {
	net.UnimplementedOrchestratorServer
	called  chan struct{}
	release chan struct{}
}

func (o *blockingEndOrch) EndTranscodingSession(ctx context.Context, _ *net.EndTranscodingSessionRequest) (*net.EndTranscodingSessionResponse, error) {
	o.called <- struct{}{}
	select {
	case <-o.release:
	case <-ctx.Done():
	}
	return &net.EndTranscodingSessionResponse{}, nil
}

// Ending a session gives up once endSessionTimeout passes, so an orchestrator
// that never answers cannot pin the gateway's teardown goroutine.
func TestEndTranscodingSession_Bounded(t *testing.T) {
	lis, err := gonet.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	orch := &blockingEndOrch{called: make(chan struct{}, 1), release: make(chan struct{})}
	srv := grpc.NewServer()
	net.RegisterOrchestratorServer(srv, orch)
	go srv.Serve(lis)
	defer srv.Stop()
	defer close(orch.release)

	old := endSessionTimeout
	endSessionTimeout = 200 * time.Millisecond
	defer func() { endSessionTimeout = old }()

	sess := StubBroadcastSession("http://" + lis.Addr().String())
	done := make(chan error, 1)
	go func() { done <- EndTranscodingSession(context.Background(), sess) }()

	select {
	case <-orch.called:
	case <-time.After(5 * time.Second):
		t.Fatal("EndTranscodingSession never reached the orchestrator")
	}
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("EndTranscodingSession did not give up on an unresponsive orchestrator")
	}
}

// A session without an auth token has no orchestrator session to end.
func TestEndTranscodingSession_NoAuthToken(t *testing.T) {
	sess := StubBroadcastSession("http://127.0.0.1:1")
	sess.OrchestratorInfo.AuthToken = nil
	assert.NoError(t, EndTranscodingSession(context.Background(), sess))
}
