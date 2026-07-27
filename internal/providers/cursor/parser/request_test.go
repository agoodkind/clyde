package parser

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

// The fixture mirrors the shapes Cursor writes: chats in composerData with their
// turns in bubbleId rows, a composerHeaders table with real timestamp columns,
// and a per-workspace aiService.generations ring that Cursor caps.
const (
	latestChatID    = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	historyChatID   = "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	quotingChatID   = "cccccccc-3333-4333-8333-cccccccccccc"
	forgottenChatID = "dddddddd-4444-4444-8444-dddddddddddd"

	// latestRequestID is the newest request of latestChatID, so its chat header
	// carries it and Clyde's own index can answer for it.
	latestRequestID = "11111111-aaaa-4aaa-8aaa-111111111111"
	// olderRequestID is an earlier turn of historyChatID. No chat header carries
	// it, so only the live lookup can answer for it.
	olderRequestID = "22222222-bbbb-4bbb-8bbb-222222222222"
	// quotedRequestID appears inside another chat's message text but is no turn's
	// request id, so an exact match must reject it.
	quotedRequestID = "33333333-cccc-4ccc-8ccc-333333333333"
	// forgottenRequestID is a real turn of forgottenChatID that Cursor's capped
	// generation ring no longer lists, so only the opt-in full scan finds it.
	forgottenRequestID = "44444444-dddd-4ddd-8ddd-444444444444"
	// unknownRequestID belongs to nothing in the fixture.
	unknownRequestID = "55555555-eeee-4eee-8eee-555555555555"
	// partialChatID's header references a strict subset of its stored bubbles, the
	// shape Cursor writes when it rewrites a turn and leaves the old rows behind.
	partialChatID = "eeeeeeee-5555-4555-8555-eeeeeeeeeeee"
	// titledRequestID is no turn's request id, but one chat's title contains it,
	// so the single-hit title fallback would match it if the request-id path let
	// the selector reach that fallback.
	titledRequestID = "66666666-ffff-4fff-8fff-666666666666"

	generationUnixMs = 1710000000500
)

