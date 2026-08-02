package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// failingLoadIndex omits a conversation from messagesByID, which makes the fake
// index return an error for it. That is the same shape a real load failure has:
// the conversation is listed and advertised, and reading its transcript fails.
func failingLoadIndex(failingID string, healthyID string, failingStamp conversation.FileStamp) *fakeConversationSemanticIndex {
	return &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{
			{Record: semanticTestRecord(failingID), Stamp: failingStamp},
			{Record: semanticTestRecord(healthyID), Stamp: semanticTestStamp(10, 210)},
		},
		messagesByID: map[string][]transcript.Message{
			healthyID: {{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "real"}},
		},
		loadOptions: nil,
	}
}

// runPasses runs count passes and fails the test on the first pass error.
func runPasses(t *testing.T, worker *conversationSemanticSyncWorker, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		if err := worker.runPass(context.Background()); err != nil {
			t.Fatalf("runPass %d returned error: %v", i+1, err)
		}
	}
}

// TestFailedConversationLeavesTheManifestUntilItsBytesChange is the behavior
// this exists for. A conversation that fails to load delivers no document, so
// the engine never marks it satisfied and keeps listing it as needed. Without
// suppression every later pass reads and fails it again, once a minute, for as
// long as the daemon runs: the daemon never goes idle, the log grows without
// bound, and the pass never reports itself caught up.
//
// Suppression waits for the failure threshold, so a transient failure retries,
// and lifts once the transcript's bytes change, so the reader gets another
// chance against new content.
func TestFailedConversationLeavesTheManifestUntilItsBytesChange(t *testing.T) {
	failingID := "cursor:broken"
	healthyID := "codex:real"
	failingStamp := semanticTestStamp(0, 200)
	index := failingLoadIndex(failingID, healthyID, failingStamp)
	client := &fakeConversationSemanticClient{needed: []string{failingID, healthyID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	// Passes one through the threshold keep advertising the conversation,
	// because each failure alone could be transient and the next pass is its
	// retry.
	runPasses(t, worker, failedLoadSuppressThreshold)
	for passIndex, syncCall := range client.syncCalls {
		if len(syncCall.Manifest) != 2 {
			t.Fatalf("pass %d manifest size = %d, want 2: below the threshold the failing conversation must stay advertised", passIndex+1, len(syncCall.Manifest))
		}
	}

	// The failure count has reached the threshold, so the next manifest omits
	// the conversation and the engine stops asking for it.
	runPasses(t, worker, 1)
	suppressedManifest := client.syncCalls[failedLoadSuppressThreshold].Manifest
	if len(suppressedManifest) != 1 || suppressedManifest[0].ConversationID != healthyID {
		t.Fatalf("post-threshold manifest = %+v, want only %q; the failing conversation must not be re-advertised", suppressedManifest, healthyID)
	}

	// The transcript grows, so its fingerprint changes and the reader gets
	// another chance against the new bytes.
	index.records[0].Stamp = semanticTestStamp(5, 300)
	index.messagesByID[failingID] = []transcript.Message{
		{Role: "user", Timestamp: time.Unix(1710000500, 0), Text: "now readable"},
	}
	runPasses(t, worker, 1)
	finalManifest := client.syncCalls[len(client.syncCalls)-1].Manifest
	reincluded := false
	for _, fingerprint := range finalManifest {
		if fingerprint.ConversationID == failingID {
			reincluded = true
		}
	}
	if !reincluded {
		t.Fatalf("final manifest = %+v, want %q re-included once its bytes changed", finalManifest, failingID)
	}
}

// TestTransientFailureRetriesAndHeals pins the transient case. Cursor's live
// SQLite store can be busy while Cursor checkpoints, so one failed load must
// cost the passes it spans and nothing after: the conversation stays
// advertised, the next pass retries it, and a successful load clears its
// failure history so it is never suppressed.
func TestTransientFailureRetriesAndHeals(t *testing.T) {
	failingID := "cursor:busy"
	healthyID := "codex:real"
	index := failingLoadIndex(failingID, healthyID, semanticTestStamp(0, 200))
	client := &fakeConversationSemanticClient{needed: []string{failingID, healthyID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	// One failure, as a busy store produces.
	runPasses(t, worker, 1)
	if len(worker.failedLoad) != 1 {
		t.Fatalf("failedLoad size = %d after one failure, want 1", len(worker.failedLoad))
	}

	// The store is readable again with the same bytes, so the same
	// fingerprint now loads.
	index.messagesByID[failingID] = []transcript.Message{
		{Role: "user", Timestamp: time.Unix(1710000100, 0), Text: "was busy"},
	}
	runPasses(t, worker, 1)

	retryManifest := client.syncCalls[1].Manifest
	if len(retryManifest) != 2 {
		t.Fatalf("retry manifest size = %d, want 2: one failure must not suppress", len(retryManifest))
	}
	delivered := false
	for _, doc := range client.upsertCalls[len(client.upsertCalls)-1].Docs {
		if doc.ConversationID == failingID {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("the healed conversation was not delivered on the retry pass")
	}
	if len(worker.failedLoad) != 0 {
		t.Fatalf("failedLoad size = %d after a successful load, want 0", len(worker.failedLoad))
	}
}

// TestFailedSuppressionUsesTheAdvertisedFingerprint pins the key. A suppression
// stored under a different fingerprint than the manifest advertises either never
// lifts or lifts on every pass.
func TestFailedSuppressionUsesTheAdvertisedFingerprint(t *testing.T) {
	failingID := "cursor:broken"
	failingStamp := semanticTestStamp(20, 200)
	index := failingLoadIndex(failingID, "codex:real", failingStamp)
	client := &fakeConversationSemanticClient{needed: []string{failingID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	runPasses(t, worker, 1)
	want := conversation.ContentFingerprint(semanticTestRecord(failingID), failingStamp)
	failureRecord := worker.failedLoad[failingID]
	if failureRecord.fingerprint != want {
		t.Fatalf("suppression fingerprint = %q, want the advertised %q", failureRecord.fingerprint, want)
	}
	if failureRecord.failures != 1 {
		t.Fatalf("failures = %d after one pass, want 1", failureRecord.failures)
	}
}

// TestFailedAndEmptyStayInSeparateMaps keeps the two meanings apart. A
// zero-document conversation has nothing to index, so dropping it from the
// manifest is the truth. A failed one has content the reader could not produce,
// so its suppression must stay reportable rather than inherit the other's
// silence.
func TestFailedAndEmptyStayInSeparateMaps(t *testing.T) {
	failingID := "cursor:broken"
	emptyID := "cursor:empty"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{
			{Record: semanticTestRecord(failingID), Stamp: semanticTestStamp(0, 200)},
			{Record: semanticTestRecord(emptyID), Stamp: semanticTestStamp(1, 201)},
		},
		messagesByID: map[string][]transcript.Message{emptyID: {}},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{failingID, emptyID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	runPasses(t, worker, 1)
	if _, failed := worker.failedLoad[failingID]; !failed {
		t.Fatal("the failing conversation is not in failedLoad")
	}
	if _, crossed := worker.emptyDelivered[failingID]; crossed {
		t.Fatal("the failing conversation leaked into emptyDelivered")
	}
	if _, empty := worker.emptyDelivered[emptyID]; !empty {
		t.Fatal("the zero-document conversation is not in emptyDelivered")
	}
	if _, crossed := worker.failedLoad[emptyID]; crossed {
		t.Fatal("the zero-document conversation leaked into failedLoad")
	}
}

// TestFailedSuppressionIsReported keeps a shrinking manifest honest. The
// conversation is content the store does not hold, so a pass that drops it says
// so rather than quietly reporting a smaller corpus as fully indexed. The same
// count flows into the freshness surface, so search freshness keeps reporting
// the missing content as pending rather than as covered.
func TestFailedSuppressionIsReported(t *testing.T) {
	failingID := "cursor:broken"
	index := failingLoadIndex(failingID, "codex:real", semanticTestStamp(0, 200))
	client := &fakeConversationSemanticClient{needed: []string{failingID}}
	buffer := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(buffer, &slog.HandlerOptions{
		AddSource:   false,
		Level:       slog.LevelInfo,
		ReplaceAttr: nil,
	}))
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", logger, semanticTestContentKinds())
	worker.freshness = newConversationSemanticFreshness()

	runPasses(t, worker, failedLoadSuppressThreshold)
	// The suppressing manifest omits the conversation, so the engine stops
	// listing it as needed.
	client.needed = nil
	buffer.Reset()
	runPasses(t, worker, 1)

	logs := buffer.String()
	if !strings.Contains(logs, "pass_completed") {
		t.Fatalf("the suppressing pass logged nothing at Info: %s", logs)
	}
	if !strings.Contains(logs, `"failed_suppressed":1`) {
		t.Fatalf("logs do not report the suppression: %s", logs)
	}

	// The suppressed conversation left the advertised manifest, but its
	// content is still missing, so freshness must not read as fully indexed.
	freshness := worker.freshness.snapshot()
	if freshness.Manifest != 2 {
		t.Fatalf("freshness manifest = %d, want 2: the suppressed conversation still exists on disk", freshness.Manifest)
	}
	if freshness.Pending != 1 {
		t.Fatalf("freshness pending = %d, want 1: the suppressed conversation's content is still missing", freshness.Pending)
	}
}

// TestFailedSuppressionIsBounded proves the map does not grow across the
// daemon's lifetime. A conversation that leaves the scanned set leaves the
// suppression map with it.
func TestFailedSuppressionIsBounded(t *testing.T) {
	failingID := "cursor:broken"
	index := failingLoadIndex(failingID, "codex:real", semanticTestStamp(0, 200))
	client := &fakeConversationSemanticClient{needed: []string{failingID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	runPasses(t, worker, 1)
	if len(worker.failedLoad) != 1 {
		t.Fatalf("failedLoad size = %d, want 1", len(worker.failedLoad))
	}

	// The artifact is gone from disk, so the scan no longer lists it.
	index.records = index.records[1:]
	runPasses(t, worker, 1)
	if len(worker.failedLoad) != 0 {
		t.Fatalf("failedLoad size = %d, want 0 once the conversation left the scan", len(worker.failedLoad))
	}
}

// TestSuppressionOfWholeCorpusStillSyncsEmptyManifest pins the wire contract
// for the last conversation standing. The engine's copy of the manifest is
// whatever the last sync stated, so a pass whose suppression empties the
// manifest must still sync that empty manifest; skipping it would leave the
// suppressed conversation in the engine's needed set forever. Absence is
// retain-only on the wire, so the empty sync removes nothing from the store.
func TestSuppressionOfWholeCorpusStillSyncsEmptyManifest(t *testing.T) {
	failingID := "cursor:broken"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{
			{Record: semanticTestRecord(failingID), Stamp: semanticTestStamp(0, 200)},
		},
		messagesByID: map[string][]transcript.Message{},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{failingID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	runPasses(t, worker, failedLoadSuppressThreshold)
	runPasses(t, worker, 1)

	suppressingSync := len(client.syncCalls) - 1
	if suppressingSync != failedLoadSuppressThreshold {
		t.Fatalf("sync calls = %d, want %d: the suppressing pass must still sync", len(client.syncCalls), failedLoadSuppressThreshold+1)
	}
	if len(client.syncCalls[suppressingSync].Manifest) != 0 {
		t.Fatalf("suppressing sync manifest = %+v, want empty", client.syncCalls[suppressingSync].Manifest)
	}
}

// TestZeroDocumentSuppressionOfWholeCorpusStillSyncsEmptyManifest pins the
// same wire contract for the zero-document path. A conversation that delivers
// no documents enters emptyDelivered and leaves the next manifest; when it was
// the whole corpus, that pass must still sync the empty manifest so the engine
// stops listing it as needed.
func TestZeroDocumentSuppressionOfWholeCorpusStillSyncsEmptyManifest(t *testing.T) {
	emptyID := "cursor:empty"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{
			{Record: semanticTestRecord(emptyID), Stamp: semanticTestStamp(0, 200)},
		},
		messagesByID: map[string][]transcript.Message{emptyID: {}},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{emptyID}}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())

	// Pass one advertises, delivers nothing, and records the zero-document
	// suppression. Pass two omits the conversation, emptying the manifest.
	runPasses(t, worker, 2)

	if len(client.syncCalls) != 2 {
		t.Fatalf("sync calls = %d, want 2: the suppressing pass must still sync", len(client.syncCalls))
	}
	if len(client.syncCalls[1].Manifest) != 0 {
		t.Fatalf("suppressing sync manifest = %+v, want empty", client.syncCalls[1].Manifest)
	}
}
