package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/compact"
	contextusage "goodkind.io/clyde/internal/providers/claude/contextusage"
	"goodkind.io/clyde/internal/session"
)

func TestSessionSummaryUsesCachedContextUsageWithinTTL(t *testing.T) {
	store, sess := contextUsageTestStore(t, "chat-1", "uuid-1", "one\n")
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		probeCalls.Add(1)
		return contextUsageTestState(123), nil
	})

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("refresh context usage: %v", err)
	}
	first := srv.sessionSummary(context.Background(), store, sess)
	second := srv.sessionSummary(context.Background(), store, sess)

	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls = %d want 1", probeCalls.Load())
	}
	if !first.GetContextUsageLoaded() || !second.GetContextUsageLoaded() {
		t.Fatalf("summaries did not return loaded cached context usage")
	}
	if first.GetContextTotalTokens() != 123 || second.GetContextTotalTokens() != 123 {
		t.Fatalf("context totals = %d/%d want 123/123", first.GetContextTotalTokens(), second.GetContextTotalTokens())
	}
}

func TestContextUsageCacheInvalidatesOnTranscriptFreshnessChange(t *testing.T) {
	_, sess := contextUsageTestStore(t, "chat-2", "uuid-2", "one\n")
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		call := probeCalls.Add(1)
		return contextUsageTestState(int(call * 100)), nil
	})

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("refresh context usage: %v", err)
	}
	if state := srv.contextStateForSession(context.Background(), sess); !state.Loaded || state.Usage.TotalTokens != 100 {
		t.Fatalf("initial state loaded=%v total=%d want loaded total 100", state.Loaded, state.Usage.TotalTokens)
	}

	transcriptPath := sess.Metadata.ProviderTranscriptPath()
	if err := os.WriteFile(transcriptPath, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(transcriptPath, future, future); err != nil {
		t.Fatalf("touch transcript: %v", err)
	}

	if state := srv.contextStateForSession(context.Background(), sess); state.Loaded {
		t.Fatalf("stale transcript state loaded=%v want false", state.Loaded)
	}
	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("refresh context usage after transcript change: %v", err)
	}
	if probeCalls.Load() != 2 {
		t.Fatalf("probe calls = %d want 2", probeCalls.Load())
	}
}

func TestContextUsageRefreshCoalescesConcurrentStaleKey(t *testing.T) {
	_, sess := contextUsageTestStore(t, "chat-3", "uuid-3", "one\n")
	started := make(chan struct{})
	release := make(chan struct{})
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(ctx context.Context, _ *session.Session) (sessionContextState, error) {
		if probeCalls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return sessionContextState{}, ctx.Err()
		case <-release:
			return contextUsageTestState(456), nil
		}
	})

	const goroutineCount = 12
	errs := make(chan error, goroutineCount)
	var wg sync.WaitGroup
	for i := 0; i < goroutineCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, err := srv.refreshContextUsageState(context.Background(), sess)
			if err != nil {
				errs <- err
				return
			}
			if !state.Loaded || state.Usage.TotalTokens != 456 {
				errs <- errUnexpectedContextUsageState
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh returned error: %v", err)
		}
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls = %d want 1", probeCalls.Load())
	}
}

func TestSessionSummariesDoNotFanOutContextProbesForManyStaleSessions(t *testing.T) {
	store, sessions := contextUsageManyTestSessions(t, 40)
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		probeCalls.Add(1)
		return contextUsageTestState(789), nil
	})

	for _, sess := range sessions {
		summary := srv.sessionSummary(context.Background(), store, sess)
		if summary.GetContextUsageLoaded() {
			t.Fatalf("summary for %q loaded context usage without explicit refresh", sess.Name)
		}
	}
	if probeCalls.Load() != 0 {
		t.Fatalf("probe calls = %d want 0", probeCalls.Load())
	}
}

var errUnexpectedContextUsageState = errors.New("unexpected context usage state")

func newContextUsageTestServer(probe contextUsageProbeFunc) *Server {
	return &Server{
		log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		bridges:           make(map[string]*clydev1.Bridge),
		contextStates:     make(map[string]sessionContextState),
		contextRefreshSem: make(chan contextRefreshPermit, 2),
		contextUsageCache: newContextUsageStateCache(30 * time.Second),
		contextUsageProbe: probe,
	}
}

func contextUsageTestStore(t *testing.T, name, sessionID, transcript string) (*session.FileStore, *session.Session) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	transcriptPath := filepath.Join(tmp, name+".jsonl")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	sess := session.NewSession(name, sessionID)
	sess.Metadata.TranscriptPath = transcriptPath
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return store, sess
}

func contextUsageManyTestSessions(t *testing.T, count int) (*session.FileStore, []*session.Session) {
	t.Helper()
	store, first := contextUsageTestStore(t, "chat-0", "uuid-0", "zero\n")
	sessions := []*session.Session{first}
	for i := 1; i < count; i++ {
		name := "chat-" + time.Duration(i).String()
		sessionID := "uuid-" + time.Duration(i).String()
		transcriptPath := filepath.Join(t.TempDir(), name+".jsonl")
		if err := os.WriteFile(transcriptPath, []byte("one\n"), 0o600); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
		sess := session.NewSession(name, sessionID)
		sess.Metadata.TranscriptPath = transcriptPath
		if err := store.Create(sess); err != nil {
			t.Fatalf("create session: %v", err)
		}
		sessions = append(sessions, sess)
	}
	return store, sessions
}

func contextUsageTestState(totalTokens int) sessionContextState {
	return sessionContextState{
		Usage: contextusage.Usage{
			ContextUsage: compact.ContextUsage{
				Model:       "claude-sonnet-4-5",
				TotalTokens: totalTokens,
				MaxTokens:   200000,
				Percentage:  12,
				Categories: []compact.ContextCategory{
					{Name: "Messages", Tokens: totalTokens / 2},
				},
			},
			CapturedAt: time.Now().UTC(),
			Source:     contextusage.SourceProbe,
		},
		Loaded: true,
	}
}
