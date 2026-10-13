package server

import (
	"context"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"

	pb "github.com/Livepeer-FrameWorks/monorepo/pkg/proto/ipc"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/telemetryspool"
	ethcommon "github.com/ethereum/go-ethereum/common"
	wireproto "github.com/golang/protobuf/proto"
	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/monitor/frameworks"
	"github.com/livepeer/go-tools/drivers"
	"github.com/livepeer/lpms/stream"
	"github.com/stretchr/testify/mock"

	"github.com/livepeer/go-livepeer/net"
	"github.com/livepeer/go-livepeer/pm"
	"github.com/stretchr/testify/require"
)

func TestFrameworksAccountingKeepsFractionalChargesAndKnownZero(t *testing.T) {
	charge := frameworksAccountingAmount(big.NewRat(2, 3))
	require.Equal(t, "2", charge.Numerator)
	require.Equal(t, "3", charge.Denominator)
	require.Equal(t, "wei", charge.Unit)
	zero := frameworksAccountingAmount(big.NewRat(0, 1))
	require.NotNil(t, zero)
	require.Equal(t, "0", zero.Numerator)
	require.Nil(t, frameworksAccountingAmount(nil))
	require.Nil(t, frameworksAccountingAmount(big.NewRat(-1, 1)))
}

func TestFrameworksAccountingPreservesRealTicketExpectedValue(t *testing.T) {
	ticket := &pm.Ticket{FaceValue: big.NewInt(1000000000000000000), WinProb: new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(2))}
	expected := ticket.EV()
	credit := frameworksAccountingAmount(expected)
	require.NotNil(t, credit, "the actual Livepeer ticket fraction must not become unknown")
	require.Equal(t, expected.Num().String(), credit.Numerator)
	require.Equal(t, expected.Denom().String(), credit.Denominator)
}

func TestFrameworksReturnedWorkRejectsInvalidPixels(t *testing.T) {
	for _, counts := range [][]int64{{-1}, {math.MaxInt64, 1}} {
		result := &net.TranscodeData{}
		for _, count := range counts {
			result.Segments = append(result.Segments, &net.TranscodedSegmentData{Pixels: count})
		}
		pixels, known := frameworksReturnedPixels(result)
		require.False(t, known)
		require.Zero(t, pixels)
	}
	pixels, known := frameworksReturnedPixels(&net.TranscodeData{Segments: []*net.TranscodedSegmentData{{Pixels: 9007199254740993}, {Pixels: 7}}})
	require.True(t, known)
	require.Equal(t, uint64(9007199254741000), pixels)
}

// frameworksTestSpool points gateway telemetry at a stopped Decklog client
// and returns the spool its records land in.
func frameworksTestSpool(t *testing.T) *telemetryspool.Queue {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("FRAMEWORKS_DECKLOG_GRPC_ADDR", "127.0.0.1:1")
	t.Setenv("FRAMEWORKS_DECKLOG_TLS_MODE", "insecure")
	t.Setenv("FRAMEWORKS_GATEWAY_ID", "accounting-test")
	t.Setenv("FRAMEWORKS_CLUSTER_ID", "test-cluster")
	t.Setenv("FRAMEWORKS_CLUSTER_OWNER_TENANT_ID", "11111111-1111-4111-8111-111111111111")
	t.Setenv("FRAMEWORKS_TELEMETRY_SPOOL_DIR", directory)
	t.Setenv("FRAMEWORKS_NETWORK_OBSERVER_URL", "")
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, frameworks.Init(stopped))
	t.Cleanup(func() {
		t.Setenv("FRAMEWORKS_DECKLOG_GRPC_ADDR", "")
		require.NoError(t, frameworks.Init(context.Background()))
	})
	queue, err := telemetryspool.Open(telemetryspool.Config{Directory: directory})
	require.NoError(t, err)
	return queue
}

