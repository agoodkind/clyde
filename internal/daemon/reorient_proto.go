package daemon

import (
	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func reorientPageFromProto(resp *clydev1.ReorientConversationResponse) conversation.ReorientPage {
	return conversation.ReorientPage{
		CurrentConversation: reorientConversationRefFromProto(resp.GetCurrentConversation()),
		Body:                string(resp.GetPageBody()),
		NextCursor:          resp.GetNextCursor(),
		Remaining:           int(resp.GetRemaining()),
		Offset:              int(resp.GetOffset()),
		TotalBytes:          int(resp.GetTotalBytes()),
		TotalLines:          int(resp.GetTotalLines()),
		Truncated:           resp.GetTruncated(),
		Restart:             resp.GetRestart(),
		Warnings:            resp.GetWarnings(),
	}
}

func reorientConversationRefFromProto(wire *clydev1.ReorientConversationRef) conversation.ReorientConversationRef {
	if wire == nil {
		return conversation.ReorientConversationRef{
			ID:            "",
			Provider:      "",
			Title:         "",
			WorkspaceRoot: "",
		}
	}
	return conversation.ReorientConversationRef{
		ID:            wire.GetId(),
		Provider:      providerFromProto(wire.GetProvider()).String(),
		Title:         wire.GetTitle(),
		WorkspaceRoot: wire.GetWorkspaceRoot(),
	}
}
