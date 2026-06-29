package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestDiscoverReturnsVirtualZedThreadCandidate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}
	now := "2026-06-27T09:00:00Z"

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE threads(
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			summary TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			data_type TEXT NOT NULL,
			data BLOB NOT NULL,
			created_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	metadataPath := filepath.Join(root, "db", "0-stable", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	metadataDB, err := sql.Open("sqlite3", "file:"+metadataPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db returned error: %v", err)
	}
	t.Cleanup(func() { _ = metadataDB.Close() })
	if _, err := metadataDB.Exec(`
		CREATE TABLE sidebar_threads(
			session_id TEXT PRIMARY KEY,
			agent_id TEXT,
			title TEXT NOT NULL,
			title_override TEXT,
			updated_at TEXT NOT NULL,
			created_at TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			archived INTEGER DEFAULT 0,
			main_worktree_paths TEXT,
			main_worktree_paths_order TEXT
		) STRICT
	`); err != nil {
		t.Fatalf("create sidebar_threads table: %v", err)
	}
	if _, err := metadataDB.Exec(
		`INSERT INTO sidebar_threads(session_id, agent_id, title, title_override, updated_at, created_at, folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"Thread title",
		"",
		now,
		now,
		"/repo",
		"0",
		0,
		"/repo",
		"0",
	); err != nil {
		t.Fatalf("insert sidebar row: %v", err)
	}

	payload := []byte(`{"version":"0.3.0","title":"Thread","updated_at":"2026-06-27T09:00:00Z","messages":[]}`)
	if _, err := db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"",
		"",
		"Summary",
		now,
		"json",
		payload,
		now,
	); err != nil {
		t.Fatalf("insert thread row: %v", err)
	}

	candidates, err := New().Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
	parsed, err := ParseVirtualPath(candidates[0].Path)
	if err != nil {
		t.Fatalf("ParseVirtualPath returned error: %v", err)
	}
	if parsed.Channel != "0-stable" || parsed.SessionID != "thread-1" {
		t.Fatalf("parsed virtual path = %#v", parsed)
	}
}

func TestDiscoverSkipsUnreadableThreadsDatabaseCandidate(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}
	now := "2026-06-27T09:00:00Z"

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE threads(
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			summary TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			data_type TEXT NOT NULL,
			data BLOB NOT NULL,
			created_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	metadataPath := filepath.Join(root, "db", "0-stable", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	metadataDB, err := sql.Open("sqlite3", "file:"+metadataPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db returned error: %v", err)
	}
	t.Cleanup(func() { _ = metadataDB.Close() })
	if _, err := metadataDB.Exec(`
		CREATE TABLE sidebar_threads(
			session_id TEXT PRIMARY KEY,
			agent_id TEXT,
			title TEXT NOT NULL,
			title_override TEXT,
			updated_at TEXT NOT NULL,
			created_at TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			archived INTEGER DEFAULT 0,
			main_worktree_paths TEXT,
			main_worktree_paths_order TEXT
		) STRICT
	`); err != nil {
		t.Fatalf("create sidebar_threads table: %v", err)
	}
	if _, err := metadataDB.Exec(
		`INSERT INTO sidebar_threads(session_id, agent_id, title, title_override, updated_at, created_at, folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"Thread title",
		"",
		now,
		now,
		"/repo",
		"0",
		0,
		"/repo",
		"0",
	); err != nil {
		t.Fatalf("insert sidebar row: %v", err)
	}

	if _, err := db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"",
		"",
		"Summary",
		now,
		"json",
		[]byte("{not json"),
		now,
	); err != nil {
		t.Fatalf("insert thread row: %v", err)
	}

	candidates, err := New().Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates len = %d, want 0", len(candidates))
	}
}

