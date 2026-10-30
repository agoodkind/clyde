package parser_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providers/cursor/parser"
)

const sourceTimesConversationID = "77777777-7777-4777-8777-777777777777"

// TestTranscriptMessagesTakeTimesFromTheComposerStore checks source timestamp matching.
// It also checks existing, ambiguous, and unmatched transcript timestamps.
func TestTranscriptMessagesTakeTimesFromTheComposerStore(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-source-times", sourceTimesConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"first question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"repeated answer"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>2026-05-20T09:00:00Z</timestamp>\n<user_query>\nsecond question\n</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"read_file","input":{"path":"main.go"}}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"third question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"same second reply"}]}}`,
		`{"role":"user","message":{"content":[{"type":"text","text":"listed last but dated first"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"answer the store never saved"}]}}`,
	})

	prefix := "bubbleId:" + sourceTimesConversationID + ":"
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + sourceTimesConversationID + `', '{"composerId":"` + sourceTimesConversationID + `","name":"Source Times","createdAt":1779267600000,"lastUpdatedAt":1779268000000,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2},{"bubbleId":"b4","type":1},{"bubbleId":"b5","type":2},{"bubbleId":"b6","type":1},{"bubbleId":"b7","type":2},{"bubbleId":"b8","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b1', '{"_v":3,"type":1,"bubbleId":"b1","text":"first question","createdAt":"2026-05-20T10:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b2', '{"_v":3,"type":2,"bubbleId":"b2","text":"repeated answer","createdAt":"2026-05-20T10:01:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b3', '{"_v":3,"type":2,"bubbleId":"b3","text":"","createdAt":"2026-05-20T10:02:00.000Z","toolFormerData":{"name":"read_file","rawArgs":"{\"path\":\"main.go\"}","status":"completed"}}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b4', '{"_v":3,"type":1,"bubbleId":"b4","text":"third question","createdAt":"2026-05-20T10:05:00.100Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b5', '{"_v":3,"type":2,"bubbleId":"b5","text":"same second reply","createdAt":"2026-05-20T10:05:00.900Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b6', '{"_v":3,"type":1,"bubbleId":"b6","text":"listed last but dated first","createdAt":"2026-05-20T08:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b7', '{"_v":3,"type":2,"bubbleId":"b7","text":"repeated answer","createdAt":"2026-05-20T10:06:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('` + prefix + `b8', '{"_v":3,"type":1,"bubbleId":"b8","text":"a turn the transcript lacks","createdAt":"2026-05-20T10:07:00.000Z"}')`,
	})

	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	want := []time.Time{
		time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC),
		{},
		time.Date(2026, time.May, 20, 9, 0, 0, 0, time.UTC),
		time.Date(2026, time.May, 20, 10, 2, 0, 0, time.UTC),
		time.Date(2026, time.May, 20, 10, 5, 0, 100_000_000, time.UTC),
		time.Date(2026, time.May, 20, 10, 5, 0, 900_000_000, time.UTC),
		time.Date(2026, time.May, 20, 8, 0, 0, 0, time.UTC),
		{},
	}
	if len(messages) != len(want) {
		t.Fatalf("message count differs from expected %d, %d", len(messages), len(want))
	}
	for position, message := range messages {
		if !message.Timestamp.Equal(want[position]) {
			t.Errorf("message timestamp differs from expected at position %d (%s %q): %v, %v", position, message.Role, message.Text, message.Timestamp, want[position])
		}
	}
}

// TestTranscriptWithoutAComposerRecordKeepsZeroTimes checks timestamps without a composer record.
func TestTranscriptWithoutAComposerRecordKeepsZeroTimes(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)
	transcriptPath := writeSourceTimesTranscript(t, projectsDir, "Users-alice-source-times", sourceTimesConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"first question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"an answer"}]}}`,
	})
	createSourceTimesGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), nil)

	messages, err := conversation.CollectMessages(parser.New().Stream(transcriptPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("stream transcript: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("message count differs from expected %d, %d", len(messages), 2)
	}
	for position, message := range messages {
		if !message.Timestamp.IsZero() {
			t.Errorf("message has an unexpected timestamp at position %d: %v", position, message.Timestamp)
		}
	}
}

func writeSourceTimesTranscript(t *testing.T, root string, projectKey string, conversationID string, lines []string) string {
	t.Helper()
	path := filepath.Join(root, projectKey, "agent-transcripts", conversationID, conversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll transcript dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile transcript: %v", err)
	}
	return path
}

func createSourceTimesGlobalDB(t *testing.T, dbPath string, extra []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	statements := append([]string{"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)", "CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)"}, extra...)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("exec global statement %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close global db: %v", err)
	}
}
