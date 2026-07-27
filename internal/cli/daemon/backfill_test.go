package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/transcript"
)

// backfillTestContentKinds is the shipped default content selection, so the
// backfill tests project documents exactly as the backfill command does.
func backfillTestContentKinds(t *testing.T) conversation.ContentKindSet {
	t.Helper()
	kinds, err := daemonsvc.SemanticContentKinds(config.NewConfigWithDefaults().Conversation.Semantic)
	if err != nil {
		t.Fatalf("resolve default content kinds: %v", err)
	}
	return kinds
}

func testBackfillRecord(id, workspace string, archived bool) conversation.Record {
	return conversation.Record{
		ID:            id,
		Provider:      conversation.ProviderClaude,
		NativeID:      id,
		Lineage:       nil,
		Title:         "",
		WorkspaceRoot: workspace,
		ArtifactPath:  "/tmp/clyde-backfill-test.jsonl",
		ArtifactKind:  "jsonl",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      archived,
	}
}

func TestBuildBackfillEntriesProjectsIDWorkspaceAndArchived(t *testing.T) {
	t.Parallel()
	records := []conversation.Record{
		testBackfillRecord("claude:one", "/repo/alpha", false),
		testBackfillRecord("codex:two", "/repo/beta", true),
	}

	entries := buildBackfillEntries(records)

	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].ConversationID != "claude:one" || entries[0].WorkspaceRoot != "/repo/alpha" || entries[0].Archived {
		t.Fatalf("entries[0] = %+v, want {claude:one /repo/alpha false}", entries[0])
	}
	if entries[1].ConversationID != "codex:two" || entries[1].WorkspaceRoot != "/repo/beta" || !entries[1].Archived {
		t.Fatalf("entries[1] = %+v, want {codex:two /repo/beta true}", entries[1])
	}
}

func TestBuildBackfillEntriesEmpty(t *testing.T) {
	t.Parallel()
	entries := buildBackfillEntries(nil)
	if len(entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(entries))
	}
}

func TestBuildBackfillEntriesCoalescesDuplicateIDPreferringWorkspace(t *testing.T) {
	t.Parallel()
	// One derived conversation id can come from two artifacts (the same session
	// recorded under two project dirs): one record carries the real workspace, the
	// other is empty. The empty record must never overwrite the real one, in either
	// input order. Regression for CLYDE-538.
	cases := []struct {
		name    string
		records []conversation.Record
	}{
		{
			name: "empty before real",
			records: []conversation.Record{
				testBackfillRecord("claude:dup", "", false),
				testBackfillRecord("claude:dup", "/repo/real", false),
			},
		},
		{
			name: "real before empty",
			records: []conversation.Record{
				testBackfillRecord("claude:dup", "/repo/real", false),
				testBackfillRecord("claude:dup", "", false),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			entries := buildBackfillEntries(testCase.records)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1 coalesced entry", len(entries))
			}
			if entries[0].ConversationID != "claude:dup" || entries[0].WorkspaceRoot != "/repo/real" {
				t.Fatalf("entries[0] = %+v, want {claude:dup /repo/real false}", entries[0])
			}
		})
	}
}

func TestBuildBackfillEntriesDuplicateIDPrefersMostRecentWhenWorkspacesEqualPresence(t *testing.T) {
	t.Parallel()
	// When duplicate records are equal on workspace-root presence (here both carry
	// one), the more recently updated record wins, in either input order.
	older := testBackfillRecord("claude:dup", "/repo/old", false)
	older.UpdatedAt = time.Unix(100, 0)
	newer := testBackfillRecord("claude:dup", "/repo/new", true)
	newer.UpdatedAt = time.Unix(200, 0)

	for _, records := range [][]conversation.Record{
		{older, newer},
		{newer, older},
	} {
		entries := buildBackfillEntries(records)
		if len(entries) != 1 {
			t.Fatalf("entries = %d, want 1 coalesced entry", len(entries))
		}
		if entries[0].WorkspaceRoot != "/repo/new" || !entries[0].Archived {
			t.Fatalf("entries[0] = %+v, want newer {/repo/new true}", entries[0])
		}
	}
}

