package conversation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// TestReadCacheDropsStampsWrittenByAnOlderRecordShape pins the cache version to
// the record shape. Record gained LatestRequestID, and a cache whose stamps
// survive keeps its old records for every artifact that has not changed since,
// so those conversations would resolve no request id at all.
func TestReadCacheDropsStampsWrittenByAnOlderRecordShape(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), cacheFilename)
	records := []Record{{
		ID:              "cursor:chat",
		Provider:        ProviderCursor,
		NativeID:        "chat",
		Lineage:         nil,
		Origin:          OriginUnspecified,
		Title:           "chat",
		WorkspaceRoot:   "",
		ArtifactPath:    "/tmp/chat",
		ArtifactKind:    "composer",
		Model:           "",
		CreatedAt:       time.Time{},
		UpdatedAt:       time.Time{},
		SizeBytes:       0,
		Archived:        false,
		LatestRequestID: "",
	}}
	stamps := map[string]FileStamp{"/tmp/chat": {Size: 1, Mtime: time.Unix(0, 0).UTC()}}
	if err := writeCache(path, records, stamps); err != nil {
		t.Fatalf("writeCache returned error: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	aged := bytes.Replace(
		written,
		fmt.Appendf(nil, `"version": %d`, cacheFormatVersion),
		[]byte(`"version": 1`),
		1,
	)
	if err := os.WriteFile(path, aged, 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	gotRecords, gotStamps, err := readCache(path)
	if err != nil {
		t.Fatalf("readCache returned error: %v", err)
	}
	if len(gotRecords) != 1 {
		t.Fatalf("records = %d, want the older cache still serving reads", len(gotRecords))
	}
	if len(gotStamps) != 0 {
		t.Fatalf("stamps = %v, want them dropped so the next refresh re-derives the record", gotStamps)
	}
}

// TestRefreshWaitsForTheRefreshAlreadyInFlight pins the contract a synchronous
// Refresh carries. Every read path starts a debounced background refresh a moment
// before it asks for a synchronous one, so a Refresh that returns while that scan
// is still running hands the caller back the same snapshot it already failed
// against.
func TestRefreshWaitsForTheRefreshAlreadyInFlight(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	idx := &Index{
		mu:               sync.Mutex{},
		includeSubagents: false,
		registry:         NewRegistry(),
		records:          nil,
		prevRecords:      nil,
		prevStamps:       nil,
		loaded:           false,
		refreshing:       false,
		refreshRun:       nil,
		lastRefresh:      time.Time{},
		cachePath:        filepath.Join(t.TempDir(), cacheFilename),
		debounce:         time.Hour,
		scanProvider: func(context.Context, *Registry, scanCache) (scanResult, error) {
			startOnce.Do(func() { close(started) })
			<-release
			return scanResult{
				records: []Record{{
					ID:              "cursor:scanned",
					Provider:        ProviderCursor,
					NativeID:        "scanned",
					Lineage:         nil,
					Origin:          OriginUnspecified,
					Title:           "scanned",
					WorkspaceRoot:   "",
					ArtifactPath:    "/tmp/scanned",
					ArtifactKind:    "composer",
					Model:           "",
					CreatedAt:       time.Time{},
					UpdatedAt:       time.Time{},
					SizeBytes:       0,
					Archived:        false,
					LatestRequestID: "",
				}},
				stamps: nil,
			}, nil
		},
	}

	// List starts the background refresh, which is what leaves one in flight.
	if _, err := idx.List(context.Background()); err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	<-started

	type outcome struct {
		err     error
		records []Record
	}
	refreshed := make(chan outcome, 1)
	go func() {
		err := idx.Refresh(context.Background())
		refreshed <- outcome{err: err, records: idx.recordsSnapshot()}
	}()

	time.AfterFunc(200*time.Millisecond, func() { close(release) })

	got := <-refreshed
	if got.err != nil {
		t.Fatalf("Refresh returned error: %v", got.err)
	}
	if len(got.records) != 1 || got.records[0].ID != "cursor:scanned" {
		t.Fatalf("snapshot after Refresh = %v, want the record the in-flight scan produced", got.records)
	}
}

// testIndex builds an index whose scan is under the test's control and whose
// registry holds the given parsers, so the refresh and resolver paths run without
// a real corpus while still going through the registry lookup production uses.
func testIndex(
	t *testing.T,
	scan func(context.Context, *Registry, scanCache) (scanResult, error),
	parsers ...Parser,
) *Index {
	t.Helper()

	registry := NewRegistry()
	for _, parser := range parsers {
		registry.Register(parser)
	}
	return &Index{
		mu:               sync.Mutex{},
		includeSubagents: false,
		registry:         registry,
		records:          nil,
		prevRecords:      nil,
		prevStamps:       nil,
		loaded:           false,
		refreshing:       false,
		refreshRun:       nil,
		lastRefresh:      time.Time{},
		cachePath:        filepath.Join(t.TempDir(), cacheFilename),
		debounce:         time.Hour,
		scanProvider:     scan,
	}
}

func testRecord(id string, provider Provider, nativeID string, title string) Record {
	return Record{
		ID:              id,
		Provider:        provider,
		NativeID:        nativeID,
		Lineage:         nil,
		Origin:          OriginUnspecified,
		Title:           title,
		WorkspaceRoot:   "",
		ArtifactPath:    "/tmp/" + id,
		ArtifactKind:    "composer",
		Model:           "",
		CreatedAt:       time.Time{},
		UpdatedAt:       time.Time{},
		SizeBytes:       0,
		Archived:        false,
		LatestRequestID: "",
	}
}

// stubParser is a registered provider whose only real behaviour is the request
// lookup a test is about: the match it reports, or the store failure it hits.
//
// It counts the lookups it was asked for, because whether the provider's store
// was consulted at all is a fact about the run rather than something to read out
// of the answer. The counter is a pointer so the registry's copy of the parser
// and the test's own share it.
type stubParser struct {
	provider providerid.Provider
	match    RequestMatch
	err      error
	lookups  *atomic.Int64
}

var (
	_ Parser          = stubParser{}
	_ RequestResolver = stubParser{}
)

func (parser stubParser) Provider() providerid.Provider { return parser.provider }

func (stubParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return nil, nil
}

func (stubParser) ScanRecord(string, FileStamp) (Record, bool) {
	return emptyRecord(), false
}

func (stubParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

func (parser stubParser) ResolveRequestID(
	context.Context,
	string,
	RequestLookupOptions,
) (RequestMatch, error) {
	parser.lookups.Add(1)
	return parser.match, parser.err
}

// resolverParser wraps one request-lookup outcome as a registered Cursor parser.
func resolverParser(match RequestMatch, err error) stubParser {
	return stubParser{provider: providerid.ProviderCursor, match: match, err: err, lookups: &atomic.Int64{}}
}

// TestWaitForRefreshReportsAFailedInFlightRefresh is the difference between a
// rebuild finishing and a rebuild succeeding. A waiter told the wait succeeded
// proceeds against the snapshot it was waiting to replace and reports a clean run
// over a corpus the failed scan never updated.
func TestWaitForRefreshReportsAFailedInFlightRefresh(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	scanErr := errors.New("scan the corpus: permission denied")
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		startOnce.Do(func() { close(started) })
		<-release
		return scanResult{records: nil, stamps: nil}, scanErr
	})

	// List starts the background refresh, which is what leaves one in flight.
	if _, err := idx.List(context.Background()); err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	<-started

	waited := make(chan error, 1)
	go func() { waited <- idx.Refresh(context.Background()) }()
	time.AfterFunc(50*time.Millisecond, func() { close(release) })

	err := <-waited
	if err == nil {
		t.Fatal("Refresh returned nil after waiting on a rebuild that failed")
	}
	if !errors.Is(err, scanErr) {
		t.Fatalf("Refresh error = %v, want it to carry the rebuild's own failure", err)
	}
}

