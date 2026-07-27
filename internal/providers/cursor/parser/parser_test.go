package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
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

// TestParserAdmitsComposersByWhatTheyStoreNotByTheirHeaderList covers the three
// shapes a chat's header reference list and its key range can combine into.
//
// The list alone cannot decide the empty case, because it is not a complete index
// of a chat's bubbles. Measured on a real store, 631 of 2,470 chats list no
// references, 9 of those hold stored bubbles anyway, and one is a 2,189-message
// agent run that never reached `conversation list` while the list decided this.
// The other 622 hold no bubble row at all and stay out.
func TestParserAdmitsComposersByWhatTheyStoreNotByTheirHeaderList(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(globalDBPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+globalDBPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	draftComposerID := "44444444-4444-4444-8444-444444444444"
	realComposerID := "55555555-5555-4555-8555-555555555555"
	unlistedComposerID := "66666666-6666-4666-8666-666666666666"
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + draftComposerID + `', '{"composerId":"` + draftComposerID + `","name":"","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"isDraft":true,"fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + realComposerID + `', '{"composerId":"` + realComposerID + `","name":"Real","createdAt":1710000000200,"lastUpdatedAt":1710000000300,"fullConversationHeadersOnly":[{"bubbleId":"real-user","type":1}]}')`,
		// A chat Cursor left with no header references, no `lastUpdatedAt`, and a
		// stored conversation, which is the shape of the 2,189-message agent run.
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + unlistedComposerID + `', '{"composerId":"` + unlistedComposerID + `","name":"Unlisted","createdAt":1710000000400,"fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + unlistedComposerID + `:u-1', '{"_v":3,"type":1,"bubbleId":"u-1","text":"the unlisted question","createdAt":"2026-05-06T05:00:00.000Z"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + unlistedComposerID + `:u-2', '{"_v":3,"type":2,"bubbleId":"u-2","text":"the unlisted answer","createdAt":"2026-05-06T05:00:10.000Z"}')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("exec statement %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close global db: %v", err)
	}

	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	paths := candidatePaths(candidates)
	if slices.ContainsFunc(paths, func(path string) bool {
		return strings.Contains(path, draftComposerID)
	}) {
		t.Fatalf("draft composer with nothing stored was offered as a candidate: %#v", paths)
	}
	if !slices.ContainsFunc(paths, func(path string) bool {
		return strings.Contains(path, realComposerID)
	}) {
		t.Fatalf("real composer missing from candidates: %#v", paths)
	}

	unlistedPath := findPathContaining(t, paths, unlistedComposerID)
	unlistedStamp := conversation.FileStamp{Size: 0, Mtime: time.Time{}}
	for _, candidate := range candidates {
		if candidate.Path == unlistedPath {
			unlistedStamp = candidate.Stamp
		}
	}
	if unlistedStamp.Size == 0 {
		t.Fatal("unlisted composer stamp has no bubble-range revision")
	}

	// The stamp is the change key rather than a byte count, and this chat has no
	// `lastUpdatedAt` for the stamp's time to track, so the key is the only thing
	// that moves when the chat does. Retrying a turn is the case the revision alone
	// cannot catch: it gives the chat a new request id while the stored bubbles and
	// the time stay exactly as they were.
	issued := cursorstore.ComposerHeader{
		ComposerID:               unlistedComposerID,
		LastUpdatedAt:            0,
		LatestChatGenerationUUID: "11111111-aaaa-4aaa-8aaa-111111111111",
	}
	retried := issued
	retried.LatestChatGenerationUUID = "22222222-bbbb-4bbb-8bbb-222222222222"
	if composerChangeKey(7, issued) == composerChangeKey(7, retried) {
		t.Fatal("the change key ignores the latest request id, so a retried turn leaves the record holding an id the chat no longer has")
	}

	messages, err := conversation.CollectMessages(parser.Stream(unlistedPath, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	}))
	if err != nil {
		t.Fatalf("CollectMessages for the unlisted composer returned error: %v", err)
	}
	if len(messages) != 2 || messages[0].Text != "the unlisted question" || messages[1].Text != "the unlisted answer" {
		t.Fatalf("unlisted composer messages = %#v, want both stored turns", messages)
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
		`INSERT INTO ItemTable(key, value) VALUES ('backgroundComposer.windowBcMapping', '{"1":[{"bcId":"33333333-3333-4333-8333-333333333333"}]}')`,
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

// TestDiscoverKeepsAChatWhoseStoredBubblesCannotBeRead covers a probe that fails
// rather than answering. The scan rebuilds the record set from what Discover
// returns, so a chat left out here loses the record it already had: one busy
// database while Cursor checkpoints would take a conversation out of the index
// until a later pass succeeded.
func TestDiscoverKeepsAChatWhoseStoredBubblesCannotBeRead(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(globalDBPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	unreadableComposerID := "77777777-7777-4777-8777-777777777777"
	writeRows := func(t *testing.T, bubbleValue string) {
		t.Helper()

		db, err := sql.Open("sqlite3", "file:"+globalDBPath+"?_busy_timeout=5000")
		if err != nil {
			t.Fatalf("sql.Open global db: %v", err)
		}
		for _, statement := range []string{
			"CREATE TABLE IF NOT EXISTS ItemTable(key TEXT UNIQUE, value BLOB)",
			"CREATE TABLE IF NOT EXISTS cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
			`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + unreadableComposerID + `', '{"composerId":"` + unreadableComposerID + `","name":"Unreadable","createdAt":1710000000400,"fullConversationHeadersOnly":[]}')`,
			`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + unreadableComposerID + `:u-1', ` + bubbleValue + `)`,
		} {
			if _, err := db.Exec(statement); err != nil {
				_ = db.Close()
				t.Fatalf("exec statement %q: %v", statement, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close global db: %v", err)
		}
	}

	path := BuildVirtualPath(RootHash(rootDir), VirtualKindComposer, unreadableComposerID)
	parser := New()

	// One pass that can read the chat, which is what gives the parser a stamp to
	// fall back on.
	writeRows(t, `'{"_v":3,"type":1,"bubbleId":"u-1","text":"the question"}'`)
	firstPass, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	readableStamp := conversation.FileStamp{Size: 0, Mtime: time.Time{}}
	for _, candidate := range firstPass {
		if candidate.Path == path {
			readableStamp = candidate.Stamp
		}
	}
	if readableStamp.Size == 0 {
		t.Fatalf("the readable pass did not admit the chat: %#v", candidatePaths(firstPass))
	}

	// Then the row becomes one no pass can read, which is what a busy database or
	// a NULL value looks like from here.
	writeRows(t, "NULL")
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if !slices.Contains(candidatePaths(candidates), path) {
		t.Fatalf("a chat whose bubbles could not be read was dropped from Discover: %#v", candidatePaths(candidates))
	}
	for _, candidate := range candidates {
		if candidate.Path == path && !candidate.Stamp.Equal(readableStamp) {
			t.Fatalf("stamp = %+v, want the stamp the last readable pass gave it, %+v: an unequal stamp re-runs the scan and changes the engine's fingerprint",
				candidate.Stamp, readableStamp)
		}
	}

	// A cold parser still knows the chat's key range holds a row. The read cannot
	// classify the row as empty, so discovery must keep the candidate and let the
	// stream report that the stored conversation cannot be read.
	coldCandidates, err := New().Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	if !slices.Contains(candidatePaths(coldCandidates), path) {
		t.Fatalf("a cold chat whose stored row could not be classified was dropped from Discover: %#v", candidatePaths(coldCandidates))
	}
}

// TestComposerRecordCarriesACreationTimeWhenCursorNeverStampedAnUpdate covers the
// listing order. A listing sorts by UpdatedAt descending, and the chats this
// parser admits by their stored bubbles are exactly the ones Cursor never gave a
// lastUpdatedAt, so taking that field alone puts the 2,189-message agent run at
// the zero time and last of roughly 2,470 records.
func TestComposerRecordCarriesACreationTimeWhenCursorNeverStampedAnUpdate(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(globalDBPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+globalDBPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	unstampedComposerID := "88888888-8888-4888-8888-888888888888"
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + unstampedComposerID + `', '{"composerId":"` + unstampedComposerID + `","name":"Unstamped","createdAt":1710000000400,"fullConversationHeadersOnly":[]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + unstampedComposerID + `:u-1', '{"_v":3,"type":1,"bubbleId":"u-1","text":"the question","createdAt":"2026-05-06T05:00:00.000Z"}')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("exec statement %q: %v", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close global db: %v", err)
	}

	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	path := findPathContaining(t, candidatePaths(candidates), unstampedComposerID)
	var stamp conversation.FileStamp
	for _, candidate := range candidates {
		if candidate.Path == path {
			stamp = candidate.Stamp
		}
	}
	record, ok := parser.ScanRecord(path, stamp)
	if !ok {
		t.Fatalf("ScanRecord(%q) ok = false", path)
	}
	if record.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt is the zero time, so the chat sorts below every dated conversation")
	}
	if !record.UpdatedAt.Equal(time.UnixMilli(1710000000400).UTC()) {
		t.Fatalf("UpdatedAt = %v, want the chat's creation time", record.UpdatedAt)
	}
}

// TestComposerStampMovesWhenAnUnreferencedBubbleArrives covers a chat that gains
// a bubble its header does not reference. Assembly reads the whole key range, so
// the chat has changed, and a stamp taken from the reference list and
// `lastUpdatedAt` alone would leave the old transcript in the index and in
// semantic search for good.
func TestComposerStampMovesWhenAnUnreferencedBubbleArrives(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(globalDBPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}

	composerID := "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	exec := func(t *testing.T, statements []string) {
		t.Helper()

		db, err := sql.Open("sqlite3", "file:"+globalDBPath+"?_busy_timeout=5000")
		if err != nil {
			t.Fatalf("sql.Open global db: %v", err)
		}
		for _, statement := range statements {
			if _, err := db.Exec(statement); err != nil {
				_ = db.Close()
				t.Fatalf("exec %q: %v", statement, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close global db: %v", err)
		}
	}
	stampFor := func(t *testing.T, parser *Parser) conversation.FileStamp {
		t.Helper()

		candidates, err := parser.Discover(context.Background(), nil)
		if err != nil {
			t.Fatalf("Discover returned error: %v", err)
		}
		path := findPathContaining(t, candidatePaths(candidates), composerID)
		for _, candidate := range candidates {
			if candidate.Path == path {
				return candidate.Stamp
			}
		}
		t.Fatalf("no candidate for %q", path)
		return conversation.FileStamp{Size: 0, Mtime: time.Time{}}
	}

	// A chat whose header references one bubble, with a fixed lastUpdatedAt.
	exec(t, []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + composerID + `', '{"composerId":"` + composerID + `","name":"Chat","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"b-1","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + composerID + `:b-1', '{"_v":3,"type":1,"bubbleId":"b-1","text":"the question"}')`,
	})
	parser := New()
	before := stampFor(t, parser)

	// Cursor adds a bubble the header does not reference and touches neither the
	// reference list nor lastUpdatedAt.
	exec(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + composerID + `:b-2', '{"_v":3,"type":2,"bubbleId":"b-2","text":"the unreferenced answer"}')`,
	})
	after := stampFor(t, parser)

	if before.Equal(after) {
		t.Fatalf("stamp %+v is unchanged after the chat gained an unreferenced bubble, so the scan keeps the old transcript", before)
	}

	// Cursor replaces one stored row without changing the row count or header
	// timestamp. The stamp still has to move because search must re-index the new
	// text.
	exec(t, []string{
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + composerID + `:b-2', '{"_v":3,"type":2,"bubbleId":"b-2","text":"the replacement answer"}')`,
	})
	afterRewrite := stampFor(t, parser)
	if after.Equal(afterRewrite) {
		t.Fatalf("stamp %+v is unchanged after one stored bubble was rewritten, so search keeps the old content", after)
	}
}