func TestResolveRequestIDFromClydeIndex(t *testing.T) {
	index := newCursorRequestIndex(t)

	resolution, err := index.ResolveRequest(context.Background(), latestRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if !resolution.Found {
		t.Fatalf("ResolveRequest found = false, want true (reason %s)", resolution.Reason)
	}
	if resolution.Origin != conversation.RequestOriginIndex {
		t.Fatalf("origin = %s, want index", resolution.Origin)
	}
	if resolution.Record.NativeID != latestChatID {
		t.Fatalf("native id = %q, want %q", resolution.Record.NativeID, latestChatID)
	}
}

func TestResolveRequestIDFallsBackToTheLiveLookup(t *testing.T) {
	index := newCursorRequestIndex(t)

	resolution, err := index.ResolveRequest(context.Background(), olderRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if !resolution.Found {
		t.Fatalf("ResolveRequest found = false, want true (reason %s)", resolution.Reason)
	}
	if resolution.Origin != conversation.RequestOriginLive {
		t.Fatalf("origin = %s, want live", resolution.Origin)
	}
	if resolution.Record.NativeID != historyChatID {
		t.Fatalf("native id = %q, want %q", resolution.Record.NativeID, historyChatID)
	}
}

func TestResolveRequestIDReportsNotFoundInsteadOfANearbyChat(t *testing.T) {
	index := newCursorRequestIndex(t)

	resolution, err := index.ResolveRequest(context.Background(), unknownRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Found {
		t.Fatalf("ResolveRequest found = true for an unknown id, resolved to %q", resolution.Record.NativeID)
	}
	if resolution.Record.NativeID != "" {
		t.Fatalf("record native id = %q, want empty for a miss", resolution.Record.NativeID)
	}
	if resolution.Reason != conversation.RequestNotFoundReasonNotRetained {
		t.Fatalf("reason = %s, want not_retained", resolution.Reason)
	}
}

func TestResolveRequestIDRejectsASubstringOnlyMatch(t *testing.T) {
	index := newCursorRequestIndex(t)

	resolution, err := index.ResolveRequest(context.Background(), quotedRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if resolution.Found {
		t.Fatalf("a chat that merely quotes the id matched: %q", resolution.Record.NativeID)
	}
	if resolution.Reason != conversation.RequestNotFoundReasonNoMatchingConversation {
		t.Fatalf("reason = %s, want no_matching_conversation", resolution.Reason)
	}
}

func TestResolveRequestIDFullScanIsOptIn(t *testing.T) {
	index := newCursorRequestIndex(t)

	bounded, err := index.ResolveRequest(context.Background(), forgottenRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if bounded.Found {
		t.Fatalf("bounded lookup resolved an id outside the generation ring: %q", bounded.Record.NativeID)
	}
	if bounded.Reason != conversation.RequestNotFoundReasonNotRetained {
		t.Fatalf("bounded reason = %s, want not_retained", bounded.Reason)
	}

	scanned, err := index.ResolveRequest(context.Background(), forgottenRequestID, conversation.RequestLookupOptions{AllowFullScan: true})
	if err != nil {
		t.Fatalf("ResolveRequest with full scan returned error: %v", err)
	}
	if !scanned.Found {
		t.Fatalf("full scan found = false, want true (reason %s)", scanned.Reason)
	}
	if scanned.Origin != conversation.RequestOriginFullScan {
		t.Fatalf("origin = %s, want full_scan", scanned.Origin)
	}
	if scanned.Record.NativeID != forgottenChatID {
		t.Fatalf("native id = %q, want %q", scanned.Record.NativeID, forgottenChatID)
	}
}

func TestRequestIDSelectorReadsTheWholeTranscript(t *testing.T) {
	index := newCursorRequestIndex(t)
	ctx := context.Background()

	record, err := index.Resolve(ctx, olderRequestID)
	if err != nil {
		t.Fatalf("Resolve by request id returned error: %v", err)
	}
	if record.NativeID != historyChatID {
		t.Fatalf("native id = %q, want %q", record.NativeID, historyChatID)
	}

	messages, err := index.LoadMessagesWithOptions(record, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if err != nil {
		t.Fatalf("Load transcript returned error: %v", err)
	}
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Text)
	}
	joined := strings.Join(texts, "|")
	if joined != "history first question|history first answer|history second question|history second answer" {
		t.Fatalf("transcript = %q, want the whole chat in order", joined)
	}
}

// TestStreamReadsBubblesTheComposerHeaderDoesNotReference is the ingestion test.
// Cursor's `fullConversationHeadersOnly` is not a complete index of a chat, so a
// reader that follows it drops stored turns. This fails against a header-driven
// read, which is the point.
func TestStreamReadsBubblesTheComposerHeaderDoesNotReference(t *testing.T) {
	index := newCursorRequestIndex(t)
	ctx := context.Background()

	record, err := index.Resolve(ctx, partialChatID)
	if err != nil {
		t.Fatalf("Resolve partial chat: %v", err)
	}
	messages, err := index.LoadMessagesWithOptions(record, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if err != nil {
		t.Fatalf("Load partial chat: %v", err)
	}
	texts := make([]string, 0, len(messages))
	for _, message := range messages {
		texts = append(texts, message.Text)
	}
	joined := strings.Join(texts, "|")
	if joined != "partial question|partial dropped answer|partial final answer" {
		t.Fatalf("transcript = %q, want the unreferenced answer read and the superseded copy dropped", joined)
	}
}

// TestUnresolvableRequestIDNeverMatchesATitle pins the rule that a request id
// names one exact thing. The quoting chat's title contains the id, and the
// single-hit substring fallback would otherwise hand that chat back as if it
// were the answer.
func TestUnresolvableRequestIDNeverMatchesATitle(t *testing.T) {
	index := newCursorRequestIndex(t)

	record, err := index.Resolve(context.Background(), titledRequestID)
	if err == nil {
		t.Fatalf("Resolve returned chat %q for a request id no chat issued", record.NativeID)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want a not-found error", err)
	}
}

// TestResolveUnknownSelectorStillFailsWithoutAStoreLookup covers the shape gate
// in front of the provider lookup. A selector that is not shaped like a request
// id must not reach the store at all, and the two paths are told apart by their
// error: only the request-id route appends the reason the lookup missed for, so
// an implementation that scanned the store before giving up would say why.
func TestResolveUnknownSelectorStillFailsWithoutAStoreLookup(t *testing.T) {
	index := newCursorRequestIndex(t)

	_, err := index.Resolve(context.Background(), "not-a-request-id")
	if err == nil {
		t.Fatal("Resolve of an unknown selector returned no error")
	}
	for _, reason := range []conversation.RequestNotFoundReason{
		conversation.RequestNotFoundReasonNotRetained,
		conversation.RequestNotFoundReasonNoMatchingConversation,
		conversation.RequestNotFoundReasonInconclusive,
		conversation.RequestNotFoundReasonUnindexedConversation,
		conversation.RequestNotFoundReasonAmbiguousConversation,
	} {
		if strings.Contains(err.Error(), reason.Describe()) {
			t.Fatalf("error = %v, want no lookup reason: a selector this shape never reaches the store", err)
		}
	}

	// The positive control: a selector that is shaped like a request id does take
	// the store route, and says what it found there. Without this the assertion
	// above would pass against an implementation that never reports a reason at
	// all.
	_, err = index.Resolve(context.Background(), unknownRequestID)
	if err == nil {
		t.Fatal("Resolve of an unknown request id returned no error")
	}
	if !strings.Contains(err.Error(), conversation.RequestNotFoundReasonNotRetained.Describe()) {
		t.Fatalf("error = %v, want the store lookup's reason for a request-id-shaped selector", err)
	}
}

// TestResolveFindsAChatIDTheCachedIndexHasNotSeenYet is the regression the
// request-id route introduced. Every Cursor composer id is a UUID, as are Claude
// session ids and Codex thread ids, so the shape check cannot tell a conversation
// id from a request id. A selector that took the request-id route and returned
// from it never reached the refresh-and-retry that a conversation absent from the
// cached index relies on.
func TestResolveFindsAChatIDTheCachedIndexHasNotSeenYet(t *testing.T) {
	index := newColdCursorRequestIndex(t)

	record, err := index.Resolve(context.Background(), historyChatID)
	if err != nil {
		t.Fatalf("Resolve by native chat id returned error: %v", err)
	}
	if record.NativeID != historyChatID {
		t.Fatalf("native id = %q, want %q", record.NativeID, historyChatID)
	}
}

// TestResolveByRequestIDStillWorksFromAColdIndex holds the other half: the
// refresh-and-retry must not cost the request-id route its own answer.
func TestResolveByRequestIDStillWorksFromAColdIndex(t *testing.T) {
	index := newColdCursorRequestIndex(t)

	record, err := index.Resolve(context.Background(), olderRequestID)
	if err != nil {
		t.Fatalf("Resolve by request id returned error: %v", err)
	}
	if record.NativeID != historyChatID {
		t.Fatalf("native id = %q, want %q", record.NativeID, historyChatID)
	}
}

// TestResolveRequestIDReportsInconclusiveWhenTheStoreCannotBeRead keeps the
// guess-versus-no-match distinction the reasons exist for. Cursor runs while
// Clyde reads, so a store Clyde could not open reads exactly like one that never
// held the id, and reporting that as a plain miss tells the operator the request
// does not exist.
func TestResolveRequestIDReportsInconclusiveWhenTheStoreCannotBeRead(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(globalDBPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	if err := os.WriteFile(globalDBPath, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("WriteFile unreadable global db: %v", err)
	}
	writeCursorRequestWorkspaceDB(t, rootDir, "workspace-hash")

	match, err := New().ResolveRequestID(
		context.Background(),
		latestRequestID,
		conversation.RequestLookupOptions{AllowFullScan: false},
	)
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Found {
		t.Fatalf("ResolveRequestID found = true against an unreadable store, resolved %q", match.NativeConversationID)
	}
	if match.Reason != conversation.RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %s, want inconclusive for a store that could not be read", match.Reason)
	}
}

// newCursorRequestIndex builds a conversation index over a fixture Cursor data
// root, with the real Cursor parser registered, so the tests enter through the
// same boundary the daemon uses. The index is warmed the way the daemon's
// background worker warms it, so the tests observe the resolution paths rather
// than a cold-start scan.
func newCursorRequestIndex(t *testing.T) *conversation.Index {
	t.Helper()

	index := newColdCursorRequestIndex(t)
	if err := index.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh index: %v", err)
	}
	return index
}

// newColdCursorRequestIndex builds the same index over the same fixture without
// warming it, which is what a freshly started daemon and a conversation written
// since the last refresh both look like.
func newColdCursorRequestIndex(t *testing.T) *conversation.Index {
	t.Helper()

	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	writeCursorRequestGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"))
	writeCursorRequestWorkspaceDB(t, rootDir, "workspace-hash")

	registry := conversation.NewRegistry()
	registry.Register(New())
	return conversation.NewIndex(registry, config.ConversationConfig{})
}

func writeCursorRequestGlobalDB(t *testing.T, dbPath string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("MkdirAll global db dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	defer func() { _ = db.Close() }()

	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER, value TEXT)",

		composerDataStatement(latestChatID, "Latest Chat", latestRequestID, []string{"latest-user", "latest-assistant"}),
		bubbleStatement(latestChatID, "latest-user", 1, "latest question", latestRequestID),
		bubbleStatement(latestChatID, "latest-assistant", 2, "latest answer", latestRequestID),

		composerDataStatement(historyChatID, "History Chat", "99999999-ffff-4fff-8fff-999999999999", []string{
			"history-user-1", "history-assistant-1", "history-user-2", "history-assistant-2",
		}),
		bubbleStatement(historyChatID, "history-user-1", 1, "history first question", olderRequestID),
		bubbleStatement(historyChatID, "history-assistant-1", 2, "history first answer", olderRequestID),
		bubbleStatement(historyChatID, "history-user-2", 1, "history second question", "99999999-ffff-4fff-8fff-999999999999"),
		bubbleStatement(historyChatID, "history-assistant-2", 2, "history second answer", "99999999-ffff-4fff-8fff-999999999999"),

		// This chat's message text quotes quotedRequestID, but no turn of it was
		// issued under that id.
		composerDataStatement(quotingChatID, "Quoting Chat about "+titledRequestID, "88888888-ffff-4fff-8fff-888888888888", []string{"quoting-user"}),
		bubbleStatement(quotingChatID, "quoting-user", 1, "please look at request "+quotedRequestID+" for me", "88888888-ffff-4fff-8fff-888888888888"),

		composerDataStatement(forgottenChatID, "Forgotten Chat", "77777777-ffff-4fff-8fff-777777777777", []string{"forgotten-user"}),
		bubbleStatement(forgottenChatID, "forgotten-user", 1, "forgotten question", forgottenRequestID),

		// Every chat's recorded lifetime spans the generation instant, so the
		// header narrowing alone cannot pick the right one.
		composerHeaderStatement(latestChatID),
		composerHeaderStatement(historyChatID),
		composerHeaderStatement(quotingChatID),
		composerHeaderStatement(forgottenChatID),

		// The header names only the opening question and the final reply. The two
		// bubbles between them are stored but unreferenced, and one of them repeats
		// the final reply verbatim the way a superseded row does.
		composerDataStatement(partialChatID, "Partial Chat", "", []string{"partial-user", "partial-final"}),
		datedBubbleStatement(partialChatID, "partial-user", 1, "partial question", "2026-05-06T05:00:00.000Z"),
		stampedBubbleStatement(partialChatID, "partial-final", 2, "partial final answer", "2026-05-06T05:00:30.000Z", "s-final"),
		datedBubbleStatement(partialChatID, "partial-orphan", 2, "partial dropped answer", "2026-05-06T05:00:10.000Z"),
		// The copy Cursor left behind when it rewrote the final answer, stamped with
		// the server id that says which row it acknowledged. Without that id nothing
		// separates a copy from a turn that happened twice, and assembly keeps both.
		stampedBubbleStatement(partialChatID, "partial-superseded", 2, "partial final answer", "2026-05-06T05:00:20.000Z", "s-final"),
		composerHeaderStatement(partialChatID),
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec global statement %q: %v", statement, err)
		}
	}
}

func writeCursorRequestWorkspaceDB(t *testing.T, rootDir string, workspaceHash string) {
	t.Helper()

	workspaceDir := filepath.Join(rootDir, "workspaceStorage", workspaceHash)
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll workspace dir: %v", err)
	}
	workspaceJSON := filepath.Join(workspaceDir, "workspace.json")
	if err := os.WriteFile(workspaceJSON, []byte(`{"folder":"file:///Users/alice/source/cursor%20repo"}`), 0o644); err != nil {
		t.Fatalf("WriteFile workspace json: %v", err)
	}

	db, err := sql.Open("sqlite3", "file:"+filepath.Join(workspaceDir, "state.vscdb")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open workspace db: %v", err)
	}
	defer func() { _ = db.Close() }()

	// The ring lists the recent requests and deliberately omits forgottenRequestID,
	// which is what a request older than Cursor's cap looks like.
	generations := `[` +
		generationEntry(latestRequestID) + `,` +
		generationEntry(olderRequestID) + `,` +
		generationEntry(quotedRequestID) +
		`]`
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('aiService.generations', '` + generations + `')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec workspace statement %q: %v", statement, err)
		}
	}
}

