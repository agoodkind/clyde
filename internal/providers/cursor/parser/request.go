package parser

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/providerid"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

var _ conversation.RequestResolver = (*Parser)(nil)

// ResolveRequestID maps one Cursor request id, the value Cursor's own Copy
// Request ID action yields, to the chat that issued it.
//
// Cursor's global database is tens of gigabytes with only its key index, so the
// bounded path never filters on a value. It reads the request's timestamp from
// the small per-workspace generation ring, narrows the chat header table to the
// chats whose lifetime spans that instant, and reads only those chats' bubbles
// through the key index.
//
// The exhaustive scan over every bubble Cursor has ever written runs only when
// the caller sets opts.AllowFullScan.
func (*Parser) ResolveRequestID(
	ctx context.Context,
	requestID string,
	opts conversation.RequestLookupOptions,
) (conversation.RequestMatch, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return missingRequestMatch(conversation.RequestNotFoundReasonUnspecified), nil
	}

	roots, err := cursorstore.ResolveDataRootsFromEnv(ctx)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.resolve_data_roots_failed", "concern", concern, "err", err)
		return conversation.RequestMatch{}, fmt.Errorf("resolve cursor data roots: %w", err)
	}

	// The reason is folded through the shared precedence rather than latched at the
	// first knowable one. Two roots is an ordinary shape, Cursor beside Cursor
	// Insiders, and a clean miss in the first must not be reported as a confirmed
	// absence over a second whose database was locked: the id may be sitting in the
	// root this lookup could not open.
	//
	// A root that fails outright is the same case and is treated the same way. Its
	// store is what broke, the id may be in it, and abandoning the roots after it
	// would let one unreadable directory hide an answer sitting in the next one.
	reason := conversation.RequestNotFoundReasonUnspecified
	for _, root := range roots {
		match, err := resolveRequestIDInRoot(ctx, root, requestID, opts)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.data_root_lookup_failed", "concern", concern, "path", root.RootDir, "request_id", requestID, "err", err)
			reason = conversation.MergeRequestNotFoundReason(reason, conversation.RequestNotFoundReasonInconclusive)
			continue
		}
		if match.Found {
			return match, nil
		}
		reason = conversation.MergeRequestNotFoundReason(reason, match.Reason)
	}
	return missingRequestMatch(reason), nil
}

// resolveRequestIDInRoot runs the bounded lookup against one Cursor data root,
// then the opt-in exhaustive scan when the caller allowed it.
func resolveRequestIDInRoot(
	ctx context.Context,
	root cursorstore.DataRoot,
	requestID string,
	opts conversation.RequestLookupOptions,
) (conversation.RequestMatch, error) {
	db, availability := openOptionalDatabase(ctx, root.GlobalDBPath, "global_db")
	switch availability {
	case databaseAvailabilityUnreadable:
		// Cursor runs while Clyde reads, so a global database Clyde could not open
		// reads exactly like one that never held the id. Reporting a confirmed miss
		// here would hand the caller an absence this lookup never established, which
		// is the distinction the reason exists to keep.
		return missingRequestMatch(conversation.RequestNotFoundReasonInconclusive), nil
	case databaseAvailabilityAbsent:
		// A database that is not there was read successfully as absent, so this root
		// stores no chats. What it does not establish is whether the request was
		// issued here, because the recent-request rings live in the per-workspace
		// databases this branch never opens, so the ring still has to be asked
		// before anything is asserted about the id.
		return resolveRequestIDWithoutChatStore(ctx, root, requestID)
	case databaseAvailabilityOpen:
	}
	defer func() { _ = db.Close() }()

	reason, match, err := resolveRequestIDFromGenerationRing(ctx, root, db, requestID)
	if err != nil {
		return conversation.RequestMatch{}, err
	}
	if match.Found {
		return match, nil
	}
	if !opts.AllowFullScan {
		return missingRequestMatch(reason), nil
	}

	slog.InfoContext(ctx, "providers.cursor.parser.request_full_scan_started", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID)
	search, err := cursorstore.ScanAllBubblesForRequestID(ctx, db, requestID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.request_full_scan_failed", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "err", err)
		return conversation.RequestMatch{}, fmt.Errorf("scan cursor bubbles for request %q: %w", requestID, err)
	}
	return matchFromCarriers(ctx, root, requestID, carriersOf(search), conversation.RequestOriginFullScan, reason), nil
}