type fakeDocumentBackfillIndex struct {
	stampedRecords []conversation.StampedRecord
	messagesByID   map[string][]transcript.Message
	refreshCount   int
	loadedIDs      []string
	loadOptions    []conversation.LoadOptions
}

func (idx *fakeDocumentBackfillIndex) Refresh(_ context.Context) error {
	idx.refreshCount++
	return nil
}

func (idx *fakeDocumentBackfillIndex) ListWithStamps(_ context.Context) ([]conversation.StampedRecord, error) {
	return append([]conversation.StampedRecord(nil), idx.stampedRecords...), nil
}

func (idx *fakeDocumentBackfillIndex) LoadMessagesWithOptions(record conversation.Record, opts conversation.LoadOptions) ([]transcript.Message, error) {
	idx.loadedIDs = append(idx.loadedIDs, record.ID)
	idx.loadOptions = append(idx.loadOptions, opts)
	messages, ok := idx.messagesByID[record.ID]
	if !ok {
		return nil, errors.New("missing messages for " + record.ID)
	}
	return append([]transcript.Message(nil), messages...), nil
}

func TestSelectBackfillDocumentRecordsPrefersFreshestDuplicate(t *testing.T) {
	t.Parallel()
	stale := testDocumentStampedRecord("claude:dup", 10)
	stale.Record.ArtifactPath = "/tmp/clyde-backfill-stale.jsonl"
	stale.Record.UpdatedAt = time.Unix(100, 0)
	fresh := testDocumentStampedRecord("claude:dup", 20)
	fresh.Record.ArtifactPath = "/tmp/clyde-backfill-fresh.jsonl"
	fresh.Record.UpdatedAt = time.Unix(200, 0)
	other := testDocumentStampedRecord("claude:other", 30)

	selected, nextCursor, err := selectBackfillDocumentRecords([]conversation.StampedRecord{
		stale,
		other,
		fresh,
	}, 0, "", "")
	if err != nil {
		t.Fatalf("select document records: %v", err)
	}

	if nextCursor != "" {
		t.Fatalf("next cursor = %q, want empty for an unbounded selection", nextCursor)
	}
	if len(selected) != 2 {
		t.Fatalf("selected records = %d, want 2", len(selected))
	}
	if selected[0].Record.ArtifactPath != fresh.Record.ArtifactPath {
		t.Fatalf("selected duplicate path = %q, want %q", selected[0].Record.ArtifactPath, fresh.Record.ArtifactPath)
	}
	if selected[0].Stamp.Fingerprint() != fresh.Stamp.Fingerprint() {
		t.Fatalf("selected duplicate fingerprint = %q, want %q", selected[0].Stamp.Fingerprint(), fresh.Stamp.Fingerprint())
	}
	if selected[1].Record.ID != other.Record.ID {
		t.Fatalf("second selected record = %q, want %q", selected[1].Record.ID, other.Record.ID)
	}
}

func TestSelectBackfillDocumentRecordsFirstBatchLimitUnchanged(t *testing.T) {
	t.Parallel()
	records := []conversation.StampedRecord{
		testDocumentStampedRecord("claude:one", 10),
		testDocumentStampedRecord("claude:two", 20),
		testDocumentStampedRecord("claude:three", 30),
		testDocumentStampedRecord("claude:four", 40),
	}

	selected, nextCursor, err := selectBackfillDocumentRecords(records, 2, "", "")
	if err != nil {
		t.Fatalf("select document records: %v", err)
	}

	if len(selected) != 2 {
		t.Fatalf("selected records = %d, want 2", len(selected))
	}
	if selected[0].Record.ID != "claude:one" || selected[1].Record.ID != "claude:two" {
		t.Fatalf("selected ids = [%q %q], want [claude:one claude:two] (first batch in caller order)", selected[0].Record.ID, selected[1].Record.ID)
	}
	if nextCursor != "claude:two" {
		t.Fatalf("next cursor = %q, want claude:two (last id of first batch)", nextCursor)
	}
}

