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
	"goodkind.io/clyde/internal/transcript"
)

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

	selected, err := selectBackfillDocumentRecords([]conversation.StampedRecord{
		stale,
		other,
		fresh,
	}, 0, "")
	if err != nil {
		t.Fatalf("select document records: %v", err)
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

	docs, manifest, skipped := buildBackfillConversationDocuments(context.Background(), &index, stampedRecords)

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
	for _, opts := range index.loadOptions {
		if !opts.IncludeToolOutputs {
			t.Fatalf("load options = %+v, want IncludeToolOutputs true", opts)
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
	if !strings.Contains(output.String(), "Would send conversation documents from 1 conversations: 1 documents, 0 skipped conversations.") {
		t.Fatalf("output = %q", output.String())
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