// resolveRequestIDWithoutChatStore answers for a data root whose chat database is
// not there, by asking the per-workspace rings whether the request was issued
// here at all.
//
// The rings are what decide the reason. A ring that lists the request says it was
// issued on this machine, and the chats that would say which conversation issued
// it are gone, so the miss proves nothing about the conversation. A ring sweep
// that comes up empty over workspaces it could all read is a real absence, which
// is the ordinary answer on a machine where Cursor was never installed.
func resolveRequestIDWithoutChatStore(
	ctx context.Context,
	root cursorstore.DataRoot,
	requestID string,
) (conversation.RequestMatch, error) {
	lookup, err := cursorstore.FindGenerationEntry(ctx, root, requestID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.generation_lookup_failed", "concern", concern, "path", root.WorkspaceStorageDir, "request_id", requestID, "err", err)
		return conversation.RequestMatch{}, fmt.Errorf("find cursor generation entry for request %q: %w", requestID, err)
	}
	if lookup.Found {
		slog.WarnContext(ctx, "providers.cursor.parser.request_listed_without_chat_store", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "workspace_hash", lookup.Hit.WorkspaceHash)
		return missingRequestMatch(conversation.RequestNotFoundReasonInconclusive), nil
	}
	if lookup.UnreadableWorkspaces > 0 {
		return missingRequestMatch(conversation.RequestNotFoundReasonInconclusive), nil
	}
	return missingRequestMatch(conversation.RequestNotFoundReasonNotRetained), nil
}

// resolveRequestIDFromGenerationRing runs the three indexed steps: read the
// request's timestamp, narrow the chat headers to that instant, then read only
// those chats' bubbles. The first result is the reason the bounded path missed,
// which the caller reports when the exhaustive scan is not allowed or also
// misses.
func resolveRequestIDFromGenerationRing(
	ctx context.Context,
	root cursorstore.DataRoot,
	db *sql.DB,
	requestID string,
) (conversation.RequestNotFoundReason, conversation.RequestMatch, error) {
	lookup, err := cursorstore.FindGenerationEntry(ctx, root, requestID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.generation_lookup_failed", "concern", concern, "path", root.WorkspaceStorageDir, "request_id", requestID, "err", err)
		return conversation.RequestNotFoundReasonUnspecified, conversation.RequestMatch{}, fmt.Errorf("find cursor generation entry for request %q: %w", requestID, err)
	}
	if !lookup.Found {
		// A miss is only meaningful when every workspace was actually readable.
		// Cursor runs while Clyde reads, so a locked workspace reads the same as
		// one that never held the id, and claiming the request is gone would be
		// asserting something this lookup did not establish.
		reason := conversation.RequestNotFoundReasonNotRetained
		if lookup.UnreadableWorkspaces > 0 {
			slog.WarnContext(ctx, "providers.cursor.parser.generation_lookup_inconclusive", "concern", concern, "path", root.WorkspaceStorageDir, "request_id", requestID, "unreadable_workspaces", lookup.UnreadableWorkspaces)
			reason = conversation.RequestNotFoundReasonInconclusive
		}
		return reason, missingRequestMatch(reason), nil
	}

	// The candidates are narrowed by the instant alone, and deliberately not by the
	// workspace the ring entry came from. A workspace-scoped pass would be a strict
	// subset of this one, because the unscoped predicate matches every workspace,
	// so running it first buys nothing once both passes always run: it reads every
	// candidate in that workspace a second time and counts an unreadable row into
	// the unread total twice. Narrowing by workspace would also have to be a
	// preference rather than a filter, since a header whose workspace disagrees
	// with the ring must not turn a real chat into a reported miss.
	hit := lookup.Hit
	carriers := newRequestCarriers()
	findRequestBubbleAmongCandidates(ctx, root, db, requestID, hit.Entry.UnixMs, carriers)
	if len(carriers.composerIDs) > 0 {
		match := matchFromCarriers(ctx, root, requestID, carriers, conversation.RequestOriginLive,
			conversation.RequestNotFoundReasonNoMatchingConversation)
		return match.Reason, match, nil
	}

	// The narrowing searched the chats the header table lists, so it can only claim
	// a confirmed absence when that table lists them all. It does not always: on a
	// real store it accounts for 2,390 of 2,470 chats, and the 80 it omits are
	// unreachable from any predicate over it. Nor can it claim one when part of the
	// search went unread.
	reason := conversation.RequestNotFoundReasonNoMatchingConversation
	if carriers.unread > 0 {
		slog.WarnContext(ctx, "providers.cursor.parser.request_candidates_partially_read", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "unread", carriers.unread)
		reason = conversation.RequestNotFoundReasonInconclusive
	}
	coverage, err := cursorstore.ReadComposerHeaderCoverage(ctx, db)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.composer_header_coverage_failed", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "err", err)
		reason = conversation.RequestNotFoundReasonInconclusive
	} else if !coverage.Complete() {
		slog.DebugContext(ctx, "providers.cursor.parser.composer_headers_incomplete", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "stored_chats", coverage.StoredChats, "unlisted_chats", coverage.UnlistedChats, "readable", coverage.Readable)
		reason = conversation.RequestNotFoundReasonInconclusive
	}
	return reason, missingRequestMatch(reason), nil
}

// requestCarriers accumulates what a search for one request id established across
// several candidate chats: the chats found carrying it, and how much of the
// search went unread.
type requestCarriers struct {
	composerIDs []string
	seen        map[string]bool
	// unread counts everything the search could not look at or could not decode: a
	// candidate whose bubbles would not read, a narrowing that could not run, and
	// the rows a byte search selected and a decode then rejected.
	unread int
}