func TestSelectBackfillDocumentRecordsCursorAdvancesPastID(t *testing.T) {
	t.Parallel()
	records := []conversation.StampedRecord{
		testDocumentStampedRecord("claude:one", 10),
		testDocumentStampedRecord("claude:two", 20),
		testDocumentStampedRecord("claude:three", 30),
		testDocumentStampedRecord("claude:four", 40),
	}

	// Resume after the first batch's last id: selection must continue with the next
	// records in order, not re-select the first batch.
	selected, nextCursor, err := selectBackfillDocumentRecords(records, 2, "", "claude:two")
	if err != nil {
		t.Fatalf("select document records: %v", err)
	}

	if len(selected) != 2 {
		t.Fatalf("selected records = %d, want 2", len(selected))
	}
	if selected[0].Record.ID != "claude:three" || selected[1].Record.ID != "claude:four" {
		t.Fatalf("selected ids = [%q %q], want [claude:three claude:four]", selected[0].Record.ID, selected[1].Record.ID)
	}
	if nextCursor != "" {
		t.Fatalf("next cursor = %q, want empty at end of corpus", nextCursor)
	}
}

func TestSelectBackfillDocumentRecordsCursorChainCoversCorpusWithoutOverlap(t *testing.T) {
	t.Parallel()
	records := []conversation.StampedRecord{
		testDocumentStampedRecord("claude:one", 10),
		testDocumentStampedRecord("claude:two", 20),
		testDocumentStampedRecord("claude:three", 30),
		testDocumentStampedRecord("claude:four", 40),
		testDocumentStampedRecord("claude:five", 50),
	}

	seen := make([]string, 0, len(records))
	cursor := ""
	for range make([]struct{}, len(records)+1) {
		selected, nextCursor, err := selectBackfillDocumentRecords(records, 2, "", cursor)
		if err != nil {
			t.Fatalf("select document records: %v", err)
		}
		for _, record := range selected {
			seen = append(seen, record.Record.ID)
		}
		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	want := []string{"claude:one", "claude:two", "claude:three", "claude:four", "claude:five"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Fatalf("chained selection = %v, want %v (no gaps, no overlap)", seen, want)
	}
}

func TestSelectBackfillDocumentRecordsUnknownCursorErrors(t *testing.T) {
	t.Parallel()
	records := []conversation.StampedRecord{
		testDocumentStampedRecord("claude:one", 10),
		testDocumentStampedRecord("claude:two", 20),
	}

	_, _, err := selectBackfillDocumentRecords(records, 2, "", "claude:missing")
	if err == nil {
		t.Fatal("expected an error for an unknown cursor id")
	}
	if !strings.Contains(err.Error(), "cursor conversation") {
		t.Fatalf("error = %v, want a cursor-not-found error", err)
	}
}

func TestSelectBackfillDocumentRecordsConversationUnchanged(t *testing.T) {
	t.Parallel()
	records := []conversation.StampedRecord{
		testDocumentStampedRecord("claude:one", 10),
		testDocumentStampedRecord("codex:target", 20),
		testDocumentStampedRecord("claude:three", 30),
	}

	selected, nextCursor, err := selectBackfillDocumentRecords(records, 0, "codex:target", "")
	if err != nil {
		t.Fatalf("select document records: %v", err)
	}

	if len(selected) != 1 || selected[0].Record.ID != "codex:target" {
		t.Fatalf("selected = %+v, want exactly codex:target", selected)
	}
	if nextCursor != "" {
		t.Fatalf("next cursor = %q, want empty for a single --conversation selection", nextCursor)
	}
}

func TestBuildBackfillConversationDocumentsSkipsLoadFailure(t *testing.T) {
	t.Parallel()
	index := fakeDocumentBackfillIndex{
		messagesByID: map[string][]transcript.Message{
			"claude:one":   {{Role: "user", Text: "one"}},
			"claude:three": {{Role: "assistant", Text: "three"}},
		},
	}
	stampedRecords := []conversation.StampedRecord{
		testDocumentStampedRecord("claude:one", 10),
		testDocumentStampedRecord("claude:missing", 20),
		testDocumentStampedRecord("claude:three", 30),
	}

	docs, manifest, skipped := buildBackfillConversationDocuments(context.Background(), &index, stampedRecords, backfillTestContentKinds(t))

	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if len(docs) != 2 {
		t.Fatalf("documents = %d, want 2", len(docs))
	}
	if len(manifest) != 2 {
		t.Fatalf("manifest = %d, want 2", len(manifest))
	}
	if manifest[0].ConversationID != "claude:one" || manifest[1].ConversationID != "claude:three" {
		t.Fatalf("manifest = %+v, want successful conversations only", manifest)
	}
	if strings.Join(index.loadedIDs, ",") != "claude:one,claude:missing,claude:three" {
		t.Fatalf("loaded ids = %v, want all selected conversations attempted", index.loadedIDs)
	}
}

func TestBackfillConversationDocumentsDryRunSelectsLimit(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
			testDocumentStampedRecord("claude:two", 20),
			testDocumentStampedRecord("claude:three", 30),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one":   {{Role: "user", Text: "one"}},
			"claude:two":   {{Role: "assistant", Text: "two"}},
			"claude:three": {{Role: "user", Text: "three"}},
		},
	}
	dialCalled := false

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: true, Limit: 2, ConversationID: ""},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatalf("run backfill conversation documents: %v", err)
	}

	if index.refreshCount != 1 {
		t.Fatalf("refresh count = %d, want 1", index.refreshCount)
	}
	if strings.Join(index.loadedIDs, ",") != "claude:one,claude:two" {
		t.Fatalf("loaded ids = %v, want first two records", index.loadedIDs)
	}
	// The backfill reads under the same content policy the daemon feeder runs, so
	// it does not read tool outputs the policy would withhold anyway.
	for _, opts := range index.loadOptions {
		if opts.IncludeToolOutputs {
			t.Fatalf("load options = %+v, want IncludeToolOutputs false under the default content policy", opts)
		}
	}
	if dialCalled {
		t.Fatal("dry-run dialed semantic client")
	}
	if !strings.Contains(output.String(), "Would send conversation documents from 2 conversations: 2 documents, 0 skipped conversations.") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestBackfillConversationDocumentsDryRunReportsSkippedConversations(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
			testDocumentStampedRecord("claude:missing", 20),
			testDocumentStampedRecord("claude:three", 30),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one":   {{Role: "user", Text: "one"}},
			"claude:three": {{Role: "assistant", Text: "three"}},
		},
	}

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: true, Limit: 0, ConversationID: ""},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatalf("run backfill conversation documents: %v", err)
	}

	if !strings.Contains(output.String(), "Would send conversation documents from 3 conversations: 2 documents, 1 skipped conversation.") {
		t.Fatalf("output = %q", output.String())
	}
}

