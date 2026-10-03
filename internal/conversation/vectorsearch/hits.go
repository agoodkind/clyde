package vectorsearch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

// rowMetadata is the metadata JSON the lm-semantic-search daemon wrote on every
// conversation row. Rows written before the scalar columns existed store the
// conversation identity only here, so hit conversion reads these fields.
type rowMetadata struct {
	ConversationID       string `json:"conversation_id,omitempty"`
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	MessageIndex         *int32 `json:"message_index,omitempty"`
	Role                 string `json:"role,omitempty"`
	TimestampUnix        *int64 `json:"timestamp_unix,omitempty"`
}

func emptyRowMetadata() rowMetadata {
	return rowMetadata{
		ConversationID:       "",
		ParentConversationID: "",
		MessageIndex:         nil,
		Role:                 "",
		TimestampUnix:        nil,
	}
}

func decodeRowMetadata(metadata string) rowMetadata {
	if metadata == "" {
		return emptyRowMetadata()
	}
	var parsed rowMetadata
	if err := json.Unmarshal([]byte(metadata), &parsed); err != nil {
		return emptyRowMetadata()
	}
	return parsed
}

// semHit converts one ranked row to a conversation hit. The identity, message
// index, role, and timestamp come from the row metadata JSON, which every row
// stores. The loadRules value comes from the declared column and reads empty
// when the row lacks it.
func semHit(hit collection.Hit) semsearch.SemHit {
	metadata := decodeRowMetadata(hit.Metadata)
	var messageIndex int32
	if metadata.MessageIndex != nil {
		messageIndex = *metadata.MessageIndex
	}
	var timestampUnix int64
	if metadata.TimestampUnix != nil {
		timestampUnix = *metadata.TimestampUnix
	}
	return semsearch.SemHit{
		ConversationID:       metadata.ConversationID,
		ParentConversationID: metadata.ParentConversationID,
		MessageIndex:         messageIndex,
		Role:                 metadata.Role,
		TimestampUnix:        timestampUnix,
		Content:              hit.Content,
		Score:                hit.Score,
		LoadRules:            scalarString(hit, loadRulesColumn),
	}
}

func semHits(hits []collection.Hit) []semsearch.SemHit {
	converted := make([]semsearch.SemHit, 0, len(hits))
	for _, hit := range hits {
		converted = append(converted, semHit(hit))
	}
	return converted
}

// scalarString returns the string value of a hit's scalar cell, or empty when
// the hit lacks the cell, the cell is null, or the value is not a string.
func scalarString(hit collection.Hit, columnName string) string {
	cell, found := hit.Scalars[columnName]
	if !found || cell.State != collection.ScalarCellValue {
		return ""
	}
	return cell.Value.String
}

// resolveLegacyGroups sets the group of each candidate with a null
// conversationId column. It queries those rows by primary key and reads the
// conversation ID from each row's metadata JSON. A legacy row without a
// metadata conversation ID groups under the empty conversation ID. The result
// omits a legacy row deleted after the ranking search.
func resolveLegacyGroups(
	ctx context.Context,
	client *milvusclient.Client,
	collectionName string,
	candidates []milvusstore.Candidate,
) ([]milvusstore.Candidate, error) {
	legacyKeys := make([]string, 0)
	for _, candidate := range candidates {
		if candidate.Group.State != collection.ScalarCellValue {
			legacyKeys = append(legacyKeys, candidate.PrimaryKey)
		}
	}
	if len(legacyKeys) == 0 {
		return candidates, nil
	}
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(collectionName).
		WithIDs(column.NewColumnVarChar(milvusstore.IDField, legacyKeys)).
		WithOutputFields(milvusstore.IDField, milvusstore.MetadataField))
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.legacy_identity_query_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", collectionName,
			"err", err,
		)
		return nil, fmt.Errorf("load legacy conversation identity from %s: %w", collectionName, err)
	}
	idColumn := resultSet.GetColumn(milvusstore.IDField)
	metadataColumn := resultSet.GetColumn(milvusstore.MetadataField)
	if resultSet.ResultCount > 0 && (idColumn == nil || metadataColumn == nil) {
		return nil, collection.ErrSearchResultIncomplete
	}
	legacyIDs := make(map[string]string, resultSet.ResultCount)
	for i := range resultSet.ResultCount {
		primaryKey, idErr := idColumn.GetAsString(i)
		if idErr != nil {
			return nil, fmt.Errorf("read legacy primary key %d from %s: %w", i, collectionName, idErr)
		}
		metadata, metadataErr := metadataColumn.GetAsString(i)
		if metadataErr != nil {
			return nil, fmt.Errorf("read legacy metadata %d from %s: %w", i, collectionName, metadataErr)
		}
		legacyIDs[primaryKey] = decodeRowMetadata(metadata).ConversationID
	}
	return applyLegacyGroups(candidates, legacyIDs), nil
}

// applyLegacyGroups sets each null-group candidate's group from legacyIDs and
// drops a null-group candidate that legacyIDs does not contain.
func applyLegacyGroups(candidates []milvusstore.Candidate, legacyIDs map[string]string) []milvusstore.Candidate {
	resolved := make([]milvusstore.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Group.State != collection.ScalarCellValue {
			conversationID, found := legacyIDs[candidate.PrimaryKey]
			if !found {
				continue
			}
			candidate.Group = collection.ValueCell(conversationIDColumn, collection.StringScalar(conversationID))
		}
		resolved = append(resolved, candidate)
	}
	return resolved
}