func TestParserReadsComposerMetadataFromGlobalComposerHeaders(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	globalOnlyID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	createCursorParserGlobalDBWithStatements(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		composerHeadersTableDDL,
		`INSERT INTO composerHeaders VALUES ('` + globalOnlyID + `', 'ws', 1710000000000, 1710000000100, 1, 0, 0, 0, '{"name":"Global Title","subtitle":"repo","isArchived":true,"workspaceIdentifier":{"uri":{"fsPath":"/Users/alice/source/global repo","path":"/Users/alice/source/global repo"}}}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + globalOnlyID + `', '{"composerId":"` + globalOnlyID + `","name":"","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"global-user","type":1}]}')`,
	})

	record := scanOneComposerRecord(t, globalOnlyID)
	if record.WorkspaceRoot != filepath.FromSlash("/Users/alice/source/global repo") {
		t.Fatalf("record WorkspaceRoot = %q, want the global workspace root", record.WorkspaceRoot)
	}
	if record.Title != "Global Title" {
		t.Fatalf("record Title = %q, want Global Title", record.Title)
	}
	if !record.Archived {
		t.Fatal("record Archived = false, want true")
	}
}

// Finding 3: a metadata-only change must move the candidate stamp, because the
// scan reuses the cached record whenever the stamp is unchanged.
func TestParserStampFollowsGlobalMetadataTimestamp(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	renamedID := "99999999-9999-4999-8999-999999999999"
	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	// The record's own lastUpdatedAt stays at 1710000000100 while the global
	// table has moved on to 1710000999000, which is what a rename or an archive
	// looks like on disk.
	createCursorParserGlobalDBWithStatements(t, globalDBPath, []string{
		composerHeadersTableDDL,
		`INSERT INTO composerHeaders VALUES ('` + renamedID + `', 'ws', 1710000000000, 1710000999000, 0, 0, 0, 0, '{"name":"Renamed"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + renamedID + `', '{"composerId":"` + renamedID + `","name":"","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}')`,
	})

	stamp := findComposerStamp(t, renamedID)
	want := msToTime(1710000999000)
	if !stamp.Mtime.Equal(want) {
		t.Fatalf("stamp Mtime = %s, want the global metadata timestamp %s; a rename would leave the cached record in place", stamp.Mtime, want)
	}
}

func findComposerStamp(t *testing.T, composerID string) conversation.FileStamp {
	t.Helper()

	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	for _, candidate := range candidates {
		if strings.Contains(candidate.Path, composerID) {
			return candidate.Stamp
		}
	}
	t.Fatalf("no candidate for composer %q in %#v", composerID, candidatePaths(candidates))
	return conversation.FileStamp{Size: 0, Mtime: time.Time{}}
}

func scanOneComposerRecord(t *testing.T, composerID string) conversation.Record {
	t.Helper()

	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	for _, candidate := range candidates {
		if !strings.Contains(candidate.Path, composerID) {
			continue
		}
		record, ok := parser.ScanRecord(candidate.Path, candidate.Stamp)
		if !ok {
			t.Fatalf("ScanRecord(%q) ok = false, want true", candidate.Path)
		}
		return record
	}
	t.Fatalf("no candidate for composer %q in %#v", composerID, candidatePaths(candidates))
	return conversation.Record{}
}

const composerHeadersTableDDL = "CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER, value TEXT)"

func createCursorParserGlobalDBWithStatements(t *testing.T, dbPath string, extra []string) {
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
	}
	statements = append(statements, extra...)
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

// Finding 1: a metadata table Clyde cannot read must cost that store's metadata,
// not the index. Discover feeds one scan shared by every provider, and an error
// from any provider discards the whole pass, so a Cursor schema change would stop
// Claude and Codex conversations from ever being indexed again.
func TestParserKeepsDiscoveringWhenGlobalMetadataIsUnreadable(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	transcriptPath := writeCursorJSONLTranscript(t, projectsDir, "Users-alice-source-repo", jsonlOnlyConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"still indexed"}]}}`,
	})

	brokenID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	createCursorParserGlobalDBWithStatements(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		// The table is present but carries none of the columns Clyde reads, which
		// is what a Cursor schema change looks like.
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, somethingElse TEXT)",
		`INSERT INTO composerHeaders VALUES ('` + brokenID + `', 'x')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + brokenID + `', '{"composerId":"` + brokenID + `","name":"Still Named","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}')`,
	})

	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error %v; an unreadable metadata table must not stop the scan for every provider", err)
	}
	paths := candidatePaths(candidates)
	if !slices.Contains(paths, transcriptPath) {
		t.Fatalf("transcript candidate lost to a metadata failure: %#v", paths)
	}

	composerPath := findPathContaining(t, paths, brokenID)
	var stamp conversation.FileStamp
	for _, candidate := range candidates {
		if candidate.Path == composerPath {
			stamp = candidate.Stamp
		}
	}
	record, ok := parser.ScanRecord(composerPath, stamp)
	if !ok {
		t.Fatalf("ScanRecord(%q) ok = false, want the chat kept with what its own record supplies", composerPath)
	}
	if record.Title != "Still Named" {
		t.Fatalf("record Title = %q, want the title from the chat's own record", record.Title)
	}
	if record.WorkspaceRoot != "" {
		t.Fatalf("record WorkspaceRoot = %q, want empty rather than invented when metadata is unreadable", record.WorkspaceRoot)
	}
}

