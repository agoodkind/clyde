package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"hash"
	"log/slog"
	"slices"
	"sort"
	"strconv"
)

// StreamComposerBubbles passes one composer's conversation to visit in reading
// order, built from every bubble row Cursor stored rather than from the composer
// header's reference list alone. Returning false from visit stops the read
// without decoding the rest of the chat.
//
// The header's `fullConversationHeadersOnly` is not a complete index of a chat's
// bubbles. Measured on a real store, one chat referenced 591 bubbles while its
// key range held 3,610 rows, and content the header never referenced included the
// turn a request id pointed at. Reading only the referenced bubbles therefore
// drops real conversation.
//
// Reading the whole key range naively is worse, because most unreferenced rows
// are superseded copies: Cursor rewrites a turn under a new local bubble id and
// leaves the previous row in place. Assembly is consequently three rules rather
// than one:
//
//  1. The header order is authoritative for the bubbles it references and is
//     reproduced exactly, each reference appearing once.
//  2. An unreferenced bubble is dropped only when it is a superseded copy of a
//     bubble already kept, which [keptUnreferencedBubbles] establishes by
//     identity rather than by content wherever Cursor recorded one.
//  3. A kept unreferenced bubble is placed by its `createdAt` between the two
//     dated header bubbles that bracket it, never by reordering the header.
//
// The read runs in two passes. The first projects each row of the key range down
// to a [bubbleDigest] inside SQLite, so ordering never pulls a bubble's payload
// into memory; the second reads each bubble the order actually emits, by exact
// key, and stops the moment visit declines the next one.
//
// Ordering is inherently eager, because an unreferenced bubble's place cannot be
// known until every stored `createdAt` has been seen. What the projection buys is
// the size of that eager step: the widest chat in a real store holds 22,724 rows
// and 933 MB of bubble payload, of which the fields assembly reads are 267 MB,
// and none of it is decoded in Go.
// composerID is the chat whose rows are read, and it is the caller's key-derived
// id rather than anything in the header payload. The two agree on every chat this
// repository has seen, and only one of them is authoritative: the key range is
// what the rows are actually stored under, while `composerData.composerId` is a
// field the payload may omit, which would silently read the `bubbleId::` range
// and serve an empty conversation.
func StreamComposerBubbles(
	ctx context.Context,
	db *sql.DB,
	composerID string,
	header ComposerHeader,
	visit func(Bubble) bool,
) error {
	stored, err := readComposerBubbleDigests(ctx, db, composerID)
	if err != nil {
		return err
	}
	order, err := assembleBubbleOrder(ctx, composerID, header, stored)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_assembly_failed", "concern", concern, "composer_id", composerID, "err", err)
		return err
	}
	for _, entry := range order {
		bubble, found, err := readBubble(ctx, db, composerID, entry)
		if err != nil {
			return err
		}
		if !found {
			if entry.Referenced {
				// The digest pass saw this row. The two passes are separate reads, so a
				// referenced bubble that has since vanished is a message the header
				// ordered into the conversation and the store no longer holds, which is
				// the same loss as one that will not parse.
				slog.WarnContext(ctx, "providers.cursor.store.referenced_bubble_vanished", "concern", concern, "composer_id", composerID, "bubble_id", entry.BubbleID)
				return fmt.Errorf(
					"read cursor bubble %q of composer %q: row disappeared between the ordering pass and the read",
					entry.BubbleID, composerID,
				)
			}
			continue
		}
		if !visit(bubble) {
			return nil
		}
	}
	return nil
}

// bubbleDigest is everything assembly needs about one stored bubble: enough to
// place it and to recognise a superseded copy, and nothing that would retain the
// bubble's content.
//
// Holding the decoded bubbles instead costs the whole chat at once. One real
// chat's key range holds 22,724 rows whose text, thinking, and tool results run
// to 933 MB, which a caller reading the first few turns would pay in full. A
// digest is a fixed hundred-odd bytes.
type bubbleDigest struct {
	BubbleID string
	// ServerBubbleID is Cursor's server-side identity for the message, empty on
	// the rows Cursor never gave one.
	ServerBubbleID string
	// CreatedAt is the bubble's RFC3339 write time, empty when the row carries
	// none.
	CreatedAt string
	// WriteOrder is the row's SQLite rowid, which orders bubbles that share a
	// timestamp.
	WriteOrder int64
	// HasContent reports whether a reader would see anything in the bubble.
	HasContent bool
	// Fingerprint identifies what a reader would see, including the bubble's role,
	// so two bubbles compare equal only when they would read identically.
	Fingerprint [sha256.Size]byte
}

