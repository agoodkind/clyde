package daemon

import (
	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/conversation"
)

// contentKindSetFromExportRequest rebuilds the content-kind set from the export
// request's per-kind booleans, which remain the wire encoding of the selection.
func contentKindSetFromExportRequest(
	req *clydev1.ExportTranscriptRequest,
) conversation.ContentKindSet {
	var kinds []conversation.ContentKind
	if req.GetIncludeChat() {
		kinds = append(kinds, conversation.ContentKindChat)
	}

	if req.GetIncludeThinking() {
		kinds = append(kinds, conversation.ContentKindThinking)
	}

	if req.GetIncludeToolSummaries() {
		kinds = append(kinds, conversation.ContentKindToolSummaries)
	}

	if req.GetIncludeToolCalls() {
		kinds = append(kinds, conversation.ContentKindToolCalls)
	}

	if req.GetIncludeToolOutputs() {
		kinds = append(kinds, conversation.ContentKindToolOutputs)
	}

	if req.GetIncludeSystemPrompts() {
		kinds = append(kinds, conversation.ContentKindSystemPrompts)
	}

	if req.GetIncludeSystemMessages() {
		kinds = append(kinds, conversation.ContentKindSystemMessages)
	}

	if req.GetIncludeInjected() {
		kinds = append(kinds, conversation.ContentKindInjected)
	}

	if req.GetIncludeRawJsonMetadata() {
		kinds = append(kinds, conversation.ContentKindRawJSONMetadata)
	}

	return conversation.NewContentKindSet(kinds...)
}
