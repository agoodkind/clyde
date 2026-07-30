package cursorstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestFindBubbleRequestIDInComposerStaysInsideTheChat(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	// composer-a and composer-ab share a key prefix, so a lookup scoped to
	// composer-a must not reach composer-ab's turn.
	search, err := FindBubbleRequestIDInComposer(context.Background(), readonly, "composer-a", "req-neighbor")
	if err != nil {
		t.Fatalf("FindBubbleRequestIDInComposer returned error: %v", err)
	}
	if len(search.Carriers) != 0 {
		t.Fatalf("lookup scoped to composer-a reached %+v", search.Carriers)
	}

	search, err = FindBubbleRequestIDInComposer(context.Background(), readonly, "composer-ab", "req-neighbor")
	if err != nil {
		t.Fatalf("FindBubbleRequestIDInComposer returned error: %v", err)
	}
	if len(search.Carriers) != 1 {
		t.Fatalf("carriers = %+v, want composer-ab's own turn", search.Carriers)
	}
	if search.Carriers[0].ComposerID != "composer-ab" || search.Carriers[0].BubbleID != "bubble-n" {
		t.Fatalf("carrier = %+v, want composer-ab/bubble-n", search.Carriers[0])
	}
}

func TestFindBubbleRequestIDRequiresWholeStringEquality(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	// bubble-2's text quotes the id, and bubble-1's requestId has it as a prefix.
	// Neither is a match.
	search, err := FindBubbleRequestIDInComposer(context.Background(), readonly, "composer-a", "req-quoted")
	if err != nil {
		t.Fatalf("FindBubbleRequestIDInComposer returned error: %v", err)
	}
	if len(search.Carriers) != 0 {
		t.Fatalf("a substring occurrence matched %+v, want whole-string equality", search.Carriers)
	}
}

func TestScanAllBubblesForRequestIDCrossesChats(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	search, err := ScanAllBubblesForRequestID(context.Background(), readonly, "req-neighbor")
	if err != nil {
		t.Fatalf("ScanAllBubblesForRequestID returned error: %v", err)
	}
	if len(search.Carriers) != 1 || search.Carriers[0].ComposerID != "composer-ab" {
		t.Fatalf("carriers = %+v, want composer-ab", search.Carriers)
	}
}

func TestListComposerIDsCoveringTimeNarrowsToSpanningChats(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	composerIDs, err := listCoveringComposerIDs(t, context.Background(), readonly, 1710000002000, "")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if len(composerIDs) != 1 || composerIDs[0] != "composer-a" {
		t.Fatalf("composer ids = %v, want [composer-a]", composerIDs)
	}
}

func TestListComposerIDsCoveringTimeNarrowsByWorkspace(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	// composer-a and composer-c both span the instant; only composer-a belongs to
	// workspace ws.
	spanning, err := listCoveringComposerIDs(t, context.Background(), readonly, 1710000007000, "")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if len(spanning) != 2 {
		t.Fatalf("instant-only candidates = %v, want two", spanning)
	}

	scoped, err := listCoveringComposerIDs(t, context.Background(), readonly, 1710000007000, "ws")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if len(scoped) != 1 || scoped[0] != "composer-a" {
		t.Fatalf("workspace-scoped candidates = %v, want [composer-a]", scoped)
	}
}

// TestListComposerIDsCoveringTimeKeepsChatsWithNoLastUpdatedAt is the majority
// case, not an edge. On a real store `lastUpdatedAt` is null on 1,034 of 2,390
// chats and zero on another 221, so a predicate requiring it excludes 52.5% of
// every chat from every lookup, including the chat that issued the request being
// looked up. `checkpointAt` is what records those chats' ends: it covers 301 of
// the 381 request instants whose owning chat is known, against 68 for
// `lastUpdatedAt`.
func TestListComposerIDsCoveringTimeKeepsChatsWithNoLastUpdatedAt(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	composerIDs, err := listCoveringComposerIDs(t, context.Background(), readonly, 1710000045000, "")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if len(composerIDs) != 1 || composerIDs[0] != "composer-unstamped" {
		t.Fatalf("composer ids = %v, want [composer-unstamped]", composerIDs)
	}
}