// composerBubbles is what one pass over a chat's key range establishes: a digest
// for every row Clyde could read, and the ids of the rows it could not.
//
// The two are kept apart because they answer different questions. A bubble id in
// neither set names a row the store does not hold, which the header is free to
// reference and assembly simply skips. A bubble id in Unreadable names a row that
// is there and that Clyde could not parse, which assembly must never quietly drop
// when the header ordered it into the conversation.
type composerBubbles struct {
	Digests    map[string]bubbleDigest
	Unreadable map[string]bool
	// Unaccounted counts the rows the chat's range held that neither pass could
	// describe, so a header reference with no digest is not read as a row the store
	// does not hold when it may be one of these.
	Unaccounted int
}

func emptyComposerBubbles() composerBubbles {
	return composerBubbles{
		Digests:     make(map[string]bubbleDigest),
		Unreadable:  make(map[string]bool),
		Unaccounted: 0,
	}
}

// readComposerBubbleDigests projects every bubble row stored under one composer's
// key prefix down to a digest, inside SQLite, and reports which rows it could not
// read at all.
//
// The projection is what keeps this pass affordable. Pulling each row's value and
// decoding it in Go costs the chat's whole payload in transferred bytes and in
// `encoding/json` work before a single message is served; asking SQLite for the
// dozen fields a digest needs costs the fields.
//
// It is also stricter about what "unreadable" means. A value SQLite parses gives
// up its fields whatever their JSON types are, so the shape mismatches that
// defeat a Go struct decode, such as the object-valued tool result, cannot cost a
// row here. Only a value that is not JSON at all is unreadable, and the row count
// of the range is what proves whether any were.
func readComposerBubbleDigests(
	ctx context.Context,
	db *sql.DB,
	composerID string,
) (composerBubbles, error) {
	stored := emptyComposerBubbles()

	snapshot, err := beginReadSnapshot(ctx, db)
	if err != nil {
		return stored, err
	}
	defer snapshot.rollback()
	return readComposerBubbleDigestsInSnapshot(ctx, snapshot, composerID)
}

// readComposerBubbleDigestsInSnapshot does the work with every statement on one
// read snapshot, which is what makes the counts reconcilable.
func readComposerBubbleDigestsInSnapshot(
	ctx context.Context,
	snapshot readSnapshot,
	composerID string,
) (composerBubbles, error) {
	stored := emptyComposerBubbles()
	bounds := keyRangeForPrefix(bubbleKeyPrefix + composerID + ":")

	storedRows, err := snapshot.countRange(ctx, bounds)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_bubble_count_failed", "concern", concern, "composer_id", composerID, "err", err)
		return stored, fmt.Errorf("count cursor bubbles for composer %q: %w", composerID, err)
	}

	// A row is accounted for only once its digest is stored. Counting a row whose
	// key will not parse as projected would keep the totals level and hide it from
	// the pass below, which is the one place that names what went unread.
	projected := 0
	err = forEachComposerBubbleProjection(ctx, snapshot, bounds, func(row bubbleProjection) error {
		keyBubbleID, ok := bubbleIDFromKey(row.Key, composerID)
		if !ok {
			slog.WarnContext(ctx, "providers.cursor.store.bubble_key_unparsed", "concern", concern, "composer_id", composerID, "key", row.Key)
			return nil
		}
		projected++
		bubble := row.bubble()
		digest := newBubbleDigest(keyBubbleID, bubble, row.RowID)
		if bubble.CreatedAt != "" && digest.CreatedAt == "" {
			slog.WarnContext(ctx, "providers.cursor.store.bubble_timestamp_unparsed", "concern", concern, "composer_id", composerID, "bubble_id", keyBubbleID, "created_at", bubble.CreatedAt)
		}
		stored.Digests[keyBubbleID] = digest
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_bubble_range_failed", "concern", concern, "composer_id", composerID, "err", err)
		return stored, fmt.Errorf("read cursor bubbles for composer %q: %w", composerID, err)
	}
	if projected >= storedRows {
		return stored, nil
	}

	// Rows the projection did not see are rows SQLite would not parse. Naming
	// them costs a second pass over the range, which is why it runs only when the
	// counts say there is something to name.
	if err := readUnreadableComposerBubbleIDs(ctx, snapshot, composerID, bounds, stored.Unreadable); err != nil {
		return stored, fmt.Errorf("read unreadable cursor bubbles for composer %q: %w", composerID, err)
	}
	// Whatever the two passes together still do not account for is recorded as a
	// number rather than assumed to be nothing. Every statement above ran on one
	// snapshot, so this is a real shortfall rather than a row Cursor appended
	// between two reads, and a header reference that finds no digest may be one of
	// these rather than a row the store does not hold.
	stored.Unaccounted = max(storedRows-projected-len(stored.Unreadable), 0)
	slog.WarnContext(ctx, "providers.cursor.store.composer_bubbles_unreadable", "concern", concern, "composer_id", composerID, "stored", storedRows, "projected", projected, "named", len(stored.Unreadable), "unaccounted", stored.Unaccounted)
	return stored, nil
}

