package parser_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providers/codex/parser"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
)

const (
	compressedRolloutThreadID = "019de9cc-3a00-7010-bd9f-a6ee71559357"
	compressedRolloutName     = "rollout-2026-05-02T10-09-00-" + compressedRolloutThreadID
	compressedRolloutBody     = `{"timestamp":"2026-05-02T17:09:00.000Z","type":"session_meta","payload":{"id":"` + compressedRolloutThreadID + `","timestamp":"2026-05-02T17:09:00.000Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}
{"timestamp":"2026-05-02T17:09:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"first question about retention"}}
{"timestamp":"2026-05-02T17:09:02.000Z","type":"event_msg","payload":{"type":"agent_message","message":"first answer about retention"}}
{"timestamp":"2026-05-02T17:09:03.000Z","type":"event_msg","payload":{"type":"user_message","message":"second question about snapshots"}}
{"timestamp":"2026-05-02T17:09:04.000Z","type":"event_msg","payload":{"type":"agent_message","message":"second answer about snapshots"}}
`
	compressedRolloutCenter = 1
	corruptRolloutContent   = "this is not a zstd frame"
)

type compressedRolloutFixture struct {
	index      *conversation.Index
	sessionDir string
}

func newCompressedRolloutFixture(t *testing.T) compressedRolloutFixture {
	t.Helper()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("CODEX_SQLITE_HOME", codexHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	sessionDir := filepath.Join(codexHome, "sessions", "2026", "05", "02")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("the test could not create the session directory: %v", err)
	}
	registry := conversation.NewRegistry()
	registry.Register(parser.New())
	return compressedRolloutFixture{
		index:      conversation.NewIndex(registry, config.ConversationConfig{IncludeSubagentConversations: true, Semantic: config.ConversationSemanticConfig{}}),
		sessionDir: sessionDir,
	}
}

func (fixture compressedRolloutFixture) plainPath() string {
	return filepath.Join(fixture.sessionDir, compressedRolloutName+".jsonl")
}

func (fixture compressedRolloutFixture) compressedPath() string {
	return fixture.plainPath() + ".zst"
}

func (fixture compressedRolloutFixture) writePlain(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(fixture.plainPath(), []byte(compressedRolloutBody), 0o600); err != nil {
		t.Fatalf("the test could not write the plain rollout: %v", err)
	}
}

func (fixture compressedRolloutFixture) writeCompressed(t *testing.T) {
	t.Helper()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("the test could not create the zstd encoder: %v", err)
	}
	defer func() { _ = encoder.Close() }()
	if err := os.WriteFile(fixture.compressedPath(), encoder.EncodeAll([]byte(compressedRolloutBody), nil), 0o600); err != nil {
		t.Fatalf("the test could not write the compressed rollout: %v", err)
	}
}

func (fixture compressedRolloutFixture) writeCorrupt(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(fixture.compressedPath(), []byte(corruptRolloutContent), 0o600); err != nil {
		t.Fatalf("the test could not write the compressed rollout: %v", err)
	}
}

func (fixture compressedRolloutFixture) writeTruncated(t *testing.T) {
	t.Helper()
	fixture.writeCompressed(t)
	compressed, err := os.ReadFile(fixture.compressedPath())
	if err != nil {
		t.Fatalf("the test could not read the compressed rollout: %v", err)
	}
	if err := os.WriteFile(fixture.compressedPath(), compressed[:len(compressed)/2], 0o600); err != nil {
		t.Fatalf("the test could not write the compressed rollout: %v", err)
	}
}

func (fixture compressedRolloutFixture) refreshedRecord(t *testing.T) conversation.Record {
	t.Helper()
	if err := fixture.index.Refresh(context.Background()); err != nil {
		t.Fatalf("the conversation index could not refresh: %v", err)
	}
	record, found := fixture.index.RecordByID("codex:" + compressedRolloutThreadID)
	if !found {
		t.Fatal("the conversation index did not find the expected conversation")
	}
	return record
}

func (fixture compressedRolloutFixture) recordWithRemovedPlainPath(t *testing.T) conversation.Record {
	t.Helper()
	fixture.writePlain(t)
	record := fixture.refreshedRecord(t)
	removeRollout(t, fixture.plainPath())
	return record
}

func (fixture compressedRolloutFixture) recordWithRemovedCompressedPath(t *testing.T) conversation.Record {
	t.Helper()
	fixture.writeCompressed(t)
	record := fixture.refreshedRecord(t)
	removeRollout(t, fixture.compressedPath())
	return record
}

func removeRollout(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("the test could not remove the rollout: %v", err)
	}
}

func compressedRolloutReadError(compressedRolloutFixture) string {
	return "read codex rollout"
}

func compressedRolloutExportOptions() conversation.ExportOptions {
	return conversation.ExportOptions{
		Format:       conversation.ExportFormatPlainText,
		HistoryStart: 0,
		LastN:        0,
		MaxLines:     0,
		MaxTokens:    "",
		TokenModel:   "",
		Whitespace:   conversation.WhitespacePreserve,
		Content:      conversation.NewContentKindSet(conversation.ContentKindChat),
		Compaction:   conversation.CompactionExportOptions{IncludeSelector: "", FullHistory: true},
	}
}

