package cursorstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// generationsItemKey is the per-workspace `ItemTable` row holding Cursor's
// recent generation ring. Cursor caps the array, so a request older than the cap
// has no entry left in any workspace database.
const generationsItemKey = "aiService.generations"

// GenerationEntry is one entry of a workspace's `aiService.generations` array.
// The array also carries a `textDescription` field holding the prompt the
// request started from; it is deliberately not decoded, because resolving a
// request id needs the identifier and never the content.
type GenerationEntry struct {
	GenerationUUID string `json:"generationUUID"`
	UnixMs         int64  `json:"unixMs"`
	Type           string `json:"type"`
}

// GenerationHit names the workspace database that carried one request id and the
// generation entry found there.
type GenerationHit struct {
	WorkspaceHash string
	Entry         GenerationEntry
}

// GenerationLookup reports the outcome of searching a data root's workspace
// databases for one request id.
type GenerationLookup struct {
	// Hits holds every workspace ring that listed the id, in the order the sweep
	// read them. It is plural because a workspace ring is per window and the same
	// request can be listed by more than one of them, at instants that differ by
	// however long the two windows were apart when they wrote it. Keeping only the
	// first hit would narrow the chat search to one of those instants and answer
	// definitively from a search the other instant was never part of.
	Hits []GenerationHit
	// UnreadableWorkspaces counts the workspaces this search did not read: the
	// databases that could not be opened or decoded, and the directory entries the
	// listing could not examine. Cursor runs while Clyde reads, so a locked
	// database reads exactly like one that never held the id. A miss with a
	// non-zero count is therefore inconclusive rather than a confirmed absence, and
	// the caller must say so instead of asserting the request is gone.
	UnreadableWorkspaces int
}

// Found reports whether any workspace ring listed the request id.
func (lookup GenerationLookup) Found() bool {
	return len(lookup.Hits) > 0
}

// Instants returns the distinct times the rings recorded for the request, in the
// order the sweep found them. Two windows writing the same instant is one search
// to run, not two, so the caller narrows the chat headers once per value here.
func (lookup GenerationLookup) Instants() []int64 {
	seen := make(map[int64]bool, len(lookup.Hits))
	instants := make([]int64, 0, len(lookup.Hits))
	for _, hit := range lookup.Hits {
		if seen[hit.Entry.UnixMs] {
			continue
		}
		seen[hit.Entry.UnixMs] = true
		instants = append(instants, hit.Entry.UnixMs)
	}
	return instants
}

// WorkspaceHashes returns the workspaces whose rings listed the request, for the
// log line that says where it was seen.
func (lookup GenerationLookup) WorkspaceHashes() []string {
	hashes := make([]string, 0, len(lookup.Hits))
	for _, hit := range lookup.Hits {
		hashes = append(hashes, hit.WorkspaceHash)
	}
	return hashes
}

// BubbleRequestRef locates the composer bubble that carried one request id.
type BubbleRequestRef struct {
	ComposerID string
	BubbleID   string
}

// DecodeGenerationEntriesJSON decodes one `aiService.generations` value.
func DecodeGenerationEntriesJSON(data []byte) ([]GenerationEntry, error) {
	var entries []GenerationEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, CursorJSONDecodeError{Description: "generation entries", Err: err}
	}
	return entries, nil
}

// ReadGenerationEntries reads one workspace database's recent generation ring.
func ReadGenerationEntries(ctx context.Context, workspaceDB *sql.DB) ([]GenerationEntry, bool, error) {
	value, found, err := ReadKVValue(ctx, workspaceDB, KVTableItemTable, generationsItemKey)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.generations_read_failed", "concern", concern, "err", err)
		return nil, false, fmt.Errorf("read cursor generation entries: %w", err)
	}
	if !found {
		return nil, false, nil
	}

	entries, err := DecodeGenerationEntriesJSON(value)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.generations_decode_failed", "concern", concern, "err", err)
		return nil, false, fmt.Errorf("decode cursor generation entries: %w", err)
	}
	return entries, true, nil
}