// bubbleProjection is one row of the digest pass: the identifiers and the fields
// [fingerprintOf] and [Bubble.HasContent] read, and nothing else the bubble
// carries. Every field is nullable because SQLite reports an absent JSON member
// as NULL, which is how a bubble that has no tool call, no thinking, or no
// timestamp arrives here.
type bubbleProjection struct {
	RowID          int64
	Key            string
	ServerBubbleID sql.NullString
	CreatedAt      sql.NullString
	Type           sql.NullInt64
	Text           sql.NullString
	Thinking       sql.NullString
	// ToolCallJSONType is the JSON type of the row's `toolFormerData` member, and
	// is null exactly when the row carries no tool call. It is read instead of the
	// member itself so the digest never pulls a tool result twice.
	ToolCallJSONType sql.NullString
	ToolCallID       sql.NullString
	ToolName         sql.NullString
	ToolRawArgs      sql.NullString
	ToolResult       sql.NullString
	ToolStatus       sql.NullString
}

// bubble rebuilds the part of a [Bubble] the digest is derived from, so the
// fingerprint and the content test stay one definition shared with the decoded
// path rather than a second one written in SQL.
func (row bubbleProjection) bubble() Bubble {
	bubble := Bubble{
		BubbleID:      "",
		Type:          int(row.Type.Int64),
		SchemaVersion: 0,
		Text:          row.Text.String,
		Thinking:      BubbleThinking{Text: row.Thinking.String},
		ToolCall:      nil,
		CreatedAt:     row.CreatedAt.String,
		// RequestID is not projected. Nothing about ordering or supersession reads
		// it, and the lookup that does have a use for it reads the row itself.
		RequestID:      "",
		ServerBubbleID: row.ServerBubbleID.String,
	}
	// `json_type` answers the string "null" for a member explicitly set to JSON
	// null, which is a member that is there and holds nothing. Reading that as a
	// tool call builds a digest with content the decoder will map to nil, so the
	// chat is admitted for content it does not have and then streams nothing.
	if row.ToolCallJSONType.Valid && row.ToolCallJSONType.String != jsonNullType {
		bubble.ToolCall = &BubbleToolCall{
			Name:       row.ToolName.String,
			RawArgs:    row.ToolRawArgs.String,
			Result:     BubbleToolResult(row.ToolResult.String),
			Status:     row.ToolStatus.String,
			ToolCallID: row.ToolCallID.String,
		}
	}
	return bubble
}

// decodableValueSQL selects the rows the digest projection can read, and
// undecodableValueSQL selects exactly the rest. They are written as a pair
// because the two passes have to partition the range: a row in neither is a row
// the accounting loses.
//
// The negation is `IS NOT 1` rather than `NOT json_valid(value)` because
// `json_valid` returns NULL for a NULL value, and NULL satisfies neither a
// predicate nor its `NOT`, while `count(*)` still counts the row. Cursor does
// write NULLs: the operator's store holds 409 NULL-valued rows, 3 of them in the
// bubble range and one at `composerData:empty-state-draft`.
const (
	decodableValueSQL   = " AND json_valid(value)"
	undecodableValueSQL = " AND json_valid(value) IS NOT 1"
	// jsonNullType is what `json_type` answers for a member explicitly set to JSON
	// null, as opposed to a member that is absent, which answers SQL NULL.
	jsonNullType = "null"
)

// composerBubbleProjectionColumns are the JSON members the digest pass reads. The
// [decodableValueSQL] predicate keeps every one of them off a value SQLite cannot
// parse, which would otherwise abort the whole range read.
const composerBubbleProjectionColumns = "rowid, key," +
	" json_extract(value, '$.serverBubbleId')," +
	" json_extract(value, '$.createdAt')," +
	" json_extract(value, '$.type')," +
	" json_extract(value, '$.text')," +
	" json_extract(value, '$.thinking.text')," +
	" json_type(value, '$.toolFormerData')," +
	" json_extract(value, '$.toolFormerData.toolCallId')," +
	" json_extract(value, '$.toolFormerData.name')," +
	" json_extract(value, '$.toolFormerData.rawArgs')," +
	" json_extract(value, '$.toolFormerData.result')," +
	" json_extract(value, '$.toolFormerData.status')"