func TestDiscoverReturnsVirtualZedThreadCandidateForZstdPayload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}
	now := "2026-06-27T09:00:00Z"

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE threads(
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			summary TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			data_type TEXT NOT NULL,
			data BLOB NOT NULL,
			created_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	metadataPath := filepath.Join(root, "db", "0-stable", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	metadataDB, err := sql.Open("sqlite3", "file:"+metadataPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db returned error: %v", err)
	}
	t.Cleanup(func() { _ = metadataDB.Close() })
	if _, err := metadataDB.Exec(`
		CREATE TABLE sidebar_threads(
			session_id TEXT PRIMARY KEY,
			agent_id TEXT,
			title TEXT NOT NULL,
			title_override TEXT,
			updated_at TEXT NOT NULL,
			created_at TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			archived INTEGER DEFAULT 0,
			main_worktree_paths TEXT,
			main_worktree_paths_order TEXT
		) STRICT
	`); err != nil {
		t.Fatalf("create sidebar_threads table: %v", err)
	}
	if _, err := metadataDB.Exec(
		`INSERT INTO sidebar_threads(session_id, agent_id, title, title_override, updated_at, created_at, folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"Thread title",
		"",
		now,
		now,
		"/repo",
		"0",
		0,
		"/repo",
		"0",
	); err != nil {
		t.Fatalf("insert sidebar row: %v", err)
	}

	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter returned error: %v", err)
	}
	payload := encoder.EncodeAll([]byte(`{"version":"0.3.0","title":"Thread","updated_at":"2026-06-27T09:00:00Z","messages":[]}`), nil)
	if closeErr := encoder.Close(); closeErr != nil {
		t.Fatalf("zstd encoder close: %v", closeErr)
	}

	if _, err := db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"thread-1",
		"",
		"",
		"",
		"Summary",
		now,
		"zstd",
		payload,
		now,
	); err != nil {
		t.Fatalf("insert thread row: %v", err)
	}

	candidates, err := New().Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}
}

func TestScanRecordDerivesFieldsFromDiscoveredThreadAndMetadata(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	rowCreatedAt := time.Date(2026, time.June, 26, 7, 0, 0, 0, time.UTC)
	rowUpdatedAt := time.Date(2026, time.June, 27, 12, 0, 0, 0, time.UTC)
	metadataCreatedAt := time.Date(2026, time.June, 25, 9, 0, 0, 0, time.UTC)
	metadataUpdatedAt := time.Date(2026, time.June, 27, 11, 0, 0, 0, time.UTC)

	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Payload title",
		"updated_at":"2026-06-27T10:30:00Z",
		"model":{"provider":"anthropic","model":"claude-sonnet"},
		"subagent_context":{"parent_thread_id":"parent-from-subagent","depth":1},
		"messages":[]
	}`)

	writeThreadRow(t, root, threadRowOptions{
		ThreadID:  "thread-1",
		ParentID:  "parent-from-row",
		UpdatedAt: rowUpdatedAt,
		CreatedAt: rowCreatedAt,
		DataType:  "json",
		Data:      threadJSON,
	})
	writeSidebarThreadRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), sidebarThreadRowOptions{
		SessionID:              "thread-1",
		Title:                  "Metadata title",
		TitleOverride:          "User title",
		UpdatedAt:              metadataUpdatedAt,
		CreatedAt:              metadataCreatedAt,
		FolderPaths:            "/repo/a\n/repo/b",
		FolderPathsOrder:       "1,0",
		Archived:               true,
		MainWorktreePaths:      "/repo/a\n/repo/b",
		MainWorktreePathsOrder: "1,0",
	})

	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates len = %d, want 1", len(candidates))
	}

	record, ok := parser.ScanRecord(candidates[0].Path, candidates[0].Stamp)
	if !ok {
		t.Fatal("ScanRecord returned ok=false")
	}
	if record.Title != "User title" {
		t.Fatalf("record.Title = %q, want user override", record.Title)
	}
	if record.WorkspaceRoot != "/repo/b" {
		t.Fatalf("record.WorkspaceRoot = %q, want ordered first folder path", record.WorkspaceRoot)
	}
	if record.Lineage == nil || record.Lineage.Kind != conversation.ConversationLineageKindSpawn || record.Lineage.ParentNativeID != "parent-from-subagent" {
		t.Fatalf("record.Lineage = %#v", record.Lineage)
	}
	if !record.Archived {
		t.Fatal("record.Archived = false, want true")
	}
	if !record.CreatedAt.Equal(metadataCreatedAt) {
		t.Fatalf("record.CreatedAt = %v, want %v", record.CreatedAt, metadataCreatedAt)
	}
	if !record.UpdatedAt.Equal(rowUpdatedAt) {
		t.Fatalf("record.UpdatedAt = %v, want %v", record.UpdatedAt, rowUpdatedAt)
	}
}