func TestSubmitSegmentExportsExecutionChargeSeparatelyFromTicketCredit(t *testing.T) {
	queue := frameworksTestSpool(t)
	ts, mux := stubTLSServer()
	defer ts.Close()
	body, err := wireproto.Marshal(&net.TranscodeResult{Result: &net.TranscodeResult_Data{Data: &net.TranscodeData{Segments: []*net.TranscodedSegmentData{{Url: "first", Pixels: 3}, {Url: "second", Pixels: 5}}}}})
	require.NoError(t, err)
	var responseStatus atomic.Int32
	responseStatus.Store(http.StatusOK)
	mux.HandleFunc("/segment", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(responseStatus.Load()))
		_, _ = w.Write(body)
	})
	value := big.NewRat(7, 2)
	balance := &mockBalance{}
	balance.On("StageUpdate", mock.Anything, mock.Anything).Return(0, value, big.NewRat(0, 1))
	balance.On("Credit", mock.Anything)
	sender := &pm.MockSender{}
	sender.On("EV", mock.Anything).Return(big.NewRat(0, 1), nil)
	sess := &BroadcastSession{Broadcaster: stubBroadcaster2(), Params: &core.StreamParameters{ManifestID: core.RandomManifestID(), SessionID: "test-session", TenantID: "22222222-2222-4222-8222-222222222222"}, Balance: balance, Sender: sender, OrchestratorInfo: &net.OrchestratorInfo{Transcoder: ts.URL, PriceInfo: &net.PriceInfo{PricePerUnit: 1, PixelsPerUnit: 3}, AuthToken: stubAuthToken}}
	result, err := SubmitSegment(context.Background(), sess, &stream.HLSSegment{Data: []byte("segment"), Duration: 2, SeqNo: 1}, nil, 0, true)
	require.NoError(t, err)
	require.NotNil(t, result)
	responseStatus.Store(http.StatusForbidden)
	_, err = SubmitSegment(context.Background(), sess, &stream.HLSSegment{Data: []byte("segment"), Duration: 2, SeqNo: 2}, nil, 0, true)
	require.Error(t, err)
	sess.OrchestratorInfo.PriceInfo.PixelsPerUnit = 0
	_, err = SubmitSegment(context.Background(), sess, &stream.HLSSegment{Data: []byte("segment"), Duration: 2, SeqNo: 3}, nil, 0, true)
	require.Error(t, err)
	records, err := queue.Pending(10)
	require.NoError(t, err)
	require.Len(t, records, 3)
	seen := map[uint64]*pb.OrchestratorTranscodeOutcome{}
	for _, record := range records {
		var event pb.GatewayTelemetryEvent
		require.NoError(t, wireproto.Unmarshal(record.Payload, &event))
		seen[event.GetTranscode().SeqNo] = event.GetTranscode()
	}
	successful := seen[1]
	require.Equal(t, uint32(1), successful.Accounting.Version)
	require.True(t, successful.Accounting.RequestDispatched)
	require.True(t, successful.Accounting.ReturnedPixelsKnown)
	require.Equal(t, uint64(8), successful.Pixels)
	require.Equal(t, "8", successful.Accounting.ServiceCharge.Numerator)
	require.Equal(t, "3", successful.Accounting.ServiceCharge.Denominator)
	require.Equal(t, "7", successful.Accounting.TicketCreditSent.Numerator)
	require.Equal(t, "2", successful.Accounting.TicketCreditSent.Denominator)
	rejected := seen[2]
	require.True(t, rejected.Accounting.RequestDispatched)
	require.Nil(t, rejected.Accounting.ServiceCharge)
	require.NotNil(t, rejected.Accounting.TicketCreditSent, "submitted payment credit must survive a rejected response")
	require.False(t, rejected.Accounting.ReturnedPixelsKnown)
	preflight := seen[3]
	require.False(t, preflight.Accounting.RequestDispatched)
	require.Nil(t, preflight.Accounting.ServiceCharge)
	require.Nil(t, preflight.Accounting.TicketCreditSent)
}

// An offchain orchestrator started with -pricePerUnit quotes its price through
// the same OrchestratorInfo an onchain one does; the offchain gateway has no
// ticket sender, so it charges the returned pixels at that price and reports a
// known zero ticket credit with no tickets.
func TestSubmitSegmentChargesOffchainQuotedPriceWithZeroTicketCredit(t *testing.T) {
	queue := frameworksTestSpool(t)
	oldStorage := drivers.NodeStorage
	drivers.NodeStorage = drivers.NewMemoryDriver(nil)
	t.Cleanup(func() { drivers.NodeStorage = oldStorage })

	node, err := core.NewLivepeerNode(nil, t.TempDir(), nil)
	require.NoError(t, err)
	// -pricePerUnit=1500 -pixelsPerUnit=1000
	node.SetBasePrice("default", core.NewFixedPrice(big.NewRat(1500, 1000)))
	node.QuoteOffchainPrice = true
	ts, mux := stubTLSServer()
	defer ts.Close()
	serviceURI, err := url.Parse(ts.URL)
	require.NoError(t, err)
	node.SetServiceURI(serviceURI)
	// The gateway requests orchestrator info with its transcoding capabilities.
	info, err := orchestratorInfoWithCaps(core.NewOrchestrator(node, nil), ethcommon.Address{}, ts.URL, "", &net.Capabilities{})
	require.NoError(t, err)
	require.Equal(t, int64(3), info.PriceInfo.GetPricePerUnit())
	require.Equal(t, int64(2), info.PriceInfo.GetPixelsPerUnit())
	require.Nil(t, info.TicketParams)

	body, err := wireproto.Marshal(&net.TranscodeResult{Result: &net.TranscodeResult_Data{Data: &net.TranscodeData{Segments: []*net.TranscodedSegmentData{{Url: "first", Pixels: 3}, {Url: "second", Pixels: 5}}}}})
	require.NoError(t, err)
	var payment atomic.Value
	mux.HandleFunc("/segment", func(w http.ResponseWriter, r *http.Request) {
		payment.Store(r.Header.Get(paymentHeader))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	sess := &BroadcastSession{Broadcaster: stubBroadcaster2(), Params: &core.StreamParameters{ManifestID: core.RandomManifestID(), SessionID: "test-session", TenantID: "22222222-2222-4222-8222-222222222222"}, OrchestratorInfo: info}
	result, err := SubmitSegment(context.Background(), sess, &stream.HLSSegment{Data: []byte("segment"), Duration: 2, SeqNo: 1}, nil, 0, true)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "", payment.Load(), "an offchain gateway sends no payment")

	records, err := queue.Pending(10)
	require.NoError(t, err)
	require.Len(t, records, 1)
	var event pb.GatewayTelemetryEvent
	require.NoError(t, wireproto.Unmarshal(records[0].Payload, &event))
	outcome := event.GetTranscode()
	require.Equal(t, uint64(8), outcome.Pixels)
	require.True(t, outcome.Accounting.RequestDispatched)
	require.True(t, outcome.Accounting.ReturnedPixelsKnown)
	// 8 returned pixels at 1500 wei per 1000 pixels.
	require.Equal(t, "12", outcome.Accounting.ServiceCharge.Numerator)
	require.Equal(t, "1", outcome.Accounting.ServiceCharge.Denominator)
	require.Equal(t, "wei", outcome.Accounting.ServiceCharge.Unit)
	require.NotNil(t, outcome.Accounting.TicketCreditSent, "zero ticket credit is known, not missing")
	require.Equal(t, "0", outcome.Accounting.TicketCreditSent.Numerator)
	require.Zero(t, outcome.Accounting.TicketsSent)
}
