package daemon

import (
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
	"goodkind.io/clyde/internal/transcript"
)

// projectEmbeddedConversationFields builds the embedded search fields for one
// loaded conversation. BuildSemanticConversationDocuments selects the content,
// and each document keeps its position in the complete loaded message
// sequence. artifactSettled reports that the artifact stayed unchanged for
// embeddedTrailingSettleWindow.
func projectEmbeddedConversationFields(
	record conversation.Record,
	messages []transcript.Message,
	kinds conversation.ContentKindSet,
	artifactSettled bool,
) (searchbackend.ProjectedFields, SemanticConversationDocuments, error) {
	built, err := BuildSemanticConversationDocuments(record, messages, kinds)
	if err != nil {
		return searchbackend.ProjectedFields{Fields: nil, WithheldOpenFields: 0}, built, err
	}
	projectionMessages := make([]searchbackend.Message, 0, len(built.Docs))
	for _, doc := range built.Docs {
		messageIndex := int(doc.MessageIndex)
		tools := make([]searchbackend.Tool, 0, len(doc.Tools))
		for _, tool := range doc.Tools {
			tools = append(tools, searchbackend.Tool{
				Name:        tool.Name,
				Display:     tool.Display,
				DisplayLang: tool.LangHint,
				Output:      tool.Output,
			})
		}
		projectionMessages = append(projectionMessages, searchbackend.Message{
			Index:             messageIndex,
			ProviderMessageID: messages[messageIndex].UUID,
			Role:              doc.Role,
			Timestamp:         messages[messageIndex].Timestamp,
			Text:              doc.Text,
			Thinking:          doc.Thinking,
			Tools:             tools,
		})
	}
	projected := searchbackend.ProjectFields(searchbackend.Conversation{
		ID:                     record.ID,
		LoadRules:              conversation.LoadRulesTag(kinds),
		MessageCount:           len(messages),
		TrailingMessageMayGrow: conversation.TrailingMessageMayGrow(record),
		ArtifactSettled:        artifactSettled,
		ToolDetail:             embeddedToolDetail(kinds),
	}, projectionMessages)
	return projected, built, nil
}

// embeddedToolDetail maps the nested tool content kinds onto the projection
// tool level. [conversation.NewContentKindSet] keeps at most one of them.
func embeddedToolDetail(kinds conversation.ContentKindSet) searchbackend.ToolDetail {
	switch {
	case kinds.Has(conversation.ContentKindToolOutputs):
		return searchbackend.ToolDetailOutput
	case kinds.Has(conversation.ContentKindToolCalls):
		return searchbackend.ToolDetailCall
	case kinds.Has(conversation.ContentKindToolSummaries):
		return searchbackend.ToolDetailName
	default:
		return searchbackend.ToolDetailNone
	}
}