func TestStreamShapesVisibleZedMessagesWithoutCompactionByDefault(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	updatedAt := time.Date(2026, time.June, 27, 13, 0, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T13:00:00Z",
		"messages":[
			{"User":{"id":"user-empty","content":[{"Text":"   "}]}},
			{"User":{"id":"user-1","content":[{"Text":"hello"},{"Mention":{"uri":"file:///repo/readme","content":"README excerpt"}}]}},
			{"Agent":{"content":[{"Text":"   "},{"Thinking":{"text":"   ","signature":"sig"}}],"tool_results":{}}},
			{"Agent":{"content":[{"Text":"answer"},{"Thinking":{"text":"reasoning","signature":"sig"}},{"ToolUse":{"id":"call-1","name":"Read","input":{"path":"/repo/readme"}}}],"tool_results":{}}},
			"Resume",
			{"Compaction":{"Summary":"Earlier summary"}}
		]
	}`)

	writeThreadRow(t, root, threadRowOptions{
		ThreadID:  "thread-1",
		ParentID:  "",
		UpdatedAt: updatedAt,
		CreatedAt: updatedAt,
		DataType:  "json",
		Data:      threadJSON,
	})
	writeSidebarThreadRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), sidebarThreadRowOptions{
		SessionID:              "thread-1",
		Title:                  "Thread title",
		UpdatedAt:              updatedAt,
		CreatedAt:              updatedAt,
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		Archived:               false,
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
	})

	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	messages, err := conversation.CollectMessages(parser.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	if messages[0].Role != "user" || messages[0].Text != "hello\nREADME excerpt" {
		t.Fatalf("user message = %#v", messages[0])
	}
	if messages[1].Role != "assistant" || messages[1].Text != "answer" || messages[1].Thinking != "reasoning" {
		t.Fatalf("assistant message = %#v", messages[1])
	}
	if !messages[1].HasTools || len(messages[1].Tools) != 1 || messages[1].Tools[0].Name != "Read" {
		t.Fatalf("assistant tools = %#v", messages[1].Tools)
	}
	if messages[2].Role != "user" || messages[2].Text != "Continue where you left off" {
		t.Fatalf("resume message = %#v", messages[2])
	}
}

func TestStreamIncludesZedCompactionMessagesWhenRequested(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)

	updatedAt := time.Date(2026, time.June, 27, 13, 30, 0, 0, time.UTC)
	threadJSON := []byte(`{
		"version":"0.3.0",
		"title":"Thread title",
		"updated_at":"2026-06-27T13:30:00Z",
		"messages":[
			{"Compaction":{"Summary":"Earlier summary"}},
			{"Compaction":{"ProviderNative":{"provider":"anthropic","items":[{"type":"thinking"}]}}}
		]
	}`)

	writeThreadRow(t, root, threadRowOptions{
		ThreadID:  "thread-2",
		ParentID:  "",
		UpdatedAt: updatedAt,
		CreatedAt: updatedAt,
		DataType:  "json",
		Data:      threadJSON,
	})
	writeSidebarThreadRow(t, filepath.Join(root, "db", "0-preview", "db.sqlite"), sidebarThreadRowOptions{
		SessionID:              "thread-2",
		Title:                  "Thread title",
		UpdatedAt:              updatedAt,
		CreatedAt:              updatedAt,
		FolderPaths:            "/repo",
		FolderPathsOrder:       "0",
		Archived:               false,
		MainWorktreePaths:      "/repo",
		MainWorktreePathsOrder: "0",
	})

	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	messages, err := conversation.CollectMessages(parser.Stream(candidates[0].Path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: true,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("collect stream messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Compaction == nil || messages[0].Compaction.Kind != transcript.CompactionKindSummary || messages[0].Text != "Earlier summary" {
		t.Fatalf("summary compaction = %#v", messages[0])
	}
	if messages[1].Role != "system" || messages[1].Compaction == nil || messages[1].Compaction.Kind != transcript.CompactionKindBoundary || messages[1].Text != "anthropic compaction boundary" {
		t.Fatalf("provider-native compaction = %#v", messages[1])
	}
}

type threadRowOptions struct {
	ThreadID  string
	ParentID  string
	UpdatedAt time.Time
	CreatedAt time.Time
	DataType  string
	Data      []byte
}

func writeThreadRow(t *testing.T, root string, options threadRowOptions) {
	t.Helper()

	dbPath := filepath.Join(root, "threads", "threads.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir threads dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE threads(
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			summary TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			data_type TEXT NOT NULL,
			data BLOB NOT NULL,
			created_at TEXT NOT NULL
		) STRICT
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	if _, err := db.Exec(
		`INSERT INTO threads(id, parent_id, folder_paths, folder_paths_order, summary, updated_at, data_type, data, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		options.ThreadID,
		options.ParentID,
		"",
		"",
		"Summary",
		options.UpdatedAt.Format(time.RFC3339),
		options.DataType,
		options.Data,
		options.CreatedAt.Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert thread row: %v", err)
	}
}

