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

// RawResponsesCompactionV2Plan stores the wrapped injection and the raw
// request replacement for one valid v2 compaction request.
type RawResponsesCompactionV2Plan struct {
	Request   RawResponsesRequest
	Injection string
	SessionID string
}

// PlanRawResponsesCompactionV2 splits a v2 compaction request through the
// shared splitter. Every failure preserves the original request by returning
// false. A nil splitter disables the split.
func PlanRawResponsesCompactionV2(
	request RawResponsesRequest,
	splitter CompactionSplitter,
) (RawResponsesCompactionV2Plan, bool) {
	var emptyRequest RawResponsesRequest
	emptyPlan := RawResponsesCompactionV2Plan{
		Request:   emptyRequest,
		Injection: "",
		SessionID: "",
	}
	if splitter == nil {
		return emptyPlan, false
	}
	if _, ok := ParseRawResponsesCompactionV2(request); !ok {
		return emptyPlan, false
	}
	sessionID, ok := rawResponsesCompactionV2SessionID(request)
	if !ok {
		return emptyPlan, false
	}
	split, ok := splitter.Plan(request.Body)
	if !ok {
		return emptyPlan, false
	}
	transformed := request
	transformed.Body = split.Forwarded
	return RawResponsesCompactionV2Plan{
		Request:   transformed,
		Injection: split.Injection,
		SessionID: sessionID,
	}, true
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
	if !rawResponsesCompactionV2TranscriptIsKnown(transcriptItems) {
		return emptyLayout, false
	}
	completeEnd, ok := rawResponsesCompactionV2CompletePrefixEnd(transcriptItems)
	if !ok {
		return RawResponsesCompactionV2Layout{
			SetupEnd:        transcriptStart,
			TranscriptStart: transcriptStart,
			TriggerIndex:    triggerIndex,
		}, rawCompactionPairsAreComplete(transcriptItems)
	}
	if !rawResponsesCompactionV2UnfinishedSuffixIsValid(transcriptItems[completeEnd:]) {
		return emptyLayout, false
	}
	return RawResponsesCompactionV2Layout{
		SetupEnd:        transcriptStart,
		TranscriptStart: transcriptStart,
		TriggerIndex:    triggerIndex,
	}, true
}

func rawResponsesCompactionV2CompletePrefixEnd(items []transcript.CompactedContextItem) (int, bool) {
	completeEnd := 0
	for itemIndex, item := range items {
		if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil || item.Message.Role != "assistant" {
			continue
		}
		if item.Message.Phase != "" && item.Message.Phase != "final_answer" {
			continue
		}
		completeEnd = itemIndex + 1
	}
	if completeEnd == 0 || !rawCompactionPairsAreComplete(items[:completeEnd]) {
		return 0, false
	}
	return completeEnd, true
}

func rawResponsesCompactionV2UnfinishedSuffixIsValid(items []transcript.CompactedContextItem) bool {
	calls := make(map[string]rawCompactionCallKind)
	outputs := make(map[string]rawCompactionCallKind)
	for _, item := range items {
		if callID, callKind, ok := rawCompactionCall(item); ok {
			if callID == "" {
				return false
			}
			if _, duplicate := calls[callID]; duplicate {
				return false
			}
			calls[callID] = callKind
		}
		if callID, outputKind, ok := rawCompactionOutput(item); ok {
			if callID == "" {
				return false
			}
			if _, duplicate := outputs[callID]; duplicate {
				return false
			}
			outputs[callID] = outputKind
		}
	}
	for callID, outputKind := range outputs {
		callKind, exists := calls[callID]
		if !exists || !rawCompactionKindsPair(callKind, outputKind) {
			return false
		}
	}
	return true
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

func rawResponsesCompactionV2SessionID(request RawResponsesRequest) (string, bool) {
	var metadata TurnMetadata
	if json.Unmarshal([]byte(request.Header.Get(CodexTurnMetadataHeader)), &metadata) != nil {
		return "", false
	}
	sessionID := strings.TrimSpace(metadata.SessionID)
	return sessionID, sessionID != ""
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
