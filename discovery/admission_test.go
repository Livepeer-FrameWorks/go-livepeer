package discovery

import (
	"context"
	"errors"
	"testing"

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
