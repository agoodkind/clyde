package parser

import (
	"context"
	"database/sql"
	"log/slog"

	"goodkind.io/clyde/internal/conversation"
	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

// composerOrigin reads a composer's origin from the subagent flag Cursor wrote
// beside it. A composer Cursor did not mark stays unspecified rather than being
// called the person's own, because the absence of the flag is not a statement
// that a conversation did not dispatch this one.
func composerOrigin(subagent bool) conversation.Origin {
	if subagent {
		return conversation.OriginSubagent
	}
	return conversation.OriginUnspecified
}

// composerMetadataIndex reads the descriptive metadata Cursor keeps beside its
// chats, and says whether it got all of it.
//
// A failure is deliberately not raised to the caller. Discover feeds one scan
// shared by every provider, and an error from any provider throws away that whole
// pass, so raising it would stop Claude, Codex, and Cursor's own transcripts from
// being indexed for as long as the condition lasted. A missing title is worth
// less than the corpus.
//
// It is logged at error rather than warning because nothing downstream reports
// it: the chats still appear, so a reader has no way to tell a chat with no
// workspace from a chat whose workspace could not be read.
func composerMetadataIndex(
	ctx context.Context,
	db *sql.DB,
	root cursorstore.DataRoot,
) cursorstore.ComposerMetadataIndex {
	index := cursorstore.BuildComposerMetadataIndex(ctx, db, root)
	if index.Err != nil {
		slog.ErrorContext(ctx, "providers.cursor.parser.composer_metadata_incomplete", "concern", concern, "path", root.GlobalDBPath, "read", len(index.ByComposerID), "err", index.Err)
	}
	return index
}

// stampCoveringMetadata advances a chat's stamp to the moment its metadata last
// changed, whenever that is later than the chat's own record says.
//
// A record's title, project, and archived flag come from a store the chat's own
// record knows nothing about, and the scan reuses a cached record whenever its
// stamp is unchanged. A stamp built only from the chat's record therefore leaves
// a renamed or archived chat showing its old title with nothing to correct it.
// Measured on a real store, the metadata timestamp runs ahead of the record's own
// for 32 of 2,370 chats, and never behind it.
//
// A rename that moves no timestamp in either store is still missed. A stamp
// carries a size and a time, the size is the chat's message count, and neither
// can express that a title changed, so closing that gap needs a wider change than
// this parser can make.
func stampCoveringMetadata(
	stamp conversation.FileStamp,
	metadata cursorstore.ComposerMetadata,
	hasMetadata bool,
) conversation.FileStamp {
	if !hasMetadata {
		return stamp
	}
	metadataMtime := msToTime(metadata.LastUpdatedAt)
	if metadataMtime.After(stamp.Mtime) {
		stamp.Mtime = metadataMtime
	}
	return stamp
}
