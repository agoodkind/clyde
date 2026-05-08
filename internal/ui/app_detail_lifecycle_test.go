package ui

import (
	"errors"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

// TestDetailLifecycle_SessionUpdatedWritesRecentlyUpdatedAt verifies that
// applySessionEvent records the wall clock time of a SESSION_UPDATED event
// in recentlyUpdatedAt so the row tinting in rowForLockedLastUsed has
// something to read.
func TestDetailLifecycle_SessionUpdatedWritesRecentlyUpdatedAt(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()

	name := a.sessions[0].Name
	updated := &session.Session{
		Name: name,
		Metadata: session.Metadata{
			Name:          name,
			SessionID:     "00000000-0000-0000-0000-000000000000",
			WorkspaceRoot: "/Users/test/Sites/ws-0",
			Created:       time.Now().Add(-time.Hour),
			LastAccessed:  time.Now(),
		},
	}

	before := a.now()
	a.applySessionEvent(SessionEvent{
		Kind:        "SESSION_UPDATED",
		SessionName: name,
		SessionID:   updated.Metadata.ProviderSessionID(),
		Session:     updated,
	})
	after := a.now()

	a.lastUsedMu.Lock()
	stamp, ok := a.recentlyUpdatedAt[name]
	a.lastUsedMu.Unlock()

	if !ok {
		t.Fatalf("recentlyUpdatedAt[%q] not written by SESSION_UPDATED", name)
	}
	if stamp.Before(before) || stamp.After(after) {
		t.Fatalf("recentlyUpdatedAt[%q] = %v, not within [%v, %v]", name, stamp, before, after)
	}
}

// TestDetailLifecycle_RenameDiscardsLateGoroutineCompletion verifies that a
// loadDetailAsync goroutine launched against the OLD name does not write
// to detailCache after a SESSION_RENAMED event arrives. The stub callback
// blocks until released, the test applies the rename event, then releases
// the callback so the late completion can race in.
func TestDetailLifecycle_RenameDiscardsLateGoroutineCompletion(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()

	oldName := a.sessions[0].Name
	newName := "renamed-session"

	gate := make(chan struct{})
	released := make(chan struct{})
	var done sync.WaitGroup
	done.Add(1)

	a.cb.GetSessionDetail = func(*session.Session) (SessionDetail, error) {
		defer done.Done()
		<-gate
		close(released)
		return SessionDetail{TranscriptStatsLoaded: true, TranscriptStatsStatus: "done"}, nil
	}

	// Kick off the async load against the OLD name.
	a.loadDetailAsync(a.sessions[0])

	// Apply the rename while the goroutine is parked inside the callback.
	renamed := &session.Session{
		Name: newName,
		Metadata: session.Metadata{
			Name:          newName,
			SessionID:     "00000000-0000-0000-0000-000000000000",
			WorkspaceRoot: "/Users/test/Sites/ws-0",
			Created:       time.Now().Add(-time.Hour),
			LastAccessed:  time.Now(),
		},
	}
	a.applySessionEvent(SessionEvent{
		Kind:        "SESSION_RENAMED",
		OldName:     oldName,
		SessionName: newName,
		SessionID:   renamed.Metadata.ProviderSessionID(),
		Session:     renamed,
	})

	// Release the goroutine. It will call postInterrupt, but since the
	// app event loop is not running, we drive handleDetailsLoaded by
	// reaching into the captured generation directly.
	close(gate)
	<-released
	done.Wait()

	// Simulate the event-loop dispatch: build the detailsLoaded payload
	// the goroutine would post. The captured generation matches the gen
	// the loadDetailAsync stamped on the OLD name BEFORE the rename
	// bumped it, so handleDetailsLoaded must drop the result.
	stale := detailsLoaded{
		name:   oldName,
		gen:    1,
		detail: SessionDetail{TranscriptStatsLoaded: true, TranscriptStatsStatus: "done"},
	}
	a.handleDetailsLoaded(stale)

	a.detailMu.Lock()
	_, present := a.detailCache[oldName]
	a.detailMu.Unlock()
	if present {
		t.Fatalf("detailCache[%q] resurrected after rename; expected stale completion to be dropped", oldName)
	}
}

// TestDetailLifecycle_ErrorPreservesPriorTranscriptStatsLoaded verifies
// that handleDetailsLoaded keeps the cached TranscriptStatsLoaded=true
// when a follow-up RPC returns an error. A transient daemon hiccup
// after a successful prior load must not flicker the panel back to
// "loading...".
func TestDetailLifecycle_ErrorPreservesPriorTranscriptStatsLoaded(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()

	name := a.sessions[0].Name

	// Seed a successful prior load through the public path so detailGen
	// is incremented in step.
	prior := SessionDetail{TranscriptStatsLoaded: true, TranscriptStatsStatus: "ok"}
	a.detailMu.Lock()
	a.detailGen[name]++
	priorGen := a.detailGen[name]
	a.detailCache[name] = prior
	a.detailMu.Unlock()

	// A follow-up async load bumps gen and fails.
	a.detailMu.Lock()
	a.detailGen[name]++
	failGen := a.detailGen[name]
	a.detailLoading[name] = true
	a.detailMu.Unlock()

	if failGen == priorGen {
		t.Fatalf("expected new gen to differ from prior; both = %d", failGen)
	}

	a.handleDetailsLoaded(detailsLoaded{
		name:   name,
		gen:    failGen,
		detail: SessionDetail{},
		err:    errors.New("boom"),
	})

	a.detailMu.Lock()
	got, ok := a.detailCache[name]
	a.detailMu.Unlock()
	if !ok {
		t.Fatalf("detailCache[%q] missing after error fallback", name)
	}
	if !got.TranscriptStatsLoaded {
		t.Fatalf("TranscriptStatsLoaded = false; want true preserved from prior cache")
	}
	if got.TranscriptStatsStatus == "" {
		t.Fatalf("TranscriptStatsStatus blanked; want a non-empty failure message")
	}
}
