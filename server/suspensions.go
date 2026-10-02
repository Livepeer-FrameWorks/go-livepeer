package server

import (
	"sync"
	"time"
)

// suspender is a list that keep track of suspender orchestrators
// and the count until which they are suspended
type suspender struct {
	mu    sync.Mutex
	list  map[string]int // list of orchestrator => refresh count at which the orchestrator is no longer suspended
	count int
}

// newSuspender returns the pointer to a new Suspender instance
func newSuspender() *suspender {
	return &suspender{
		list: make(map[string]int),
	}
}

// suspend an orchestrator for 'penalty' refreshes
func (s *suspender) suspend(orch string, penalty int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.list[orch] += penalty
}

// unsuspend removes up to 'penalty' from an orchestrator's suspension
func (s *suspender) unsuspend(orch string, penalty int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.list[orch] <= penalty {
		delete(s.list, orch)
		return
	}
	s.list[orch] -= penalty
}

// sharedSuspended is always false: in-memory suspensions belong to one stream,
// whose pool already removed the suspended orchestrator.
func (s *suspender) sharedSuspended(orch string) bool { return false }

// Suspended returns a non-zero value if the orchestrator is suspended
// 'orch' is the service URI of the orchestrator
// The value returned is the suspension penalty associated with the orchestrator whereby lower is better
func (s *suspender) Suspended(orch string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.list[orch] < s.count {
		delete(s.list, orch)
	}
	return s.list[orch]
}

// signalRefresh increases Suspender.count
func (s *suspender) signalRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
}

// windowSuspender is the in-memory suspender of one transcoding session pool
// when no shared Redis store is configured. A suspension lasts window from the
// latest suspend, like a Redis suspension key's TTL, and penalties accumulate
// so discovery orders suspended orchestrators by them. Suspensions are not
// shared with other streams.
type windowSuspender struct {
	mu     sync.Mutex
	window time.Duration
	list   map[string]windowSuspension
}

type windowSuspension struct {
	penalty int
	until   time.Time
}

func newWindowSuspender(window time.Duration) *windowSuspender {
	return &windowSuspender{window: window, list: make(map[string]windowSuspension)}
}

func (s *windowSuspender) suspend(orch string, penalty int) {
	if penalty <= 0 {
		penalty = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.list[orch]
	if time.Now().After(cur.until) {
		cur.penalty = 0
	}
	s.list[orch] = windowSuspension{penalty: cur.penalty + penalty, until: time.Now().Add(s.window)}
}

func (s *windowSuspender) unsuspend(orch string, penalty int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.list[orch]
	if !ok {
		return
	}
	if cur.penalty <= penalty {
		delete(s.list, orch)
		return
	}
	cur.penalty -= penalty
	s.list[orch] = cur
}

// Suspended returns the accumulated penalty while the suspension window lasts
// and zero afterwards.
func (s *windowSuspender) Suspended(orch string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.list[orch]
	if !ok {
		return 0
	}
	if time.Now().After(cur.until) {
		delete(s.list, orch)
		return 0
	}
	return cur.penalty
}

// sharedSuspended is always false: the suspension belongs to one stream, whose
// pool already removed the suspended orchestrator.
func (s *windowSuspender) sharedSuspended(orch string) bool { return false }

// signalRefresh does nothing: the suspension window is measured in time, not
// in discovery refreshes.
func (s *windowSuspender) signalRefresh() {}
