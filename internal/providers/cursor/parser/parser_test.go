package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
)

const (
	sharedConversationID    = "11111111-1111-4111-8111-111111111111"
	jsonlOnlyConversationID = "22222222-2222-4222-8222-222222222222"
	composerOnlyID          = "33333333-3333-4333-8333-333333333333"
)

var _ conversation.Parser = (*Parser)(nil)

func TestParserDiscoversScansAndStreamsCursorSources(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	projectKey := "Users-alice-source-cursor-repo"
	sharedJSONLPath := writeCursorJSONLTranscript(t, projectsDir, projectKey, sharedConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"jsonl title"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"jsonl answer"},{"type":"tool_use","name":"read_file","input":{"path":"main.go"}}]}}`,
	})
	jsonlOnlyPath := writeCursorJSONLTranscript(t, projectsDir, projectKey, jsonlOnlyConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"jsonl only title"}]}}`,
	})

	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	createCursorParserGlobalDB(t, globalDBPath)
	createCursorParserWorkspaceDB(t, rootDir, "workspace-hash")

	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if len(candidates) != 4 {
		t.Fatalf("Discover returned %d candidates, want 4: %#v", len(candidates), candidates)
	}
	if !slices.IsSortedFunc(candidates, func(left conversation.ScanCandidate, right conversation.ScanCandidate) int {
		return strings.Compare(left.Path, right.Path)
	}) {
		t.Fatalf("candidates are not sorted by path: %#v", candidates)
	}

	paths := candidatePaths(candidates)
	if !slices.Contains(paths, sharedJSONLPath) {
		t.Fatalf("shared JSONL path missing from candidates: %#v", paths)
	}
	if !slices.Contains(paths, jsonlOnlyPath) {
		t.Fatalf("jsonl-only path missing from candidates: %#v", paths)
	}
	if slices.ContainsFunc(paths, func(path string) bool {
		return strings.HasPrefix(path, "cursor://") && strings.Contains(path, sharedConversationID)
	}) {
		t.Fatalf("shared composer was not deduped in favor of JSONL: %#v", paths)
	}

	recordsByPath := make(map[string]conversation.Record)
	for _, candidate := range candidates {
		record, ok := parser.ScanRecord(candidate.Path, candidate.Stamp)
		if !ok {
			t.Fatalf("ScanRecord(%q) ok = false, want true", candidate.Path)
		}
		recordsByPath[candidate.Path] = record
	}

	sharedRecord := recordsByPath[sharedJSONLPath]
	if sharedRecord.ID != conversation.DerivedID(providerid.ProviderCursor, sharedConversationID, sharedJSONLPath) {
		t.Fatalf("shared ID = %q", sharedRecord.ID)
	}
	if sharedRecord.Title != "jsonl title" {
		t.Fatalf("shared Title = %q, want jsonl title", sharedRecord.Title)
	}
	if sharedRecord.ArtifactKind != "cursor_agent_transcript" {
		t.Fatalf("shared ArtifactKind = %q", sharedRecord.ArtifactKind)
	}

	composerPath := findPathContaining(t, paths, composerOnlyID)
	composerRecord := recordsByPath[composerPath]
	if composerRecord.NativeID != composerOnlyID {
		t.Fatalf("composer NativeID = %q", composerRecord.NativeID)
	}
	if composerRecord.Title != "Composer Workspace Title" {
		t.Fatalf("composer Title = %q, want workspace title", composerRecord.Title)
	}
	if composerRecord.WorkspaceRoot != filepath.FromSlash("/Users/alice/source/cursor repo") {
		t.Fatalf("composer WorkspaceRoot = %q", composerRecord.WorkspaceRoot)
	}
	if composerRecord.ArtifactKind != "cursor_background_composer" {
		t.Fatalf("composer ArtifactKind = %q", composerRecord.ArtifactKind)
	}
	if !composerRecord.Archived {
		t.Fatal("composer Archived = false, want true")
	}

	legacyPath := findPathContaining(t, paths, legacyID("workspace-hash", "tab-a"))
	legacyRecord := recordsByPath[legacyPath]
	if legacyRecord.NativeID != legacyID("workspace-hash", "tab-a") {
		t.Fatalf("legacy NativeID = %q, want %q", legacyRecord.NativeID, legacyID("workspace-hash", "tab-a"))
	}
	if legacyRecord.Title != "Legacy Title" {
		t.Fatalf("legacy Title = %q, want Legacy Title", legacyRecord.Title)
	}
	if legacyRecord.ArtifactKind != "cursor_legacy_chat" {
		t.Fatalf("legacy ArtifactKind = %q", legacyRecord.ArtifactKind)
	}

	jsonlMessages, err := conversation.CollectMessages(parser.Stream(sharedJSONLPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("CollectMessages JSONL returned error: %v", err)
	}
	if len(jsonlMessages) != 2 {
		t.Fatalf("jsonl messages len = %d, want 2", len(jsonlMessages))
	}
	if jsonlMessages[1].Text != "jsonl answer" {
		t.Fatalf("jsonl assistant text = %q", jsonlMessages[1].Text)
	}
	if len(jsonlMessages[1].Tools) != 1 || jsonlMessages[1].Tools[0].Name != "read_file" {
		t.Fatalf("jsonl assistant tools = %#v", jsonlMessages[1].Tools)
	}

	composerMessages, err := conversation.CollectMessages(parser.Stream(composerPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	}))
	if err != nil {
		t.Fatalf("CollectMessages composer returned error: %v", err)
	}
	if len(composerMessages) != 2 {
		t.Fatalf("composer messages len = %d, want 2", len(composerMessages))
	}
	if composerMessages[0].Role != "user" || composerMessages[0].Text != "composer question" {
		t.Fatalf("first composer message = %#v", composerMessages[0])
	}
	if composerMessages[1].Thinking != "composer thought" {
		t.Fatalf("composer thinking = %q", composerMessages[1].Thinking)
	}
	if len(composerMessages[1].Tools) != 1 || composerMessages[1].Tools[0].Output != "tool output" {
		t.Fatalf("composer tools = %#v", composerMessages[1].Tools)
	}

	legacyMessages, err := conversation.CollectMessages(parser.Stream(legacyPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("CollectMessages legacy returned error: %v", err)
	}
	if len(legacyMessages) != 2 {
		t.Fatalf("legacy messages len = %d, want 2", len(legacyMessages))
	}
	if legacyMessages[0].Role != "user" || legacyMessages[1].Role != "assistant" {
		t.Fatalf("legacy roles = %#v", legacyMessages)
	}
}

