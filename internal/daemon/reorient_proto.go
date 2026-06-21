package daemon

import (
	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

func reorientPageFromProto(resp *clydev1.ReorientConversationResponse) conversation.ReorientPage {
	return conversation.ReorientPage{
		CurrentConversation: reorientConversationRefFromProto(resp.GetCurrentConversation()),
		ParentConversation:  optionalReorientConversationRefFromProto(resp.GetParentConversation()),
		CheckpointNumber:    int(resp.GetCheckpointNumber()),
		Items:               reorientItemsFromProto(resp.GetItems()),
		NextCursor:          resp.GetNextCursor(),
		Remaining:           int(resp.GetRemaining()),
		Offset:              int(resp.GetOffset()),
		TotalItems:          int(resp.GetTotalItems()),
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

func optionalReorientConversationRefFromProto(wire *clydev1.ReorientConversationRef) *conversation.ReorientConversationRef {
	if wire == nil {
		return nil
	}
	ref := reorientConversationRefFromProto(wire)
	return &ref
}

func reorientItemsFromProto(wireItems []*clydev1.ReorientItem) []conversation.ReorientItem {
	items := make([]conversation.ReorientItem, 0, len(wireItems))
	for _, wire := range wireItems {
		items = append(items, conversation.ReorientItem{
			Kind:           reorientItemKindFromProto(wire.GetKind()),
			Title:          wire.GetTitle(),
			Body:           wire.GetBody(),
			ConversationID: wire.GetConversationId(),
			MessageIndex:   int(wire.GetMessageIndex()),
		})
	}
	return items
}

func reorientItemKindFromProto(kind clydev1.ReorientItemKind) conversation.ReorientItemKind {
	switch kind {
	case clydev1.ReorientItemKind_REORIENT_ITEM_KIND_UNSPECIFIED:
		return conversation.ReorientItemKind("")
	case clydev1.ReorientItemKind_REORIENT_ITEM_KIND_HEADER:
		return conversation.ReorientItemKindHeader
	case clydev1.ReorientItemKind_REORIENT_ITEM_KIND_RECOVERED_CONTEXT:
		return conversation.ReorientItemKindRecoveredContext
	case clydev1.ReorientItemKind_REORIENT_ITEM_KIND_TAIL:
		return conversation.ReorientItemKindTail
	case clydev1.ReorientItemKind_REORIENT_ITEM_KIND_PARENT_ANCHOR:
		return conversation.ReorientItemKindParentAnchor
	case clydev1.ReorientItemKind_REORIENT_ITEM_KIND_MEMORY_DOC:
		return conversation.ReorientItemKindMemoryDoc
	case clydev1.ReorientItemKind_REORIENT_ITEM_KIND_SEARCH_HIT:
		return conversation.ReorientItemKindSearchHit
	default:
		return conversation.ReorientItemKind("")
	}
}
