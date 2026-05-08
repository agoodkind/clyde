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

// TestContextUsageCacheStableAcrossModTimeOnlyChange exercises the
// rule that mod time alone does not invalidate the cache. The
// transcript file size is unchanged, so the cached entry should
// continue to satisfy lookups even after a touch.
func TestContextUsageCacheStableAcrossModTimeOnlyChange(t *testing.T) {
	_, sess := contextUsageTestStore(t, "chat-mt", "uuid-mt", "one\n")
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		probeCalls.Add(1)
		return contextUsageTestState(321), nil
	})

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}

	transcriptPath := sess.Metadata.ProviderTranscriptPath()
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(transcriptPath, future, future); err != nil {
		t.Fatalf("touch transcript: %v", err)
	}

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("post-touch refresh: %v", err)
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls = %d want 1 after mod-time-only change", probeCalls.Load())
	}
	state := srv.contextStateForSession(context.Background(), sess)
	if !state.Loaded || state.Usage.TotalTokens != 321 {
		t.Fatalf("post-touch state loaded=%v total=%d want loaded total 321", state.Loaded, state.Usage.TotalTokens)
	}
}

// TestContextUsageCacheInvalidatesWhenTranscriptGrows asserts that
// appending bytes to the transcript shifts the cache key and triggers
// exactly one fresh probe. Append-only JSONL guarantees this signal.
func TestContextUsageCacheInvalidatesWhenTranscriptGrows(t *testing.T) {
	_, sess := contextUsageTestStore(t, "chat-grow", "uuid-grow", "one\n")
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		call := probeCalls.Add(1)
		return contextUsageTestState(int(call * 100)), nil
	})

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	transcriptPath := sess.Metadata.ProviderTranscriptPath()
	if err := os.WriteFile(transcriptPath, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatalf("grow transcript: %v", err)
	}
	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("post-grow refresh: %v", err)
	}
	if probeCalls.Load() != 2 {
		t.Fatalf("probe calls = %d want 2 after size growth", probeCalls.Load())
	}
}

// TestContextUsageCacheCooldownAfterFailedProbe exercises the
// per-session failure cooldown. A failed probe must not be retried
// within the cooldown window so a hot loop cannot re-spawn after a
// transient probe error.
func TestContextUsageCacheCooldownAfterFailedProbe(t *testing.T) {
	_, sess := contextUsageTestStore(t, "chat-cool", "uuid-cool", "one\n")
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		probeCalls.Add(1)
		return sessionContextState{}, errUnexpectedContextUsageState
	})

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err == nil {
		t.Fatalf("first refresh: want error, got nil")
	}
	state, err := srv.refreshContextUsageState(context.Background(), sess)
	if !errors.Is(err, errContextUsageCooldown) {
		t.Fatalf("second refresh err=%v want %v", err, errContextUsageCooldown)
	}
	if state.Status != "cooldown" {
		t.Fatalf("cooldown state status=%q want %q", state.Status, "cooldown")
	}
	if probeCalls.Load() != 1 {
		t.Fatalf("probe calls = %d want 1 (second call gated by cooldown)", probeCalls.Load())
	}
}

// TestContextUsageCacheSweepsStaleSameSessionEntries verifies that
// growing a transcript and re-probing leaves only the newest entry
// for that session. Append-only transcripts grow monotonically so
// older size-keyed entries are obsolete and should not accumulate.
func TestContextUsageCacheSweepsStaleSameSessionEntries(t *testing.T) {
	_, sess := contextUsageTestStore(t, "chat-sweep", "uuid-sweep", "one\n")
	var probeCalls atomic.Int32
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		call := probeCalls.Add(1)
		return contextUsageTestState(int(call * 100)), nil
	})

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	transcriptPath := sess.Metadata.ProviderTranscriptPath()
	if err := os.WriteFile(transcriptPath, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatalf("grow transcript: %v", err)
	}
	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("second refresh: %v", err)
	}

	cache := srv.contextUsageStateCache()
	cache.mu.Lock()
	count := 0
	for key := range cache.states {
		if key.SessionName == sess.Name {
			count++
		}
	}
	cache.mu.Unlock()
	if count != 1 {
		t.Fatalf("entries for session = %d want 1 after sweep", count)
	}
}