// TestResolveDoesNotFailOverAProviderStore covers the blast radius of one broken
// store. A UUID-shaped selector runs the provider lookup for every provider, so a
// Cursor directory Clyde cannot read would otherwise turn an ordinary Claude
// conversation miss into an unrelated permission error.
func TestResolveDoesNotFailOverAProviderStore(t *testing.T) {
	t.Parallel()

	storeErr := errors.New("read cursor workspace storage dir: operation not permitted")
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: nil, stamps: nil}, nil
	}, resolverParser(RequestMatch{}, storeErr))

	_, err := idx.Resolve(context.Background(), "6f1c8a2e-0000-4000-8000-000000000000")
	if err == nil {
		t.Fatal("Resolve returned no error for a selector nothing holds")
	}
	if strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("Resolve error = %v, want a not-found answer rather than the provider's store error", err)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Resolve error = %v, want it to report the conversation as not found", err)
	}
	if !strings.Contains(err.Error(), RequestNotFoundReasonInconclusive.Describe()) {
		t.Fatalf("Resolve error = %v, want the miss reported as inconclusive", err)
	}
}

// TestResolveRequestDoesNotAnswerWithAnotherProvidersConversation is the guess
// the design says must never happen. A conversation whose title happens to be the
// composer id, which is what pasting one into a chat while debugging produces,
// must not be returned as the conversation that issued the request.
func TestResolveRequestDoesNotAnswerWithAnotherProvidersConversation(t *testing.T) {
	t.Parallel()

	composerID := "6f1c8a2e-0000-4000-8000-000000000000"
	claudeRecord := testRecord("claude:debug", ProviderClaude, "debug-session", composerID)
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{claudeRecord}, stamps: nil}, nil
	}, resolverParser(RequestMatch{
		Found:                true,
		Provider:             ProviderCursor,
		NativeConversationID: composerID,
		Origin:               RequestOriginLive,
		Reason:               RequestNotFoundReasonUnspecified,
	}, nil))

	resolution, err := idx.ResolveRequest(context.Background(), "0e3f0000-0000-4000-8000-000000000000", RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Found {
		t.Fatalf("ResolveRequest answered with %+v, want no answer rather than a record that merely carries the id as its title", resolution.Record)
	}
	if resolution.Reason != RequestNotFoundReasonUnindexedConversation {
		t.Fatalf("reason = %v, want unindexed_conversation", resolution.Reason)
	}
}

