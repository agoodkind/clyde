package replay

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
)

//go:embed sql/has_tool_index.sql
var hasToolIndexSQL string

//go:embed sql/load_with_tools.sql
var loadWithToolsSQL string

//go:embed sql/find_tool.sql
var findToolSQL string

// Load reads a reply without creating the database.
//
// A missing database or an unknown reference returns found=false.
//
//	record, found, err := store.Load(ctx, ref)
func (store Store) Load(ctx context.Context, ref Reference) (Record, bool, error) {
	var empty Record

	database, err := store.openExisting(ctx)
	if err != nil || database == nil {
		return empty, false, err
	}

	defer func() { _ = database.Close() }()

	indexed, err := hasToolIndex(ctx, database)
	if err != nil {
		return empty, false, err
	}

	query := loadSQL
	if indexed {
		query = loadWithToolsSQL
	}

	return readRecord(ctx, database.QueryRowContext(ctx, query, ref.ConversationID, ref.ReplyID))
}

// FindToolReply returns the one reply matching a digest of ordered tool calls.
//
// A native conversation ID limits the search to that conversation. Missing or
// ambiguous matches return found=false.
//
//	record, found, err := store.FindToolReply(ctx, nativeID, digest)
func (store Store) FindToolReply(
	ctx context.Context,
	nativeConversationID string,
	digest string,
) (Record, bool, error) {
	var empty Record
	if digest == "" {
		return empty, false, nil
	}

	database, err := store.openExisting(ctx)
	if err != nil || database == nil {
		return empty, false, err
	}

	defer func() { _ = database.Close() }()

	indexed, err := hasToolIndex(ctx, database)
	if err != nil || !indexed {
		return empty, false, err
	}

	return readRecord(ctx, database.QueryRowContext(ctx, findToolSQL, digest, nativeConversationID))
}

func hasToolIndex(ctx context.Context, database *sql.DB) (bool, error) {
	var indexed bool
	if err := database.QueryRowContext(ctx, hasToolIndexSQL).Scan(&indexed); err != nil {
		return false, storeError(ctx, "read tool index schema", err)
	}

	return indexed, nil
}

func readRecord(ctx context.Context, row *sql.Row) (Record, bool, error) {
	var record Record
	var blocks []byte

	err := row.Scan(
		&record.Reference.ConversationID,
		&record.Reference.ReplyID,
		&record.Provider,
		&record.Digest,
		&record.VisibleDigest,
		&record.ToolDigest,
		&blocks,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}

	if err != nil {
		return record, false, storeError(ctx, "read reply", err)
	}

	if err := json.Unmarshal(blocks, &record.Blocks); err != nil {
		return record, false, storeError(ctx, "decode blocks", err)
	}

	return record, true, nil
}
