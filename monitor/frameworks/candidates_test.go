package frameworks

import (
	obs "github.com/Livepeer-FrameWorks/monorepo/pkg/networkobservation"
	"net/url"
	"testing"
	"time"
)

func TestCandidateFeedNeverErasesLocalFallbackAndValidatesScope(t *testing.T) {
	u, _ := url.Parse("https://local.example")
	local := []*url.URL{u}
	now := time.Now()
	feedState.feed = &obs.CandidateFeed{Version: 1, Provider: "livepeer", Network: "42161", Environment: "production", GeneratedAt: now, ExpiresAt: now.Add(time.Minute)}
	feedState.mode = "supplement"
	feedState.network = "42161"
	feedState.environment = "production"
	defer func() { feedState.feed = nil }()
	if got := SupplementURLs(local, nil); len(got) != 1 || got[0] != u {
		t.Fatal("empty feed erased local candidates")
	}
	feedState.feed.Candidates = []obs.Endpoint{{Resource: obs.Resource{ID: "0xabc", URI: "https://extra.example", Active: true}}}
	if got := SupplementURLs(local, map[string]bool{"0xabc": true}); len(got) != 2 {
		t.Fatal("valid supplementary candidate omitted")
	}
	feedState.feed.Environment = "staging"
	if got := SupplementURLs(local, map[string]bool{"0xabc": true}); len(got) != 1 {
		t.Fatal("cross-environment candidate admitted")
	}
	feedState.feed.Environment = "production"
	feedState.feed.ExpiresAt = now.Add(-time.Second)
	if got := SupplementURLs(local, map[string]bool{"0xabc": true}); len(got) != 1 {
		t.Fatal("stale feed admitted")
	}
}
