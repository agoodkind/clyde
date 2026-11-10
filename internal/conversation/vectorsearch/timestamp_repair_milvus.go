package vectorsearch

import (
	"context"
	"fmt"
	"slices"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/lm-semantic-search/collection"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

// timestampKnown distinguishes a null timestampUnix cell from zero.
type storedTimestamp struct {
	metadata       string
	timestampUnix  int64
	timestampKnown bool
}

// Strong consistency detects rows deleted since the first read.
func (b *milvusBackend) readRowTimestamps(ctx context.Context, collectionName string, ids []string) (map[string]storedTimestamp, error) {
	stored := make(map[string]storedTimestamp, len(ids))
	timestampColumn := collection.ScalarColumn{Name: timestampUnixColumn, Type: collection.ScalarTypeInt64, Nullable: true, MaxLength: 0}
	for start := 0; start < len(ids); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(ids))
		resultSet, err := b.client.Query(ctx, milvusclient.NewQueryOption(collectionName).
			WithIDs(column.NewColumnVarChar(milvusstore.IDField, slices.Clone(ids[start:end]))).
			WithOutputFields(milvusstore.IDField, milvusstore.MetadataField, timestampUnixColumn).
			WithConsistencyLevel(entity.ClStrong))
		if err != nil {
			return nil, failRead("read stored row timestamps from "+collectionName, err)
		}
		if resultSet.ResultCount == 0 {
			continue
		}
		idColumn := resultSet.GetColumn(milvusstore.IDField)
		metadataColumn := resultSet.GetColumn(milvusstore.MetadataField)
		cellColumn := resultSet.GetColumn(timestampUnixColumn)
		if idColumn == nil || metadataColumn == nil || cellColumn == nil {
			return nil, collection.ErrSearchResultIncomplete
		}
		for rowIndex := range resultSet.ResultCount {
			id, idErr := idColumn.GetAsString(rowIndex)
			if idErr != nil {
				return nil, failRead(fmt.Sprintf("read row id %d from %s", rowIndex, collectionName), idErr)
			}
			metadata, metadataErr := metadataColumn.GetAsString(rowIndex)
			if metadataErr != nil {
				return nil, failRead(fmt.Sprintf("read metadata of row %s from %s", id, collectionName), metadataErr)
			}
			value, valueErr := milvusstore.ScalarValueAt(cellColumn, timestampColumn, rowIndex)
			if valueErr != nil {
				return nil, failRead(fmt.Sprintf("read timestampUnix of row %s from %s", id, collectionName), valueErr)
			}
			stored[id] = storedTimestamp{metadata: metadata, timestampUnix: value.Int64, timestampKnown: !value.Null}
		}
	}
	return stored, nil
}

// The partial update sends only the primary key and timestamp columns.
func (b *milvusBackend) writeRowTimestamps(ctx context.Context, collectionName string, columns []collection.ScalarColumn, updates []timestampUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	ids := make([]string, 0, len(updates))
	metadata := make([]string, 0, len(updates))
	rows := make([]collection.Row, 0, len(updates))
	for _, update := range updates {
		ids = append(ids, update.id)
		metadata = append(metadata, update.metadata)
		rows = append(rows, collection.Row{
			ID:                update.id,
			Content:           "",
			RelativePath:      update.relativePath,
			StartLine:         0,
			EndLine:           0,
			FileExtension:     "",
			Metadata:          update.metadata,
			SplitPart:         0,
			SplitPartRecorded: false,
			Vector:            nil,
			Scalars:           map[string]collection.ScalarValue{timestampUnixColumn: timestampScalar(update.timestampUnix)},
		})
	}
	scalarColumns, err := milvusstore.DeclaredScalarInsertColumns(collectionName, columns, rows)
	if err != nil {
		return failRead("build timestamp columns for "+collectionName, err)
	}
	option := milvusclient.NewColumnBasedInsertOption(collectionName).
		WithVarcharColumn(milvusstore.IDField, ids).
		WithVarcharColumn(milvusstore.MetadataField, metadata).
		WithColumns(scalarColumns...).
		WithPartialUpdate(true)
	if _, err := b.client.Upsert(ctx, option); err != nil {
		return failRead("write row timestamps in "+collectionName, err)
	}
	return nil
}