// TestListComposerIDsCoveringTimeKeepsAChatWithNoRecordedEnd covers a chat whose
// only timestamp is its creation. Its own first request arrives at or just after
// that instant, so an upper bound taken from `createdAt` is what keeps it in its
// own lookup.
func TestListComposerIDsCoveringTimeKeepsAChatWithNoRecordedEnd(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	composerIDs, err := listCoveringComposerIDs(t, context.Background(), readonly, 1710000060000, "")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if len(composerIDs) != 1 || composerIDs[0] != "composer-openended" {
		t.Fatalf("composer ids = %v, want [composer-openended]", composerIDs)
	}
}

// TestListComposerIDsCoveringTimeToleratesTheStampLag pins the measured slack.
// Cursor writes the generation ring entry a moment before it stamps the chat, so
// on a real machine 24 of 381 known pairs have the request arriving after the
// chat's newest stamp, every one of them by between 10 and 234 milliseconds.
func TestListComposerIDsCoveringTimeToleratesTheStampLag(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	// 234ms past composer-unstamped's checkpoint, the widest lag measured.
	composerIDs, err := listCoveringComposerIDs(t, context.Background(), readonly, 1710000050234, "")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if len(composerIDs) != 1 || composerIDs[0] != "composer-unstamped" {
		t.Fatalf("composer ids = %v, want the chat whose stamp lags its own request", composerIDs)
	}
}

// TestReadComposerHeaderCoverageReportsChatsTheHeaderTableOmits is what stops a
// narrowed miss claiming a confirmed absence. The header table is not a complete
// index of the store: on a real machine it lists 2,390 chats while `composerData:`
// holds 2,470.
//
// The membership matters, not the totals. The fixture stores three chats and
// lists three, one of which the store does not hold, so the counts match while a
// stored chat goes unlisted. A gate comparing counts calls that complete.
func TestReadComposerHeaderCoverageReportsChatsTheHeaderTableOmits(t *testing.T) {
	readonly := openCoverageTestDatabase(t, []string{"chat-a", "chat-b", "chat-unlisted"},
		[]string{"chat-a", "chat-b", "chat-the-store-does-not-hold"})

	coverage, err := ReadComposerHeaderCoverage(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerHeaderCoverage returned error: %v", err)
	}
	if coverage.StoredChats != 3 {
		t.Fatalf("StoredChats = %d, want 3", coverage.StoredChats)
	}
	if coverage.UnlistedChats != 1 {
		t.Fatalf("UnlistedChats = %d, want the one stored chat the header table omits", coverage.UnlistedChats)
	}
	if coverage.Complete() {
		t.Fatalf("coverage %+v reported complete while a stored chat is unlisted", coverage)
	}
}

// TestReadComposerHeaderCoverageIsCompleteWhenEveryStoredChatIsListed is the
// other half, so the gate is not simply stuck reporting incomplete.
func TestReadComposerHeaderCoverageIsCompleteWhenEveryStoredChatIsListed(t *testing.T) {
	readonly := openCoverageTestDatabase(t, []string{"chat-a", "chat-b"},
		[]string{"chat-a", "chat-b", "chat-the-store-does-not-hold"})

	coverage, err := ReadComposerHeaderCoverage(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerHeaderCoverage returned error: %v", err)
	}
	if !coverage.Complete() {
		t.Fatalf("coverage %+v reported incomplete when every stored chat is listed", coverage)
	}
}

// TestReadComposerHeaderCoverageIsNotCompleteWithoutAChatTable covers a database
// holding no chat table at all. Reading zero stored chats out of a table that is
// not there and calling the coverage complete would let a confirmed absence rest
// on a store never read.
func TestReadComposerHeaderCoverageIsNotCompleteWithoutAChatTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	if _, err := writable.Exec("CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	coverage, err := ReadComposerHeaderCoverage(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerHeaderCoverage returned error: %v", err)
	}
	if coverage.Readable {
		t.Fatalf("coverage %+v reported readable without a chat table", coverage)
	}
	if coverage.Complete() {
		t.Fatalf("coverage %+v reported complete over a store it never read", coverage)
	}
}

