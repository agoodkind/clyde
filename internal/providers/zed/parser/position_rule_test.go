package parser

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// TestStreamKeepsEarlierMessagePositionsAfterZedThreadRewrite writes a Zed thread
// with two messages and streams it. The test rewrites the same thread row with
// those two messages plus one appended message and a later updated_at, then streams
// again. Every message from the first stream must keep its index and exact Text in
// the second stream, and the second stream must return one more message.
func TestStreamKeepsEarlierMessagePositionsAfterZedThreadRewrite(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	firstUpdatedAt := time.Date(2026, time.June, 27, 16, 0, 0, 0, time.UTC)
	secondUpdatedAt := time.Date(2026, time.June, 27, 16, 5, 0, 0, time.UTC)
	firstThreadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T16:00:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"first question"}]}},
			{"Agent":{"content":[{"Text":"first answer"}],"tool_results":{}}}
		]
	}`)
	secondThreadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T16:05:00Z",
		"messages":[
			{"User":{"id":"user-1","content":[{"Text":"first question"}]}},
			{"Agent":{"content":[{"Text":"first answer"}],"tool_results":{}}},
			{"User":{"id":"user-2","content":[{"Text":"follow up question"}]}}
		]
	}`)
	options := conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
		IncludeInjected:       false,
		HarnessTally:          nil,
	}

	writeThreadsRow(t, root, "thread-append", "", firstUpdatedAt, firstThreadJSON)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "thread-append", "", "Thread title", "", firstUpdatedAt)

	p := New()
	first := discoverAndStreamSingleZedThread(t, p, options)
	if len(first) < 2 {
		t.Fatalf("first stream len = %d, want at least 2", len(first))
	}

	rewriteThreadsRow(t, root, "thread-append", secondUpdatedAt, secondThreadJSON)

	second := discoverAndStreamSingleZedThread(t, p, options)
	if len(second) != len(first)+1 {
		t.Fatalf("second stream len = %d, want %d", len(second), len(first)+1)
	}
	for i, message := range first {
		if second[i].Text != message.Text {
			t.Fatalf("second stream message %d Text = %q, want %q from the first stream", i, second[i].Text, message.Text)
		}
	}
	if second[len(first)].Text != "follow up question" {
		t.Fatalf("appended message Text = %q, want %q", second[len(first)].Text, "follow up question")
	}
}

func discoverAndStreamSingleZedThread(t *testing.T, p *Parser, options conversation.LoadOptions) []transcript.Message {
	t.Helper()
	candidates, err := p.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	messages, err := conversation.CollectMessages(p.Stream(candidates[0].Path, options))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	return messages
}

// rewriteThreadsRow replaces the data and updated_at of an existing threads row.
// Zed stores each thread as one row and rewrites the whole document on update.
func rewriteThreadsRow(t *testing.T, root, sessionID string, updatedAt time.Time, data []byte) {
	t.Helper()
	dbPath := filepath.Join(root, "threads", "threads.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open threads db: %v", err)
	}
	defer func() { _ = db.Close() }()
	result, err := db.Exec(`UPDATE threads SET updated_at = ?, data = ? WHERE id = ?`, updatedAt.Format(time.RFC3339), data, sessionID)
	if err != nil {
		t.Fatalf("update threads row: %v", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("threads row rows affected: %v", err)
	}
	if affected != 1 {
		t.Fatalf("threads rows updated = %d, want 1", affected)
	}
}