// TestSessionDetailReturnsCachedContextUsageWhenWarm asserts that the
// daemon GetSessionDetail RPC carries the four numeric context fields
// plus ContextUsageLoaded=true once the cache is warm. The TUI relies
// on these to replace the persistent "loading..." placeholder.
func TestSessionDetailReturnsCachedContextUsageWhenWarm(t *testing.T) {
	store, sess := contextUsageTestStore(t, "chat-detail-warm", "uuid-detail-warm", "one\n")
	srv := newContextUsageTestServer(func(_ context.Context, _ *session.Session) (sessionContextState, error) {
		return contextUsageTestState(1234), nil
	})

	if _, err := srv.refreshContextUsageState(context.Background(), sess); err != nil {
		t.Fatalf("warm cache: %v", err)
	}

	resp := srv.sessionDetail(context.Background(), store, sess)
	if !resp.GetContextUsageLoaded() {
		t.Fatalf("detail.ContextUsageLoaded=false want true after warm cache")
	}
	if resp.GetContextTotalTokens() != 1234 {
		t.Fatalf("detail.ContextTotalTokens=%d want 1234", resp.GetContextTotalTokens())
	}
	if resp.GetContextMaxTokens() != 200000 {
		t.Fatalf("detail.ContextMaxTokens=%d want 200000", resp.GetContextMaxTokens())
	}
	if resp.GetContextMessagesTokens() != 1234/2 {
		t.Fatalf("detail.ContextMessagesTokens=%d want %d", resp.GetContextMessagesTokens(), 1234/2)
	}
	if resp.GetContextUsageStatus() != "" {
		t.Fatalf("detail.ContextUsageStatus=%q want empty", resp.GetContextUsageStatus())
	}
}

// TestSessionDetailReturnsProbingPlaceholderWhenColdAndProbeBlocks
// verifies that a cold-cache detail call returns within the wait
// budget with Status="probing" rather than blocking on the probe. The
// probe keeps running in the background; subsequent detail calls hit
// the warm cache. This is the lazy single-flight behavior that fixes
// the persistent "loading..." regression without reactivating the S0
// hot-loop.
func TestSessionDetailReturnsProbingPlaceholderWhenColdAndProbeBlocks(t *testing.T) {
	store, sess := contextUsageTestStore(t, "chat-detail-cold", "uuid-detail-cold", "one\n")
	releaseProbe := make(chan struct{})
	probeStarted := make(chan struct{}, 1)
	srv := newContextUsageTestServer(func(ctx context.Context, _ *session.Session) (sessionContextState, error) {
		select {
		case probeStarted <- struct{}{}:
		default:
		}
		select {
		case <-releaseProbe:
			return contextUsageTestState(987), nil
		case <-ctx.Done():
			return sessionContextState{}, ctx.Err()
		}
	})

	// Shrink the wait budget for the test so we do not pay the
	// production budget for a synthetic "always-blocks" probe.
	origBudget := serverDetailLazyProbeWaitBudgetForTest
	serverDetailLazyProbeWaitBudgetForTest = 50 * time.Millisecond
	t.Cleanup(func() { serverDetailLazyProbeWaitBudgetForTest = origBudget })

	resp := srv.sessionDetail(context.Background(), store, sess)
	if resp.GetContextUsageLoaded() {
		t.Fatalf("cold detail.ContextUsageLoaded=true want false")
	}
	if resp.GetContextUsageStatus() != "probing" {
		t.Fatalf("cold detail.ContextUsageStatus=%q want %q", resp.GetContextUsageStatus(), "probing")
	}

	// Confirm the background probe was started exactly once and
	// release it so the test does not leak the goroutine.
	select {
	case <-probeStarted:
	case <-time.After(time.Second):
		t.Fatalf("background probe did not start within 1s")
	}
	close(releaseProbe)
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
