package parser

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/conversation"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

// composerUpdatedAt is the most recent time a chat is known to have changed.
//
// `lastUpdatedAt` alone puts a chat Cursor never stamped at the zero time, which
// sorts it below every dated conversation in a listing ordered newest first. The
// chats this parser admits by their stored bubbles are exactly the ones missing
// that stamp, so the 2,189-message agent run would land last of roughly 2,470
// records and never appear in a default listing. Its creation time is a real
// lower bound on when it changed, and it is the newest one the header carries.
func composerUpdatedAt(header cursorstore.ComposerHeader) time.Time {
	if header.LastUpdatedAt > header.CreatedAt {
		return msToTime(header.LastUpdatedAt)
	}
	return msToTime(header.CreatedAt)
}

// composerScanStamp decides whether one chat is a conversation Clyde should hold,
// and returns the stamp that says when to re-read it. The second result is false
// for a chat with nothing stored to deliver.
//
// A chat's header reference list is not a complete index of its stored bubbles,
// so it decides neither whether the chat exists nor when it changed. Measured on
// a real store, 631 of 2,470 chats list no references, 9 of those hold stored
// bubbles anyway, and one of the 9 is a 2,189-message agent run. Trusting the
// empty list drops those 9 conversations out of Clyde entirely, so `conversation
// list` never names them and a request id belonging to one resolves to nothing.
//
// The remaining 622 stay out. They hold no bubble row at all, so they are the
// drafts and empty panes the guard was written for, and admitting them would put
// 622 conversations with nothing to read into every listing and every search.
//
// A chat whose key range could not be read is not one of the 622. Dropping it
// here does not merely skip it: the scan rebuilds the record set from the
// candidates this pass returns, so a chat left out loses the record it already
// had. One busy database while Cursor checkpoints would take a conversation out
// of the index until a later pass succeeded. Such a chat keeps its previous
// stamp when it has one, which re-admits it unchanged, and is reported when it
// does not.
func composerScanStamp(
	ctx context.Context,
	db *sql.DB,
	globalDBPath string,
	composerID string,
	header cursorstore.ComposerHeader,
	priorStamp conversation.FileStamp,
) (conversation.FileStamp, bool) {
	stock, err := cursorstore.ReadComposerBubbleStock(ctx, db, composerID)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.parser.composer_bubble_stock_failed", "concern", concern, "path", globalDBPath, "composer_id", composerID, "err", err)
		return keepPriorComposer(ctx, globalDBPath, composerID, priorStamp)
	}
	if !stock.Conclusive {
		if priorStamp.Size == 0 && priorStamp.Mtime.IsZero() {
			slog.WarnContext(ctx, "providers.cursor.parser.composer_admitted_unread", "concern", concern, "path", globalDBPath, "composer_id", composerID, "stored_rows", stock.StoredRows)
			return composerStamp(header, stock), true
		}
		return keepPriorComposer(ctx, globalDBPath, composerID, priorStamp)
	}
	if len(header.FullConversationHeadersOnly) == 0 {
		if !stock.HasContent {
			return conversation.FileStamp{Size: 0, Mtime: time.Time{}}, false
		}
		slog.DebugContext(ctx, "providers.cursor.parser.composer_admitted_by_stored_bubbles", "concern", concern, "path", globalDBPath, "composer_id", composerID, "stored_rows", stock.StoredRows)
	}
	return composerStamp(header, stock), true
}

// composerStamp carries the bubble range's content revision beside the header's
// last update time.
func composerStamp(
	header cursorstore.ComposerHeader,
	stock cursorstore.ComposerBubbleStock,
) conversation.FileStamp {
	return conversation.FileStamp{
		Size:  composerChangeKey(stock.Revision, header),
		Mtime: msToTime(header.LastUpdatedAt),
	}
}

// keepPriorComposer re-admits a chat whose stored bubbles could not be read, on
// the stamp the last successful pass gave it, so a transient read failure does
// not remove a conversation Clyde already holds.
//
// It takes the previous stamp rather than rebuilding one from the record, because
// a composer record carries no size: reconstructing it would hand back a stamp
// the chat never had, which compares unequal, re-runs the scan and changes the
// fingerprint the engine keys re-embedding on. A chat with no previous stamp has
// nothing to preserve and is reported rather than quietly dropped.
func keepPriorComposer(
	ctx context.Context,
	globalDBPath string,
	composerID string,
	priorStamp conversation.FileStamp,
) (conversation.FileStamp, bool) {
	if priorStamp.Size == 0 && priorStamp.Mtime.IsZero() {
		slog.WarnContext(ctx, "providers.cursor.parser.composer_dropped_unread", "concern", concern, "path", globalDBPath, "composer_id", composerID)
		return conversation.FileStamp{Size: 0, Mtime: time.Time{}}, false
	}
	slog.WarnContext(ctx, "providers.cursor.parser.composer_kept_on_prior_stamp", "concern", concern, "path", globalDBPath, "composer_id", composerID)
	return priorStamp, true
}

// composerChangeKey folds everything that decides a composer's record into the
// stamp's size, which is what the incremental scan compares to decide whether to
// re-read a chat.
//
// A virtual composer path has no file, so this field was never a byte count; it
// is the change key, and it has to move whenever the record would. The bubble
// range's revision carries the body of the chat, because assembly reads the whole
// range and a chat that gains, loses, or replaces a bubble has changed even
// though its reference list has not. The latest request id carries the rest:
// retrying a turn gives the chat a new one while the revision and `lastUpdatedAt`
// stay put, and `lastUpdatedAt` is null on 52.5% of chats so the stamp's time is
// frozen for exactly those. Without it the record keeps a request id the chat no
// longer has, and resolving the new one misses the index and falls through to the
// provider's live store.
func composerChangeKey(revision int64, header cursorstore.ComposerHeader) int64 {
	// A 32-bit key is wide enough because it is only ever compared against this
	// same chat's previous key, never across chats, so a collision costs one
	// missed refresh for one chat at odds of about one in four billion per change.
	hasher := fnv.New32a()
	_, _ = fmt.Fprintf(hasher, "%d\x00%d\x00%s", revision, header.LastUpdatedAt, header.LatestChatGenerationUUID)
	return int64(hasher.Sum32())
}
