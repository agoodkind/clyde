package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
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

// blockingSemanticConnector stalls inside a connection attempt until that
// attempt's context is cancelled, standing in for a production dial or
// collection registration that hangs against a wedged engine. attemptStarted
// closes once an attempt is in flight, and cancelObserved closes once the
// in-flight attempt has seen its context cancelled.
type blockingSemanticConnector struct {
	attemptStarted chan struct{}
	cancelObserved chan struct{}
	startOnce      sync.Once
	observeOnce    sync.Once
}

func newBlockingSemanticConnector() *blockingSemanticConnector {
	return &blockingSemanticConnector{
		attemptStarted: make(chan struct{}),
		cancelObserved: make(chan struct{}),
		startOnce:      sync.Once{},
		observeOnce:    sync.Once{},
	}
}

func (c *blockingSemanticConnector) connect(ctx context.Context) (semanticConnection, error) {
	c.startOnce.Do(func() { close(c.attemptStarted) })
	<-ctx.Done()
	c.observeOnce.Do(func() { close(c.cancelObserved) })
	return semanticConnection{search: nil, feeder: nil, connCloser: nil, close: nil},
		fmt.Errorf("dial semantic search daemon: %w", ctx.Err())
}

// awaitSignal blocks until signal closes, failing the test with description if
// it does not close within the test's patience window.
func awaitSignal(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(description)
	}
}

// assertClosed fails the test with description unless signal is already closed,
// so an assertion made right after Quiesce returns cannot pass by waiting.
func assertClosed(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	default:
		t.Fatal(description)
	}
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
		records: map[string]conversation.Record{"claude:one": daemonTestRecord("claude:one", false)},
	}
	// index stays nil: the source resolves records through its narrow index, and
	// the response mapper only consults the full index for fork lineage.
	srv := &controlServer{
		searchSource: &semanticConversationSearchSource{
			index: idx,
			searchClient: func() conversationSemanticSearchClient {
				return runtime.currentSearchClient()
			},
			collectionID: "conversations",
		},
	}
	req := &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10}

	_, err := srv.SearchConversations(ctx, req)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code before recovery = %v (err %v), want FailedPrecondition", status.Code(err), err)
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

// TestConversationSemanticRetryWorkerStopsAnInFlightAttemptBeforeRegistryDrain
// proves the registration retry worker is owned by the lifecycle group while it
// is doing work, not only while it sleeps between attempts. The connector stalls
// inside a dial-and-register attempt until its context is cancelled, so the
// worker is provably mid-attempt when the drain starts. The group is quiesced
// with the daemon context still live, and by the time Quiesce returns the
// attempt has observed cancellation and the worker has returned, so nothing can
// dial or register after the drain boundary.
//
// The blocked attempt is the point. A regression that stopped passing the
// worker context down into the connector would leave a stalled dial running
// past the drain, and the worker would only be joined at the drain's cap.
func TestConversationSemanticRetryWorkerStopsAnInFlightAttemptBeforeRegistryDrain(t *testing.T) {
	t.Parallel()
	connector := newBlockingSemanticConnector()
	runtime, group := newTestSemanticRuntime(t, connector.connect)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if !runtime.startRetryWorker(ctx, group) {
		t.Fatal("the retry worker must start when the lifecycle group owns its stop")
	}
	done := runtime.retryDone
	if done == nil {
		t.Fatal("starting the retry worker must publish a completion channel")
	}
	awaitSignal(t, connector.attemptStarted, "the retry worker never started a connection attempt")

	group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0})

	if ctx.Err() != nil {
		t.Fatal("daemon context must stay uncancelled so only the group stops the worker")
	}
	assertClosed(t, connector.cancelObserved, "the in-flight connection attempt never observed cancellation")
	assertClosed(t, done, "retry worker was still running after Quiesce returned")
}