func generationEntry(requestID string) string {
	return `{"unixMs":` + strconv.Itoa(generationUnixMs) + `,"generationUUID":"` + requestID + `","type":"composer"}`
}

func composerDataStatement(composerID string, name string, latestRequest string, bubbleIDs []string) string {
	headers := make([]string, 0, len(bubbleIDs))
	for index, bubbleID := range bubbleIDs {
		bubbleType := 1
		if index%2 == 1 {
			bubbleType = 2
		}
		headers = append(headers, `{"bubbleId":"`+bubbleID+`","type":`+strconv.Itoa(bubbleType)+`}`)
	}
	value := `{"composerId":"` + composerID + `","name":"` + name + `",` +
		`"createdAt":` + strconv.Itoa(generationUnixMs-1000) + `,"lastUpdatedAt":` + strconv.Itoa(generationUnixMs+1000) + `,` +
		`"latestChatGenerationUUID":"` + latestRequest + `",` +
		`"fullConversationHeadersOnly":[` + strings.Join(headers, ",") + `]}`
	return `INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:` + composerID + `', '` + value + `')`
}

func bubbleStatement(composerID string, bubbleID string, bubbleType int, text string, requestID string) string {
	return datedBubbleStatementWithRequest(composerID, bubbleID, bubbleType, text, requestID, "")
}