// Finding 3: Cursor marks a dispatched agent on its own record. The transcript
// path has to infer this from the path shape; here it is stated outright.
func TestParserMarksSubagentComposersFromTheGlobalFlag(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	subagentID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	ownID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	createCursorParserGlobalDBWithStatements(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		composerHeadersTableDDL,
		`INSERT INTO composerHeaders VALUES ('` + subagentID + `', 'ws', 1710000000000, 1710000000100, 0, 1, 0, 0, '{"name":"Dispatched"}')`,
		`INSERT INTO composerHeaders VALUES ('` + ownID + `', 'ws', 1710000000000, 1710000000100, 0, 0, 0, 0, '{"name":"Mine"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + subagentID + `', '{"composerId":"` + subagentID + `","name":"Dispatched","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + ownID + `', '{"composerId":"` + ownID + `","name":"Mine","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}')`,
	})

	if origin := scanOneComposerRecord(t, subagentID).Origin; origin != conversation.OriginSubagent {
		t.Fatalf("subagent composer Origin = %v, want OriginSubagent", origin)
	}
	if origin := scanOneComposerRecord(t, ownID).Origin; origin == conversation.OriginSubagent {
		t.Fatalf("ordinary composer Origin = %v, want it left alone", origin)
	}
}

// Blocker 2: a metadata read that failed is not a chat with no workspace. The
// scan rebuilds the whole record set from this pass, so writing the blank would
// replace a record that had a workspace root with one that never regains it.
func TestParserKeepsThePriorRecordWhenMetadataCannotBeRead(t *testing.T) {
	rootDir := t.TempDir()
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	brokenID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	createCursorParserGlobalDBWithStatements(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"), []string{
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, somethingElse TEXT)",
		`INSERT INTO composerHeaders VALUES ('` + brokenID + `', 'x')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + brokenID + `', '{"composerId":"` + brokenID + `","name":"Named","createdAt":1710000000000,"lastUpdatedAt":1710000000100,"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}')`,
	})

	path := BuildVirtualPath(RootHash(rootDir), VirtualKindComposer, brokenID)
	priorRecord := conversation.Record{
		ID:            conversation.DerivedID(providerid.ProviderCursor, brokenID, path),
		Provider:      providerid.ProviderCursor,
		NativeID:      brokenID,
		Title:         "Named",
		WorkspaceRoot: filepath.FromSlash("/Users/alice/source/known"),
		ArtifactPath:  path,
		ArtifactKind:  "cursor_composer",
		Origin:        conversation.OriginSubagent,
		Archived:      true,
	}

	parser := New()
	candidates, err := parser.Discover(context.Background(), map[string]conversation.Record{path: priorRecord})
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	var stamp conversation.FileStamp
	for _, candidate := range candidates {
		if candidate.Path == path {
			stamp = candidate.Stamp
		}
	}

	record, ok := parser.ScanRecord(path, stamp)
	if !ok {
		t.Fatalf("ScanRecord(%q) ok = false, want the prior record kept", path)
	}
	if record.WorkspaceRoot != priorRecord.WorkspaceRoot {
		t.Fatalf("WorkspaceRoot = %q, want the prior %q kept rather than blanked by a failed read", record.WorkspaceRoot, priorRecord.WorkspaceRoot)
	}
	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("Origin = %v, want the prior origin kept", record.Origin)
	}
	if !record.Archived {
		t.Fatal("Archived = false, want the prior flag kept")
	}
}

