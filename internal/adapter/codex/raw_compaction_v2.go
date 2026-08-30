package codex

import (
	"encoding/json"
	"strings"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

// RawResponsesCompactionV2Layout identifies the setup, transcript, and
// terminal trigger boundaries in a captured v2 request.
type RawResponsesCompactionV2Layout struct {
	SetupEnd        int
	TranscriptStart int
	TriggerIndex    int
}

// ParseRawResponsesCompactionV2 validates the captured mid-turn memento shape.
// It does not change the request.
func ParseRawResponsesCompactionV2(
	request RawResponsesRequest,
) (RawResponsesCompactionV2Layout, bool) {
	emptyLayout := RawResponsesCompactionV2Layout{
		SetupEnd:        0,
		TranscriptStart: 0,
		TriggerIndex:    0,
	}
	if !rawResponsesCompactionV2MetadataIsValid(request) {
		return emptyLayout, false
	}
	inputStart, inputEnd, ok := jsonObjectFieldValueRange(request.Body, "input")
	if !ok {
		return emptyLayout, false
	}
	var rawItems []json.RawMessage
	if json.Unmarshal(request.Body[inputStart:inputEnd], &rawItems) != nil {
		return emptyLayout, false
	}
	items := codexstore.NormalizeResponseInputItems(rawItems)
	triggerIndex, ok := rawResponsesCompactionV2TriggerIndex(items)
	if !ok {
		return emptyLayout, false
	}
	transcriptStart, ok := rawResponsesCompactionV2TranscriptStart(items[:triggerIndex])
	if !ok {
		return emptyLayout, false
	}
	transcriptItems := items[transcriptStart:triggerIndex]
	if !rawResponsesCompactionV2TranscriptIsKnown(transcriptItems) ||
		!rawCompactionPairsAreComplete(transcriptItems) {
		return emptyLayout, false
	}
	return RawResponsesCompactionV2Layout{
		SetupEnd:        transcriptStart,
		TranscriptStart: transcriptStart,
		TriggerIndex:    triggerIndex,
	}, true
}

func rawResponsesCompactionV2MetadataIsValid(request RawResponsesRequest) bool {
	metadataValue := strings.TrimSpace(request.Header.Get(CodexTurnMetadataHeader))
	if metadataValue == "" {
		return false
	}
	var metadata rawResponsesCompactionMetadata
	if json.Unmarshal([]byte(metadataValue), &metadata) != nil {
		return false
	}
	return metadata.RequestKind == "compaction" &&
		metadata.Compaction.Implementation == string(RawResponsesCompactionV2) &&
		metadata.Compaction.Phase == "mid_turn" &&
		metadata.Compaction.Strategy == "memento"
}

func rawResponsesCompactionV2TriggerIndex(
	items []transcript.CompactedContextItem,
) (int, bool) {
	triggerIndex := -1
	for itemIndex, item := range items {
		if item.Kind != transcript.CompactedContextItemKindCompactionTrigger {
			continue
		}
		if triggerIndex >= 0 {
			return 0, false
		}
		triggerIndex = itemIndex
	}
	if triggerIndex < 0 || triggerIndex != len(items)-1 {
		return 0, false
	}
	return triggerIndex, true
}

func rawResponsesCompactionV2TranscriptStart(
	items []transcript.CompactedContextItem,
) (int, bool) {
	for itemIndex, item := range items {
		if item.Kind == transcript.CompactedContextItemKindMessage &&
			item.Message != nil && item.Message.Role == "user" {
			return itemIndex, true
		}
	}
	return 0, false
}

func rawResponsesCompactionV2TranscriptIsKnown(
	items []transcript.CompactedContextItem,
) bool {
	for _, item := range items {
		if item.Kind == transcript.CompactedContextItemKindOther ||
			item.Kind == transcript.CompactedContextItemKindCompactionTrigger {
			return false
		}
	}
	return true
}