// TestResolveRequestFindsTheProvidersOwnRecord is the other half: the record that
// really is the conversation the resolver named still resolves.
func TestResolveRequestFindsTheProvidersOwnRecord(t *testing.T) {
	t.Parallel()

	composerID := "6f1c8a2e-0000-4000-8000-000000000000"
	cursorRecord := testRecord("cursor:chat", ProviderCursor, composerID, "the chat")
	claudeRecord := testRecord("claude:debug", ProviderClaude, "debug-session", composerID)
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{claudeRecord, cursorRecord}, stamps: nil}, nil
	}, resolverParser(RequestMatch{
		Found:                true,
		Provider:             ProviderCursor,
		NativeConversationID: composerID,
		Origin:               RequestOriginLive,
		Reason:               RequestNotFoundReasonUnspecified,
	}, nil))

	resolution, err := idx.ResolveRequest(context.Background(), "0e3f0000-0000-4000-8000-000000000000", RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if !resolution.Found || resolution.Record.ID != "cursor:chat" {
		t.Fatalf("resolution = %+v, want the Cursor chat that owns the composer id", resolution)
	}
}

// TestResolveRequestReportsAnInconclusiveMissWhenTheIndexIsStale covers the
// refresh a lookup could not complete. Answering from records that may be out of
// date is the right trade against failing the command, and saying the miss is
// confirmed is not.
func TestResolveRequestReportsAnInconclusiveMissWhenTheIndexIsStale(t *testing.T) {
	t.Parallel()

	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: nil, stamps: nil}, errors.New("scan the corpus: disk gone")
	}, resolverParser(RequestMatch{
		Found:                false,
		Provider:             providerid.ProviderUnspecified,
		NativeConversationID: "",
		Origin:               RequestOriginUnspecified,
		Reason:               RequestNotFoundReasonNotRetained,
	}, nil))

	resolution, err := idx.ResolveRequest(context.Background(), "0e3f0000-0000-4000-8000-000000000000", RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Found {
		t.Fatalf("resolution = %+v, want a miss", resolution)
	}
	if resolution.Reason != RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %v, want inconclusive because the refresh behind the miss did not finish", resolution.Reason)
	}
}

