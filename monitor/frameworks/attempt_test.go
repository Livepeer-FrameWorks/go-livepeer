package frameworks

import (
	"context"
	"testing"
)

func TestSocketAttributionDoesNotLeakBetweenConcurrentAttempts(t *testing.T) {
	a := WithAttempt(context.Background(), "")
	b := WithAttempt(context.Background(), "")
	SetRemoteIP(a, "192.0.2.1")
	SetRemoteIP(b, "192.0.2.2")
	if RemoteIP(a) != "192.0.2.1" || RemoteIP(b) != "192.0.2.2" {
		t.Fatal("socket attribution crossed requests")
	}
	SetRemoteIP(a, "hostname")
	if RemoteIP(a) != "192.0.2.1" {
		t.Fatal("accepted non-IP attribution")
	}
}