// FindGenerationEntry searches one data root's workspace databases for the
// generation entries whose uuid equals requestID exactly, and returns every
// workspace that carried one.
//
// Every workspace is read, because each holds its own capped ring and skipping
// one would turn a real hit into a reported miss. The sweep is affordable at that
// width: measured against a store of 122 workspaces with Cursor running, a
// complete miss cost 63ms, which is nowhere near the exhaustive bubble scan the
// opt-in flag exists to gate. Its bound is the caller's own context deadline,
// checked here rather than left to fail one database open at a time.
//
// The sweep runs to the end rather than stopping at the first ring that lists
// the id, because the instant a hit carries is what narrows the chat search, and
// two windows can list the same request at instants far enough apart to select
// different chats. Stopping early would answer definitively from a search the
// second instant was never part of, and the cost of going on is the same 63ms a
// complete miss already pays.
//
// A workspace database that fails to open or decode is skipped rather than
// failing the search, so one damaged workspace cannot hide a hit in another, and
// it is counted so the caller can tell an inconclusive miss from a confirmed one.
// Workspaces the listing could not examine, and workspaces left unread when the
// deadline expires, count the same way.
//
// A workspaceStorage directory that does not exist is not counted, because it is
// an answer: there are no rings on this machine, so a miss across them is real.
func FindGenerationEntry(ctx context.Context, root DataRoot, requestID string) (GenerationLookup, error) {
	var emptyLookup GenerationLookup

	listing, err := root.ListWorkspaceEntries()
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.generation_workspaces_list_failed", "concern", concern, "path", root.WorkspaceStorageDir, "err", err)
		return emptyLookup, fmt.Errorf("list cursor workspaces for request %q: %w", requestID, err)
	}
	entries := listing.Entries
	sortWorkspaceEntriesNewestFirst(entries)

	lookup := GenerationLookup{Hits: nil, UnreadableWorkspaces: listing.Unreadable}
	for position, entry := range entries {
		if ctxErr := ctx.Err(); ctxErr != nil {
			unread := len(entries) - position
			lookup.UnreadableWorkspaces += unread
			slog.WarnContext(ctx, "providers.cursor.store.generation_sweep_cut_short", "concern", concern, "path", root.WorkspaceStorageDir, "request_id", requestID, "read", position, "unread", unread, "err", ctxErr)
			return lookup, nil
		}
		hit, found, readable := findGenerationEntryInWorkspace(ctx, entry, requestID)
		if !readable {
			lookup.UnreadableWorkspaces++
			continue
		}
		if found {
			lookup.Hits = append(lookup.Hits, hit)
			slog.DebugContext(ctx, "providers.cursor.store.generation_sweep_hit", "concern", concern, "path", root.WorkspaceStorageDir, "request_id", requestID, "workspace_hash", hit.WorkspaceHash, "read", position+1)
		}
	}
	slog.DebugContext(ctx, "providers.cursor.store.generation_sweep_finished", "concern", concern, "path", root.WorkspaceStorageDir, "request_id", requestID, "count", len(entries), "hits", len(lookup.Hits), "unreadable", lookup.UnreadableWorkspaces)
	return lookup, nil
}

// sortWorkspaceEntriesNewestFirst orders workspace databases by modification
// time, most recently written first. A workspace whose file cannot be stat'ed
// sorts last rather than dropping out.
func sortWorkspaceEntriesNewestFirst(entries []WorkspaceEntry) {
	modTimes := make(map[string]time.Time, len(entries))
	for _, entry := range entries {
		info, err := os.Stat(entry.StateDBPath)
		if err != nil {
			continue
		}
		modTimes[entry.StateDBPath] = info.ModTime()
	}
	sort.SliceStable(entries, func(i int, j int) bool {
		return modTimes[entries[i].StateDBPath].After(modTimes[entries[j].StateDBPath])
	})
}