func forEachComposerBubbleProjection(
	ctx context.Context,
	snapshot readSnapshot,
	bounds keyRange,
	visit func(bubbleProjection) error,
) error {
	if ctx == nil {
		return fmt.Errorf("read cursor bubble projection in key range %q: nil context", bounds.Lower)
	}
	sqlTableName, err := KVTableCursorDiskKV.sqlTableName()
	if err != nil {
		return err
	}
	exists, err := snapshot.tableExists(ctx, sqlTableName)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	query := "SELECT " + composerBubbleProjectionColumns + " FROM " + sqlTableName +
		keyRangePredicate(bounds, "") + decodableValueSQL + " ORDER BY key"
	rows, err := snapshot.queryRange(ctx, query, bounds, "bubble digests")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var row bubbleProjection
		if err := rows.Scan(
			&row.RowID, &row.Key,
			&row.ServerBubbleID, &row.CreatedAt, &row.Type, &row.Text, &row.Thinking,
			&row.ToolCallJSONType, &row.ToolCallID, &row.ToolName, &row.ToolRawArgs,
			&row.ToolResult, &row.ToolStatus,
		); err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.bubble_projection_scan_failed", "concern", concern, "key_lower", bounds.Lower, "err", err)
			return fmt.Errorf("scan cursor bubble projection in key range %q: %w", bounds.Lower, err)
		}
		if err := visit(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.bubble_projection_iterate_failed", "concern", concern, "key_lower", bounds.Lower, "err", err)
		return fmt.Errorf("iterate cursor bubble projection in key range %q: %w", bounds.Lower, err)
	}
	return nil
}

// readUnreadableComposerBubbleIDs names the rows of one key range the digest
// projection could not read, which [undecodableValueSQL] defines as exactly the
// rows [decodableValueSQL] left behind.
func readUnreadableComposerBubbleIDs(
	ctx context.Context,
	snapshot readSnapshot,
	composerID string,
	bounds keyRange,
	into map[string]bool,
) error {
	sqlTableName, err := KVTableCursorDiskKV.sqlTableName()
	if err != nil {
		return err
	}
	query := "SELECT key FROM " + sqlTableName + keyRangePredicate(bounds, "") + undecodableValueSQL
	rows, err := snapshot.queryRange(ctx, query, bounds, "unreadable bubbles")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.unreadable_bubble_scan_failed", "concern", concern, "key_lower", bounds.Lower, "err", err)
			return fmt.Errorf("scan unreadable cursor bubble in key range %q: %w", bounds.Lower, err)
		}
		if bubbleID, ok := bubbleIDFromKey(key, composerID); ok {
			into[bubbleID] = true
		}
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.unreadable_bubble_iterate_failed", "concern", concern, "key_lower", bounds.Lower, "err", err)
		return fmt.Errorf("iterate unreadable cursor bubbles in key range %q: %w", bounds.Lower, err)
	}
	return nil
}

// readBubble reads one composer bubble by its exact key, which seeks the primary
// key index rather than rescanning the chat's range.
//
// A row that fails to decode is fatal, and so is one whose content changed since
// the ordering pass digested it. Ordering placed this bubble using the content it
// read then, so serving the content it holds now would put a message somewhere
// its own timestamp does not support, silently. The row's SQLite rowid is what
// detects that: Cursor's key-value tables declare `key TEXT UNIQUE ON CONFLICT
// REPLACE`, so a rewritten row is reinserted and takes a new, higher rowid, which
// the digest already recorded as its write order.
func readBubble(
	ctx context.Context,
	db *sql.DB,
	composerID string,
	entry orderedBubble,
) (Bubble, bool, error) {
	var emptyBubble Bubble

	value, writeOrder, found, err := readKVRowInKnownTable(ctx, db, KVTableCursorDiskKV, bubbleKey(composerID, entry.BubbleID))
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.bubble_read_failed", "concern", concern, "composer_id", composerID, "bubble_id", entry.BubbleID, "err", err)
		return emptyBubble, false, fmt.Errorf("read cursor bubble %q of composer %q: %w", entry.BubbleID, composerID, err)
	}
	if !found {
		return emptyBubble, false, nil
	}
	if writeOrder != entry.WriteOrder {
		slog.WarnContext(ctx, "providers.cursor.store.ordered_bubble_rewritten", "concern", concern, "composer_id", composerID, "bubble_id", entry.BubbleID, "ordered_write_order", entry.WriteOrder, "read_write_order", writeOrder)
		return emptyBubble, false, fmt.Errorf(
			"read cursor bubble %q of composer %q: the row was rewritten between the ordering pass and the read",
			entry.BubbleID, composerID,
		)
	}
	bubble, err := DecodeBubbleJSON(value)
	if err != nil {
		// Whether the header named it or not, the ordering pass read this row,
		// found content in it and put it in the transcript. The two passes
		// disagreeing is a fault in Clyde's own reading rather than a property of
		// the store, and serving the conversation without a message assembly
		// committed to emitting would present an incomplete transcript as whole.
		slog.WarnContext(ctx, "providers.cursor.store.ordered_bubble_decode_failed", "concern", concern, "composer_id", composerID, "bubble_id", entry.BubbleID, "referenced", entry.Referenced, "err", err)
		return emptyBubble, false, fmt.Errorf("decode cursor bubble %q of composer %q: %w", entry.BubbleID, composerID, err)
	}
	if bubble.BubbleID == "" {
		bubble.BubbleID = entry.BubbleID
	}
	return bubble, true, nil
}