// TestIndexReadsCompressedCodexRollouts checks that discovery and stale paths produce the same conversation identity, context window, and chat export as a plain rollout.
func TestIndexReadsCompressedCodexRollouts(t *testing.T) {
	plain := newCompressedRolloutFixture(t)
	plain.writePlain(t)
	plainRecord := plain.refreshedRecord(t)
	wantContext, err := plain.index.HitContextWindow(plainRecord, compressedRolloutCenter, 1, 1, "")
	if err != nil {
		t.Fatalf("the conversation index could not read the context window: %v", err)
	}
	wantExport, err := plain.index.Export(plainRecord, compressedRolloutExportOptions())
	if err != nil {
		t.Fatalf("the conversation index could not export the conversation: %v", err)
	}
	if !strings.Contains(wantContext, "first answer about retention") || !strings.Contains(string(wantExport), "second answer about snapshots") {
		t.Fatalf("the context window or export from the plain rollout lacks the expected text: %q, %q", wantContext, wantExport)
	}

	tests := []struct {
		name      string
		record    func(compressedRolloutFixture, *testing.T) conversation.Record
		wantStore func(compressedRolloutFixture) string
	}{
		{
			name: "compressed rollout found by discovery",
			record: func(fixture compressedRolloutFixture, t *testing.T) conversation.Record {
				t.Helper()
				fixture.writeCompressed(t)
				return fixture.refreshedRecord(t)
			},
			wantStore: compressedRolloutFixture.compressedPath,
		},
		{
			name: "stored plain path after compression",
			record: func(fixture compressedRolloutFixture, t *testing.T) conversation.Record {
				t.Helper()
				record := fixture.recordWithRemovedPlainPath(t)
				fixture.writeCompressed(t)
				return record
			},
			wantStore: compressedRolloutFixture.plainPath,
		},
		{
			name: "stored compressed path after decompression",
			record: func(fixture compressedRolloutFixture, t *testing.T) conversation.Record {
				t.Helper()
				record := fixture.recordWithRemovedCompressedPath(t)
				fixture.writePlain(t)
				return record
			},
			wantStore: compressedRolloutFixture.compressedPath,
		},
		{
			name: "plain and compressed copies found once",
			record: func(fixture compressedRolloutFixture, t *testing.T) conversation.Record {
				t.Helper()
				fixture.writePlain(t)
				fixture.writeCompressed(t)
				return fixture.refreshedRecord(t)
			},
			wantStore: compressedRolloutFixture.plainPath,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCompressedRolloutFixture(t)
			record := testCase.record(fixture, t)
			if record.ID != plainRecord.ID || record.ArtifactPath != testCase.wantStore(fixture) {
				t.Fatalf("the record does not match the expected conversation identity or artifact path: %q, %q", record.ID, record.ArtifactPath)
			}
			gotContext, err := fixture.index.HitContextWindow(record, compressedRolloutCenter, 1, 1, "")
			if err != nil {
				t.Fatalf("the conversation index could not read the context window: %v", err)
			}
			if gotContext != wantContext {
				t.Fatalf("the context window differs from the plain rollout baseline: %q, %q", gotContext, wantContext)
			}
			gotExport, err := fixture.index.Export(record, compressedRolloutExportOptions())
			if err != nil {
				t.Fatalf("the conversation index could not export the conversation: %v", err)
			}
			if string(gotExport) != string(wantExport) {
				t.Fatalf("the export differs from the plain rollout baseline: %q, %q", gotExport, wantExport)
			}
		})
	}
}

// TestIndexReportsUnreadableCompressedCodexRollouts checks that context reads and exports report errors for corrupt, truncated, and missing rollouts.
func TestIndexReportsUnreadableCompressedCodexRollouts(t *testing.T) {
	tests := []struct {
		name    string
		replace func(compressedRolloutFixture, *testing.T)
		wantErr func(compressedRolloutFixture) string
	}{
		{
			name:    "corrupt compressed rollout",
			replace: compressedRolloutFixture.writeCorrupt,
			wantErr: compressedRolloutReadError,
		},
		{
			name:    "truncated compressed rollout",
			replace: compressedRolloutFixture.writeTruncated,
			wantErr: compressedRolloutReadError,
		},
		{
			name:    "missing plain and compressed rollout",
			replace: func(compressedRolloutFixture, *testing.T) {},
			wantErr: func(fixture compressedRolloutFixture) string {
				plainPath := fixture.plainPath()
				return "open codex rollout " + plainPath + ": open " + plainPath + ": no such file"
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCompressedRolloutFixture(t)
			record := fixture.recordWithRemovedPlainPath(t)
			testCase.replace(fixture, t)
			wantErr := testCase.wantErr(fixture)
			if _, err := fixture.index.HitContextWindow(record, compressedRolloutCenter, 1, 1, ""); err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("reading the context window did not return the expected error: %v", err)
			}
			if _, err := fixture.index.Export(record, compressedRolloutExportOptions()); err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Fatalf("exporting the conversation did not return the expected error: %v", err)
			}
		})
	}
}

// TestCorruptCompressedRolloutKeepsFilenameIdentity checks that discovery and filename parsing preserve a corrupt compressed rollout's thread identity.
func TestCorruptCompressedRolloutKeepsFilenameIdentity(t *testing.T) {
	fixture := newCompressedRolloutFixture(t)
	fixture.writeCorrupt(t)
	record := fixture.refreshedRecord(t)
	if record.ArtifactPath != fixture.compressedPath() {
		t.Fatalf("the record does not match the expected conversation identity or artifact path: %q, %q", record.ID, record.ArtifactPath)
	}
	identity, _, identified := codexstore.RolloutIdentityFromFilename(filepath.Base(fixture.compressedPath()))
	if !identified || identity != compressedRolloutThreadID {
		t.Fatalf("the compressed filename did not produce the expected thread identity: %q, %t", identity, identified)
	}
}
