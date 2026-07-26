package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/transcript"
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
	feeder                conversationSemanticClient
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
		feeder:     c.feeder,
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

// awaitSyncClient waits for background registration to publish the feeder
// surface, failing the test if it never does.
func awaitSyncClient(t *testing.T, runtime *conversationSemanticRuntime) conversationSemanticClient {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if client := runtime.syncClient(); client != nil {
			return client
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background registration never published a feeder client")
	return nil
}

// awaitConnectorAttempts waits until the connector has been called at least want
// times, proving the retry loop is running rather than merely started.
func awaitConnectorAttempts(t *testing.T, connector *flakySemanticConnector, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if connector.attemptCount() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("connector attempts = %d, want at least %d", connector.attemptCount(), want)
}

// TestControlServerSearchConversationsRecoversAfterFailedInitialRegister proves
// the boot-failure defect is fixed end to end through the RPC the client calls.
// One control server is built while the engine is down and is never rebuilt: its
// first SearchConversations fails fast with FailedPrecondition and no literal
// scan, the background retry then succeeds, and the same server answers the next
// identical request from the engine with no daemon reload in between.
//
// Building the server before the retry is the point. A regression that snapshots
// the client at construction instead of resolving it per query would still pass
// the first assertion and fail the second.
func TestControlServerSearchConversationsRecoversAfterFailedInitialRegister(t *testing.T) {
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
		feeder:                nil,
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
	// index stays nil: the search path reads searchIndex, and the response mapper
	// only consults the full index to resolve fork lineage, which is nil-safe.
	srv := &controlServer{
		searchIndex: idx,
		semanticSearch: func() conversationSemanticSearchClient {
			return runtime.currentSearchClient()
		},
		semanticCollectionID: "conversations",
		literalFallback:      true,
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	_, err := srv.SearchConversations(ctx, req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code before recovery = %v (err %v), want FailedPrecondition", status.Code(err), err)
	}
	if idx.liveCalls != 0 {
		t.Fatalf("live scan called %d times before recovery, want 0", idx.liveCalls)
	}

	runtime.startRetryWorker(ctx, group)
	awaitSearchClient(t, runtime)

	resp, err := srv.SearchConversations(ctx, req)
	if err != nil {
		t.Fatalf("search after recovery: %v", err)
	}
	if resp.GetSource() != clydev1.SearchSource_SEARCH_SOURCE_SEMANTIC {
		t.Fatalf("source after recovery = %v, want semantic", resp.GetSource())
	}
	if len(resp.GetMatches()) != 1 {
		t.Fatalf("matches after recovery = %d, want 1", len(resp.GetMatches()))
	}
	if got := resp.GetMatches()[0].GetConversation().GetId(); got != "claude:one" {
		t.Fatalf("match conversation = %q, want claude:one", got)
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

// alwaysFailingSemanticConnector makes flakySemanticConnector fail every
// attempt, standing in for an engine that never comes back.
const alwaysFailingSemanticConnector = math.MaxInt

// TestConversationSemanticRetryWorkerStopsBeforeRegistryDrain proves the
// registration retry worker is owned by the lifecycle group rather than running
// as an unowned goroutine. A permanently failing retry is left looping, the
// group is quiesced with the daemon context still live, and the worker has
// already returned by the time Quiesce returns, so nothing can dial or register
// after the drain boundary. Without the PhaseWorkers stop-hook the worker would
// still be sleeping or mid-attempt here.
func TestConversationSemanticRetryWorkerStopsBeforeRegistryDrain(t *testing.T) {
	t.Parallel()
	connector := &flakySemanticConnector{
		mu:                    sync.Mutex{},
		attempts:              0,
		failuresBeforeSuccess: alwaysFailingSemanticConnector,
		search:                nil,
		feeder:                nil,
		conn:                  &fakeSemanticConn{mu: sync.Mutex{}, closes: 0},
	}
	runtime, group := newTestSemanticRuntime(t, connector.connect)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := runtime.attemptRegister(ctx); err == nil {
		t.Fatal("expected the boot registration attempt to fail while the engine is down")
	}
	runtime.startRetryWorker(ctx, group)
	done := runtime.retryDone
	if done == nil {
		t.Fatal("starting the retry worker must publish a completion channel")
	}
	// Wait until the loop has retried at least once so the worker is provably
	// live (sleeping on backoff or inside an attempt) when the drain starts.
	awaitConnectorAttempts(t, connector, 2)

	group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0})

	if ctx.Err() != nil {
		t.Fatal("daemon context must stay uncancelled so only the group stops the worker")
	}
	select {
	case <-done:
	default:
		t.Fatal("retry worker was still running after Quiesce returned")
	}
}

// TestConversationSemanticSyncFeederRecoversAfterFailedInitialRegister proves
// the background sync feeder recovers on its own, the same way search does. The
// feeder is wired to the runtime resolver while the engine is down: it starts,
// its first pass delivers nothing and reads no transcript, and after the
// background registration succeeds the very next pass states the manifest and
// upserts the needed conversation, with no daemon reload in between.
//
// Before the fix the feeder resolved its client once at boot, so a boot-time
// engine outage left it dead until a manual reload.
func TestConversationSemanticSyncFeederRecoversAfterFailedInitialRegister(t *testing.T) {
	t.Parallel()
	conversationID := "codex:sync-recovery"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{Record: semanticTestRecord(conversationID), Stamp: semanticTestStamp(20, 200)}},
		messagesByID: map[string][]transcript.Message{
			conversationID: {
				{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "the auth timeout note"},
			},
		},
		loadOptions: nil,
	}
	engineFeeder := &fakeConversationSemanticClient{needed: []string{conversationID}}
	connector := &flakySemanticConnector{
		mu:                    sync.Mutex{},
		attempts:              0,
		failuresBeforeSuccess: 1,
		search:                nil,
		feeder:                engineFeeder,
		conn:                  &fakeSemanticConn{mu: sync.Mutex{}, closes: 0},
	}
	runtime, group := newTestSemanticRuntime(t, connector.connect)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := runtime.attemptRegister(ctx); err == nil {
		t.Fatal("expected the boot registration attempt to fail while the engine is down")
	}

	// The daemon starts the feeder even though the engine is unreachable, which
	// is what lets it recover later. Stop it immediately so its background passes
	// cannot interleave with the deterministic passes below.
	stop := startConversationSemanticSync(ctx, semanticTestLogger(), index, runtime.syncClient, "collection-test", nil)
	if stop == nil {
		t.Fatal("the sync worker must start while the engine is unavailable so it can recover")
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop sync worker: %v", err)
	}

	worker := newConversationSemanticSyncWorker(index, runtime.syncClient, "collection-test", semanticTestLogger())
	if err := worker.runPass(ctx); err != nil {
		t.Fatalf("pass while the engine is down: %v", err)
	}
	if len(engineFeeder.syncCalls) != 0 || len(engineFeeder.upsertCalls) != 0 {
		t.Fatalf("feeder called the engine while it was unavailable: sync=%d upsert=%d", len(engineFeeder.syncCalls), len(engineFeeder.upsertCalls))
	}
	if len(index.loadOptions) != 0 {
		t.Fatalf("feeder read %d transcripts while the engine was unavailable, want 0", len(index.loadOptions))
	}

	runtime.startRetryWorker(ctx, group)
	awaitSyncClient(t, runtime)

	if err := worker.runPass(ctx); err != nil {
		t.Fatalf("pass after recovery: %v", err)
	}
	if len(engineFeeder.syncCalls) != 1 {
		t.Fatalf("manifest sync calls after recovery = %d, want 1", len(engineFeeder.syncCalls))
	}
	if len(engineFeeder.upsertCalls) != 1 {
		t.Fatalf("upsert calls after recovery = %d, want 1", len(engineFeeder.upsertCalls))
	}
	upsert := engineFeeder.upsertCalls[0]
	if len(upsert.Docs) != 1 || upsert.Docs[0].ConversationID != conversationID {
		t.Fatalf("upserted docs = %+v, want one document for %s", upsert.Docs, conversationID)
	}

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
		feeder:                nil,
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
		searchIndex: conversation.NewIndex(newConversationRegistry()),
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