// findGenerationEntryInWorkspace reads one workspace's ring. The third result is
// false when the database could not be read at all, which is distinct from a
// database that was read and did not carry the id.
func findGenerationEntryInWorkspace(
	ctx context.Context,
	entry WorkspaceEntry,
	requestID string,
) (GenerationHit, bool, bool) {
	var emptyHit GenerationHit

	workspaceDB, err := OpenReadOnlyDatabase(ctx, entry.StateDBPath)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.generation_workspace_open_failed", "concern", concern, "path", entry.StateDBPath, "workspace_hash", entry.WorkspaceHash, "err", err)
		return emptyHit, false, false
	}
	defer func() { _ = workspaceDB.Close() }()

	generations, found, err := ReadGenerationEntries(ctx, workspaceDB)
	if err != nil {
		return emptyHit, false, false
	}
	if !found {
		return emptyHit, false, true
	}
	for _, generation := range generations {
		if generation.GenerationUUID != requestID {
			continue
		}
		return GenerationHit{WorkspaceHash: entry.WorkspaceHash, Entry: generation}, true, true
	}
	return emptyHit, false, true
}

// composerLifetimeToleranceMs widens each end of a chat's recorded lifetime, so
// the ordinary lag between Cursor stamping a request and stamping the chat that
// issued it does not exclude the chat from its own lookup.
//
// It is a measured bound rather than a margin of comfort. Joining every
// generation ring on a real machine to the chat each entry belongs to gives 381
// pairs where both the request instant and the owning chat are known. Under the
// widest end stamp Cursor recorded, 24 of those pairs still have the request
// arriving after it, and every one of the 24 by between 10 and 234 milliseconds,
// which is Cursor writing the ring entry a moment before the chat's own
// checkpoint. One second covers all of them with two orders of magnitude to
// spare, and it costs almost nothing: the candidate set it produces averages 6.3
// chats against 6.0 with no tolerance at all.
const composerLifetimeToleranceMs = 1000

// composerLifetimeEndSQL is the end of a chat's recorded lifetime, taken as the
// newest of the timestamps `composerHeaders` keeps.
//
// `lastUpdatedAt` alone cannot serve. On a real store it is null on 1,034 of
// 2,390 chats and zero on another 221, so 52.5% of chats have no end recorded
// there at all, and a predicate requiring one excludes every one of them from
// every lookup, including the chat that issued the request being looked up.
// Measured over the 381 known pairs, the predicate admitted the owning chat for
// only 68 of them.
//
// `checkpointAt` is what actually records a chat's end: it covers 301 of those
// 381 against 68 for `lastUpdatedAt` and 68 for `recency`. Taking the newest of
// all four, `createdAt` included so a chat with only a creation time still
// brackets its own first instant, admits the owning chat for 354 of the 381, and
// 378 once [composerLifetimeToleranceMs] is added.
//
// Dropping the upper bound entirely would admit 378 as well, and it is the wrong
// trade: the candidate set goes from 6.3 chats to 1,614, each of which costs a
// bubble key-range read, which is the exhaustive scan this path exists to avoid.
//
// Each of the three end columns is optional, because Cursor's schema is
// undocumented and version pinned and a column it renames or drops would
// otherwise make every request-id lookup on that machine fail. What remains
// required is `composerId`, which is the answer, and `createdAt`, which is the
// only column every observed version writes on every row.
var composerLifetimeEndColumns = []string{"lastUpdatedAt", "checkpointAt", "recency"}

const (
	composerIDColumn        = "composerId"
	composerCreatedAtColumn = "createdAt"
	composerWorkspaceColumn = "workspaceId"
)