// TestConversationSemanticRetryWorkerRefusesToStartWithoutALifecycleOwner proves
// the retry loop never runs as an unowned goroutine: with no lifecycle group
// there is nothing to install the stop hook on, so no worker starts and no
// attempt is made.
func TestConversationSemanticRetryWorkerRefusesToStartWithoutALifecycleOwner(t *testing.T) {
	t.Parallel()
	connector := newBlockingSemanticConnector()
	runtime, _ := newTestSemanticRuntime(t, connector.connect)

	if runtime.startRetryWorker(context.Background(), nil) {
		t.Fatal("the retry worker must not start without a lifecycle group to own its stop")
	}
	if runtime.retryDone != nil {
		t.Fatal("a refused start must publish no completion channel")
	}
	select {
	case <-connector.attemptStarted:
		t.Fatal("a retry worker dialed the engine without an installed stop hook")
	case <-time.After(unownedWorkerObservationWindow):
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
	// is what lets it recover later. It runs on its own lifecycle group here so
	// draining it immediately keeps its background passes from interleaving with
	// the deterministic passes below, without draining the engine connection this
	// test still needs.
	feederGroup := newLifecycleGroup(semanticTestLogger())
	if !startConversationSemanticSync(ctx, semanticTestLogger(), index, runtime.syncClient, "collection-test", nil, feederGroup, semanticTestContentKinds()) {
		t.Fatal("the sync worker must start while the engine is unavailable so it can recover")
	}
	feederGroup.Quiesce(context.Background(), "test", livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0})

	worker := newConversationSemanticSyncWorker(index, runtime.syncClient, "collection-test", semanticTestLogger(), semanticTestContentKinds())
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

// unownedWorkerObservationWindow is how long a refused start is watched before
// concluding no background goroutine ran. A refused start launches nothing, so
// any observation at all within this window is a failure.
const unownedWorkerObservationWindow = 100 * time.Millisecond

// blockingConversationSemanticIndex parks the sync feeder inside its first pass
// until the worker context is cancelled, so a test can hold the feeder provably
// live across a drain boundary. listing closes once a pass has entered the
// index, and cancelled closes once that pass has observed cancellation.
type blockingConversationSemanticIndex struct {
	listing    chan struct{}
	cancelled  chan struct{}
	listOnce   sync.Once
	cancelOnce sync.Once
}

func newBlockingConversationSemanticIndex() *blockingConversationSemanticIndex {
	return &blockingConversationSemanticIndex{
		listing:    make(chan struct{}),
		cancelled:  make(chan struct{}),
		listOnce:   sync.Once{},
		cancelOnce: sync.Once{},
	}
}

func (idx *blockingConversationSemanticIndex) ListWithStamps(ctx context.Context) ([]conversation.StampedRecord, error) {
	idx.listOnce.Do(func() { close(idx.listing) })
	<-ctx.Done()
	idx.cancelOnce.Do(func() { close(idx.cancelled) })
	return nil, fmt.Errorf("list conversation records with stamps: %w", ctx.Err())
}

func (idx *blockingConversationSemanticIndex) LoadMessagesWithOptions(record conversation.Record, _ conversation.LoadOptions) ([]transcript.Message, error) {
	return nil, fmt.Errorf("blocking index loads no messages for %s", record.ID)
}

// TestConversationSemanticSyncFeederIsGroupOwnedFromTheMomentItStarts proves a
// drain that is already underway cannot finish while the feeder runs. The feeder
// is started from inside a PhaseIngress hook, so the drain has begun but the
// workers phase has not yet snapshotted its hooks, and the hook returns only once
// the feeder is parked inside its first pass. Because the stop hook is installed
// before the goroutine launches, the workers phase picks it up, cancels the
// feeder, and joins it before Quiesce returns.
//
// Registering the stop after the start call returns is what this ordering
// removes: a hook added after the workers phase took its snapshot is skipped by
// that drain, and the feeder would still be writing to an engine connection the
// registry is about to close.
func TestConversationSemanticSyncFeederIsGroupOwnedFromTheMomentItStarts(t *testing.T) {
	t.Parallel()
	log := semanticTestLogger()
	group := newLifecycleGroup(log)
	index := newBlockingConversationSemanticIndex()
	engineFeeder := &fakeConversationSemanticClient{needed: nil}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan bool, 1)
	group.AddHookBefore(livetrack.PhaseIngress, "test.start_feeder_mid_drain", func(hookCtx context.Context) error {
		started <- startConversationSemanticSync(ctx, log, index, staticSemanticSyncClient(engineFeeder), "collection-test", nil, group, semanticTestContentKinds())
		select {
		case <-index.listing:
			return nil
		case <-hookCtx.Done():
			return fmt.Errorf("feeder never entered its first pass: %w", hookCtx.Err())
		}
	})

	group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0})

	if !<-started {
		t.Fatal("the feeder must start when the lifecycle group owns its stop")
	}
	if ctx.Err() != nil {
		t.Fatal("daemon context must stay uncancelled so only the group stops the feeder")
	}
	assertClosed(t, index.cancelled, "the feeder was still running after the workers phase drained")
}

// TestConversationSemanticSyncRefusesToStartWithoutALifecycleOwner proves the
// feeder never launches unowned: with no lifecycle group there is nothing to
// install the stop hook on, so no goroutine runs and no pass reads the index.
func TestConversationSemanticSyncRefusesToStartWithoutALifecycleOwner(t *testing.T) {
	t.Parallel()
	index := newBlockingConversationSemanticIndex()
	engineFeeder := &fakeConversationSemanticClient{needed: nil}

	if startConversationSemanticSync(context.Background(), semanticTestLogger(), index, staticSemanticSyncClient(engineFeeder), "collection-test", nil, nil, semanticTestContentKinds()) {
		t.Fatal("the feeder must not start without a lifecycle group to own its stop")
	}
	select {
	case <-index.listing:
		t.Fatal("a feeder goroutine ran without an installed stop hook")
	case <-time.After(unownedWorkerObservationWindow):
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
		searchSource: &semanticConversationSearchSource{
			index: conversation.NewIndex(newConversationRegistry(), config.ConversationConfig{}),
			searchClient: func() conversationSemanticSearchClient {
				return nil
			},
			collectionID: "conversations",
		},
	}
	_, err := srv.SearchConversations(context.Background(), &clydev1.SearchConversationsRequest{Query: "auth", Limit: 10})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("status code = %v (err %v), want FailedPrecondition", status.Code(err), err)
	}
}