// datedBubbleStatement writes a bubble carrying a write time, which is what
// places a bubble the composer header does not reference.
func datedBubbleStatement(
	composerID string,
	bubbleID string,
	bubbleType int,
	text string,
	createdAt string,
) string {
	return datedBubbleStatementWithRequest(composerID, bubbleID, bubbleType, text, "", createdAt)
}

func datedBubbleStatementWithRequest(
	composerID string,
	bubbleID string,
	bubbleType int,
	text string,
	requestID string,
	createdAt string,
) string {
	value := `{"_v":3,"type":` + strconv.Itoa(bubbleType) + `,"bubbleId":"` + bubbleID + `",` +
		`"text":"` + text + `","requestId":"` + requestID + `","createdAt":"` + createdAt + `"}`
	return `INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + composerID + `:` + bubbleID + `', '` + value + `')`
}

func composerHeaderStatement(composerID string) string {
	return `INSERT INTO composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, isArchived, isSubagent, recency, checkpointAt, value) VALUES ('` +
		composerID + `', 'workspace-hash', ` + strconv.Itoa(generationUnixMs-1000) + `, ` + strconv.Itoa(generationUnixMs+1000) + `, 0, 0, 0, 0, '{}')`
}

// TestResolveRequestIDReadsAWorkspaceWithNoFolderDescriptor is the workspace a
// listing keyed on `workspace.json` drops. Cursor stores a window opened on no
// folder under `workspaceStorage/empty-window`, with no descriptor and its own
// `aiService.generations` ring. On the operator's machine that directory holds a
// full ring of 50 request ids, and the id below stands for all of them.
func TestResolveRequestIDReadsAWorkspaceWithNoFolderDescriptor(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	writeCursorRequestGlobalDB(t, filepath.Join(rootDir, "globalStorage", "state.vscdb"))
	writeEmptyWindowWorkspaceDB(t, rootDir, olderRequestID)

	registry := conversation.NewRegistry()
	registry.Register(New())
	index := conversation.NewIndex(registry, config.ConversationConfig{})
	if err := index.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh index: %v", err)
	}

	resolution, err := index.ResolveRequest(context.Background(), olderRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequest returned error: %v", err)
	}
	if !resolution.Found {
		t.Fatalf("ResolveRequest found = false (reason %s), want the chat the empty-window ring points at", resolution.Reason)
	}
	if resolution.Record.NativeID != historyChatID {
		t.Fatalf("native id = %q, want %q", resolution.Record.NativeID, historyChatID)
	}
}

