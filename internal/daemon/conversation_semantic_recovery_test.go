package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/livetrack"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSemanticConn stands in for the engine grpc connection the livetrack
// registry owns, recording that a drain actually closed it.
type fakeSemanticConn struct {
	mu     sync.Mutex
	closes int
}

func (c *fakeSemanticConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return nil
}

func (c *fakeSemanticConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closes
}

// flakySemanticConnector reproduces an engine that is down when the daemon
// boots and comes back later: the first failuresBeforeSuccess attempts fail,
// every later attempt returns a ready connection.
type flakySemanticConnector struct {
	mu                    sync.Mutex
	attempts              int
	failuresBeforeSuccess int
	search                conversationSemanticSearchClient
	conn                  *fakeSemanticConn
}

func (c *flakySemanticConnector) connect(context.Context) (semanticConnection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if c.attempts <= c.failuresBeforeSuccess {
		return semanticConnection{search: nil, feeder: nil, connCloser: nil, close: nil}, errors.New("dial semantic search daemon: connection refused")
	}
	return semanticConnection{
		search:     c.search,
		feeder:     nil,
		connCloser: c.conn,
		close:      c.conn.Close,
	}, nil
}

func (c *flakySemanticConnector) attemptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts
}

// newTestSemanticRuntime builds a semantic runtime over the given connector,
// attached to its own lifecycle group exactly as the daemon attaches it, with a
// retry cadence fast enough for a test.
func newTestSemanticRuntime(t *testing.T, connector semanticConnector) (*conversationSemanticRuntime, *livetrack.Group) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	group := newLifecycleGroup(log)
	registry := livetrack.Attach[conversationSemanticConnectionMeta](group, livetrack.MemberSpec{
		Phase:         livetrack.PhaseWorkers,
		QuietRelevant: false,
		CancelNoWait:  false,
	}, livetrack.Options[conversationSemanticConnectionMeta]{
		Component:     "daemon",
		Concern:       "conversation.semantic",
		Log:           log,
		PollEvery:     0,
		CloserGrace:   0,
		ParallelClose: false,
		Now:           nil,
	})
	runtime := newConversationSemanticRuntime(log, connector, registry, conversationSemanticConnectionMeta{
		CollectionID: "conversations",
		SocketPath:   "/tmp/semantic-search-test.sock",
	})
	runtime.initialRetryBackoff = time.Millisecond
	runtime.maxRetryBackoff = 5 * time.Millisecond
	return runtime, group
}