func candidatePaths(candidates []conversation.ScanCandidate) []string {
	paths := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		paths = append(paths, candidate.Path)
	}
	return paths
}

func findPathContaining(t *testing.T, paths []string, needle string) string {
	t.Helper()

	for _, path := range paths {
		if strings.Contains(path, needle) {
			return path
		}
	}
	t.Fatalf("no path contains %q in %#v", needle, paths)
	return ""
}

func writeCursorJSONLTranscript(
	t *testing.T,
	root string,
	projectKey string,
	conversationID string,
	lines []string,
) string {
	t.Helper()

	path := filepath.Join(root, projectKey, "agent-transcripts", conversationID, conversationID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll transcript dir: %v", err)
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile transcript: %v", err)
	}
	return path
}

func createCursorParserGlobalDB(t *testing.T, dbPath string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('backgroundComposer.windowBcMapping', '{"1":["33333333-3333-4333-8333-333333333333"]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:11111111-1111-4111-8111-111111111111', '{"composerId":"11111111-1111-4111-8111-111111111111","name":"Shared Composer","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"status":"none","unifiedMode":"agent","forceMode":"","fullConversationHeadersOnly":[{"bubbleId":"shared-user","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:33333333-3333-4333-8333-333333333333', '{"composerId":"33333333-3333-4333-8333-333333333333","name":"","createdAt":1710000000200,"lastUpdatedAt":1710000000300,"status":"none","unifiedMode":"agent","forceMode":"","fullConversationHeadersOnly":[{"bubbleId":"composer-user","type":1},{"bubbleId":"composer-assistant","type":2}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:33333333-3333-4333-8333-333333333333:composer-user', '{"_v":3,"type":1,"bubbleId":"composer-user","text":"composer question"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:33333333-3333-4333-8333-333333333333:composer-assistant', '{"_v":3,"type":2,"bubbleId":"composer-assistant","text":"composer answer","thinking":{"text":"composer thought"},"toolFormerData":{"name":"read_file","rawArgs":"{\"path\":\"README.md\"}","result":"tool output","status":"success"}}')`,
	}
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

func createCursorParserWorkspaceDB(t *testing.T, rootDir string, workspaceHash string) {
	t.Helper()

	workspaceDir := filepath.Join(rootDir, "workspaceStorage", workspaceHash)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workspace dir: %v", err)
	}
	workspaceJSONPath := filepath.Join(workspaceDir, "workspace.json")
	if err := os.WriteFile(workspaceJSONPath, []byte(`{"folder":"file:///Users/alice/source/cursor%20repo"}`), 0o644); err != nil {
		t.Fatalf("WriteFile workspace json: %v", err)
	}

	dbPath := filepath.Join(workspaceDir, "state.vscdb")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open workspace db: %v", err)
	}
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('composer.composerData.allComposers', '{"allComposers":[{"composerId":"33333333-3333-4333-8333-333333333333","name":"Composer Workspace Title","createdAt":1710000000200,"lastUpdatedAt":1710000000300,"subtitle":"cursor repo","isArchived":true}],"selectedComposerId":"33333333-3333-4333-8333-333333333333"}')`,
		`INSERT INTO ItemTable(key, value) VALUES ('workbench.panel.aichat.view.aichat.chatdata', '{"tabs":[{"tabId":"tab-a","chatTitle":"Legacy Title","bubbles":[{"type":"user","text":"legacy question"},{"type":"ai","text":"legacy answer"}]},{"tabId":"empty","chatTitle":"Skip","bubbles":[]}]}')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("exec workspace statement %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close workspace db: %v", err)
	}
}