func newBubbleDigest(bubbleID string, bubble Bubble, writeOrder int64) bubbleDigest {
	return bubbleDigest{
		BubbleID:       bubbleID,
		ServerBubbleID: bubble.ServerBubbleID,
		CreatedAt:      OrderableTimestamp(bubble.CreatedAt),
		WriteOrder:     writeOrder,
		HasContent:     bubble.HasContent(),
		Fingerprint:    fingerprintOf(bubble),
	}
}

// fingerprintOf hashes everything a reader would see, plus the bubble's role and
// the tool call it belongs to, so two bubbles collide only when they would render
// identically as part of the same call. Every field is taken exactly as stored:
// nothing is trimmed, lowercased, normalized, or truncated, and the fields are
// length-prefixed so no combination of them can be confused with another.
//
// Whitespace is content, not noise. Trimming it collapses a message holding an
// indented code block into the same message unindented, and the indentation is
// what makes it a code block.
//
// The role is part of the hash because without it a user bubble and an assistant
// bubble carrying the same text compare equal, which would silently collapse a
// quoted reply into the message it quotes.
//
// The tool call's status is part of it because a reader sees it: [toolErrored]
// turns the status into the transcript's error marker, so two rows for one call
// stored as `completed` and as `cancelled` render differently and must not
// collapse into whichever copy assembly happened to keep.
//
// The tool call id is part of it because identical content is not evidence of a
// copy when the model made two calls. Measured over 226 real chats, folding it in
// keeps 2,254 unreferenced bubbles the content alone discarded, and it can never
// discard one the content alone kept: adding a field to a hash only ever splits a
// collision.
//
// It scopes the content rather than replacing it, which is the load-bearing part.
// Dropping a row purely because a kept row shares its call id would lose real
// output: of 1,006 pairs in the same store that share a call id, 213 carry
// different content, 206 of those differing in the call's arguments and 183 in
// its result, which is Cursor storing a call while its arguments and result were
// still arriving. Keeping the first of those and dropping the rest would keep the
// emptiest copy.
func fingerprintOf(bubble Bubble) [sha256.Size]byte {
	hasher := sha256.New()
	writeFingerprintField(hasher, strconv.Itoa(bubble.Type))
	writeFingerprintField(hasher, bubble.Text)
	writeFingerprintField(hasher, bubble.Thinking.Text)
	if bubble.ToolCall != nil {
		writeFingerprintField(hasher, bubble.ToolCall.ToolCallID)
		writeFingerprintField(hasher, bubble.ToolCall.Name)
		writeFingerprintField(hasher, bubble.ToolCall.RawArgs)
		writeFingerprintField(hasher, string(bubble.ToolCall.Result))
		writeFingerprintField(hasher, bubble.ToolCall.Status)
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hasher.Sum(nil))
	return fingerprint
}

func writeFingerprintField(hasher hash.Hash, field string) {
	_, _ = hasher.Write([]byte(strconv.Itoa(len(field))))
	_, _ = hasher.Write([]byte{':'})
	_, _ = hasher.Write([]byte(field))
}

// orderedBubble is one entry of a chat's reading order, and whether the composer
// header named it. The second field travels with the id because it decides what a
// failure to read the row means: a header-referenced bubble Clyde cannot read is
// a message the store promised and did not deliver, while an unreferenced one is
// a recovery that may be dropped.
type orderedBubble struct {
	BubbleID   string
	Referenced bool
	// WriteOrder is the rowid the ordering pass saw. The per-bubble read compares
	// it, because a row Cursor rewrote between the two passes comes back under a
	// new rowid and holds content the placement was not derived from.
	WriteOrder int64
}

// assembleBubbleOrder applies the three assembly rules to one composer's header
// and the digests of its stored bubbles, and returns the bubbles in reading
// order. It is separated from the read so the ordering is testable without a
// database.
func assembleBubbleOrder(
	ctx context.Context,
	composerID string,
	header ComposerHeader,
	stored composerBubbles,
) ([]orderedBubble, error) {
	spine, referenced, err := composerSpine(ctx, composerID, header, stored)
	if err != nil {
		return nil, err
	}
	kept := keptUnreferencedBubbles(spine, stored.Digests, referenced)
	return mergeBubbleOrder(spine, kept), nil
}