// TestResolveRequestIDReportsANotRetainedMissWhereCursorIsNotInstalled covers the
// machine that has no Cursor at all. The default data root is returned whether or
// not it exists, so collapsing a missing database into an unreadable one tells
// that user to retry once Cursor is idle about software they do not have.
func TestResolveRequestIDReportsANotRetainedMissWhereCursorIsNotInstalled(t *testing.T) {
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", filepath.Join(t.TempDir(), "no-cursor-here"))
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	match, err := (&Parser{}).ResolveRequestID(context.Background(), unknownRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Found {
		t.Fatalf("match = %+v, want a miss", match)
	}
	if match.Reason != conversation.RequestNotFoundReasonNotRetained {
		t.Fatalf("reason = %s, want not_retained for a store that is simply not there", match.Reason)
	}
}

// TestResolveRequestIDLetsAnUnreadableRootOutrankACleanMiss covers two data roots,
// which is the ordinary shape when Cursor sits beside Cursor Insiders. A clean
// miss in the first root must not be reported as a confirmed absence when the
// second could not be opened, because the id may be sitting in the one this
// lookup could not read.
func TestResolveRequestIDLetsAnUnreadableRootOutrankACleanMiss(t *testing.T) {
	cleanRoot := t.TempDir()
	writeCursorRequestGlobalDB(t, filepath.Join(cleanRoot, "globalStorage", "state.vscdb"))
	writeCursorRequestWorkspaceDB(t, cleanRoot, "workspace-hash")

	lockedRoot := t.TempDir()
	lockedDBPath := filepath.Join(lockedRoot, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(lockedDBPath), 0o755); err != nil {
		t.Fatalf("MkdirAll locked global db dir: %v", err)
	}
	// A file that exists and is not a database is what an unopenable store looks
	// like from here: present, and refusing to be read.
	if err := os.WriteFile(lockedDBPath, []byte("not a sqlite database at all"), 0o000); err != nil {
		t.Fatalf("WriteFile locked global db: %v", err)
	}

	t.Setenv("CLYDE_CURSOR_DATA_DIRS", cleanRoot+string(os.PathListSeparator)+lockedRoot)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	match, err := (&Parser{}).ResolveRequestID(context.Background(), unknownRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Found {
		t.Fatalf("match = %+v, want a miss", match)
	}
	if match.Reason != conversation.RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %s, want inconclusive because one root could not be read", match.Reason)
	}
}

// writeEmptyWindowWorkspaceDB writes the workspace Cursor keeps for a window
// opened on no folder: a state.vscdb holding a generation ring, and no
// workspace.json beside it.
func writeEmptyWindowWorkspaceDB(t *testing.T, rootDir string, requestID string) {
	t.Helper()

	workspaceDir := filepath.Join(rootDir, "workspaceStorage", "empty-window")
	if err := os.MkdirAll(workspaceDir, 0o755); err != nil {
		t.Fatalf("MkdirAll empty window dir: %v", err)
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(workspaceDir, "state.vscdb")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open empty window db: %v", err)
	}
	defer func() { _ = db.Close() }()

	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		`INSERT INTO ItemTable(key, value) VALUES ('aiService.generations', '[` + generationEntry(requestID) + `]')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec empty window statement %q: %v", statement, err)
		}
	}
}

// TestResolveRequestIDSearchesEveryRootAfterOneFails covers a first data root
// that fails outright, which is different from one that reads and misses. A root
// whose workspace storage cannot be listed must not abandon the roots after it,
// because the id may be sitting in the next one.
func TestResolveRequestIDSearchesEveryRootAfterOneFails(t *testing.T) {
	brokenRoot := t.TempDir()
	writeCursorRequestGlobalDB(t, filepath.Join(brokenRoot, "globalStorage", "state.vscdb"))
	// A workspaceStorage that exists and cannot be read is what makes the root's
	// lookup fail rather than miss.
	brokenWorkspaces := filepath.Join(brokenRoot, "workspaceStorage")
	if err := os.MkdirAll(brokenWorkspaces, 0o755); err != nil {
		t.Fatalf("MkdirAll broken workspaceStorage: %v", err)
	}
	if err := os.Chmod(brokenWorkspaces, 0o000); err != nil {
		t.Fatalf("Chmod broken workspaceStorage: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(brokenWorkspaces, 0o755) })

	goodRoot := t.TempDir()
	writeCursorRequestGlobalDB(t, filepath.Join(goodRoot, "globalStorage", "state.vscdb"))
	writeCursorRequestWorkspaceDB(t, goodRoot, "workspace-hash")

	t.Setenv("CLYDE_CURSOR_DATA_DIRS", brokenRoot+string(os.PathListSeparator)+goodRoot)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	match, err := (&Parser{}).ResolveRequestID(context.Background(), olderRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if !match.Found {
		t.Fatalf("match = %+v, want the chat in the root after the one that failed", match)
	}
	if match.NativeConversationID != historyChatID {
		t.Fatalf("native id = %q, want %q", match.NativeConversationID, historyChatID)
	}
}

// TestResolveRequestIDReportsAmbiguityRatherThanTheFirstCarrier covers the copy a
// chat duplicate leaves behind. Measured on a real store, 767 of 11,396 distinct
// request ids appear in more than one chat, the worst in nine, so answering with
// the first is answering with whichever the key order reached.
func TestResolveRequestIDReportsAmbiguityRatherThanTheFirstCarrier(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	dbPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	writeCursorRequestGlobalDB(t, dbPath)
	writeCursorRequestWorkspaceDB(t, rootDir, "workspace-hash")

	// A duplicate of historyChatID carrying the same turn under a fresh bubble id,
	// which is the shape Cursor's duplicate action leaves.
	duplicateChatID := "aaaabbbb-6666-4666-8666-aaaabbbbcccc"
	appendCursorRequestStatements(t, dbPath, []string{
		composerDataStatement(duplicateChatID, "(1) History Chat", "", []string{"copy-user-1"}),
		bubbleStatement(duplicateChatID, "copy-user-1", 1, "history first question", olderRequestID),
		composerHeaderStatement(duplicateChatID),
	})

	match, err := (&Parser{}).ResolveRequestID(context.Background(), olderRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Found {
		t.Fatalf("match = %+v, want no answer when two chats carry the request", match)
	}
	if match.Reason != conversation.RequestNotFoundReasonAmbiguousConversation {
		t.Fatalf("reason = %s, want ambiguous_conversation", match.Reason)
	}
}

// TestResolveRequestIDReportsInconclusiveForAnUndecodableCarrier covers a stored
// row the byte search selected, so it provably holds the id's bytes, and a decode
// then rejected. That row is the likeliest carrier, not the least, so a miss over
// it is not a confirmed absence.
func TestResolveRequestIDReportsInconclusiveForAnUndecodableCarrier(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	dbPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	writeCursorRequestGlobalDB(t, dbPath)
	writeCursorRequestWorkspaceDB(t, rootDir, "workspace-hash")

	brokenChatID := "ccccdddd-7777-4777-8777-ccccddddeeee"
	appendCursorRequestStatements(t, dbPath, []string{
		composerDataStatement(brokenChatID, "Broken Chat", "", []string{"broken-user"}),
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + brokenChatID +
			`:broken-user', 'not json, and it carries ` + quotedRequestID + `')`,
		composerHeaderStatement(brokenChatID),
	})

	match, err := (&Parser{}).ResolveRequestID(context.Background(), quotedRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Found {
		t.Fatalf("match = %+v, want a miss", match)
	}
	if match.Reason != conversation.RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %s, want inconclusive over a row that would not decode", match.Reason)
	}
}