func TestBackfillConversationDocumentsDryRunSelectsConversation(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
			testDocumentStampedRecord("codex:target", 20),
			testDocumentStampedRecord("claude:three", 30),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one":   {{Role: "user", Text: "one"}},
			"codex:target": {{Role: "assistant", Text: "target"}},
			"claude:three": {{Role: "user", Text: "three"}},
		},
	}

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: true, Limit: 0, ConversationID: "codex:target"},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatalf("run backfill conversation documents: %v", err)
	}

	if strings.Join(index.loadedIDs, ",") != "codex:target" {
		t.Fatalf("loaded ids = %v, want only codex:target", index.loadedIDs)
	}
	if !strings.Contains(output.String(), "Would send conversation documents from 1 conversation: 1 document, 0 skipped conversations.") {
		t.Fatalf("output = %q", output.String())
	}
}

type fakeReexamineBackfillClient struct {
	reexamineCalls int
	syncCalls      int
	deliveredDocs  int
	closed         bool
}

func (c *fakeReexamineBackfillClient) SyncConversationManifest(_ context.Context, _ string, _ []semsearch.Fingerprint) ([]string, error) {
	c.syncCalls++
	return nil, nil
}

func (c *fakeReexamineBackfillClient) ReexamineConversationDocuments(_ context.Context, _ string, docs []semsearch.SemDoc, _ []semsearch.Fingerprint) (string, error) {
	c.reexamineCalls++
	c.deliveredDocs = len(docs)
	return "job-test", nil
}

func (c *fakeReexamineBackfillClient) Close() error {
	c.closed = true
	return nil
}