// composerSpine returns the header-referenced bubbles in header order, plus the
// set of bubble ids the header named.
//
// A reference whose row the store does not hold is skipped, because there is
// nothing to read. A reference whose row is there and unreadable fails the whole
// assembly instead, since serving the rest would present a conversation with a
// message missing as if it were whole.
//
// Skipping rests on the read having accounted for every row the range held. When
// it did not, a reference with no digest may be one of the rows that went
// unaccounted for rather than one the store does not hold, and the two cannot be
// told apart, so the whole assembly fails rather than guessing the harmless one.
//
// A bubble id the header lists more than once is emitted once. The list is not
// guaranteed to hold distinct ids: querying a real global store, 1 of 400 sampled
// `composerData:` rows has more entries than distinct bubble ids, and following
// the list literally reads that row twice and repeats the turn in the transcript.
func composerSpine(
	ctx context.Context,
	composerID string,
	header ComposerHeader,
	stored composerBubbles,
) ([]bubbleDigest, map[string]bool, error) {
	spine := make([]bubbleDigest, 0, len(header.FullConversationHeadersOnly))
	referenced := make(map[string]bool, len(header.FullConversationHeadersOnly))
	for _, bubbleRef := range header.FullConversationHeadersOnly {
		if referenced[bubbleRef.BubbleID] {
			continue
		}
		referenced[bubbleRef.BubbleID] = true
		if stored.Unreadable[bubbleRef.BubbleID] {
			return nil, nil, fmt.Errorf(
				"read cursor bubble %q of composer %q: stored value is not decodable",
				bubbleRef.BubbleID, composerID,
			)
		}
		if _, ok := stored.Digests[bubbleRef.BubbleID]; ok {
			spine = append(spine, stored.Digests[bubbleRef.BubbleID])
			continue
		}
		if stored.Unaccounted > 0 {
			return nil, nil, fmt.Errorf(
				"read cursor bubble %q of composer %q: %d rows of the chat went unread, so this reference cannot be shown to be absent",
				bubbleRef.BubbleID, composerID, stored.Unaccounted,
			)
		}
		// The store holds no row for this reference, and the read accounted for
		// every row it does hold, so the message is gone rather than unread. That is
		// still a turn the header ordered into the conversation and the transcript
		// will not carry, which nothing else records.
		slog.WarnContext(ctx, "providers.cursor.store.referenced_bubble_absent", "concern", concern, "composer_id", composerID, "bubble_id", bubbleRef.BubbleID)
	}
	return spine, referenced, nil
}

// keptUnreferencedBubbles selects the unreferenced bubbles worth adding, in
// ascending `createdAt` order. It reads two provider-supplied identities in
// order, and reaches content only where neither exists.
//
// The first is `serverBubbleId`. Most unreferenced rows are superseded copies,
// and Cursor says so itself: when it rewrites a turn under a new local bubble id,
// both rows keep the same server id. Measured over 226 real chats, 22,934 rows
// shared a server bubble id with another row and not one of those pairs carried
// different content, so the id is an exact supersession key rather than a
// heuristic.
//
// Identity is preferred to content because content alone cannot tell a superseded
// copy from a turn that genuinely happened twice. Over the same 226 chats a
// chat-wide content key dropped 29,992 of 40,031 unreferenced bubbles, and 3,358
// of those drops were rows carrying a distinct server bubble id, which is Cursor
// saying they are separate messages. Half of them were tool calls, so an agent
// that ran the same command twice lost the second run.
//
// Preferring the id does not mean skipping the content, and the distinction is
// what keeps the rule symmetric. Two rows are separate messages only when Cursor
// gave both of them a server id and the ids differ; wherever either row lacks
// one, the ids cannot tell them apart and identical content is the ordinary
// rewrite shape, where Cursor stamped the acknowledged row and left the previous
// one unstamped. Testing the id alone in that case keeps the copy and repeats the
// turn, and it does so asymmetrically, so the same pair deduplicates or does not
// purely on which row Cursor stamped. Measured over 1,295 real chats and 127,010
// content-bearing rows, groups sharing content where some row lacks a server id
// outnumber groups distinguished by two different ids 1,456 to 97, and account
// for 2,348 rows this rule drops that testing the id alone emits twice.
//
// The tool call id and the call's status are folded into the fingerprint by
// [fingerprintOf] rather than tested here, because they tell two calls apart
// without being safe to merge on. The call id recovers a further 2,254 rows over
// the 226-chat sample.
//
// Where neither row carries a server id, both are kept. Identical content is not
// evidence of a copy when Cursor recorded no identity either way: it is equally
// the shape of a turn that genuinely happened twice, and nothing in the store
// separates the two. Dropping one there traded a duplicate a reader can see and
// ignore for a message a reader can never recover, which is the wrong way round.
// Measured over 1,295 real chats, the groups where no row carries a server id are
// 398 groups holding 2,643 rows, so keeping them adds about 1.7 rows per chat.
func keptUnreferencedBubbles(
	spine []bubbleDigest,
	digests map[string]bubbleDigest,
	referenced map[string]bool,
) []bubbleDigest {
	seen := newSeenBubbles(len(spine))
	for _, digest := range spine {
		seen.keep(digest)
	}

	candidates := make([]bubbleDigest, 0)
	for bubbleID, digest := range digests {
		if referenced[bubbleID] || !digest.HasContent {
			continue
		}
		candidates = append(candidates, digest)
	}
	sortBubbleDigests(candidates)

	kept := make([]bubbleDigest, 0, len(candidates))
	for _, digest := range candidates {
		if seen.supersedes(digest) {
			continue
		}
		seen.keep(digest)
		kept = append(kept, digest)
	}
	return kept
}

