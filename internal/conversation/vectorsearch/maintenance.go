package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

// ErrCollectionAbsent reports that a maintenance operation found no Milvus
// collection for the collection id. A maintenance operation never creates one.
var ErrCollectionAbsent = errors.New("conversation collection does not exist")

// legacyPathFamilies are the relativePath prefixes of conversation rows. A row
// written before the conversationId column existed stores its conversation id
// only in this path.
var legacyPathFamilies = []string{"conv/", "convtool/", "convthink/"}

// BackfillConversationScalars writes the workspace root and archived status of
// each entry onto the rows of that conversation with a null or empty
// workspaceRoot or archived column. A row keeps its vector and every other
// column, and nothing is re-embedded. It returns the rows that need the
// backfill: changed counts the rows of listed conversations, and orphan counts
// the rest, which stay unchanged. A dry run counts and writes nothing. A missing
// collection returns ErrCollectionAbsent.
func (c *Client) BackfillConversationScalars(
	ctx context.Context,
	collectionID string,
	entries []semsearch.BackfillScalarEntry,
	dryRun bool,
) (int, int, error) {
	if c == nil {
		return 0, 0, errors.New("backfill conversation scalars: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return 0, 0, errors.New("backfill conversation scalars: collection id is empty")
	}
	collectionName := CollectionName(trimmedCollectionID)
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return 0, 0, err
	}
	if !exists {
		slog.WarnContext(ctx, "conversation.vectorsearch.backfill_collection_absent",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", collectionName,
		)
		return 0, 0, fmt.Errorf("backfill conversation scalars in %s: %w", collectionName, ErrCollectionAbsent)
	}
	if !dryRun {
		// ensureCollection adds a declared scalar column that an older
		// collection lacks. The dimension applies only to a created collection.
		if ensureErr := c.ensureCollection(ctx, collectionName, c.dimension); ensureErr != nil {
			return 0, 0, ensureErr
		}
	}
	backfill := collection.ScalarBackfill{
		ItemColumn:         c.declaration.ItemIDColumn,
		Columns:            declaredColumns(c.declaration, workspaceRootColumn, archivedColumn),
		Values:             backfillValues(entries),
		LegacyPathFamilies: legacyPathFamilies,
		DryRun:             dryRun,
	}
	changed, orphan, err := c.store.BackfillScalars(ctx, collectionName, backfill)
	if err != nil {
		return changed, orphan, failRead("backfill conversation scalars in "+collectionName, err)
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.scalar_backfill_completed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", collectionName,
		"entries", len(entries),
		"changed", changed,
		"orphan", orphan,
		"dry_run", dryRun,
	)
	return changed, orphan, nil
}

// backfillValues maps each trimmed conversation id to its workspaceRoot and
// archived values. An entry without a conversation id is skipped, and a later
// entry for the same id replaces an earlier one.
func backfillValues(entries []semsearch.BackfillScalarEntry) map[string]map[string]collection.ScalarValue {
	values := make(map[string]map[string]collection.ScalarValue, len(entries))
	for _, entry := range entries {
		conversationID := strings.TrimSpace(entry.ConversationID)
		if conversationID == "" {
			continue
		}
		values[conversationID] = map[string]collection.ScalarValue{
			workspaceRootColumn: collection.StringScalar(entry.WorkspaceRoot),
			archivedColumn:      collection.BoolScalar(entry.Archived),
		}
	}
	return values
}

// declaredColumns returns the declared columns with the given names, in the
// order of names. It skips a name the declaration lacks.
func declaredColumns(declaration collection.Declaration, names ...string) []collection.ScalarColumn {
	columns := make([]collection.ScalarColumn, 0, len(names))
	for _, name := range names {
		for _, column := range declaration.Scalars {
			if column.Name == name {
				columns = append(columns, column)
				break
			}
		}
	}
	return columns
}

// CountConversationRows returns the number of stored rows that DeleteConversation
// would delete for the conversation. It runs one read-only query that selects
// rows the same way, reads no vector, and changes no row. A missing collection
// returns 0.
func (c *Client) CountConversationRows(ctx context.Context, collectionID string, conversationID string) (int64, error) {
	if c == nil {
		return 0, errors.New("count semantic conversation rows: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return 0, errors.New("count semantic conversation rows: collection id is empty")
	}
	trimmedConversationID := strings.TrimSpace(conversationID)
	if trimmedConversationID == "" {
		return 0, errors.New("count semantic conversation rows: conversation id is empty")
	}
	collectionName := CollectionName(trimmedCollectionID)
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	rows, err := c.store.QueryRows(ctx, collection.RowsRequest{
		Collection:  collectionName,
		Declaration: c.declaration,
		ItemIDs:     []string{trimmedConversationID},
		PathPrefixes: []string{
			conversationRelativePathPrefix(trimmedConversationID),
			conversationToolRelativePathPrefix(trimmedConversationID),
			conversationThinkingRelativePathPrefix(trimmedConversationID),
		},
		IncludeVector: false,
	})
	if err != nil {
		return 0, failRead("count conversation "+trimmedConversationID+" rows in "+collectionName, err)
	}
	return int64(len(rows)), nil
}

// DeleteConversation removes the stored rows of one conversation and returns
// the deleted row count. A row belongs to the conversation when its
// conversationId column equals the id, or when its relativePath starts with
// conv/<id>/, convtool/<id>/, or convthink/<id>/. The three prefixes match rows
// written before the conversationId column existed. No other row is deleted. A
// missing collection deletes nothing. Only an operator command invokes this
// method, and normal ingestion never deletes a row.
func (c *Client) DeleteConversation(ctx context.Context, collectionID string, conversationID string) (int64, error) {
	if c == nil {
		return 0, errors.New("delete semantic conversation: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return 0, errors.New("delete semantic conversation: collection id is empty")
	}
	trimmedConversationID := strings.TrimSpace(conversationID)
	if trimmedConversationID == "" {
		return 0, errors.New("delete semantic conversation: conversation id is empty")
	}
	collectionName := CollectionName(trimmedCollectionID)
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	removed, err := c.store.DeleteItems(ctx, collection.DeleteItemsRequest{
		Collection:  collectionName,
		Declaration: c.declaration,
		ItemIDs:     []string{trimmedConversationID},
		PathPrefixes: []string{
			conversationRelativePathPrefix(trimmedConversationID),
			conversationToolRelativePathPrefix(trimmedConversationID),
			conversationThinkingRelativePathPrefix(trimmedConversationID),
		},
	})
	if err != nil {
		return removed, failRead("delete conversation "+trimmedConversationID+" from "+collectionName, err)
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.conversation_deleted",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", collectionName,
		"conversation_id", trimmedConversationID,
		"rows_removed", removed,
	)
	return removed, nil
}
