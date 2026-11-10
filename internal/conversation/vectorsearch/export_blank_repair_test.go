//go:build milvusintegration

package vectorsearch

import (
	"context"

	"goodkind.io/lm-semantic-search/collection"
)

// WriteBlankRowForTest stores a text row at messageIndex with an unknown date.
// It uses the given whitespace content, role, and split part.
func WriteBlankRowForTest(
	ctx context.Context,
	client *Client,
	collectionID string,
	conversationID string,
	pathConversationID string,
	messageIndex int32,
	role string,
	content string,
	splitPart int32,
) error {
	vector, err := client.embedder.Embed(ctx, conversationID)
	if err != nil {
		return err
	}
	chunk := storedChunk{
		Content:              content,
		RelativePath:         conversationRelativePath(pathConversationID, messageIndex, 0, false),
		ConversationID:       conversationID,
		ParentConversationID: "",
		MessageIndex:         messageIndex,
		Role:                 role,
		TimestampUnix:        missingTimestampUnix,
		WorkspaceRoot:        "",
		Archived:             false,
		SplitPart:            splitPart,
		LoadRules:            "",
	}
	return client.store.Upsert(
		ctx,
		CollectionName(collectionID),
		client.declaration,
		[]collection.Row{chunkRow(chunk, vector)},
	)
}

// WriteTextPartForTest stores one part of a multipart text row at messageIndex with
// an unknown date.
func WriteTextPartForTest(
	ctx context.Context,
	client *Client,
	collectionID string,
	conversationID string,
	messageIndex int32,
	partIndex int,
	role string,
	content string,
) error {
	vector, err := client.embedder.Embed(ctx, conversationID)
	if err != nil {
		return err
	}
	chunk := storedChunk{
		Content: content,
		RelativePath: conversationRelativePath(
			conversationID,
			messageIndex,
			partIndex,
			true,
		),
		ConversationID:       conversationID,
		ParentConversationID: "",
		MessageIndex:         messageIndex,
		Role:                 role,
		TimestampUnix:        missingTimestampUnix,
		WorkspaceRoot:        "",
		Archived:             false,
		SplitPart:            0,
		LoadRules:            "",
	}
	return client.store.Upsert(
		ctx,
		CollectionName(collectionID),
		client.declaration,
		[]collection.Row{chunkRow(chunk, vector)},
	)
}
