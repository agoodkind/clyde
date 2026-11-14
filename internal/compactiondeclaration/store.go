// Package compactiondeclaration records single-use compaction declarations.
package compactiondeclaration

import (
	"sync"
	"time"
)

const lifetime = 60 * time.Second

// Store records declarations by canonical conversation ID.
type Store struct {
	mu      sync.Mutex
	now     func() time.Time
	expires map[string]time.Time
	closed  bool
}

// New constructs a declaration store with the supplied clock.
//
// The caller supplies the clock for expiry checks.
//
// Example:
//
//	store := compactiondeclaration.New(time.Now)
func New(now func() time.Time) *Store {
	return &Store{mu: sync.Mutex{}, now: now, expires: make(map[string]time.Time), closed: false}
}

// Declare authorizes one compaction for the canonical conversation ID.
//
// Each declaration expires after 60 seconds. Closed stores reject declarations.
//
// Example:
//
//	accepted := store.Declare("claude:session-id")
func (s *Store) Declare(conversationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	if s.closed || conversationID == "" {
		return false
	}

	s.expires[conversationID] = s.now().Add(lifetime)

	return true
}

// Pending reports whether a conversation has an unexpired declaration.
//
// This check does not consume the declaration.
//
// Example:
//
//	pending := store.Pending("claude:session-id")
func (s *Store) Pending(conversationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	_, pending := s.expires[conversationID]

	return !s.closed && pending
}

// Consume removes one unexpired declaration and reports whether it existed.
//
// Concurrent callers can consume each declaration only once.
//
// Example:
//
//	matched := store.Consume("claude:session-id")
func (s *Store) Consume(conversationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	_, pending := s.expires[conversationID]
	delete(s.expires, conversationID)

	return !s.closed && pending
}

// Close clears declarations and rejects subsequent declarations and matches.
//
// The daemon calls Close during lifecycle quiescence.
//
// Example:
//
//	err := store.Close()
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	clear(s.expires)

	return nil
}

func (s *Store) prune() {
	now := s.now()

	for conversationID, expiry := range s.expires {
		if !now.Before(expiry) {
			delete(s.expires, conversationID)
		}
	}
}