// composerLifetimeEndSQL renders the end of a chat's recorded lifetime from the
// end columns this database actually has, always including `createdAt` so a chat
// with only a creation time still brackets its own first instant.
//
// A database with none of the optional columns yields `createdAt` alone rather
// than a call wrapping it, because SQLite's one-argument `max` is the aggregate
// rather than the scalar and would be a misuse here.
func composerLifetimeEndSQL(columns map[string]bool) string {
	present := make([]string, 0, len(composerLifetimeEndColumns))
	for _, column := range composerLifetimeEndColumns {
		if columns[column] {
			present = append(present, column)
		}
	}
	if len(present) == 0 {
		return composerCreatedAtColumn
	}

	var expression strings.Builder
	expression.WriteString("max(")
	expression.WriteString(composerCreatedAtColumn)
	for _, column := range present {
		expression.WriteString(", coalesce(")
		expression.WriteString(column)
		expression.WriteString(", 0)")
	}
	expression.WriteString(")")
	return expression.String()
}

// selectComposerIDsCoveringTimeQuery builds the narrowing query. Placeholders
// bind the tolerance and the instant twice, then the workspace twice when the
// table has a workspace column. Every name in the statement is a constant of this
// package or a column drawn from [composerLifetimeEndColumns], never caller text.
func selectComposerIDsCoveringTimeQuery(columns map[string]bool, scopeByWorkspace bool) string {
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(composerIDColumn)
	query.WriteString(" FROM ")
	query.WriteString(composerHeadersTableName)
	query.WriteString(" WHERE ")
	query.WriteString(composerCreatedAtColumn)
	query.WriteString(" - ? <= ? AND ")
	query.WriteString(composerLifetimeEndSQL(columns))
	query.WriteString(" + ? >= ?")
	if scopeByWorkspace {
		query.WriteString(" AND (? = '' OR ")
		query.WriteString(composerWorkspaceColumn)
		query.WriteString(" = ?)")
	}
	query.WriteString(" ORDER BY ")
	query.WriteString(composerIDColumn)
	return query.String()
}

// ComposerNarrowing is what the chat header table could tell one lookup: the
// candidate chats, and whether the table was able to answer at all.
//
// Usable is false when the table is missing or lacks a column the predicate
// needs. That is not a miss. A caller that reads an empty candidate list as a
// confirmed absence would be asserting something a table it could not query
// never said.
type ComposerNarrowing struct {
	ComposerIDs []string
	Usable      bool
	// ExcludedChats counts the chats the header table lists, under a row a
	// predicate over it can select, that this instant's predicate did not select.
	//
	// It is what separates the two questions a narrowed miss has to answer.
	// [ComposerHeaderCoverage] says the table accounts for every stored chat, which
	// is membership; this says the search actually reached them, which is not the
	// same thing. A chat whose recorded lifetime does not span the instant is
	// listed, is reachable in principle, and was never read, so a miss over the
	// candidates it produced proves nothing about that chat. A non-zero count is
	// therefore an inconclusive miss rather than a confirmed absence.
	ExcludedChats int
}

// reachableComposerHeaderCountQuery counts the header rows a predicate over the
// table can select at all, under the same workspace scope the candidate query
// runs with, so the two counts are taken over the same set of chats.
//
// A NULL `createdAt` makes both sides of the lifetime comparison NULL, and a zero
// one with no end stamp gives an end of zero, so either row is listed and never
// selected. Counting those as excluded would report every lookup inconclusive
// over rows no instant could ever reach.
func reachableComposerHeaderCountQuery(scopeByWorkspace bool) string {
	query := "SELECT count(*) FROM " + composerHeadersTableName +
		" WHERE " + composerCreatedAtColumn + " IS NOT NULL AND " + composerCreatedAtColumn + " > 0"
	if scopeByWorkspace {
		query += " AND (? = '' OR " + composerWorkspaceColumn + " = ?)"
	}
	return query
}

// readTableColumns reports which columns one table has, so a predicate over an
// undocumented, version-pinned schema can be built from what is there rather
// than from what one Cursor version happened to write.
func readTableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, tableColumnsQuery, table)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.table_columns_query_failed", "concern", concern, "table", table, "err", err)
		return nil, fmt.Errorf("query cursor %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	return scanTableColumns(ctx, rows, table)
}