// TestMergeRequestNotFoundReasonLetsInconclusiveOutrankAConfirmedMiss pins the
// precedence every multi-store lookup folds through, in both orders.
func TestMergeRequestNotFoundReasonLetsInconclusiveOutrankAConfirmedMiss(t *testing.T) {
	t.Parallel()

	confirmedThenUnread := MergeRequestNotFoundReason(RequestNotFoundReasonNotRetained, RequestNotFoundReasonInconclusive)
	if confirmedThenUnread != RequestNotFoundReasonInconclusive {
		t.Fatalf("confirmed then unread = %v, want inconclusive", confirmedThenUnread)
	}
	unreadThenConfirmed := MergeRequestNotFoundReason(RequestNotFoundReasonInconclusive, RequestNotFoundReasonNotRetained)
	if unreadThenConfirmed != RequestNotFoundReasonInconclusive {
		t.Fatalf("unread then confirmed = %v, want inconclusive", unreadThenConfirmed)
	}
	firstKnowable := MergeRequestNotFoundReason(RequestNotFoundReasonNotRetained, RequestNotFoundReasonNoMatchingConversation)
	if firstKnowable != RequestNotFoundReasonNotRetained {
		t.Fatalf("two confirmed reasons = %v, want the first to stand", firstKnowable)
	}
}

// TestResolveRequestAnswersUnderADeadlineTheRescanCannotMeet is the named
// symptom. The first lookup after a cache-format upgrade re-parses the whole
// corpus in a background rescan that runs under a context without cancellation,
// so a caller waiting on it under an RPC deadline waits for work it cannot
// shorten. Answering from the records already held is the right trade; failing
// the command is not, and neither is calling the miss confirmed.
func TestResolveRequestAnswersUnderADeadlineTheRescanCannotMeet(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	t.Cleanup(func() { close(release) })
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		startOnce.Do(func() { close(started) })
		<-release
		return scanResult{records: nil, stamps: nil}, nil
	}, resolverParser(RequestMatch{
		Found:                false,
		Provider:             providerid.ProviderUnspecified,
		NativeConversationID: "",
		Origin:               RequestOriginUnspecified,
		Reason:               RequestNotFoundReasonNotRetained,
	}, nil))

	// List starts the rescan, which is what the deadline then runs out against.
	if _, err := idx.List(context.Background()); err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)

	resolution, err := idx.ResolveRequest(ctx, "0e3f0000-0000-4000-8000-000000000000", RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error under a deadline the rescan could not meet: %v", err)
	}
	if resolution.Found {
		t.Fatalf("resolution = %+v, want a miss", resolution)
	}
	if resolution.Reason != RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %v, want inconclusive because the refresh behind the miss did not finish", resolution.Reason)
	}
}