// awaitSearchClient waits for background registration to publish a search
// client, failing the test if it never does.
func awaitSearchClient(t *testing.T, runtime *conversationSemanticRuntime) conversationSemanticSearchClient {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client := runtime.currentSearchClient(); client != nil {
			return client
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background registration never published a search client")
	return nil
}

// TestConversationSemanticRuntimeRecoversAfterFailedInitialRegister proves the
// boot-failure defect is fixed end to end at the seam the control server uses.
// The first registration fails, so search fails fast with no literal scan; the
// background retry then succeeds and the same resolver the control server holds
// starts answering from the engine, with no daemon reload in between.
//
// Before the fix this test would fail at the recovery step: a boot-time dial or
// register failure returned a nil runtime and search stayed dead for the life of
// the process.
func TestConversationSemanticRuntimeRecoversAfterFailedInitialRegister(t *testing.T) {
	t.Parallel()
	engine := &fakeSemanticSearch{
		hits: []semsearch.SemHit{
			{ConversationID: "claude:one", MessageIndex: 2, Role: "assistant", TimestampUnix: 7, Content: "the auth timeout note"},
		},
		err: nil,
	}
	connector := &flakySemanticConnector{
		mu:                    sync.Mutex{},
		attempts:              0,
		failuresBeforeSuccess: 1,
		search:                engine,
		conn:                  &fakeSemanticConn{mu: sync.Mutex{}, closes: 0},
	}
	runtime, group := newTestSemanticRuntime(t, connector.connect)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := runtime.attemptRegister(ctx); err == nil {
		t.Fatal("expected the boot registration attempt to fail while the engine is down")
	}
	if runtime.currentSearchClient() != nil {
		t.Fatal("search client must stay nil until registration succeeds")
	}

	idx := &fakeSearchIndex{
		records:   map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
		live:      conversation.SearchConversationsResult{},
		liveErr:   nil,
		liveCalls: 0,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}
	_, err := searchConversationsResult(ctx, idx, runtime.currentSearchClient(), true, "conversations", true, req)
	var unavailable semanticEngineUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error before recovery = %v, want semanticEngineUnavailableError", err)
	}
	if idx.liveCalls != 0 {
		t.Fatalf("live scan called %d times before recovery, want 0", idx.liveCalls)
	}

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("retry loop panicked: %v", recovered)
			}
		}()
		runtime.retryRegisterLoop(ctx)
	}()
	awaitSearchClient(t, runtime)

	result, err := searchConversationsResult(ctx, idx, runtime.currentSearchClient(), true, "conversations", true, req)
	if err != nil {
		t.Fatalf("search after recovery: %v", err)
	}
	if result.Source != conversation.SearchSourceSemantic {
		t.Fatalf("source after recovery = %v, want semantic", result.Source)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("matches after recovery = %d, want 1", len(result.Matches))
	}
	if idx.liveCalls != 0 {
		t.Fatalf("live scan called %d times after recovery, want 0", idx.liveCalls)
	}
	if connector.attemptCount() < 2 {
		t.Fatalf("connector attempts = %d, want at least 2 (boot plus retry)", connector.attemptCount())
	}

	// The recovered connection is livetrack-owned, so a group drain closes it.
	// The session is held for the daemon's life, so the drain force-closes it at
	// the budget cap; the cap here is short only to keep the test quick.
	group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: 250 * time.Millisecond, IdleGrace: 0})
	if connector.conn.closeCount() == 0 {
		t.Fatal("livetrack drain did not close the recovered engine connection")
	}
}

// TestConversationSemanticRuntimeRegisterIsIdempotent proves a redundant attempt
// after success neither redials nor replaces the live connection, so the lazy
// re-attempt on a later query cannot churn the engine link.
func TestConversationSemanticRuntimeRegisterIsIdempotent(t *testing.T) {
	t.Parallel()
	connector := &flakySemanticConnector{
		mu:                    sync.Mutex{},
		attempts:              0,
		failuresBeforeSuccess: 0,
		search:                &fakeSemanticSearch{hits: nil, err: nil},
		conn:                  &fakeSemanticConn{mu: sync.Mutex{}, closes: 0},
	}
	runtime, _ := newTestSemanticRuntime(t, connector.connect)
	ctx := context.Background()

	if err := runtime.attemptRegister(ctx); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := runtime.attemptRegister(ctx); err != nil {
		t.Fatalf("second registration: %v", err)
	}
	if connector.attemptCount() != 1 {
		t.Fatalf("connector attempts = %d, want 1", connector.attemptCount())
	}
}

// TestControlServerSearchConversationsUnreachableEngineFailsPrecondition proves
// the RPC boundary turns the unreachable-engine failure into a typed gRPC
// FailedPrecondition rather than an Internal error, a misleading Unavailable
// (which the client renders as "clyde daemon is not running"), or a hang.
func TestControlServerSearchConversationsUnreachableEngineFailsPrecondition(t *testing.T) {
	t.Parallel()
	srv := &controlServer{
		index: conversation.NewIndex(newConversationRegistry()),
		semanticSearch: func() conversationSemanticSearchClient {
			return nil
		},
		semanticCollectionID: "conversations",
		literalFallback:      true,
	}
	_, err := srv.SearchConversations(context.Background(), &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code = %v (err %v), want FailedPrecondition", status.Code(err), err)
	}
}