// seenBubbles remembers what assembly has already kept, in the two terms rule 2
// compares: Cursor's server-side identity, and what a reader would see.
type seenBubbles struct {
	serverIDs map[string]bool
	// stampedFingerprints holds the content of the kept rows Cursor gave a server
	// id, and unstampedFingerprints the content of those it did not. A pair is a
	// copy when exactly one side carries an id, because the ids cannot separate
	// them and one of them is the row Cursor acknowledged; a pair where neither
	// side carries one is not, because nothing separates them at all.
	stampedFingerprints   map[[sha256.Size]byte]bool
	unstampedFingerprints map[[sha256.Size]byte]bool
}

func newSeenBubbles(size int) seenBubbles {
	return seenBubbles{
		serverIDs:             make(map[string]bool, size),
		stampedFingerprints:   make(map[[sha256.Size]byte]bool, size),
		unstampedFingerprints: make(map[[sha256.Size]byte]bool, size),
	}
}

func (seen seenBubbles) keep(digest bubbleDigest) {
	if digest.ServerBubbleID != "" {
		seen.serverIDs[digest.ServerBubbleID] = true
		seen.stampedFingerprints[digest.Fingerprint] = true
		return
	}
	seen.unstampedFingerprints[digest.Fingerprint] = true
}

// supersedes reports whether a kept bubble already stands for this one.
func (seen seenBubbles) supersedes(digest bubbleDigest) bool {
	if digest.ServerBubbleID == "" {
		return seen.stampedFingerprints[digest.Fingerprint]
	}
	return seen.serverIDs[digest.ServerBubbleID] || seen.unstampedFingerprints[digest.Fingerprint]
}

// mergeBubbleOrder interleaves the kept bubbles into the header order and returns
// the resulting reading order.
//
// The header order is reproduced exactly, including the places where its own
// timestamps run backwards, which they do: one real chat holds 185 adjacent
// header pairs whose second bubble is stamped earlier than its first. Each
// addition lands in a gap of that order, where the gap before header bubble i
// accepts a timestamp at or after the nearest dated header bubble before it and
// strictly before the nearest dated header bubble at or after it.
//
// A bubble whose timestamp ties a header bubble lands after it, because the gap
// is half open. A bubble no gap brackets, including one with no timestamp at all,
// lands after the whole header order, because nothing in the data supports an
// interior position for it.
func mergeBubbleOrder(spine []bubbleDigest, kept []bubbleDigest) []orderedBubble {
	order := make([]orderedBubble, 0, len(spine)+len(kept))
	appendReferenced := func(digest bubbleDigest) {
		order = append(order, orderedBubble{BubbleID: digest.BubbleID, Referenced: true, WriteOrder: digest.WriteOrder})
	}
	appendAddition := func(digest bubbleDigest) {
		order = append(order, orderedBubble{BubbleID: digest.BubbleID, Referenced: false, WriteOrder: digest.WriteOrder})
	}
	if len(kept) == 0 {
		for _, digest := range spine {
			appendReferenced(digest)
		}
		return order
	}

	// sortBubbleDigests already sorted the undated bubbles to the end, so the
	// dated prefix is the part a gap can bracket.
	datedCount := 0
	for _, digest := range kept {
		if digest.CreatedAt == "" {
			break
		}
		datedCount++
	}
	dated := kept[:datedCount]

	byGap := make([][]int, len(spine)+1)
	for position, gap := range assignBubbleGaps(spine, dated) {
		byGap[gap] = append(byGap[gap], position)
	}
	for index, digest := range spine {
		for _, position := range byGap[index] {
			appendAddition(dated[position])
		}
		appendReferenced(digest)
	}
	for _, position := range byGap[len(spine)] {
		appendAddition(dated[position])
	}
	for _, digest := range kept[datedCount:] {
		appendAddition(digest)
	}
	return order
}

