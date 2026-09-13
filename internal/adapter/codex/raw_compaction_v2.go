package codex

import (
	"encoding/json"
	"strings"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

const maxRawResponsesCompactionV2RecoveryBytes = 1 * 1024 * 1024

// RawResponsesCompactionV2Layout identifies the setup, transcript, and
// terminal trigger boundaries in a captured v2 request.
type RawResponsesCompactionV2Layout struct {
	SetupEnd        int
	TranscriptStart int
	TriggerIndex    int
}

// RawResponsesCompactionV2Plan carries the bounded transcript recovery data
// and raw request replacement for one valid v2 compaction request.
type RawResponsesCompactionV2Plan struct {
	Request    RawResponsesRequest
	Transcript string
	SessionID  string
}

// PlanRawResponsesCompactionV2 selects a bounded whole-turn transcript tail.
// Every failure preserves the original request by returning false.
func PlanRawResponsesCompactionV2(
	request RawResponsesRequest,
	settings RawResponsesCompactionSettings,
) (RawResponsesCompactionV2Plan, bool) {
	var emptyRequest RawResponsesRequest
	emptyPlan := RawResponsesCompactionV2Plan{
		Request:    emptyRequest,
		Transcript: "",
		SessionID:  "",
	}
	if !settings.Enabled {
		return emptyPlan, false
	}
	layout, ok := ParseRawResponsesCompactionV2(request)
	if !ok {
		return emptyPlan, false
	}
	sessionID, ok := rawResponsesCompactionV2SessionID(request)
	if !ok {
		return emptyPlan, false
	}
	inputStart, inputEnd, ok := jsonObjectFieldValueRange(request.Body, "input")
	if !ok {
		return emptyPlan, false
	}
	var rawItems []json.RawMessage
	if json.Unmarshal(request.Body[inputStart:inputEnd], &rawItems) != nil {
		return emptyPlan, false
	}
	transcriptRawItems := rawItems[layout.TranscriptStart:layout.TriggerIndex]
	transcriptItems := codexstore.NormalizeResponseInputItems(transcriptRawItems)
	completeEnd, ok := rawResponsesCompactionV2CompletePrefixEnd(transcriptItems)
	if !ok {
		return emptyPlan, false
	}
	plan, ok := planRawResponsesCompactionTail(
		transcriptRawItems[:completeEnd],
		transcriptItems[:completeEnd],
		maxRawResponsesCompactionV2RecoveryBytes,
		normalizedRecentFraction(settings.RecentFraction),
	)
	if !ok {
		return emptyPlan, false
	}
	remaining := make([]json.RawMessage, 0, len(rawItems)-(completeEnd-plan.removedStart))
	remaining = append(remaining, rawItems[:layout.TranscriptStart+plan.removedStart]...)
	remaining = append(remaining, rawItems[layout.TranscriptStart+completeEnd:]...)
	encodedInput, err := marshalRawArray(remaining)
	if err != nil {
		return emptyPlan, false
	}
	transformed := request
	transformed.Body = replaceByteRange(request.Body, inputStart, inputEnd, encodedInput)
	return RawResponsesCompactionV2Plan{
		Request:    transformed,
		Transcript: plan.transcript,
		SessionID:  sessionID,
	}, true
}

func planRawResponsesCompactionTail(
	rawItems []json.RawMessage,
	normalizedItems []transcript.CompactedContextItem,
	maxBytes int,
	recentFraction float64,
) (rawCompactionPlan, bool) {
	emptyPlan := rawCompactionPlan{removedStart: 0, promptIndex: 0, transcript: ""}
	if len(rawItems) != len(normalizedItems) || len(rawItems) < 2 ||
		!rawCompactionPairsAreComplete(normalizedItems) {
		return emptyPlan, false
	}
	units := rawCompactionUnits(normalizedItems)
	if len(units) < 2 {
		return emptyPlan, false
	}
	targetCount := int(float64(len(units)) * recentFraction)
	if targetCount < 1 {
		return emptyPlan, false
	}
	if targetCount >= len(units) {
		targetCount = len(units) - 1
	}
	if _, renderable := renderRawResponsesCompactionNormalizedItems(normalizedItems[units[len(units)-targetCount].start:]); !renderable {
		return emptyPlan, false
	}
	selectedCount, selectedTranscript := 0, ""
	for minimumCount, maximumCount := 1, targetCount; minimumCount <= maximumCount; {
		candidateCount := minimumCount + (maximumCount-minimumCount)/2
		candidateTranscript, renderable := renderRawResponsesCompactionNormalizedItems(normalizedItems[units[len(units)-candidateCount].start:])
		if !renderable {
			return emptyPlan, false
		}
		if maxBytes > 0 && len(candidateTranscript) > maxBytes {
			maximumCount = candidateCount - 1
			continue
		}
		selectedCount, selectedTranscript = candidateCount, candidateTranscript
		minimumCount = candidateCount + 1
	}
	if selectedCount == 0 || strings.TrimSpace(selectedTranscript) == "" {
		return emptyPlan, false
	}
	return rawCompactionPlan{
		removedStart: units[len(units)-selectedCount].start,
		promptIndex:  len(rawItems),
		transcript:   selectedTranscript,
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