// scanTableColumns collects the column names one `pragma_table_info` read
// yielded, so the pooled read and the snapshot read fold the rows identically.
func scanTableColumns(ctx context.Context, rows *sql.Rows, table string) (map[string]bool, error) {
	columns := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.table_column_scan_failed", "concern", concern, "table", table, "err", err)
			return nil, fmt.Errorf("scan cursor %s column: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.table_columns_iterate_failed", "concern", concern, "table", table, "err", err)
		return nil, fmt.Errorf("iterate cursor %s columns: %w", table, err)
	}
	return columns, nil
}

// ListComposerIDsCoveringTime lists the chats whose recorded lifetime spans
// unixMs. It reads the small `composerHeaders` table, whose real columns make
// this predicate cheap, and it exists to narrow the expensive bubble read to a
// couple of candidates.
//
// workspaceID narrows further to the chats belonging to one workspace, since
// `composerHeaders.workspaceId` holds the same hash that names a workspace's
// storage directory. Passing an empty workspaceID searches every workspace, and
// so does a database whose header table has no workspace column.
//
// It also reports how many listed chats the instant left out, because that is
// the difference between a search that read every chat and one that read a
// window of them.
func ListComposerIDsCoveringTime(
	ctx context.Context,
	db *sql.DB,
	unixMs int64,
	workspaceID string,
) (ComposerNarrowing, error) {
	narrowing := ComposerNarrowing{ComposerIDs: nil, Usable: false, ExcludedChats: 0}

	if ctx == nil {
		return narrowing, fmt.Errorf("list cursor composers covering %d: nil context", unixMs)
	}
	if db == nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_narrowing_no_database", "concern", concern, "unix_ms", unixMs)
		return narrowing, fmt.Errorf("list cursor composers covering %d: nil database", unixMs)
	}
	exists, err := TableExists(ctx, db, composerHeadersTableName)
	if err != nil {
		return narrowing, err
	}
	if !exists {
		return narrowing, nil
	}
	columns, err := readTableColumns(ctx, db, composerHeadersTableName)
	if err != nil {
		return narrowing, err
	}
	if !columns[composerIDColumn] || !columns[composerCreatedAtColumn] {
		slog.WarnContext(ctx, "providers.cursor.store.composer_headers_unusable", "concern", concern, "table", composerHeadersTableName, "has_composer_id", columns[composerIDColumn], "has_created_at", columns[composerCreatedAtColumn])
		return narrowing, nil
	}
	narrowing.Usable = true

	// The empty-workspace case is expressed in SQL rather than as a second query
	// string, so both shapes bind the same placeholders in the same order. A
	// database with no workspace column binds neither.
	scopeByWorkspace := columns[composerWorkspaceColumn]
	query := selectComposerIDsCoveringTimeQuery(columns, scopeByWorkspace)
	var rows *sql.Rows
	if scopeByWorkspace {
		rows, err = db.QueryContext(ctx, query,
			composerLifetimeToleranceMs, unixMs, composerLifetimeToleranceMs, unixMs,
			workspaceID, workspaceID)
	} else {
		rows, err = db.QueryContext(ctx, query,
			composerLifetimeToleranceMs, unixMs, composerLifetimeToleranceMs, unixMs)
	}
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_headers_query_failed", "concern", concern, "unix_ms", unixMs, "err", err)
		return ComposerNarrowing{ComposerIDs: nil, Usable: false, ExcludedChats: 0}, fmt.Errorf("query cursor composer headers covering %d: %w", unixMs, err)
	}
	defer func() { _ = rows.Close() }()

	composerIDs := make([]string, 0)
	for rows.Next() {
		var composerID string
		if err := rows.Scan(&composerID); err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.composer_header_scan_failed", "concern", concern, "unix_ms", unixMs, "err", err)
			return ComposerNarrowing{ComposerIDs: nil, Usable: false, ExcludedChats: 0}, fmt.Errorf("scan cursor composer header covering %d: %w", unixMs, err)
		}
		composerIDs = append(composerIDs, composerID)
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_headers_iterate_failed", "concern", concern, "unix_ms", unixMs, "err", err)
		return ComposerNarrowing{ComposerIDs: nil, Usable: false, ExcludedChats: 0}, fmt.Errorf("iterate cursor composer headers covering %d: %w", unixMs, err)
	}
	narrowing.ComposerIDs = composerIDs

	reachable, err := countReachableComposerHeaders(ctx, db, scopeByWorkspace, workspaceID)
	if err != nil {
		// The candidates stand; what is lost is the ability to say the search
		// reached every listed chat, so the count is left saying it did not.
		narrowing.ExcludedChats = len(composerIDs) + 1
		return narrowing, nil
	}
	if reachable > len(composerIDs) {
		narrowing.ExcludedChats = reachable - len(composerIDs)
	}
	return narrowing, nil
}