// TestListComposerIDsCoveringTimeReportsAnUnusableHeaderTable covers a Cursor
// version whose header table lacks a column the predicate reads. An empty
// candidate list from a table that could not be queried is not a miss, and a
// caller that read it as one would report a confirmed absence for every request
// id on that machine.
func TestListComposerIDsCoveringTimeReportsAnUnusableHeaderTable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	for _, statement := range []string{
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		// A header table with no createdAt, which every version of the predicate
		// needs.
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, value TEXT)",
		`INSERT INTO composerHeaders(composerId, workspaceId, value) VALUES ('composer-a', 'ws', '{}')`,
	} {
		if _, err := writable.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	narrowing, err := ListComposerIDsCoveringTime(context.Background(), readonly, 1710000000000, "")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if narrowing.Usable {
		t.Fatalf("narrowing %+v reported usable against a header table missing createdAt", narrowing)
	}
}

// TestListComposerIDsCoveringTimeWorksWithoutTheOptionalEndColumns covers a
// Cursor version that writes only createdAt. The predicate degrades to what the
// table has rather than failing every lookup on that machine.
func TestListComposerIDsCoveringTimeWorksWithoutTheOptionalEndColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	for _, statement := range []string{
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, createdAt INTEGER, value TEXT)",
		`INSERT INTO composerHeaders(composerId, createdAt, value) VALUES ('composer-a', 1710000000000, '{}')`,
	} {
		if _, err := writable.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	narrowing, err := ListComposerIDsCoveringTime(context.Background(), readonly, 1710000000000, "any-workspace")
	if err != nil {
		t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
	}
	if !narrowing.Usable {
		t.Fatal("narrowing reported unusable against a header table carrying composerId and createdAt")
	}
	if len(narrowing.ComposerIDs) != 1 || narrowing.ComposerIDs[0] != "composer-a" {
		t.Fatalf("composer ids = %v, want [composer-a] from the columns the table has", narrowing.ComposerIDs)
	}
}

// TestFindBubbleRequestIDCountsARowItCannotDecode covers a stored row the byte
// search selected, which is proof it holds the id's bytes, and a decode then
// rejected. Skipping it quietly turns the likeliest carrier into evidence that no
// carrier exists.
func TestFindBubbleRequestIDCountsARowItCannotDecode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	for _, statement := range []string{
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-1', 'not json at all but it carries req-broken')`,
	} {
		if _, err := writable.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	search, err := FindBubbleRequestIDInComposer(context.Background(), readonly, "composer-a", "req-broken")
	if err != nil {
		t.Fatalf("FindBubbleRequestIDInComposer returned error: %v", err)
	}
	if len(search.Carriers) != 0 {
		t.Fatalf("carriers = %+v, want none from a row that would not decode", search.Carriers)
	}
	if search.UnreadableRows != 1 {
		t.Fatalf("UnreadableRows = %d, want the undecodable row counted", search.UnreadableRows)
	}
}

