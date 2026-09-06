package codex

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

// RawResponsesCompactionV2Recovery carries private state for Task 5.
type RawResponsesCompactionV2Recovery struct {
	transcript string
	complete   func() bool
	release    func()
}

// CompleteRecovery removes the matched recovery after Task 5 persists it.
func (r *RawResponsesCompactionV2Recovery) CompleteRecovery() bool {
	if r == nil || r.complete == nil {
		return false
	}
	return r.complete()
}

// ReleaseRecovery makes a recovery reservation available after persistence fails.
func (r *RawResponsesCompactionV2Recovery) ReleaseRecovery() bool {
	if r == nil || r.release == nil {
		return false
	}
	r.release()
	return true
}

// InjectRawResponsesCompactionV2Recovery adds an armed transcript after its
// matching encrypted compaction item in a regular native request.
func InjectRawResponsesCompactionV2Recovery(
	request RawResponsesRequest,
	registry *RawResponsesCompactionV2Registry,
) (RawResponsesRequest, *RawResponsesCompactionV2Recovery, bool) {
	var trace rawResponsesCompactionV2InjectionTrace
	defer func() { trace.log() }()
	protocol := DetectRawResponsesCompactionProtocol(request.Header)
	trace.detectedProtocol = string(protocol)
	trace.protocolRegular = protocol == RawResponsesCompactionNone
	if !trace.protocolRegular {
		return request, nil, false
	}
	trace.metadataRequestKindTurn, trace.metadataImplementationEmpty, trace.metadataStrategyEmpty, trace.metadataPhaseFinalAnswer = rawResponsesCompactionV2RegularFinalAnswerFields(request.Header)
	trace.metadataRegular = rawResponsesCompactionV2RegularTurn(request.Header)
	if !trace.metadataRegular {
		return request, nil, false
	}
	trace.registryPresent = registry != nil
	if !trace.registryPresent {
		return request, nil, false
	}
	sessionID, ok := rawResponsesCompactionV2SessionID(request)
	trace.sessionPresent = ok
	if !ok {
		return request, nil, false
	}
	inputStart, inputEnd, ok := jsonObjectFieldValueRange(request.Body, "input")
	trace.inputRangeValid = ok
	if !ok {
		return request, nil, false
	}
	var input []json.RawMessage
	trace.inputJSONValid = json.Unmarshal(request.Body[inputStart:inputEnd], &input) == nil
	if !trace.inputJSONValid {
		return request, nil, false
	}
	trace.transcriptTagAbsent = !rawResponsesCompactionV2InputHasTranscriptTag(input)
	if !trace.transcriptTagAbsent {
		return request, nil, false
	}
	compactionIndex, encryptedContent, normalizerValid, compactionItemCount, ok := rawResponsesCompactionV2InjectedCompactionTrace(input)
	trace.normalizerValid = normalizerValid
	trace.compactionItemCount = compactionItemCount
	trace.singleCompactionItem = ok
	if !ok {
		return request, nil, false
	}
	_, trace.registryMatchBeforeReserve = registry.Match(sessionID, encryptedContent)
	transcript, generation, ok := registry.Reserve(sessionID, encryptedContent)
	trace.registryReserved = ok
	if !ok {
		return request, nil, false
	}
	item, err := json.Marshal(struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "message",
		Role: "assistant",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "output_text", Text: wrappedRawCompactionTranscript(transcript)}},
	})
	if err != nil {
		registry.Release(sessionID, encryptedContent, generation)
		return request, nil, false
	}
	mutatedInput := make([]json.RawMessage, 0, len(input)+1)
	mutatedInput = append(mutatedInput, input[:compactionIndex+1]...)
	mutatedInput = append(mutatedInput, item)
	mutatedInput = append(mutatedInput, input[compactionIndex+1:]...)
	encodedInput, err := marshalRawArray(mutatedInput)
	if err != nil {
		registry.Release(sessionID, encryptedContent, generation)
		return request, nil, false
	}
	transformed := request
	transformed.Body = replaceByteRange(request.Body, inputStart, inputEnd, encodedInput)
	trace.rawInsertionSucceeded = true
	return transformed, &RawResponsesCompactionV2Recovery{
		transcript: transcript,
		complete:   rawResponsesCompactionV2Completion(registry, sessionID, encryptedContent, generation),
		release:    rawResponsesCompactionV2Release(registry, sessionID, encryptedContent, generation),
	}, true
}