// countReachableComposerHeaders counts the header rows any instant's predicate
// could select, which is what the candidates for one instant are compared
// against.
func countReachableComposerHeaders(
	ctx context.Context,
	db *sql.DB,
	scopeByWorkspace bool,
	workspaceID string,
) (int, error) {
	query := reachableComposerHeaderCountQuery(scopeByWorkspace)
	var row *sql.Row
	if scopeByWorkspace {
		row = db.QueryRowContext(ctx, query, workspaceID, workspaceID)
	} else {
		row = db.QueryRowContext(ctx, query)
	}
	reachable := 0
	if err := row.Scan(&reachable); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_header_reachable_count_failed", "concern", concern, "err", err)
		return 0, fmt.Errorf("count reachable cursor composer headers: %w", err)
	}
	return reachable, nil
}

// ComposerHeaderCoverage says how much of the store the chat header table
// accounts for, which is what tells a narrowed lookup whether its miss covered
// every chat.
//
// The narrowing reads `composerHeaders`, and that table is not a complete index
// of the chats the store holds: on a real store it lists 2,390 chats while
// `composerData:` holds 2,470, so 100 chats are unreachable from any predicate
// over it. A miss narrowed that way is therefore only a confirmed absence when
// the table accounts for every chat.
//
// The question is which chats, not how many. Equal counts over different
// membership would pass a count comparison while leaving a stored chat unlisted,
// which is the case a confirmed absence must not be built on.
type ComposerHeaderCoverage struct {
	StoredChats int
	// UnlistedChats counts the stored chats a lookup narrowed through the header
	// table could never have reached: the ones it does not list, and the ones it
	// lists under a row no predicate over it can select.
	UnlistedChats int
	// Readable is false when the store's own chat table is absent, so this
	// coverage was never established. A database with no `cursorDiskKV` holds no
	// chats to compare against and must not read as complete.
	Readable bool
}

// Complete reports whether the header table accounts for every stored chat.
func (coverage ComposerHeaderCoverage) Complete() bool {
	return coverage.Readable && coverage.UnlistedChats == 0
}