// TestFindBubbleRequestIDReportsEveryCarrier covers the duplicate a chat copy
// leaves behind. Measured on a real store, 767 of 11,396 distinct request ids
// appear in more than one chat, so stopping at the first is an ordering artifact.
func TestFindBubbleRequestIDReportsEveryCarrier(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	for _, statement := range []string{
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:chat-original:bubble-1', '{"_v":3,"requestId":"req-copied"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:chat-duplicate:bubble-9', '{"_v":3,"requestId":"req-copied"}')`,
	} {
		if _, err := writable.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	search, err := ScanAllBubblesForRequestID(context.Background(), readonly, "req-copied")
	if err != nil {
		t.Fatalf("ScanAllBubblesForRequestID returned error: %v", err)
	}
	if len(search.Carriers) != 2 {
		t.Fatalf("carriers = %+v, want both the chat and its duplicate", search.Carriers)
	}
}

// listCoveringComposerIDs asserts the narrowing could run, so a test about which
// chats it selects never passes because the table could not be queried.
func listCoveringComposerIDs(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	unixMs int64,
	workspaceID string,
) ([]string, error) {
	t.Helper()

	narrowing, err := ListComposerIDsCoveringTime(ctx, db, unixMs, workspaceID)
	if err != nil {
		return nil, err
	}
	if !narrowing.Usable {
		t.Fatal("ListComposerIDsCoveringTime reported the header table unusable")
	}
	return narrowing.ComposerIDs, nil
}

// openCoverageTestDatabase writes a store holding one composerData row per stored
// chat and one composerHeaders row per listed chat, so a test can state the two
// memberships independently.
func openCoverageTestDatabase(t *testing.T, storedChats []string, listedChats []string) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER, value TEXT)",
	}
	for _, chat := range storedChats {
		statements = append(statements,
			`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:`+chat+`', '{"composerId":"`+chat+`"}')`)
	}
	for _, chat := range listedChats {
		statements = append(statements,
			`INSERT INTO composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, isArchived, isSubagent, recency, checkpointAt, value) VALUES ('`+
				chat+`', 'ws', 1710000000000, 1710000001000, 0, 0, 0, 0, '{}')`)
	}
	for _, statement := range statements {
		if _, err := writable.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	return readonly
}

func TestReadGenerationEntriesDecodesTheRecentRing(t *testing.T) {
	readonly := openCursorRequestTestDatabase(t)

	entries, found, err := ReadGenerationEntries(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadGenerationEntries returned error: %v", err)
	}
	if !found {
		t.Fatal("ReadGenerationEntries found = false, want true")
	}
	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0].GenerationUUID != "req-neighbor" || entries[0].UnixMs != 1710000000500 {
		t.Fatalf("entry = %+v", entries[0])
	}
}

func TestKeyPrefixUpperBoundStopsAtTheFirstKeyAfterThePrefix(t *testing.T) {
	bound, ok := keyPrefixUpperBound("bubbleId:composer-a:")
	if !ok {
		t.Fatal("keyPrefixUpperBound ok = false, want true")
	}
	if bound != "bubbleId:composer-a;" {
		t.Fatalf("bound = %q, want the prefix with its last byte incremented", bound)
	}

	if _, ok := keyPrefixUpperBound(""); ok {
		t.Fatal("an empty prefix reported an upper bound, want an open-ended range")
	}
	if _, ok := keyPrefixUpperBound("\xff\xff"); ok {
		t.Fatal("an all-0xff prefix reported an upper bound, want an open-ended range")
	}
}

func openCursorRequestTestDatabase(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		"CREATE TABLE ItemTable(key TEXT UNIQUE, value BLOB)",
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER, value TEXT)",
		`INSERT INTO ItemTable(key, value) VALUES ('aiService.generations', '[{"unixMs":1710000000500,"generationUUID":"req-neighbor","type":"composer","textDescription":"prompt text stays undecoded"}]')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-1', '{"_v":3,"text":"hello","requestId":"req-quoted-longer"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-a:bubble-2', '{"_v":3,"text":"see req-quoted for context","requestId":"req-two"}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('bubbleId:composer-ab:bubble-n', '{"_v":3,"text":"neighbor","requestId":"req-neighbor"}')`,
		`INSERT INTO composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, isArchived, isSubagent, recency, checkpointAt, value) VALUES ('composer-a', 'ws', 1710000000000, 1710000010000, 0, 0, 0, 0, '{}')`,
		`INSERT INTO composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, isArchived, isSubagent, recency, checkpointAt, value) VALUES ('composer-ab', 'ws', 1710000020000, 1710000030000, 0, 0, 0, 0, '{}')`,
		`INSERT INTO composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, isArchived, isSubagent, recency, checkpointAt, value) VALUES ('composer-c', 'other-ws', 1710000005000, 1710000010000, 0, 0, 0, 0, '{}')`,
		// A chat Cursor never gave a `lastUpdatedAt`, which is 52.5% of the chats on
		// a real store, with `checkpointAt` recording where it actually ends.
		`INSERT INTO composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, isArchived, isSubagent, recency, checkpointAt, value) VALUES ('composer-unstamped', 'ws', 1710000040000, NULL, 0, 0, 1710000040000, 1710000050000, '{}')`,
		// The same shape with no end recorded anywhere, which is what a chat created
		// and never checkpointed looks like.
		`INSERT INTO composerHeaders(composerId, workspaceId, createdAt, lastUpdatedAt, isArchived, isSubagent, recency, checkpointAt, value) VALUES ('composer-openended', 'ws', 1710000060000, 0, 0, 0, 0, NULL, '{}')`,
	}
	for _, statement := range statements {
		if _, err := writable.Exec(statement); err != nil {
			_ = writable.Close()
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}

	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	return readonly
}