// TestResolveRequestReportsAmbiguityWhenSeveralRecordsCarryTheID covers the index
// side of a duplicated conversation. Measured on a real Cursor store, 43 of 1,836
// chats advertising a latest request id share it with another chat, so the first
// record carrying one is not evidence that it is the only one.
func TestResolveRequestReportsAmbiguityWhenSeveralRecordsCarryTheID(t *testing.T) {
	t.Parallel()

	requestID := "0e3f0000-0000-4000-8000-000000000000"
	original := testRecord("cursor:original", ProviderCursor, "original", "the chat")
	original.LatestRequestID = requestID
	duplicate := testRecord("cursor:duplicate", ProviderCursor, "duplicate", "(1) the chat")
	duplicate.LatestRequestID = requestID

	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{original, duplicate}, stamps: nil}, nil
	})

	resolution, err := idx.ResolveRequest(context.Background(), requestID, RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Found {
		t.Fatalf("resolution = %+v, want no answer when two records carry the id", resolution.Record)
	}
	if resolution.Reason != RequestNotFoundReasonAmbiguousConversation {
		t.Fatalf("reason = %v, want ambiguous_conversation", resolution.Reason)
	}
}

// TestResolveRequestStillAnswersFromTheIndexForASingleCarrier is the other half,
// so the ambiguity check does not simply stop the index path answering.
func TestResolveRequestStillAnswersFromTheIndexForASingleCarrier(t *testing.T) {
	t.Parallel()

	requestID := "0e3f0000-0000-4000-8000-000000000000"
	original := testRecord("cursor:original", ProviderCursor, "original", "the chat")
	original.LatestRequestID = requestID
	other := testRecord("cursor:other", ProviderCursor, "other", "another chat")

	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{original, other}, stamps: nil}, nil
	})

	resolution, err := idx.ResolveRequest(context.Background(), requestID, RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if !resolution.Found || resolution.Record.ID != "cursor:original" {
		t.Fatalf("resolution = %+v, want the one record carrying the id", resolution)
	}
	if resolution.Origin != RequestOriginIndex {
		t.Fatalf("origin = %v, want index", resolution.Origin)
	}
}

// TestResolveAgreesWithResolveRequestAboutAmbiguity is the surface disagreement.
// Every selector path runs through Resolve, so a request id matched by the exact
// pass would answer with the first record carrying it while `resolve-request`
// reported the same id ambiguous. Measured on a real Cursor store, 43 of 1,836
// chats advertising a latest request id share it with another chat.
func TestResolveAgreesWithResolveRequestAboutAmbiguity(t *testing.T) {
	t.Parallel()

	requestID := "0e3f0000-0000-4000-8000-000000000000"
	original := testRecord("cursor:aaa-original", ProviderCursor, "aaa-original", "the chat")
	original.LatestRequestID = requestID
	duplicate := testRecord("cursor:bbb-duplicate", ProviderCursor, "bbb-duplicate", "(1) the chat")
	duplicate.LatestRequestID = requestID

	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{original, duplicate}, stamps: nil}, nil
	})

	_, err := idx.Resolve(context.Background(), requestID)
	if err == nil {
		t.Fatal("Resolve answered with one of two conversations carrying the request id")
	}
	if !strings.Contains(err.Error(), RequestNotFoundReasonAmbiguousConversation.Describe()) {
		t.Fatalf("Resolve error = %v, want the same ambiguity resolve-request reports", err)
	}

	resolution, err := idx.ResolveRequest(context.Background(), requestID, RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Reason != RequestNotFoundReasonAmbiguousConversation {
		t.Fatalf("resolve-request reason = %v while Resolve reported ambiguity: the two surfaces disagree", resolution.Reason)
	}
}

// TestResolveStillAnswersARequestIDOnlyOneConversationCarries keeps the fast path
// honest, so the ambiguity check does not simply stop Resolve answering.
func TestResolveStillAnswersARequestIDOnlyOneConversationCarries(t *testing.T) {
	t.Parallel()

	requestID := "0e3f0000-0000-4000-8000-000000000000"
	original := testRecord("cursor:original", ProviderCursor, "original", "the chat")
	original.LatestRequestID = requestID

	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{original}, stamps: nil}, nil
	})

	record, err := idx.Resolve(context.Background(), requestID)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if record.ID != "cursor:original" {
		t.Fatalf("record = %q, want the one conversation carrying the id", record.ID)
	}
}