// ReadComposerHeaderCoverage counts the stored chats the header table does not
// list. The comparison runs inside SQLite over two indexes, and it measured at
// 14ms against a 17.9 GB store, so it costs nothing next to the bubble reads it
// qualifies.
func ReadComposerHeaderCoverage(ctx context.Context, db *sql.DB) (ComposerHeaderCoverage, error) {
	coverage := ComposerHeaderCoverage{StoredChats: 0, UnlistedChats: 0, Readable: false}

	if ctx == nil {
		return coverage, fmt.Errorf("read cursor composer header coverage: nil context")
	}
	if db == nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_header_coverage_no_database", "concern", concern)
		return coverage, fmt.Errorf("read cursor composer header coverage: nil database")
	}

	// Every statement runs on one snapshot, because the answer is a comparison
	// between the chats the store holds and the chats the header table reaches, and
	// Cursor writes while Clyde reads. Two statements on a pooled handle would
	// compare two different stores.
	snapshot, err := beginReadSnapshot(ctx, db)
	if err != nil {
		return coverage, err
	}
	defer snapshot.rollback()

	chatsExist, err := snapshot.tableExists(ctx, string(KVTableCursorDiskKV))
	if err != nil {
		return coverage, err
	}
	if !chatsExist {
		slog.WarnContext(ctx, "providers.cursor.store.chat_table_absent", "concern", concern, "table", string(KVTableCursorDiskKV))
		return coverage, nil
	}
	coverage.Readable = true

	storedChats, err := snapshot.countRange(ctx, keyRangeForPrefix(composerDataKeyPrefix))
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_chat_count_failed", "concern", concern, "err", err)
		return coverage, fmt.Errorf("count cursor chats: %w", err)
	}
	coverage.StoredChats = storedChats

	headersExist, err := snapshot.tableExists(ctx, composerHeadersTableName)
	if err != nil {
		return coverage, err
	}
	if !headersExist {
		// With no header table, every stored chat is one the narrowing could not
		// reach.
		coverage.UnlistedChats = storedChats
		return coverage, nil
	}
	columns, err := snapshot.tableColumns(ctx, composerHeadersTableName)
	if err != nil {
		return coverage, err
	}
	if !columns[composerIDColumn] {
		coverage.UnlistedChats = storedChats
		return coverage, nil
	}

	// The question is reachability, not membership. A chat the header table lists
	// under a row no predicate can select is as far out of reach as one it never
	// listed: a NULL `createdAt` makes both sides of the lifetime predicate NULL,
	// and a zero one with no end stamp gives an end of zero, so either row is
	// listed and never selected. Requiring a usable `createdAt` is what makes the
	// count match what the narrowing could actually have found. Every row on the
	// operator's store carries a non-zero one, which is why this changes nothing
	// there and is not something to rely on.
	//
	// The chat id starts one byte past the key prefix, and the prefix is a
	// constant of this package rather than caller text, so its length is written
	// into the statement instead of bound beside the range.
	reachable := " AND h." + composerCreatedAtColumn + " IS NOT NULL AND h." + composerCreatedAtColumn + " > 0"
	if !columns[composerCreatedAtColumn] {
		// Without the column no row is reachable, which the NOT EXISTS renders by
		// matching nothing.
		reachable = " AND 0"
	}
	bounds := keyRangeForPrefix(composerDataKeyPrefix)
	query := "SELECT count(*) FROM " + string(KVTableCursorDiskKV) +
		keyRangePredicate(bounds, "") +
		" AND NOT EXISTS (SELECT 1 FROM " + composerHeadersTableName + " h" +
		" WHERE h." + composerIDColumn + " = substr(key, " +
		strconv.Itoa(len(composerDataKeyPrefix)+1) + ")" + reachable + ")"
	rows, err := snapshot.queryRange(ctx, query, bounds, "chats the header table cannot reach")
	if err != nil {
		return coverage, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		slog.WarnContext(ctx, "providers.cursor.store.composer_header_coverage_empty", "concern", concern)
		return coverage, fmt.Errorf("count cursor chats the header table omits: no row")
	}
	if err := rows.Scan(&coverage.UnlistedChats); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_header_coverage_scan_failed", "concern", concern, "err", err)
		return coverage, fmt.Errorf("scan cursor chats the header table omits: %w", err)
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_header_coverage_iterate_failed", "concern", concern, "err", err)
		return coverage, fmt.Errorf("iterate cursor chats the header table omits: %w", err)
	}
	return coverage, nil
}

// BubbleRequestSearch is what one search for a request id over a key range
// established: every bubble found carrying it, and how many rows the search
// could not read.
//
// Both halves are load bearing. The carriers are plural because a request id
// does not name one chat: duplicating a chat copies its turns under fresh bubble
// ids, request ids and all. Measured on a real store, 767 of 11,396 distinct
// request ids appear in more than one chat, the worst in nine.
//
// UnreadableRows counts the rows this search skipped, and a miss carrying any is
// not a confirmed absence. Every row it looks at was selected by a byte search
// for the id, so it provably holds those bytes somewhere; a row that then fails
// to decode is the likeliest place the answer was, not the least.
type BubbleRequestSearch struct {
	Carriers       []BubbleRequestRef
	UnreadableRows int
}

