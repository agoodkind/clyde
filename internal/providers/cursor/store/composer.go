package cursorstore

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

const composerDataKeyPrefix = "composerData:"

// composerHeadersTableName is Cursor's per-chat header table. Unlike the
// key-value tables it has real columns, so predicates on its timestamps are
// cheap.
const composerHeadersTableName = "composerHeaders"

// ComposerHeader models the indexed subset of Cursor's undocumented,
// version-pinned `composerData:<composerId>` on-disk payload.
type ComposerHeader struct {
	ComposerID    string `json:"composerId"`
	Name          string `json:"name"`
	CreatedAt     int64  `json:"createdAt"`
	LastUpdatedAt int64  `json:"lastUpdatedAt"`
	Status        string `json:"status"`
	UnifiedMode   string `json:"unifiedMode"`
	ForceMode     string `json:"forceMode"`
	// LatestChatGenerationUUID is the request id of the chat's most recent
	// generation. Cursor keeps only the newest one here, so it identifies the
	// last request of the chat and says nothing about earlier ones.
	LatestChatGenerationUUID    string              `json:"latestChatGenerationUUID"`
	FullConversationHeadersOnly []ComposerBubbleRef `json:"fullConversationHeadersOnly"`
}

// ComposerBubbleRef is one ordered bubble reference stored in a composer
// header.
type ComposerBubbleRef struct {
	BubbleID string `json:"bubbleId"`
	Type     int    `json:"type"`
}

// ReadComposerHeader reads and decodes one `composerData:<composerId>` row from
// a Cursor global database.
func ReadComposerHeader(ctx context.Context, db *sql.DB, composerID string) (ComposerHeader, bool, error) {
	var emptyHeader ComposerHeader

	value, found, err := ReadKVValue(ctx, db, KVTableCursorDiskKV, composerDataKey(composerID))
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_header_read_failed", "concern", concern, "composer_id", composerID, "err", err)
		return emptyHeader, false, fmt.Errorf("read cursor composer header %q: %w", composerID, err)
	}
	if !found {
		return emptyHeader, false, nil
	}

	header, err := DecodeComposerHeaderJSON(value)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_header_decode_failed", "concern", concern, "composer_id", composerID, "err", err)
		return emptyHeader, false, fmt.Errorf("decode cursor composer header %q: %w", composerID, err)
	}
	return header, true, nil
}

// ListComposerIDs lists composer ids present in a Cursor global database.
func ListComposerIDs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := ReadKVRowsByPrefix(ctx, db, KVTableCursorDiskKV, composerDataKeyPrefix)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.composer_ids_list_failed", "concern", concern, "key_prefix", composerDataKeyPrefix, "err", err)
		return nil, fmt.Errorf("list cursor composer ids: %w", err)
	}
	composerIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		composerIDs = append(composerIDs, strings.TrimPrefix(row.Key, composerDataKeyPrefix))
	}
	return composerIDs, nil
}

func composerDataKey(composerID string) string {
	return composerDataKeyPrefix + composerID
}