// TestResolveRequestDoesNotClaimUnindexedOverAFailedRefresh covers a claim about
// the index made over an index that was never brought up to date. The refresh is
// the likelier reason the conversation is missing than the provider inventing it.
func TestResolveRequestDoesNotClaimUnindexedOverAFailedRefresh(t *testing.T) {
	t.Parallel()

	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: nil, stamps: nil}, errors.New("scan the corpus: permission denied")
	}, resolverParser(RequestMatch{
		Found:                true,
		Provider:             ProviderCursor,
		NativeConversationID: "a-chat-the-index-has-not-seen",
		Origin:               RequestOriginLive,
		Reason:               RequestNotFoundReasonUnspecified,
	}, nil))

	resolution, err := idx.ResolveRequest(context.Background(), "0e3f0000-0000-4000-8000-000000000000", RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Reason == RequestNotFoundReasonUnindexedConversation {
		t.Fatal("reported the index does not hold the conversation, over an index the refresh never rebuilt")
	}
	if resolution.Reason != RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %v, want inconclusive", resolution.Reason)
	}
}

// TestResolveRequestDoesNotAnswerFromACacheThatPredatesTheDuplicate covers the
// uniqueness the index path asserts when it answers. How many conversations carry
// a request id is a question about the corpus as it is now, and the cached
// records are the corpus as it was at the last scan. A chat the operator
// duplicated since then carries the request as truly as the original does, so an
// answer read off the old snapshot names one conversation for an id the current
// corpus cannot narrow to one.
func TestResolveRequestDoesNotAnswerFromACacheThatPredatesTheDuplicate(t *testing.T) {
	t.Parallel()

	requestID := "0e3f0000-0000-4000-8000-000000000000"
	original := testRecord("cursor:original", ProviderCursor, "original", "the chat")
	original.LatestRequestID = requestID
	duplicate := testRecord("cursor:duplicate", ProviderCursor, "duplicate", "(1) the chat")
	duplicate.LatestRequestID = requestID

	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{original, duplicate}, stamps: nil}, nil
	})
	// The cache as it stood before the chat was duplicated.
	idx.records = []Record{original}
	idx.loaded = true

	resolution, err := idx.ResolveRequest(context.Background(), requestID, RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Found {
		t.Fatalf("resolution = %+v, want no answer: the cached snapshot showed one carrier and the corpus now holds two", resolution.Record)
	}
	if resolution.Reason != RequestNotFoundReasonAmbiguousConversation {
		t.Fatalf("reason = %v, want ambiguous_conversation", resolution.Reason)
	}
}

// TestResolveNeverAnswersARequestIDWithATitleThatEqualsIt covers the exact-title
// pass in front of the request-id route. A title is what a conversation is about,
// so it can be any string at all, and pasting a request id into a chat is enough
// to make it that chat's title. Answering the id with that chat says it issued
// the request, which is the guess the route exists not to make.
func TestResolveNeverAnswersARequestIDWithATitleThatEqualsIt(t *testing.T) {
	t.Parallel()

	requestID := "0e3f0000-0000-4000-8000-000000000000"
	titled := testRecord("cursor:titled", ProviderCursor, "titled-chat", requestID)
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{titled}, stamps: nil}, nil
	}, resolverParser(RequestMatch{
		Found:                false,
		Provider:             providerid.ProviderUnspecified,
		NativeConversationID: "",
		Origin:               RequestOriginUnspecified,
		Reason:               RequestNotFoundReasonNotRetained,
	}, nil))

	record, err := idx.Resolve(context.Background(), requestID)
	if err == nil {
		t.Fatalf("Resolve returned %q for a request id no conversation issued, matched on its title", record.ID)
	}
	if !strings.Contains(err.Error(), RequestNotFoundReasonNotRetained.Describe()) {
		t.Fatalf("Resolve error = %v, want the request lookup's own reason", err)
	}

	// The controls: the same conversation is still reachable by what it is, and a
	// title that is not shaped like a request id still resolves exactly.
	if found, err := idx.Resolve(context.Background(), "titled-chat"); err != nil || found.ID != "cursor:titled" {
		t.Fatalf("Resolve by native id = (%+v, %v), want the conversation", found, err)
	}
	plain := testRecord("cursor:plain", ProviderCursor, "plain-chat", "release notes")
	plainIdx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: []Record{plain}, stamps: nil}, nil
	})
	if found, err := plainIdx.Resolve(context.Background(), "release notes"); err != nil || found.ID != "cursor:plain" {
		t.Fatalf("Resolve by exact title = (%+v, %v), want the conversation", found, err)
	}
}