func rawResponsesCompactionV2InputHasTranscriptTag(input []json.RawMessage) bool {
	normalized := codexstore.NormalizeResponseInputItems(input)
	if len(normalized) != len(input) {
		return false
	}
	for _, item := range normalized {
		if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil || item.Message.Role != "assistant" {
			continue
		}
		for _, content := range item.Message.Content {
			openIndex := strings.Index(content.Text, "<pre-compaction-transcript>")
			if openIndex >= 0 && strings.Contains(content.Text[openIndex+len("<pre-compaction-transcript>"):], "</pre-compaction-transcript>") {
				return true
			}
		}
	}
	return false
}

type rawResponsesCompactionV2InjectionTrace struct {
	detectedProtocol            string
	protocolRegular             bool
	metadataRegular             bool
	metadataRequestKindTurn     bool
	metadataImplementationEmpty bool
	metadataStrategyEmpty       bool
	metadataPhaseFinalAnswer    bool
	registryPresent             bool
	transcriptTagAbsent         bool
	sessionPresent              bool
	inputRangeValid             bool
	inputJSONValid              bool
	normalizerValid             bool
	compactionItemCount         int
	singleCompactionItem        bool
	registryReserved            bool
	registryMatchBeforeReserve  bool
	rawInsertionSucceeded       bool
}

func (trace rawResponsesCompactionV2InjectionTrace) log() {
	slog.Debug("adapter.codex.raw_compaction_v2_recovery_injection", "detected_protocol", trace.detectedProtocol, "protocol_regular", trace.protocolRegular, "metadata_regular", trace.metadataRegular, "metadata_request_kind_turn", trace.metadataRequestKindTurn, "metadata_implementation_empty", trace.metadataImplementationEmpty, "metadata_strategy_empty", trace.metadataStrategyEmpty, "metadata_phase_final_answer", trace.metadataPhaseFinalAnswer, "registry_present", trace.registryPresent, "transcript_tag_absent", trace.transcriptTagAbsent, "session_present", trace.sessionPresent, "input_range_valid", trace.inputRangeValid, "input_json_valid", trace.inputJSONValid, "normalizer_valid", trace.normalizerValid, "compaction_item_count", trace.compactionItemCount, "single_compaction_item", trace.singleCompactionItem, "registry_reserved", trace.registryReserved, "registry_match_before_reserve", trace.registryMatchBeforeReserve, "raw_insertion_succeeded", trace.rawInsertionSucceeded)
}

func rawResponsesCompactionV2RegularFinalAnswerFields(header http.Header) (bool, bool, bool, bool) {
	var metadata rawResponsesCompactionMetadata
	if json.Unmarshal([]byte(header.Get(CodexTurnMetadataHeader)), &metadata) != nil {
		return false, false, false, false
	}
	return metadata.RequestKind == "turn", metadata.Compaction.Implementation == "", metadata.Compaction.Strategy == "", metadata.Compaction.Phase == "final_answer"
}

func rawResponsesCompactionV2Release(registry *RawResponsesCompactionV2Registry, sessionID, encryptedContent string, generation uint64) func() {
	var once sync.Once
	return func() { once.Do(func() { registry.Release(sessionID, encryptedContent, generation) }) }
}

func rawResponsesCompactionV2Completion(registry *RawResponsesCompactionV2Registry, sessionID, encryptedContent string, generation uint64) func() bool {
	var once sync.Once
	return func() bool {
		completed := false
		once.Do(func() { completed = registry.Complete(sessionID, encryptedContent, generation) })
		return completed
	}
}

func rawResponsesCompactionV2InjectedCompactionTrace(input []json.RawMessage) (int, string, bool, int, bool) {
	normalized := codexstore.NormalizeResponseInputItems(input)
	if len(normalized) != len(input) {
		return 0, "", false, 0, false
	}
	index := -1
	encryptedContent := ""
	compactionItemCount := 0
	for itemIndex, item := range normalized {
		if item.Kind != transcript.CompactedContextItemKindCompaction || item.Compaction == nil {
			continue
		}
		compactionItemCount++
		candidate := strings.TrimSpace(item.Compaction.EncryptedContent)
		if index >= 0 || candidate == "" {
			return 0, "", true, compactionItemCount, false
		}
		index = itemIndex
		encryptedContent = item.Compaction.EncryptedContent
	}
	return index, encryptedContent, true, compactionItemCount, index >= 0
}