// FindBubbleRequestIDInComposer looks for bubbles carrying requestID inside one
// composer. The key range keeps the read on the primary key index, so the cost
// is proportional to that one chat rather than to the whole table.
func FindBubbleRequestIDInComposer(
	ctx context.Context,
	db *sql.DB,
	composerID string,
	requestID string,
) (BubbleRequestSearch, error) {
	bounds := keyRangeForPrefix(bubbleKeyPrefix + composerID + ":")
	return findBubbleRequestIDInRange(ctx, db, bounds, requestID)
}

// ScanAllBubblesForRequestID looks for bubbles carrying requestID across every
// chat in the database. It reads every bubble row Cursor has ever written and is
// therefore the slow path: callers must offer it only as an explicit opt-in and
// never run it for a routine lookup.
func ScanAllBubblesForRequestID(
	ctx context.Context,
	db *sql.DB,
	requestID string,
) (BubbleRequestSearch, error) {
	return findBubbleRequestIDInRange(ctx, db, keyRangeForPrefix(bubbleKeyPrefix), requestID)
}

// findBubbleRequestIDInRange reads the candidate rows a byte search left in one
// key range, then requires whole-string equality on the decoded request id. The
// byte search alone would accept a value that merely mentions the id somewhere
// in its content, so the decode step is what makes the match exact.
//
// The decode is [DecodeBubbleJSON], the one decoder this package has for a
// bubble value, rather than a narrower one that reads the id field alone. A
// narrower decoder disagrees with it: a row this package cannot read as a bubble
// would be named here as the definite chat that issued the request while every
// other reader rejects it. Counting that row unread instead says what is true,
// which is that a row holding the id's bytes was not read.
//
// The read continues past every row, rather than stopping at the first match,
// because one match is not evidence that there is only one.
func findBubbleRequestIDInRange(
	ctx context.Context,
	db *sql.DB,
	bounds keyRange,
	requestID string,
) (BubbleRequestSearch, error) {
	search := BubbleRequestSearch{Carriers: nil, UnreadableRows: 0}

	if strings.TrimSpace(requestID) == "" {
		return search, nil
	}
	rows, err := readKVRowsInKeyRange(ctx, db, KVTableCursorDiskKV, bounds, requestID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.bubble_request_range_failed", "concern", concern, "key_lower", bounds.Lower, "err", err)
		return search, fmt.Errorf("read cursor bubbles for request id: %w", err)
	}
	for _, row := range rows {
		bubble, err := DecodeBubbleJSON(row.Value)
		if err != nil {
			// This row holds the id's bytes, which is why the byte search selected
			// it. Skipping it quietly would turn the likeliest carrier into evidence
			// that no carrier exists, so it is counted and the caller reports the
			// miss as inconclusive rather than confirmed.
			search.UnreadableRows++
			slog.WarnContext(ctx, "providers.cursor.store.bubble_request_decode_skipped", "concern", concern, "key", row.Key, "err", err)
			continue
		}
		if bubble.RequestID != requestID {
			continue
		}
		composerID, bubbleID, ok := parseBubbleKey(row.Key)
		if !ok {
			// A row that matched exactly and whose key will not parse is a carrier
			// this search cannot name, which is a gap in the answer rather than an
			// absence of one.
			search.UnreadableRows++
			slog.WarnContext(ctx, "providers.cursor.store.bubble_request_key_unparsed", "concern", concern, "key", row.Key)
			continue
		}
		search.Carriers = append(search.Carriers, BubbleRequestRef{ComposerID: composerID, BubbleID: bubbleID})
	}
	return search, nil
}
