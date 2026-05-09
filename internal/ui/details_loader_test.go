package ui

import (
	"errors"
	"testing"

	"goodkind.io/clyde/internal/session"
)

// TestHandleDetailsLoaded_ErrorWithoutCacheResolvesToTerminalStatus
// covers the user report that the details pane shows a spinner that
// never resolves. When GetSessionDetail returns an error and there is
// no prior cache entry, both ContextUsageStatus and TranscriptStatsStatus
// must end up in a terminal state so detailHasPendingStatus stops
// returning true and the repoll loop stops.
func TestHandleDetailsLoaded_ErrorWithoutCacheResolvesToTerminalStatus(t *testing.T) {
	app := NewApp(nil, AppCallbacks{})
	sess := &session.Session{Name: "demo"}
	app.sessions = []*session.Session{sess}
	app.rebuildVisible()
	app.selected = sess

	app.handleDetailsLoaded(detailsLoaded{
		name:   "demo",
		detail: SessionDetail{},
		err:    errors.New("rpc error: detail probe refused"),
	})

	cached, ok := app.detailCache["demo"]
	if !ok {
		t.Fatal("expected handleDetailsLoaded to leave a cache entry on error")
	}
	if !isTerminalLoadingStatus(cached.TranscriptStatsStatus) {
		t.Fatalf("expected terminal TranscriptStatsStatus, got %q", cached.TranscriptStatsStatus)
	}
	if !isTerminalLoadingStatus(cached.ContextUsageStatus) {
		t.Fatalf("expected terminal ContextUsageStatus, got %q", cached.ContextUsageStatus)
	}
	if detailHasPendingStatus(cached) {
		t.Fatal("expected detailHasPendingStatus=false after terminal failure so repoll stops")
	}
}

// TestPopulateDetails_KeepsLoadedCachedContextWhenLiveStateUnloaded
// covers the "loader shows when data is already available" case. When
// the cached SessionDetail has ContextUsageLoaded=true (because
// GetSessionDetail returned full data), populateDetails must not
// override that with the per-session contextState if the live state
// has not loaded yet, otherwise the spinner reappears every draw.
func TestPopulateDetails_KeepsLoadedCachedContextWhenLiveStateUnloaded(t *testing.T) {
	app := NewApp(nil, AppCallbacks{
		GetSessionDetail: func(_ *session.Session) (SessionDetail, error) {
			return SessionDetail{}, nil
		},
	})
	sess := &session.Session{Name: "demo"}
	app.sessions = []*session.Session{sess}
	app.rebuildVisible()
	app.selected = sess

	// Cached detail has full context data already.
	app.detailMu.Lock()
	app.detailCache["demo"] = SessionDetail{
		ContextUsage: SessionContextUsage{
			TotalTokens: 1000,
			MaxTokens:   200000,
			Percentage:  1,
		},
		ContextUsageLoaded:    true,
		ContextUsageStatus:    "",
		TranscriptStatsLoaded: true,
		TranscriptStatsStatus: "",
	}
	app.detailMu.Unlock()

	// The per-session contextState has not finished loading yet.
	app.contextStateCache["demo"] = SessionContextState{
		Loaded: false,
		Status: "probing",
	}

	app.populateDetails()

	cached := app.detailCache["demo"]
	if !cached.ContextUsageLoaded {
		t.Fatal("expected cached ContextUsageLoaded to be preserved when live state is still probing")
	}
	if cached.ContextUsage.TotalTokens != 1000 {
		t.Fatalf("expected cached ContextUsage to be preserved, got %+v", cached.ContextUsage)
	}
}