// TestBackfillConversationDocumentsExecuteReexamines proves an --execute backfill
// forces re-examination of the delivered conversation rather than a plain upsert,
// so an old conversation whose fingerprint is unchanged still gets re-indexed.
func TestBackfillConversationDocumentsExecuteReexamines(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one": {{Role: "user", Text: "one"}},
		},
	}
	client := &fakeReexamineBackfillClient{}

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: false, Limit: 0, ConversationID: "claude:one"},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			return client, nil
		},
	)
	if err != nil {
		t.Fatalf("run backfill conversation documents: %v", err)
	}
	if client.reexamineCalls != 1 {
		t.Fatalf("ReexamineConversationDocuments calls = %d, want 1 (execute must force re-examination)", client.reexamineCalls)
	}
	if client.deliveredDocs != 1 {
		t.Fatalf("delivered documents = %d, want 1", client.deliveredDocs)
	}
	if !client.closed {
		t.Fatal("backfill did not close the semantic client")
	}
	if !strings.Contains(output.String(), "Sent conversation documents from 1 conversation") {
		t.Fatalf("output = %q", output.String())
	}
}

// TestBackfillConversationDocumentsUncappedExecuteRefusedWithoutFlag proves the
// guard against a blind full reexamine: a bare --execute with no --conversation
// and no --limit is refused, and the command never touches the index or dials the
// engine.
func TestBackfillConversationDocumentsUncappedExecuteRefusedWithoutFlag(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one": {{Role: "user", Text: "one"}},
		},
	}
	dialCalled := false

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: false, Limit: 0, ConversationID: "", Cursor: "", AllowFullReexamine: false},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	)
	if err == nil {
		t.Fatal("expected an error refusing the uncapped --execute reexamine")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Fatalf("error = %v, want guidance to pass --all", err)
	}
	if index.refreshCount != 0 {
		t.Fatalf("refresh count = %d, want 0 (guard fails before touching the index)", index.refreshCount)
	}
	if dialCalled {
		t.Fatal("refused run dialed the semantic client")
	}
}

// TestBackfillConversationDocumentsUncappedExecuteAllowedWithFlag proves the
// explicit full-run opt-in still works: --all lets a bare --execute reexamine the
// whole corpus.
func TestBackfillConversationDocumentsUncappedExecuteAllowedWithFlag(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
			testDocumentStampedRecord("claude:two", 20),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one": {{Role: "user", Text: "one"}},
			"claude:two": {{Role: "assistant", Text: "two"}},
		},
	}
	client := &fakeReexamineBackfillClient{}

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: false, Limit: 0, ConversationID: "", Cursor: "", AllowFullReexamine: true},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			return client, nil
		},
	)
	if err != nil {
		t.Fatalf("run backfill conversation documents: %v", err)
	}
	if client.reexamineCalls != 1 {
		t.Fatalf("ReexamineConversationDocuments calls = %d, want 1", client.reexamineCalls)
	}
	if client.deliveredDocs != 2 {
		t.Fatalf("delivered documents = %d, want 2 (whole corpus)", client.deliveredDocs)
	}
	if !strings.Contains(output.String(), "Sent conversation documents from 2 conversations") {
		t.Fatalf("output = %q", output.String())
	}
}

// TestBackfillConversationDocumentsCursorRunPrintsNextCursor proves a bounded run
// resumes after the cursor and prints the next cursor so the operator can chain.
func TestBackfillConversationDocumentsCursorRunPrintsNextCursor(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
			testDocumentStampedRecord("claude:two", 20),
			testDocumentStampedRecord("claude:three", 30),
			testDocumentStampedRecord("claude:four", 40),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one":   {{Role: "user", Text: "one"}},
			"claude:two":   {{Role: "assistant", Text: "two"}},
			"claude:three": {{Role: "user", Text: "three"}},
			"claude:four":  {{Role: "assistant", Text: "four"}},
		},
	}

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: true, Limit: 2, ConversationID: "", Cursor: "claude:one"},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatalf("run backfill conversation documents: %v", err)
	}

	if strings.Join(index.loadedIDs, ",") != "claude:two,claude:three" {
		t.Fatalf("loaded ids = %v, want records after the cursor", index.loadedIDs)
	}
	if !strings.Contains(output.String(), "Next cursor: claude:three.") {
		t.Fatalf("output = %q, want a printed next cursor", output.String())
	}
}

