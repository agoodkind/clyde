package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/lm-semantic-search/collection"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

// The collection.Store interface cannot answer these two requests. Its query
// returns neither the dense vector, the embedding model, nor the row ID, and its
// delete filters declared scalar columns only, not the primary key. This file
// sends the two requests to the Milvus client with the library's column names,
// until the store has operations for them.

const (
	// storedRowPageSize bounds one page of the stored-row iterator.
	storedRowPageSize = 1000
	// conversationFilterIDBatchSize bounds the conversation IDs in one query.
	conversationFilterIDBatchSize = 256
	// deleteIDBatchSize bounds the primary keys in one delete expression.
	deleteIDBatchSize = 500
)

// loadCollectionIfPresent reports whether the collection exists and loads it
// into memory, because Milvus serves a query or delete on a loaded collection
// only.
func (c *Client) loadCollectionIfPresent(ctx context.Context, collectionName string) (bool, error) {
	exists, err := c.milvus.HasCollection(ctx, milvusclient.NewHasCollectionOption(collectionName))
	if err != nil {
		return false, failRead("check Milvus collection "+collectionName, err)
	}
	if !exists {
		return false, nil
	}
	task, err := c.milvus.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(collectionName))
	if err != nil {
		return false, failRead("load Milvus collection "+collectionName, err)
	}
	if err := task.Await(ctx); err != nil {
		return false, failRead("await load of Milvus collection "+collectionName, err)
	}
	return true, nil
}

// failRead logs one failed Milvus request and returns the error with the
// operation that failed.
func failRead(operation string, err error) error {
	slog.Warn("conversation.vectorsearch.milvus_request_failed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}

// loadStoredBatch reads every stored row of the requested conversations. A row
// is selected by its conversationId column value or by a conversation path
// prefix, so rows written before the column existed stay visible. A missing
// collection returns an empty batch.
func (c *Client) loadStoredBatch(ctx context.Context, collectionName string, conversationIDs []string) (*storedBatch, error) {
	batch := newStoredBatch()
	requested := dedupeIDs(conversationIDs)
	if len(requested) == 0 {
		return batch, nil
	}
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return batch, nil
	}
	for start := 0; start < len(requested); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(requested))
		if err := c.loadStoredGroup(ctx, collectionName, requested[start:end], batch); err != nil {
			return nil, err
		}
	}
	return batch, nil
}

func (c *Client) loadStoredGroup(ctx context.Context, collectionName string, requested []string, batch *storedBatch) error {
	iterator, err := c.milvus.QueryIterator(ctx, milvusclient.NewQueryIteratorOption(collectionName).
		WithBatchSize(storedRowPageSize).
		WithFilter(storedRowFilter(requested)).
		WithOutputFields(
			milvusstore.IDField,
			conversationIDColumn,
			milvusstore.RelativePathField,
			roleColumn,
			milvusstore.ContentField,
			milvusstore.EmbeddingModelField,
			milvusstore.DenseVectorField,
			milvusstore.SplitPartField,
		))
	if err != nil {
		return failRead("open stored row iterator for "+collectionName, err)
	}
	for {
		resultSet, nextErr := iterator.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			return nil
		}
		if nextErr != nil {
			return failRead("iterate stored rows of "+collectionName, nextErr)
		}
		if err := c.appendStoredRows(resultSet, requested, batch); err != nil {
			return err
		}
	}
}

func (c *Client) appendStoredRows(resultSet milvusclient.ResultSet, requested []string, batch *storedBatch) error {
	columns := storedRowColumns{
		id:           resultSet.GetColumn(milvusstore.IDField),
		content:      resultSet.GetColumn(milvusstore.ContentField),
		vector:       resultSet.GetColumn(milvusstore.DenseVectorField),
		path:         resultSet.GetColumn(milvusstore.RelativePathField),
		conversation: resultSet.GetColumn(conversationIDColumn),
		role:         resultSet.GetColumn(roleColumn),
		model:        resultSet.GetColumn(milvusstore.EmbeddingModelField),
		splitPart:    resultSet.GetColumn(milvusstore.SplitPartField),
	}
	if columns.id == nil || columns.content == nil || columns.vector == nil || columns.path == nil {
		return collection.ErrSearchResultIncomplete
	}
	for i := range resultSet.ResultCount {
		row, storedID, err := columns.rowAt(i)
		if err != nil {
			return err
		}
		conversationID := assignConversationID(storedID, row.RelativePath, requested)
		batch.add(conversationID, row, c.embeddingModel)
	}
	return nil
}