// assignBubbleGaps returns, for each dated addition, the index of the header
// bubble it belongs before, or the header length when no gap brackets it.
//
// The last bracketing gap wins, which is what keeps one odd header timestamp
// local. A header bubble stamped far ahead of its neighbours opens a gap that
// spans the rest of the chat, so preferring the earliest bracket, as a running
// maximum of the header's timestamps does, emits every later addition under that
// high-water mark at the jump instead of at its own place. Preferring the last
// bracket puts each addition among the header bubbles it actually sits between,
// and leaves the wide gap holding only the additions no tighter gap claims.
//
// Gaps are therefore walked backwards, and an addition is assigned once, so the
// wide gap costs the walk nothing beyond the additions still unclaimed when it
// is reached.
func assignBubbleGaps(spine []bubbleDigest, dated []bubbleDigest) []int {
	gaps := make([]int, len(dated))
	for position := range gaps {
		gaps[position] = len(spine)
	}
	bounds := spineGapBounds(spine)
	pending := newPendingBubbles(len(dated))
	for index := range slices.Backward(spine) {
		first := 0
		if bounds.lower[index] != "" {
			first = sort.Search(len(dated), func(position int) bool {
				return dated[position].CreatedAt >= bounds.lower[index]
			})
		}
		// A gap with no dated header bubble at or after it is open ended, and an
		// open-ended gap brackets nothing: the rule is that a bubble no gap brackets
		// lands after the whole header order, so a trailing undated header bubble
		// must not pull a late addition in front of it.
		limit := first
		if bounds.upper[index] != "" {
			limit = sort.Search(len(dated), func(position int) bool {
				return dated[position].CreatedAt >= bounds.upper[index]
			})
		}
		for position := pending.firstAtOrAfter(first); position < limit; position = pending.firstAtOrAfter(position + 1) {
			gaps[position] = index
			pending.take(position)
		}
	}
	return gaps
}

// gapBounds holds, per header position, the timestamps that bracket the gap
// before it. An empty bound means that side is open.
type gapBounds struct {
	lower []string
	upper []string
}

// spineGapBounds derives each gap's bounds from the nearest dated header bubble
// on each side, skipping over the header bubbles that carry no timestamp.
//
// An undated header bubble must not be read as a bound. Taking its empty
// `createdAt` as the lower bound reopens the gap back to the start of the chat,
// and because gaps are walked backwards and the last bracketing gap wins, that
// wide gap then claims additions belonging far earlier and places a message that
// opened the conversation next to its last turn. [sortBubbleDigests] already
// refuses to give an undated addition an interior position for the same reason;
// this is the same refusal on the header side.
func spineGapBounds(spine []bubbleDigest) gapBounds {
	bounds := gapBounds{
		lower: make([]string, len(spine)),
		upper: make([]string, len(spine)),
	}
	newest := ""
	for index, digest := range spine {
		bounds.lower[index] = newest
		if digest.CreatedAt != "" {
			newest = digest.CreatedAt
		}
	}
	oldest := ""
	for index, digest := range slices.Backward(spine) {
		if digest.CreatedAt != "" {
			oldest = digest.CreatedAt
		}
		bounds.upper[index] = oldest
	}
	return bounds
}

// pendingBubbles tracks which additions no gap has claimed yet, so each gap takes
// only the additions no later gap took without rescanning the ones it did. The
// forward pointers are path compressed, which keeps the whole merge close to
// linear even when a chat contributes thousands of additions.
type pendingBubbles struct {
	next []int
}

func newPendingBubbles(count int) pendingBubbles {
	next := make([]int, count+1)
	for position := range next {
		next[position] = position
	}
	return pendingBubbles{next: next}
}

// firstAtOrAfter returns the smallest unplaced position at or after position, or
// the count when none remains.
func (pending pendingBubbles) firstAtOrAfter(position int) int {
	for pending.next[position] != position {
		pending.next[position] = pending.next[pending.next[position]]
		position = pending.next[position]
	}
	return position
}

func (pending pendingBubbles) take(position int) {
	pending.next[position] = position + 1
}

// sortBubbleDigests orders bubbles by write time, and breaks a tie by the row's
// place in the store's write order rather than by its uuid.
//
// Ties are the common case rather than the rare one, because Cursor's `createdAt`
// is millisecond coarse: one real chat holds 861 adjacent header pairs sharing a
// single value. Measured over 222 chats and 3,351 tied adjacent header pairs,
// where the header itself gives the true order, SQLite's rowid agrees with it
// 94.6% of the time, while the bubble uuid agrees 48.8% and the server bubble
// uuid 50.2%, which is a coin flip either way.
//
// A bubble with no timestamp sorts last so it cannot claim an interior position
// it has no evidence for.
func sortBubbleDigests(digests []bubbleDigest) {
	sort.SliceStable(digests, func(i int, j int) bool {
		left, right := digests[i], digests[j]
		if (left.CreatedAt == "") != (right.CreatedAt == "") {
			return right.CreatedAt == ""
		}
		if left.CreatedAt != right.CreatedAt {
			return left.CreatedAt < right.CreatedAt
		}
		return left.WriteOrder < right.WriteOrder
	})
}