// TestBackfillConversationDocumentsCursorWithoutLimitRefused proves a cursor is
// only for bounded pagination: an --execute --after run with no --limit and no
// --all is refused before the index is touched or the engine dialed, so it can
// never reexamine the whole suffix after the cursor uncapped.
func TestBackfillConversationDocumentsCursorWithoutLimitRefused(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
			testDocumentStampedRecord("claude:two", 20),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one": {{Role: "user", Text: "one"}},
			"claude:two": {{Role: "assistant", Text: "two"}},
		},
	}
	dialCalled := false

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: false, Limit: 0, ConversationID: "", Cursor: "claude:one", AllowFullReexamine: false},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			dialCalled = true
			return nil, errors.New("unexpected dial")
		},
	)
	if err == nil {
		t.Fatal("expected an error refusing an uncapped cursor run without --limit")
	}
	if !strings.Contains(err.Error(), "--after requires --limit") {
		t.Fatalf("error = %v, want guidance that --after requires --limit", err)
	}
	if index.refreshCount != 0 {
		t.Fatalf("refresh count = %d, want 0 (guard fails before touching the index)", index.refreshCount)
	}
	if dialCalled {
		t.Fatal("refused run dialed the semantic client")
	}
}

// TestBackfillConversationDocumentsCursorSuppressedWhenSkipped proves a bounded
// cursor batch that skips a conversation (a load or build failure) emits no next
// cursor, so following the cursor cannot permanently omit the skipped record on
// the next run.
func TestBackfillConversationDocumentsCursorSuppressedWhenSkipped(t *testing.T) {
	t.Parallel()
	output := &bytes.Buffer{}
	// claude:two is absent from messagesByID, so it fails to load and is skipped.
	index := fakeDocumentBackfillIndex{
		stampedRecords: []conversation.StampedRecord{
			testDocumentStampedRecord("claude:one", 10),
			testDocumentStampedRecord("claude:two", 20),
			testDocumentStampedRecord("claude:three", 30),
			testDocumentStampedRecord("claude:four", 40),
		},
		messagesByID: map[string][]transcript.Message{
			"claude:one":   {{Role: "user", Text: "one"}},
			"claude:three": {{Role: "user", Text: "three"}},
			"claude:four":  {{Role: "assistant", Text: "four"}},
		},
	}

	err := runBackfillConversationDocumentsWithDeps(
		context.Background(),
		testDocumentBackfillFactory(output),
		backfillConversationDocumentsOptions{DryRun: true, Limit: 2, ConversationID: "", Cursor: "claude:one"},
		&index,
		func(context.Context, string) (conversationDocumentBackfillClient, error) {
			return nil, errors.New("unexpected dial")
		},
	)
	if err != nil {
		t.Fatalf("run backfill conversation documents: %v", err)
	}

	// The batch after the cursor is claude:two, claude:three; both are attempted,
	// claude:two is skipped, and the run would otherwise advance to claude:three.
	if strings.Join(index.loadedIDs, ",") != "claude:two,claude:three" {
		t.Fatalf("loaded ids = %v, want the two records after the cursor", index.loadedIDs)
	}
	if strings.Contains(output.String(), "Next cursor:") {
		t.Fatalf("output = %q, want no next cursor when a conversation was skipped", output.String())
	}
}

func testDocumentStampedRecord(conversationID string, unixSeconds int64) conversation.StampedRecord {
	return conversation.StampedRecord{
		Record: testBackfillRecord(conversationID, "/repo", false),
		Stamp: conversation.FileStamp{
			Size:  int64(len(conversationID)),
			Mtime: time.Unix(unixSeconds, 0),
		},
	}
}

func testDocumentBackfillFactory(output *bytes.Buffer) *cli.Factory {
	return &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  strings.NewReader(""),
			Out: output,
			Err: &bytes.Buffer{},
		},
		Logger: nil,
		Build:  cli.BuildInfo{Version: "test", Commit: "", Date: ""},
		Verbose: func() bool {
			return false
		},
		Config: func() (*config.Config, error) {
			return &config.Config{}, nil
		},
	}
}
