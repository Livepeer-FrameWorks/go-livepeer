package frameworks

import (
	"context"
	"fmt"
	"github.com/Livepeer-FrameWorks/monorepo/pkg/clients/networkobserver"
	obs "github.com/Livepeer-FrameWorks/monorepo/pkg/networkobservation"
	"github.com/golang/glog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

var feedState struct {
	sync.RWMutex
	feed                       *obs.CandidateFeed
	mode, network, environment string
}

func initCandidates(ctx context.Context) error {
	base := strings.TrimSpace(os.Getenv("FRAMEWORKS_NETWORK_OBSERVER_URL"))
	if base == "" {
		return nil
	}
	mode := os.Getenv("FRAMEWORKS_CANDIDATE_FEED_MODE")
	if mode == "" {
		mode = "shadow"
	}
	if mode != "shadow" && mode != "supplement" {
		return fmt.Errorf("candidate feed mode must be shadow or supplement")
	}
	client, err := networkobserver.New(base, os.Getenv("FRAMEWORKS_NETWORK_OBSERVER_SECRET"), os.Getenv("FRAMEWORKS_NETWORK_OBSERVER_ALLOW_HTTP") == "true")
	if err != nil {
		return err
	}
	network := os.Getenv("FRAMEWORKS_NETWORK_ID")
	if network == "" {
		network = "42161"
	}
	environment := os.Getenv("FRAMEWORKS_ENVIRONMENT")
	if environment == "" {
		environment = "production"
	}
	feedState.Lock()
	feedState.mode = mode
	feedState.network = network
	feedState.environment = environment
	feedState.Unlock()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			feed, err := client.Candidates(ctx)
			if err != nil {
				glog.V(2).Infof("frameworks candidate feed unavailable: %v", err)
			} else if validFeed(feed, network, environment, time.Now()) {
				feedState.Lock()
				feedState.feed = feed
				feedState.Unlock()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return nil
}
func validFeed(feed *obs.CandidateFeed, network, environment string, now time.Time) bool {
	return feed != nil && feed.Version == 1 && feed.Provider == "livepeer" && feed.Network == network && feed.Environment == environment && !feed.GeneratedAt.After(now.Add(time.Minute)) && now.Sub(feed.GeneratedAt) <= 5*time.Minute && feed.ExpiresAt.After(now) && feed.ExpiresAt.Sub(feed.GeneratedAt) <= 5*time.Minute && len(feed.Candidates) <= 10000
}

// SupplementURLs retains the gateway's complete local set. Shared candidates are hints;
// local onchain eligibility, ticket validation, pricing and pool policy still decide admission.
func SupplementURLs(local []*url.URL, allowedWallets map[string]bool) []*url.URL {
	feedState.RLock()
	feed, mode, network, environment := feedState.feed, feedState.mode, feedState.network, feedState.environment
	feedState.RUnlock()
	if !validFeed(feed, network, environment, time.Now()) {
		return local
	}
	seen := map[string]bool{}
	for _, u := range local {
		seen[u.String()] = true
	}
	out := append([]*url.URL(nil), local...)
	for _, p := range feed.Candidates {
		if !p.Resource.Active || !allowedWallets[strings.ToLower(p.Resource.ID)] {
			continue
		}
		u, err := url.Parse(p.Resource.URI)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || seen[u.String()] {
			continue
		}
		if mode == "supplement" {
			out = append(out, u)
			seen[u.String()] = true
		}
	}
	return out
}