// storedRowColumns are the columns of one stored-row result page. Only the
// id, content, vector, and path columns are required.
type storedRowColumns struct {
	id           column.Column
	content      column.Column
	vector       column.Column
	path         column.Column
	conversation column.Column
	role         column.Column
	model        column.Column
	splitPart    column.Column
}

// rowAt reads one stored row and its conversationId column value.
func (columns storedRowColumns) rowAt(i int) (storedRow, string, error) {
	empty := storedRow{ID: "", RelativePath: "", Content: "", Role: "", EmbeddingModel: "", SplitPart: 0, SplitPartRecorded: false, Vector: nil}
	id, err := columns.id.GetAsString(i)
	if err != nil {
		return empty, "", failRead(fmt.Sprintf("read id column at %d", i), err)
	}
	content, err := columns.content.GetAsString(i)
	if err != nil {
		return empty, "", failRead(fmt.Sprintf("read content column at %d", i), err)
	}
	path, err := columns.path.GetAsString(i)
	if err != nil {
		return empty, "", failRead(fmt.Sprintf("read relative path column at %d", i), err)
	}
	vector, err := milvusstore.VectorAt(columns.vector, i)
	if err != nil {
		return empty, "", failRead(fmt.Sprintf("read vector column at %d", i), err)
	}
	storedID, err := optionalString(columns.conversation, i)
	if err != nil {
		return empty, "", err
	}
	role, err := optionalString(columns.role, i)
	if err != nil {
		return empty, "", err
	}
	model, err := optionalString(columns.model, i)
	if err != nil {
		return empty, "", err
	}
	splitPart, splitRecorded, err := milvusstore.SplitPartAt(columns.splitPart, i)
	if err != nil {
		return empty, "", failRead(fmt.Sprintf("read split part column at %d", i), err)
	}
	return storedRow{
		ID:                id,
		RelativePath:      path,
		Content:           content,
		Role:              role,
		EmbeddingModel:    model,
		SplitPart:         splitPart,
		SplitPartRecorded: splitRecorded,
		Vector:            vector,
	}, storedID, nil
}

// optionalString reads a nullable string column. A missing column and a null
// value both read empty.
func optionalString(valueColumn column.Column, i int) (string, error) {
	if valueColumn == nil {
		return "", nil
	}
	isNull, err := valueColumn.IsNull(i)
	if err != nil {
		return "", failRead(fmt.Sprintf("read null state at %d", i), err)
	}
	if isNull {
		return "", nil
	}
	value, err := valueColumn.GetAsString(i)
	if err != nil {
		return "", failRead(fmt.Sprintf("read string at %d", i), err)
	}
	return value, nil
}

// storedRowFilter selects the rows of the requested conversations by
// conversationId and by the three family path prefixes.
func storedRowFilter(requested []string) string {
	clauses := []string{inStringClause(conversationIDColumn, requested)}
	for _, id := range requested {
		clauses = append(clauses,
			pathPrefixClause(conversationRelativePathPrefix(id)),
			pathPrefixClause(conversationToolRelativePathPrefix(id)),
			pathPrefixClause(conversationThinkingRelativePathPrefix(id)),
		)
	}
	return "(" + strings.Join(clauses, " or ") + ")"
}

func inStringClause(field string, values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, `"`+collection.EscapeString(value)+`"`)
	}
	return field + " in [" + strings.Join(quoted, ", ") + "]"
}

// pathPrefixClause matches rows with a relativePath that starts with prefix. It
// escapes the like wildcards in the prefix.
func pathPrefixClause(prefix string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	return fmt.Sprintf(`%s like "%s%%"`, milvusstore.RelativePathField, collection.EscapeString(escaped))
}

// deleteRowsByID deletes rows by primary key in bounded batches.
func (c *Client) deleteRowsByID(ctx context.Context, collectionName string, rowIDs []string) error {
	for start := 0; start < len(rowIDs); start += deleteIDBatchSize {
		end := min(start+deleteIDBatchSize, len(rowIDs))
		expression := inStringClause(milvusstore.IDField, rowIDs[start:end])
		if _, err := c.milvus.Delete(ctx, milvusclient.NewDeleteOption(collectionName).WithExpr(expression)); err != nil {
			slog.WarnContext(ctx, "conversation.vectorsearch.delete_rows_failed",
				"concern", "conversation.semantic",
				"component", "conversation",
				"collection", collectionName,
				"rows", end-start,
				"err", err,
			)
			return fmt.Errorf("delete replaced rows from %s: %w", collectionName, err)
		}
	}
	return nil
}