// TestResolveOnlyAsksTheProviderStoreForARequestShapedSelector counts the
// lookups instead of inferring them from the error text. An implementation that
// queried the store, discarded the result, and reported a plain not-found reads
// identically in the message and is exactly what the shape gate exists to
// prevent, because the gate runs for every selector of every provider.
func TestResolveOnlyAsksTheProviderStoreForARequestShapedSelector(t *testing.T) {
	t.Parallel()

	parser := resolverParser(RequestMatch{
		Found:                false,
		Provider:             providerid.ProviderUnspecified,
		NativeConversationID: "",
		Origin:               RequestOriginUnspecified,
		Reason:               RequestNotFoundReasonNotRetained,
	}, nil)
	idx := testIndex(t, func(context.Context, *Registry, scanCache) (scanResult, error) {
		return scanResult{records: nil, stamps: nil}, nil
	}, parser)

	if _, err := idx.Resolve(context.Background(), "not-a-request-id"); err == nil {
		t.Fatal("Resolve of an unknown selector returned no error")
	}
	if asked := parser.lookups.Load(); asked != 0 {
		t.Fatalf("the provider store was asked %d times about a selector that is not shaped like a request id", asked)
	}

	// The positive control: the counter does move, so the assertion above is about
	// the shape gate rather than about a resolver that is never reached at all.
	if _, err := idx.Resolve(context.Background(), "0e3f0000-0000-4000-8000-000000000000"); err == nil {
		t.Fatal("Resolve of an unknown request id returned no error")
	}
	if asked := parser.lookups.Load(); asked != 1 {
		t.Fatalf("lookups = %d, want the request-id-shaped selector to reach the store exactly once", asked)
	}
}

// TestMergeRequestNotFoundReasonLetsAmbiguityOutrankAConfirmedMiss covers two
// Cursor installs where the first has no ring entry and the second holds the
// request in two chats. Reporting not-retained there says the id was never issued
// on this machine, which the second root just disproved.
func TestMergeRequestNotFoundReasonLetsAmbiguityOutrankAConfirmedMiss(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name  string
		soFar RequestNotFoundReason
		next  RequestNotFoundReason
	}{
		{name: "confirmed then ambiguous", soFar: RequestNotFoundReasonNotRetained, next: RequestNotFoundReasonAmbiguousConversation},
		{name: "ambiguous then confirmed", soFar: RequestNotFoundReasonAmbiguousConversation, next: RequestNotFoundReasonNotRetained},
		{name: "unread then ambiguous", soFar: RequestNotFoundReasonInconclusive, next: RequestNotFoundReasonAmbiguousConversation},
		{name: "ambiguous then unread", soFar: RequestNotFoundReasonAmbiguousConversation, next: RequestNotFoundReasonInconclusive},
	} {
		merged := MergeRequestNotFoundReason(testCase.soFar, testCase.next)
		if merged != RequestNotFoundReasonAmbiguousConversation {
			t.Fatalf("%s = %v, want ambiguous to stand", testCase.name, merged)
		}
	}
}
