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

func protoConversationSelection(ids []string) *clydev1.ConversationSelection {
	if ids == nil {
		return nil
	}
	return &clydev1.ConversationSelection{Ids: ids}
}

func conversationSelectionFromProto(selection *clydev1.ConversationSelection) []string {
	if selection == nil {
		return nil
	}
	ids := make([]string, len(selection.GetIds()))
	copy(ids, selection.GetIds())
	return ids
}

func protoSearchConversationsRequest(options conversation.SearchConversationsOptions) *clydev1.SearchConversationsRequest {
	return &clydev1.SearchConversationsRequest{
		Query:                 options.Query,
		Limit:                 int64(options.Limit),
		Offset:                int64(options.Offset),
		Provider:              protoProvider(options.Provider),
		Workspace:             options.WorkspaceRoot,
		IncludeArchived:       options.IncludeArchived,
		Roles:                 options.Roles,
		FromUnix:              options.FromUnix,
		UntilUnix:             options.UntilUnix,
		MinScore:              options.MinScore,
		PerConversationLimit:  int64(options.PerConversationLimit),
		ConversationId:        options.ConversationID,
		ConversationSelection: protoConversationSelection(options.ConversationIDs),
		IncludeSubagents:      options.IncludeSubagents,
		ContextWindow:         int64(options.ContextWindow),
		Cursor:                options.Cursor,
	}
}