// TestResolveRequestIDAsksTheRingsWhenTheChatStoreIsAbsent covers a data root
// whose chat database is gone while its per-workspace rings survive. The rings
// are where a request is listed, so a branch that never opens them cannot assert
// that the provider does not list the request.
func TestResolveRequestIDAsksTheRingsWhenTheChatStoreIsAbsent(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	// No globalStorage at all, and a workspace ring that lists the request.
	writeCursorRequestWorkspaceDB(t, rootDir, "workspace-hash")

	match, err := (&Parser{}).ResolveRequestID(context.Background(), olderRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Found {
		t.Fatalf("match = %+v, want a miss with no chat store to resolve against", match)
	}
	if match.Reason != conversation.RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %s, want inconclusive for a request the rings still list", match.Reason)
	}

	// The same root with nothing in it at all is still a real absence.
	emptyRoot := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", emptyRoot)
	match, err = (&Parser{}).ResolveRequestID(context.Background(), olderRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Reason != conversation.RequestNotFoundReasonNotRetained {
		t.Fatalf("reason = %s, want not_retained where nothing is stored at all", match.Reason)
	}
}

// appendCursorRequestStatements adds rows to a fixture global database after it
// has been written.
func appendCursorRequestStatements(t *testing.T, dbPath string, statements []string) {
	t.Helper()

	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open global db: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
}

// TestResolveRequestIDDoesNotNameOneCarrierWhileRowsWentUnread covers a search
// that found one chat and could not read everything. A second carrier hiding in
// the unread rows is exactly what reading past the first match exists to find, so
// naming the one it saw asserts a uniqueness it never established.
func TestResolveRequestIDDoesNotNameOneCarrierWhileRowsWentUnread(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	dbPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")
	writeCursorRequestGlobalDB(t, dbPath)
	writeCursorRequestWorkspaceDB(t, rootDir, "workspace-hash")

	// historyChatID carries olderRequestID. This chat holds a row the byte search
	// selects, because it contains the id, and a decode then rejects.
	truncatedChatID := "eeeeffff-8888-4888-8888-eeeeffff0000"
	appendCursorRequestStatements(t, dbPath, []string{
		composerDataStatement(truncatedChatID, "Truncated Chat", "", []string{"cut-user"}),
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + truncatedChatID +
			`:cut-user', '{"_v":3,"requestId":"` + olderRequestID + `"')`,
		composerHeaderStatement(truncatedChatID),
	})

	match, err := (&Parser{}).ResolveRequestID(context.Background(), olderRequestID, conversation.RequestLookupOptions{AllowFullScan: false})
	if err != nil {
		t.Fatalf("ResolveRequestID returned error: %v", err)
	}
	if match.Found {
		t.Fatalf("match = %+v, want no confident answer while a row carrying the id went unread", match)
	}
	if match.Reason != conversation.RequestNotFoundReasonInconclusive {
		t.Fatalf("reason = %s, want inconclusive", match.Reason)
	}
}

// stampedBubbleStatement writes a bubble carrying Cursor's server-side identity,
// which is what tells a superseded copy apart from a turn that happened twice.
func stampedBubbleStatement(
	composerID string,
	bubbleID string,
	bubbleType int,
	text string,
	createdAt string,
	serverBubbleID string,
) string {
	value := `{"_v":3,"type":` + strconv.Itoa(bubbleType) + `,"bubbleId":"` + bubbleID + `",` +
		`"text":"` + text + `","requestId":"","createdAt":"` + createdAt + `",` +
		`"serverBubbleId":"` + serverBubbleID + `"}`
	return `INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:` + composerID + `:` + bubbleID + `', '` + value + `')`
}