func newRequestCarriers() *requestCarriers {
	return &requestCarriers{composerIDs: nil, seen: make(map[string]bool), unread: 0}
}

func (carriers *requestCarriers) add(composerID string) {
	if carriers.seen[composerID] {
		return
	}
	carriers.seen[composerID] = true
	carriers.composerIDs = append(carriers.composerIDs, composerID)
}

// carriersOf folds one store-level search into the same accumulator, so the
// bounded path and the exhaustive scan report a plural answer the same way.
func carriersOf(search cursorstore.BubbleRequestSearch) *requestCarriers {
	carriers := newRequestCarriers()
	carriers.unread = search.UnreadableRows
	for _, ref := range search.Carriers {
		carriers.add(ref.ComposerID)
	}
	return carriers
}

// matchFromCarriers turns what a search found into an answer.
//
// One carrier is the answer. Several is not: a request id names a turn, and
// duplicating a chat copies its turns under fresh bubble ids, so the copies carry
// the id as truly as the original does. Measured on a real store, 767 of 11,396
// distinct request ids appear in more than one chat, the worst in nine, and the
// nine are one chat and eight duplicates of it sharing a workspace and a creation
// time. Nothing in the store says which came first, so returning one of them
// would answer with whichever the key order happened to reach.
// One carrier is the answer only when the search could show it is the only one.
// Rows the search could not read are checked before the single-carrier case,
// because a second carrier hiding in them is exactly what reading past the first
// match exists to find, and naming the one carrier it did see would assert a
// uniqueness it never established.
func matchFromCarriers(
	ctx context.Context,
	root cursorstore.DataRoot,
	requestID string,
	carriers *requestCarriers,
	origin conversation.RequestOrigin,
	missReason conversation.RequestNotFoundReason,
) conversation.RequestMatch {
	switch {
	case len(carriers.composerIDs) > 1:
		slog.InfoContext(ctx, "providers.cursor.parser.request_carried_by_several_chats", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "count", len(carriers.composerIDs))
		return missingRequestMatch(conversation.RequestNotFoundReasonAmbiguousConversation)
	case carriers.unread > 0:
		slog.WarnContext(ctx, "providers.cursor.parser.request_carriers_partially_read", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "found", len(carriers.composerIDs), "unread", carriers.unread)
		return missingRequestMatch(conversation.RequestNotFoundReasonInconclusive)
	case len(carriers.composerIDs) == 1:
		return foundRequestMatch(carriers.composerIDs[0], origin)
	default:
		return missingRequestMatch(missReason)
	}
}

// findRequestBubbleAmongCandidates narrows the chat headers to one instant, then
// reads each candidate chat's bubbles through the key index looking for an exact
// request-id match.
//
// Every candidate is read. A candidate that fails is counted and the search
// carries on, because Cursor writes while Clyde reads and one chat hitting a busy
// database must not abandon the chat that holds the answer.
func findRequestBubbleAmongCandidates(
	ctx context.Context,
	root cursorstore.DataRoot,
	db *sql.DB,
	requestID string,
	unixMs int64,
	carriers *requestCarriers,
) {
	narrowing, err := cursorstore.ListComposerIDsCoveringTime(ctx, db, unixMs, "")
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.composer_candidates_failed", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID, "err", err)
		carriers.unread++
		return
	}
	if !narrowing.Usable {
		// The header table could not answer the predicate at all, so its empty
		// candidate list says nothing about the id.
		slog.WarnContext(ctx, "providers.cursor.parser.composer_narrowing_unusable", "concern", concern, "path", root.GlobalDBPath, "request_id", requestID)
		carriers.unread++
		return
	}
	slog.DebugContext(ctx, "providers.cursor.parser.request_candidates_narrowed", "concern", concern, "request_id", requestID, "count", len(narrowing.ComposerIDs))

	for _, composerID := range narrowing.ComposerIDs {
		search, err := cursorstore.FindBubbleRequestIDInComposer(ctx, db, composerID, requestID)
		if err != nil {
			slog.WarnContext(ctx, "providers.cursor.parser.composer_bubble_lookup_failed", "concern", concern, "path", root.GlobalDBPath, "composer_id", composerID, "request_id", requestID, "err", err)
			carriers.unread++
			continue
		}
		carriers.unread += search.UnreadableRows
		for _, ref := range search.Carriers {
			carriers.add(ref.ComposerID)
		}
	}
}

func foundRequestMatch(composerID string, origin conversation.RequestOrigin) conversation.RequestMatch {
	return conversation.RequestMatch{
		Found:                true,
		Provider:             providerid.ProviderCursor,
		NativeConversationID: composerID,
		Origin:               origin,
		Reason:               conversation.RequestNotFoundReasonUnspecified,
	}
}

func missingRequestMatch(reason conversation.RequestNotFoundReason) conversation.RequestMatch {
	return conversation.RequestMatch{
		Found:                false,
		Provider:             providerid.ProviderUnspecified,
		NativeConversationID: "",
		Origin:               conversation.RequestOriginUnspecified,
		Reason:               reason,
	}
}
