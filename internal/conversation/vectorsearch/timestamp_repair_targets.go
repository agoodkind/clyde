package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const timestampRepairTargetBatch = 4096

// ZeroTimeConversationIDs returns sorted conversation IDs with zero timestamp rows.
// A nonempty provider limits the results to that provider's ID prefix.
func (c *Client) ZeroTimeConversationIDs(ctx context.Context, collectionID string, provider string) ([]string, error) {
	if c == nil {
		return nil, errors.New("conversation search client is nil")
	}
	milvus, isMilvus := c.store.(*milvusBackend)
	if !isMilvus {
		return nil, ErrTimestampRepairUnsupported
	}
	collectionName := CollectionName(strings.TrimSpace(collectionID))
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return nil, err
	}
	if !exists {
		slog.WarnContext(ctx, "conversation.vectorsearch.zero_time_collection_absent", "concern", "conversation.semantic", "component", "conversation", "collection", collectionName)
		return nil, fmt.Errorf("collection does not exist %s: %w", collectionName, ErrCollectionAbsent)
	}
	filter := timestampUnixColumn + " == " + strconv.FormatInt(missingTimestampUnix, 10)
	iterator, err := milvus.client.QueryIterator(ctx, milvusclient.NewQueryIteratorOption(collectionName).
		WithBatchSize(timestampRepairTargetBatch).
		WithFilter(filter).
		WithOutputFields(milvusstore.IDField, milvusstore.RelativePathField, conversationIDColumn).
		WithConsistencyLevel(entity.ClStrong))
	if err != nil {
		return nil, failRead("open zero-time row iterator for "+collectionName, err)
	}
	prefix := ""
	if provider != "" {
		prefix = provider + ":"
	}
	found := make(map[string]struct{})
	for {
		resultSet, nextErr := iterator.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return nil, failRead("iterate zero-time rows of "+collectionName, nextErr)
		}
		pathColumn := resultSet.GetColumn(milvusstore.RelativePathField)
		idColumn := resultSet.GetColumn(conversationIDColumn)
		if pathColumn == nil || idColumn == nil {
			return nil, failRead("read zero-time rows of "+collectionName, errors.New("zero timestamp results lack conversation ID or relative path columns"))
		}
		for rowIndex := range resultSet.ResultCount {
			conversationID, idErr := storedConversationID(idColumn, pathColumn, rowIndex)
			if idErr != nil {
				return nil, failRead("read zero-time row of "+collectionName, idErr)
			}
			if conversationID != "" && strings.HasPrefix(conversationID, prefix) {
				found[conversationID] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	slog.InfoContext(ctx, "conversation.vectorsearch.zero_time_conversations_listed",
		"concern", "conversation.semantic", "component", "conversation", "collection", collectionName, "provider", provider, "conversations", len(ids))
	return ids, nil
}

// storedConversationID reads the ID column or extracts the ID from the relative path.
func storedConversationID(idColumn milvusColumn, pathColumn milvusColumn, rowIndex int) (conversationID string, err error) {
	defer func() {
		if err != nil {
			slog.Warn("conversation.vectorsearch.zero_time_row_unreadable", "concern", "conversation.semantic", "component", "conversation", "row", rowIndex, "err", err)
		}
	}()
	isNull, nullErr := idColumn.IsNull(rowIndex)
	if nullErr != nil {
		return "", fmt.Errorf("read conversation ID: %w", nullErr)
	}
	if !isNull {
		value, valueErr := idColumn.GetAsString(rowIndex)
		if valueErr != nil {
			return "", fmt.Errorf("read conversation ID: %w", valueErr)
		}
		if strings.TrimSpace(value) != "" {
			return value, nil
		}
	}
	path, pathErr := pathColumn.GetAsString(rowIndex)
	if pathErr != nil {
		return "", fmt.Errorf("read relative path: %w", pathErr)
	}
	parts := strings.Split(path, familyPartSeparator)
	if len(parts) < 2 {
		return "", nil
	}
	return parts[1], nil
}

type milvusColumn interface {
	IsNull(int) (bool, error)
	GetAsString(int) (string, error)
}