// TestReadComposerHeaderCoverageCountsChatsThePredicateCannotReach is the
// membership-versus-reachability distinction. A header row the narrowing can
// never select is as far out of reach as one the table never listed: a NULL
// createdAt makes both sides of the lifetime predicate NULL, and a zero one with
// no end stamp gives an end of zero.
func TestReadComposerHeaderCoverageCountsChatsThePredicateCannotReach(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	writable, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("sql.Open returned error: %v", err)
	}
	statements := []string{
		"CREATE TABLE cursorDiskKV(key TEXT UNIQUE ON CONFLICT REPLACE, value BLOB)",
		"CREATE TABLE composerHeaders(composerId TEXT PRIMARY KEY, workspaceId TEXT, createdAt INTEGER, lastUpdatedAt INTEGER, isArchived INTEGER, isSubagent INTEGER, recency INTEGER, checkpointAt INTEGER, value TEXT)",
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:chat-reachable', '{}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:chat-null-created', '{}')`,
		`INSERT INTO cursorDiskKV(key, value) VALUES ('composerData:chat-zero-created', '{}')`,
		`INSERT INTO composerHeaders VALUES ('chat-reachable', 'ws', 1710000000000, 1710000001000, 0, 0, 0, 0, '{}')`,
		`INSERT INTO composerHeaders VALUES ('chat-null-created', 'ws', NULL, 1710000001000, 0, 0, 0, 0, '{}')`,
		`INSERT INTO composerHeaders VALUES ('chat-zero-created', 'ws', 0, NULL, 0, 0, NULL, NULL, '{}')`,
	}
	for _, statement := range statements {
		if _, err := writable.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
	if err := writable.Close(); err != nil {
		t.Fatalf("close writable sqlite database: %v", err)
	}
	readonly, err := OpenReadOnlyDatabase(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnlyDatabase returned error: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })

	// Neither unreachable chat is selected by the predicate at any instant.
	for _, instant := range []int64{0, 1710000000500} {
		narrowing, err := ListComposerIDsCoveringTime(context.Background(), readonly, instant, "")
		if err != nil {
			t.Fatalf("ListComposerIDsCoveringTime returned error: %v", err)
		}
		for _, composerID := range narrowing.ComposerIDs {
			if composerID == "chat-null-created" {
				t.Fatalf("the predicate selected chat-null-created at %d, so the fixture does not model an unreachable row", instant)
			}
		}
	}

	coverage, err := ReadComposerHeaderCoverage(context.Background(), readonly)
	if err != nil {
		t.Fatalf("ReadComposerHeaderCoverage returned error: %v", err)
	}
	if coverage.UnlistedChats != 2 {
		t.Fatalf("UnlistedChats = %d, want the two chats listed under rows no predicate can select", coverage.UnlistedChats)
	}
	if coverage.Complete() {
		t.Fatalf("coverage %+v reported complete while two stored chats are unreachable", coverage)
	}
}
