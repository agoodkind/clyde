//go:build milvusintegration

package vectorsearch

import (
	"context"

	"goodkind.io/lm-semantic-search/collection"
)

// WriteBlankRowForTest stores a text row containing one space with an unknown date
// at messageIndex.
func WriteBlankRowForTest(ctx context.Context, client *Client, collectionID string, conversationID string, messageIndex int32, role string) error {
	vector, err := client.embedder.Embed(ctx, conversationID)
	if err != nil {
		return err
	}
	chunk := storedChunk{
		Content: " ", RelativePath: conversationRelativePath(conversationID, messageIndex, 0, false),
		ConversationID: conversationID, ParentConversationID: "", MessageIndex: messageIndex, Role: role,
		TimestampUnix: missingTimestampUnix, WorkspaceRoot: "", Archived: false, SplitPart: 0, LoadRules: "",
	}
	return client.store.Upsert(ctx, CollectionName(collectionID), client.declaration, []collection.Row{chunkRow(chunk, vector)})
}
