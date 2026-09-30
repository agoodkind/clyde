package daemon

import (
	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func searchSourceIdentityFromProto(identity *clydev1.SearchSourceIdentity) *conversation.SearchSourceIdentity {
	if identity == nil {
		return nil
	}
	return &conversation.SearchSourceIdentity{
		ConversationID: identity.GetConversationId(), MessageIndex: int(identity.GetMessageIndex()),
		ContentKind: identity.GetContentKind(), ToolIndex: int(identity.GetToolIndex()),
		SourceByteStart: identity.GetSourceByteStart(), SourceByteEnd: identity.GetSourceByteEnd(),
	}
}

func protoSearchSourceIdentity(identity *conversation.SearchSourceIdentity) *clydev1.SearchSourceIdentity {
	if identity == nil {
		return nil
	}
	return &clydev1.SearchSourceIdentity{
		ConversationId: identity.ConversationID, MessageIndex: int64(identity.MessageIndex),
		ContentKind: identity.ContentKind, ToolIndex: int64(identity.ToolIndex),
		SourceByteStart: identity.SourceByteStart, SourceByteEnd: identity.SourceByteEnd,
	}
}