// TestComposerStampMovesWhenTheLatestRequestIDChanges covers an incremental
// refresh that would otherwise reuse a stale record. Retrying a turn gives the
// chat a new latest request id while the stored row count stays put, and
// `lastUpdatedAt` is null on 52.5% of chats so the stamp's time is frozen for
// exactly those. The record would then keep a request id the chat no longer has,
// and resolving the new one would miss the index and fall through to the
// provider's live store.
//
// It runs the scan rather than the helper, because a change key nothing reaches
// is a change key that does not exist.
func TestComposerStampMovesWhenTheLatestRequestIDChanges(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")

	retriedComposerID := "99999999-9999-4999-8999-999999999999"
	composerRow := func(latestRequest string) string {
		return `{"composerId":"` + retriedComposerID + `","name":"Retried","createdAt":1710000000400,` +
			`"latestChatGenerationUUID":"` + latestRequest + `",` +
			`"fullConversationHeadersOnly":[{"bubbleId":"b-1","type":1},{"bubbleId":"b-2","type":2}]}`
	}

	// A chat whose turn count and (absent) lastUpdatedAt do not move while its
	// latest request id does, which is what retrying a turn leaves behind.
	stampFor := func(t *testing.T, latestRequest string) conversation.FileStamp {
		t.Helper()

		if err := os.RemoveAll(filepath.Dir(globalDBPath)); err != nil {
			t.Fatalf("RemoveAll global db dir: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(globalDBPath), 0o755); err != nil {
			t.Fatalf("MkdirAll global db dir: %v", err)
		}
		db, err := sql.Open("sqlite3", "file:"+globalDBPath+"?_busy_timeout=5000")
		if err != nil {
			t.Fatalf("sql.Open global db: %v", err)
		}
		for _, statement := range []string{
			"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
			"CREATE TABLE cursorDiskKV(key TEXT UNIQUE, value BLOB)",
			`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + retriedComposerID + `', '` + composerRow(latestRequest) + `')`,
			`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + retriedComposerID + `:b-1', '{"_v":3,"type":1,"bubbleId":"b-1","text":"q"}')`,
			`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + retriedComposerID + `:b-2', '{"_v":3,"type":2,"bubbleId":"b-2","text":"a"}')`,
		} {
			if _, err := db.Exec(statement); err != nil {
				_ = db.Close()
				t.Fatalf("exec %q: %v", statement, err)
			}
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close global db: %v", err)
		}

		candidates, err := New().Discover(context.Background(), nil)
		if err != nil {
			t.Fatalf("Discover returned error: %v", err)
		}
		path := findPathContaining(t, candidatePaths(candidates), retriedComposerID)
		for _, candidate := range candidates {
			if candidate.Path == path {
				return candidate.Stamp
			}
		}
		t.Fatalf("no candidate for %q", path)
		return conversation.FileStamp{Size: 0, Mtime: time.Time{}}
	}

	before := stampFor(t, "request-a")
	after := stampFor(t, "request-b")
	if before.Equal(after) {
		t.Fatalf("stamp %+v is unchanged after the chat's latest request id changed, so the scan would reuse the stale record", before)
	}
	if again := stampFor(t, "request-b"); !again.Equal(after) {
		t.Fatalf("stamp %+v then %+v for an unchanged chat, want the same stamp", after, again)
	}
}