type sidebarThreadRowOptions struct {
	SessionID              string
	Title                  string
	TitleOverride          string
	UpdatedAt              time.Time
	CreatedAt              time.Time
	FolderPaths            string
	FolderPathsOrder       string
	Archived               bool
	MainWorktreePaths      string
	MainWorktreePathsOrder string
}

func writeSidebarThreadRow(t *testing.T, dbPath string, options sidebarThreadRowOptions) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("mkdir metadata dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open metadata db returned error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`
		CREATE TABLE sidebar_threads(
			session_id TEXT PRIMARY KEY,
			agent_id TEXT,
			title TEXT NOT NULL,
			title_override TEXT,
			updated_at TEXT NOT NULL,
			created_at TEXT,
			folder_paths TEXT,
			folder_paths_order TEXT,
			archived INTEGER DEFAULT 0,
			main_worktree_paths TEXT,
			main_worktree_paths_order TEXT
		) STRICT
	`); err != nil {
		t.Fatalf("create sidebar_threads table: %v", err)
	}

	archivedValue := 0
	if options.Archived {
		archivedValue = 1
	}

	if _, err := db.Exec(
		`INSERT INTO sidebar_threads(session_id, agent_id, title, title_override, updated_at, created_at, folder_paths, folder_paths_order, archived, main_worktree_paths, main_worktree_paths_order)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		options.SessionID,
		nil,
		options.Title,
		nullString(options.TitleOverride),
		options.UpdatedAt.Format(time.RFC3339),
		nullTime(options.CreatedAt),
		options.FolderPaths,
		options.FolderPathsOrder,
		archivedValue,
		options.MainWorktreePaths,
		options.MainWorktreePathsOrder,
	); err != nil {
		t.Fatalf("insert sidebar row: %v", err)
	}
}

func nullString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func nullTime(value time.Time) sql.NullString {
	if value.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: value.Format(time.RFC3339), Valid: true}
}
