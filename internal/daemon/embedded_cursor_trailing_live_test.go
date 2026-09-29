//go:build live

package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEmbeddedCursorTrailingTurnLive drives the embedded sync worker over a
// real Cursor agent transcript with the real embedding endpoint and a Milvus
// database that the test creates. The first pass sees a user turn and an
// assistant turn with "part one". A later line extends the same assistant
// turn with "part two", and a user line then closes it. Every artifact write
// leaves the transcript younger than embeddedTrailingSettleWindow and older
// than the pass deferral. The published occurrence of the assistant turn must
// contain the appended text. The library at the Clyde pin has no search
// method, and the test reads the published occurrence source text from the
// catalog.
func TestEmbeddedCursorTrailingTurnLive(t *testing.T) {
	if strings.TrimSpace(os.Getenv(liveEmbeddingAPIKeyEnv)) == "" {
		t.Fatalf("environment variable %s is required for the embedding endpoint", liveEmbeddingAPIKeyEnv)
	}
	t.Logf("embedding window start %s", time.Now().UTC().Format(time.RFC3339))
	t.Cleanup(func() {
		t.Logf("embedding window end %s", time.Now().UTC().Format(time.RFC3339))
	})
	stores := isolateEmbeddedProjectionStores(t)
	database := createLiveMilvusDatabase(t)
	storeRoot := t.TempDir()
	transcriptPath := filepath.Join(stores.cursorProjects, embeddedProjectionCursorProjectKey, "agent-transcripts",
		embeddedProjectionCursorConversation, embeddedProjectionCursorConversation+".jsonl")
	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	index := newEmbeddedProjectionIndex()
	scenario := newLiveScenario(t, database, storeRoot, "cursor-trailing", index)
	for _, lines := range [][]string{
		{
			`{"role":"user","message":{"content":[{"type":"text","text":"open the config"}]}}`,
			`{"role":"assistant","message":{"content":[{"type":"text","text":"part one"}]}}`,
		},
		{`{"role":"assistant","message":{"content":[{"type":"text","text":"part two"}]}}`},
		{`{"role":"user","message":{"content":[{"type":"text","text":"now apply it"}]}}`},
	} {
		appendEmbeddedProjectionLines(t, transcriptPath, lines)
		setTrailingTestAge(t, transcriptPath, trailingTestQuietAge)
		refreshLiveIndex(t, index)
		if err := scenario.worker.runPass(t.Context()); err != nil {
			t.Fatalf("run embedded pass: %v", err)
		}
	}
	userText := liveCommittedFieldText(t, scenario.semantic.CatalogPath, "/m0/chat/")
	assistantText := liveCommittedFieldText(t, scenario.semantic.CatalogPath, "/m1/chat/")
	if userText != "open the config" || assistantText != "part one\npart two" {
		t.Fatalf("published user/assistant text = %q/%q, want %q/%q", userText, assistantText, "open the config", "part one\npart two")
	}
	scenario.close(t)
}

// liveCommittedFieldText returns the published source text of every part of
// the field with a row key that contains marker, joined in row key order.
func liveCommittedFieldText(t *testing.T, catalogPath string, marker string) string {
	t.Helper()
	catalog := openLiveReadOnly(t, catalogPath)
	rows, err := catalog.QueryContext(t.Context(),
		`SELECT source_blobs.content FROM occurrences JOIN source_blobs ON source_blobs.blob_id = occurrences.source_blob_id
		WHERE occurrences.row_key LIKE ? ORDER BY occurrences.row_key`, "%"+marker+"%")
	if err != nil {
		t.Fatalf("read published %s text: %v", marker, err)
	}
	defer func() { _ = rows.Close() }()
	var parts []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatalf("scan published %s text: %v", marker, err)
		}
		parts = append(parts, content)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read published %s text: %v", marker, err)
	}
	return strings.Join(parts, "")
}
