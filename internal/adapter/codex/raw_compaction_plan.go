package codex

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

const (
	defaultRawCompactionMaxTokens             = 500_000
	defaultRawCompactionContextWindow         = 200_000
	defaultRawCompactionContextWindowFraction = 0.5
	defaultRawCompactionBytesPerToken         = 4
	defaultRawCompactionRecentFraction        = 0.5
)

// RawResponsesCompactionSettings carries the existing reorient controls into
// the native Responses path. ContextWindowTokens is the resolved model budget.
type RawResponsesCompactionSettings struct {
	Enabled                     bool
	ContextWindowTokens         int
	FallbackContextWindowTokens int
	MaxTokens                   int
	ContextWindowFraction       float64
	BytesPerToken               int
	RecentFraction              float64
}

// HasRawResponsesNativeContinuationItem identifies native continuation items
// that generic Responses projection cannot represent without losing context.
func HasRawResponsesNativeContinuationItem(request RawResponsesRequest) bool {
	return hasRawResponsesNativeContinuationItem(
		request,
		transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindFunctionCallOutput,
		transcript.CompactedContextItemKindCustomToolCallOutput,
		transcript.CompactedContextItemKindToolSearchOutput,
	)
}

func hasRawResponsesNativeContinuationItem(request RawResponsesRequest, wanted ...transcript.CompactedContextItemKind) bool {
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(request.Body, &body) != nil {
		return false
	}
	for _, item := range codexstore.NormalizeResponseInputItems(body.Input) {
		if slices.Contains(wanted, item.Kind) {
			return true
		}
	}
	return false
}

// RawResponsesCompactionTransformer appends the removed native transcript to
// the successful compaction response.
type RawResponsesCompactionTransformer struct {
	transcript string
	stream     bool
}

type rawCompactionContentEncoding string

const (
	rawCompactionContentEncodingGzip   rawCompactionContentEncoding = "gzip"
	rawCompactionContentEncodingBrotli rawCompactionContentEncoding = "br"
)

type rawCompactionPlan struct {
	removedStart int
	promptIndex  int
	transcript   string
}

type rawCompactionInterval struct {
	start int
	end   int
}

type rawCompactionCallKind string

type rawCompactionMessageContentType string

const (
	rawCompactionCallFunction   rawCompactionCallKind = "function"
	rawCompactionCallCustom     rawCompactionCallKind = "custom"
	rawCompactionCallLocalShell rawCompactionCallKind = "local_shell"
	rawCompactionCallToolSearch rawCompactionCallKind = "tool_search"

	rawCompactionContentInputText  rawCompactionMessageContentType = "input_text"
	rawCompactionContentOutputText rawCompactionMessageContentType = "output_text"
	rawCompactionContentText       rawCompactionMessageContentType = "text"
)

type rawCompactionCallRef struct {
	index int
	kind  rawCompactionCallKind
}

type rawCompactionToolRef struct {
	messageIndex int
	toolIndex    int
	kind         rawCompactionCallKind
}

// PrepareRawResponsesCompaction trims only a matching local compaction
// request. Every failure returns the original request and no transformer.
func PrepareRawResponsesCompaction(
	raw RawResponsesRequest,
	settings RawResponsesCompactionSettings,
) (RawResponsesRequest, *RawResponsesCompactionTransformer) {
	protocol := DetectRawResponsesCompactionProtocol(raw.Header)
	if protocol == RawResponsesCompactionV2 {
		return raw, nil
	}
	if !settings.Enabled || protocol != RawResponsesCompactionV1 {
		return raw, nil
	}
	inputStart, inputEnd, ok := jsonObjectFieldValueRange(raw.Body, "input")
	if !ok {
		return raw, nil
	}
	var input []json.RawMessage
	if json.Unmarshal(raw.Body[inputStart:inputEnd], &input) != nil {
		return raw, nil
	}
	maxBytes := rawCompactionMaxBytes(settings)
	plan, ok := planRawResponsesCompaction(input, maxBytes, normalizedRecentFraction(settings.RecentFraction))
	if !ok {
		return raw, nil
	}
	trimmedInput := make([]json.RawMessage, 0, plan.removedStart+1)
	trimmedInput = append(trimmedInput, input[:plan.removedStart]...)
	trimmedInput = append(trimmedInput, input[plan.promptIndex])
	encodedInput, err := marshalRawArray(trimmedInput)
	if err != nil {
		return raw, nil
	}
	transformedBody := replaceByteRange(raw.Body, inputStart, inputEnd, encodedInput)
	transformed := raw
	transformed.Body = transformedBody
	return transformed, &RawResponsesCompactionTransformer{
		transcript: plan.transcript,
		stream:     raw.Stream,
	}
}

// IsRawResponsesV1CompactionRequest reports whether request carries the exact
// local v1 compaction metadata accepted by PrepareRawResponsesCompaction.
func IsRawResponsesV1CompactionRequest(request RawResponsesRequest) bool {
	return DetectRawResponsesCompactionProtocol(request.Header) == RawResponsesCompactionV1
}

func rawCompactionMaxBytes(settings RawResponsesCompactionSettings) int {
	contextWindow := settings.ContextWindowTokens
	if contextWindow <= 0 {
		contextWindow = settings.FallbackContextWindowTokens
	}
	if contextWindow <= 0 {
		contextWindow = defaultRawCompactionContextWindow
	}
	maxTokens := settings.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultRawCompactionMaxTokens
	}
	contextFraction := settings.ContextWindowFraction
	if contextFraction <= 0 {
		contextFraction = defaultRawCompactionContextWindowFraction
	}
	bytesPerToken := settings.BytesPerToken
	if bytesPerToken <= 0 {
		bytesPerToken = defaultRawCompactionBytesPerToken
	}
	windowTokens := int(float64(contextWindow) * contextFraction)
	return min(maxTokens, windowTokens) * bytesPerToken
}

func normalizedRecentFraction(fraction float64) float64 {
	if fraction <= 0 {
		return defaultRawCompactionRecentFraction
	}
	return fraction
}

func planRawResponsesCompaction(
	items []json.RawMessage,
	maxBytes int,
	recentFraction float64,
) (rawCompactionPlan, bool) {
	emptyPlan := rawCompactionPlan{removedStart: 0, promptIndex: 0, transcript: ""}
	if len(items) < 3 {
		return emptyPlan, false
	}
	normalized := codexstore.NormalizeResponseInputItems(items)
	promptIndex := len(items) - 1
	if !rawCompactionPromptIsValid(normalized[promptIndex]) ||
		!rawCompactionPairsAreComplete(normalized[:promptIndex]) {
		return emptyPlan, false
	}
	units := rawCompactionUnits(normalized[:promptIndex])
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
	selectedStart, rendered, ok := selectRawCompactionStart(
		units,
		maxBytes,
		targetCount,
		func(start int) (string, bool) {
			return renderRawResponsesCompactionNormalizedItems(normalized[start:promptIndex])
		},
	)
	if !ok {
		return emptyPlan, false
	}
	removedStart := units[selectedStart].start
	return rawCompactionPlan{
		removedStart: removedStart,
		promptIndex:  promptIndex,
		transcript:   rendered,
	}, true
}

func rawCompactionPromptIsValid(item transcript.CompactedContextItem) bool {
	if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil || item.Message.Role != "user" {
		return false
	}
	if len(item.Message.Content) == 0 {
		return false
	}
	for _, content := range item.Message.Content {
		if content.Type != "input_text" || strings.TrimSpace(content.Text) == "" {
			return false
		}
	}
	return true
}

func rawCompactionUnits(items []transcript.CompactedContextItem) []rawCompactionInterval {
	intervals := rawCompactionTurnIntervals(items)
	intervals = append(intervals, rawCompactionPairIntervals(items)...)
	merged := mergeRawCompactionIntervals(intervals)
	units := make([]rawCompactionInterval, 0, len(items))
	intervalIndex := 0
	for itemIndex := 0; itemIndex < len(items); {
		if intervalIndex < len(merged) && merged[intervalIndex].start == itemIndex {
			units = append(units, merged[intervalIndex])
			itemIndex = merged[intervalIndex].end
			intervalIndex++
			continue
		}
		units = append(units, rawCompactionInterval{start: itemIndex, end: itemIndex + 1})
		itemIndex++
	}
	return units
}

func rawCompactionTurnIntervals(items []transcript.CompactedContextItem) []rawCompactionInterval {
	intervals := make([]rawCompactionInterval, 0)
	turnStart := 0
	for itemIndex := 1; itemIndex < len(items); itemIndex++ {
		item := items[itemIndex]
		if item.Kind != transcript.CompactedContextItemKindMessage || item.Message == nil || item.Message.Role != "user" {
			continue
		}
		intervals = append(intervals, rawCompactionInterval{start: turnStart, end: itemIndex})
		turnStart = itemIndex
	}
	return append(intervals, rawCompactionInterval{start: turnStart, end: len(items)})
}

func rawCompactionPairIntervals(items []transcript.CompactedContextItem) []rawCompactionInterval {
	calls := make(map[string]rawCompactionCallRef)
	duplicateCalls := make(map[string]bool)
	for index, item := range items {
		callID, kind, ok := rawCompactionCall(item)
		if !ok || callID == "" {
			continue
		}
		if _, exists := calls[callID]; exists {
			duplicateCalls[callID] = true
			continue
		}
		calls[callID] = rawCompactionCallRef{index: index, kind: kind}
	}
	intervals := make([]rawCompactionInterval, 0)
	for index, item := range items {
		callID, outputKind, ok := rawCompactionOutput(item)
		if !ok || callID == "" || duplicateCalls[callID] {
			continue
		}
		call, exists := calls[callID]
		if !exists || !rawCompactionKindsPair(call.kind, outputKind) {
			continue
		}
		start := min(call.index, index)
		end := max(call.index, index) + 1
		intervals = append(intervals, rawCompactionInterval{start: start, end: end})
	}
	return intervals
}

func mergeRawCompactionIntervals(intervals []rawCompactionInterval) []rawCompactionInterval {
	if len(intervals) == 0 {
		return nil
	}
	for i := 1; i < len(intervals); i++ {
		for j := i; j > 0 && intervals[j].start < intervals[j-1].start; j-- {
			intervals[j], intervals[j-1] = intervals[j-1], intervals[j]
		}
	}
	merged := make([]rawCompactionInterval, 0, len(intervals))
	for _, interval := range intervals {
		if len(merged) == 0 || interval.start >= merged[len(merged)-1].end {
			merged = append(merged, interval)
			continue
		}
		merged[len(merged)-1].end = max(merged[len(merged)-1].end, interval.end)
	}
	return merged
}

func rawCompactionCall(item transcript.CompactedContextItem) (string, rawCompactionCallKind, bool) {
	switch item.Kind {
	case transcript.CompactedContextItemKindFunctionCall:
		if item.FunctionCall == nil {
			return "", "", false
		}
		return item.FunctionCall.CallID, rawCompactionCallFunction, true
	case transcript.CompactedContextItemKindCustomToolCall:
		if item.CustomToolCall == nil {
			return "", "", false
		}
		return item.CustomToolCall.CallID, rawCompactionCallCustom, true
	case transcript.CompactedContextItemKindLocalShellCall:
		if item.LocalShellCall == nil {
			return "", "", false
		}
		return item.LocalShellCall.CallID, rawCompactionCallLocalShell, true
	case transcript.CompactedContextItemKindToolSearchCall:
		if item.ToolSearchCall == nil {
			return "", "", false
		}
		return item.ToolSearchCall.CallID, rawCompactionCallToolSearch, true
	case transcript.CompactedContextItemKindMessage,
		transcript.CompactedContextItemKindReasoning,
		transcript.CompactedContextItemKindFunctionCallOutput,
		transcript.CompactedContextItemKindCustomToolCallOutput,
		transcript.CompactedContextItemKindToolSearchOutput,
		transcript.CompactedContextItemKindWebSearchCall,
		transcript.CompactedContextItemKindImageGenerationCall,
		transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindCompactionTrigger,
		transcript.CompactedContextItemKindContextCompaction,
		transcript.CompactedContextItemKindOther:
		return "", "", false
	default:
		return "", "", false
	}
}

func rawCompactionOutput(item transcript.CompactedContextItem) (string, rawCompactionCallKind, bool) {
	switch item.Kind {
	case transcript.CompactedContextItemKindFunctionCallOutput:
		if item.FunctionCallOutput == nil || item.FunctionCallOutput.CallID == "" ||
			len(bytes.TrimSpace(item.FunctionCallOutput.OutputRaw)) == 0 ||
			bytes.Equal(bytes.TrimSpace(item.FunctionCallOutput.OutputRaw), []byte("null")) {
			return "", "", false
		}
		return item.FunctionCallOutput.CallID, rawCompactionCallFunction, true
	case transcript.CompactedContextItemKindCustomToolCallOutput:
		if item.CustomToolCallOutput == nil || item.CustomToolCallOutput.CallID == "" ||
			len(bytes.TrimSpace(item.CustomToolCallOutput.OutputRaw)) == 0 ||
			bytes.Equal(bytes.TrimSpace(item.CustomToolCallOutput.OutputRaw), []byte("null")) {
			return "", "", false
		}
		return item.CustomToolCallOutput.CallID, rawCompactionCallCustom, true
	case transcript.CompactedContextItemKindToolSearchOutput:
		if item.ToolSearchOutput == nil {
			return "", "", false
		}
		if len(item.ToolSearchOutput.ToolsRaw) == 0 {
			return "", "", false
		}
		return item.ToolSearchOutput.CallID, rawCompactionCallToolSearch, true
	case transcript.CompactedContextItemKindMessage,
		transcript.CompactedContextItemKindReasoning,
		transcript.CompactedContextItemKindLocalShellCall,
		transcript.CompactedContextItemKindFunctionCall,
		transcript.CompactedContextItemKindToolSearchCall,
		transcript.CompactedContextItemKindCustomToolCall,
		transcript.CompactedContextItemKindWebSearchCall,
		transcript.CompactedContextItemKindImageGenerationCall,
		transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindCompactionTrigger,
		transcript.CompactedContextItemKindContextCompaction,
		transcript.CompactedContextItemKindOther:
		return "", "", false
	default:
		return "", "", false
	}
}

func rawCompactionKindsPair(callKind, outputKind rawCompactionCallKind) bool {
	if outputKind == rawCompactionCallFunction {
		return callKind == rawCompactionCallFunction || callKind == rawCompactionCallLocalShell
	}
	return callKind == outputKind
}

func rawCompactionPairsAreComplete(items []transcript.CompactedContextItem) bool {
	calls := make(map[string]rawCompactionCallKind)
	outputs := make(map[string]rawCompactionCallKind)
	for _, item := range items {
		if callID, callKind, call := rawCompactionCall(item); call {
			if callID == "" {
				return false
			}
			if _, duplicate := calls[callID]; duplicate {
				return false
			}
			calls[callID] = callKind
		}
		if callID, outputKind, output := rawCompactionOutput(item); output {
			if callID == "" {
				return false
			}
			if _, duplicate := outputs[callID]; duplicate {
				return false
			}
			outputs[callID] = outputKind
		}
	}
	if len(calls) != len(outputs) {
		return false
	}
	for callID, callKind := range calls {
		outputKind, exists := outputs[callID]
		if !exists || !rawCompactionKindsPair(callKind, outputKind) {
			return false
		}
	}
	return true
}

func renderRawResponsesCompactionNormalizedItems(items []transcript.CompactedContextItem) (string, bool) {
	if !rawCompactionPairsAreComplete(items) {
		return "", false
	}
	messages := make([]transcript.Message, 0, len(items))
	tools := make(map[string]rawCompactionToolRef)
	for _, item := range items {
		message, callID, callKind, output, outputID, outputKind, ok := rawCompactionTranscriptValue(item)
		if !ok {
			return "", false
		}
		if output {
			tool, exists := tools[outputID]
			if !exists || !rawCompactionKindsPair(tool.kind, outputKind) {
				return "", false
			}
			messages[tool.messageIndex].Tools[tool.toolIndex].Output = rawCompactionOutputText(item)
			continue
		}
		messageIndex := len(messages)
		messages = append(messages, message)
		if callID != "" {
			tools[callID] = rawCompactionToolRef{
				messageIndex: messageIndex,
				toolIndex:    len(message.Tools) - 1,
				kind:         callKind,
			}
		}
	}
	options := transcript.DefaultShapeOptions()
	options.IncludeThinking = true
	options.ToolOnly = transcript.ToolOnlyFullDetail
	rendered := transcript.RenderMarkdownWithOptions(messages, options)
	return rendered, strings.TrimSpace(rendered) != ""
}

func rawCompactionTranscriptValue(
	item transcript.CompactedContextItem,
) (transcript.Message, string, rawCompactionCallKind, bool, string, rawCompactionCallKind, bool) {
	empty := emptyRawCompactionTranscriptMessage()
	switch item.Kind {
	case transcript.CompactedContextItemKindMessage:
		message, ok := rawCompactionMessage(item)
		return message, "", "", false, "", "", ok
	case transcript.CompactedContextItemKindReasoning:
		message, ok := rawCompactionReasoningMessage(item)
		return message, "", "", false, "", "", ok
	case transcript.CompactedContextItemKindFunctionCall:
		if item.FunctionCall == nil || strings.TrimSpace(item.FunctionCall.Name) == "" {
			return empty, "", "", false, "", "", false
		}
		return rawCompactionToolMessage(
			item.FunctionCall.CallID,
			item.FunctionCall.Name,
			item.FunctionCall.Arguments,
		), item.FunctionCall.CallID, rawCompactionCallFunction, false, "", "", true
	case transcript.CompactedContextItemKindCustomToolCall:
		if item.CustomToolCall == nil || strings.TrimSpace(item.CustomToolCall.Name) == "" {
			return empty, "", "", false, "", "", false
		}
		return rawCompactionToolMessage(
			item.CustomToolCall.CallID,
			item.CustomToolCall.Name,
			item.CustomToolCall.Input,
		), item.CustomToolCall.CallID, rawCompactionCallCustom, false, "", "", true
	case transcript.CompactedContextItemKindLocalShellCall:
		if item.LocalShellCall == nil || len(item.LocalShellCall.ActionRaw) == 0 {
			return empty, "", "", false, "", "", false
		}
		return rawCompactionToolMessage(
			item.LocalShellCall.CallID,
			"local_shell",
			string(item.LocalShellCall.ActionRaw),
		), item.LocalShellCall.CallID, rawCompactionCallLocalShell, false, "", "", true
	case transcript.CompactedContextItemKindToolSearchCall:
		if item.ToolSearchCall == nil || len(item.ToolSearchCall.ArgumentsRaw) == 0 {
			return empty, "", "", false, "", "", false
		}
		return rawCompactionToolMessage(
			item.ToolSearchCall.CallID,
			"tool_search",
			string(item.ToolSearchCall.ArgumentsRaw),
		), item.ToolSearchCall.CallID, rawCompactionCallToolSearch, false, "", "", true
	case transcript.CompactedContextItemKindFunctionCallOutput:
		if item.FunctionCallOutput == nil || item.FunctionCallOutput.CallID == "" ||
			len(bytes.TrimSpace(item.FunctionCallOutput.OutputRaw)) == 0 ||
			bytes.Equal(bytes.TrimSpace(item.FunctionCallOutput.OutputRaw), []byte("null")) {
			return empty, "", "", false, "", "", false
		}
		return empty, "", "", true, item.FunctionCallOutput.CallID, rawCompactionCallFunction, true
	case transcript.CompactedContextItemKindCustomToolCallOutput:
		if item.CustomToolCallOutput == nil || item.CustomToolCallOutput.CallID == "" ||
			len(bytes.TrimSpace(item.CustomToolCallOutput.OutputRaw)) == 0 ||
			bytes.Equal(bytes.TrimSpace(item.CustomToolCallOutput.OutputRaw), []byte("null")) {
			return empty, "", "", false, "", "", false
		}
		return empty, "", "", true, item.CustomToolCallOutput.CallID, rawCompactionCallCustom, true
	case transcript.CompactedContextItemKindToolSearchOutput:
		if item.ToolSearchOutput == nil || item.ToolSearchOutput.CallID == "" {
			return empty, "", "", false, "", "", false
		}
		return empty, "", "", true, item.ToolSearchOutput.CallID, rawCompactionCallToolSearch, true
	case transcript.CompactedContextItemKindWebSearchCall,
		transcript.CompactedContextItemKindImageGenerationCall,
		transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindCompactionTrigger,
		transcript.CompactedContextItemKindContextCompaction,
		transcript.CompactedContextItemKindOther:
		return empty, "", "", false, "", "", false
	default:
		return empty, "", "", false, "", "", false
	}
}

func rawCompactionMessage(item transcript.CompactedContextItem) (transcript.Message, bool) {
	if item.Message == nil || (item.Message.Role != "user" && item.Message.Role != "assistant" && item.Message.Role != "developer") {
		return emptyRawCompactionTranscriptMessage(), false
	}
	text := make([]string, 0, len(item.Message.Content))
	for _, content := range item.Message.Content {
		switch rawCompactionMessageContentType(content.Type) {
		case rawCompactionContentInputText,
			rawCompactionContentOutputText,
			rawCompactionContentText:
			if strings.TrimSpace(content.Text) != "" {
				text = append(text, content.Text)
			}
		default:
			return emptyRawCompactionTranscriptMessage(), false
		}
	}
	if len(text) == 0 {
		return emptyRawCompactionTranscriptMessage(), false
	}
	message := emptyRawCompactionTranscriptMessage()
	message.Role = item.Message.Role
	message.Text = strings.Join(text, "\n")
	return message, true
}

func rawCompactionReasoningMessage(item transcript.CompactedContextItem) (transcript.Message, bool) {
	if item.Reasoning == nil {
		return emptyRawCompactionTranscriptMessage(), false
	}
	text := make([]string, 0, len(item.Reasoning.Summary))
	for _, summary := range item.Reasoning.Summary {
		if summary.Type != "summary_text" && summary.Type != "text" {
			return emptyRawCompactionTranscriptMessage(), false
		}
		if strings.TrimSpace(summary.Text) != "" {
			text = append(text, summary.Text)
		}
	}
	if len(text) == 0 {
		return emptyRawCompactionTranscriptMessage(), false
	}
	message := emptyRawCompactionTranscriptMessage()
	message.Role = "assistant"
	message.Thinking = strings.Join(text, "\n")
	return message, true
}

func rawCompactionToolMessage(callID, name, input string) transcript.Message {
	message := emptyRawCompactionTranscriptMessage()
	message.Role = "assistant"
	message.HasTools = true
	message.Tools = []transcript.ToolCall{{
		ID:          callID,
		Name:        name,
		Input:       rawCompactionToolInput(input),
		Display:     strings.TrimSpace(input),
		DisplayLang: "",
		Output:      "",
		IsError:     false,
		Attachments: nil,
	}}
	return message
}

func rawCompactionToolInput(input string) transcript.ToolInputJSON {
	trimmed := strings.TrimSpace(input)
	if json.Valid([]byte(trimmed)) {
		return transcript.ToolInputJSON{Raw: json.RawMessage(trimmed)}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return transcript.ToolInputJSON{Raw: nil}
	}
	return transcript.ToolInputJSON{Raw: encoded}
}

func rawCompactionOutputText(item transcript.CompactedContextItem) string {
	var raw json.RawMessage
	switch item.Kind {
	case transcript.CompactedContextItemKindFunctionCallOutput:
		raw = item.FunctionCallOutput.OutputRaw
	case transcript.CompactedContextItemKindCustomToolCallOutput:
		raw = item.CustomToolCallOutput.OutputRaw
	case transcript.CompactedContextItemKindToolSearchOutput:
		encoded, err := json.Marshal(item.ToolSearchOutput.ToolsRaw)
		if err != nil {
			return ""
		}
		raw = encoded
	case transcript.CompactedContextItemKindMessage,
		transcript.CompactedContextItemKindReasoning,
		transcript.CompactedContextItemKindLocalShellCall,
		transcript.CompactedContextItemKindFunctionCall,
		transcript.CompactedContextItemKindToolSearchCall,
		transcript.CompactedContextItemKindCustomToolCall,
		transcript.CompactedContextItemKindWebSearchCall,
		transcript.CompactedContextItemKindImageGenerationCall,
		transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindCompactionTrigger,
		transcript.CompactedContextItemKindContextCompaction,
		transcript.CompactedContextItemKindOther:
		return ""
	default:
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func emptyRawCompactionTranscriptMessage() transcript.Message {
	return transcript.Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "",
		Visibility:        transcript.MessageVisibilityVisible,
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              "",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
		Attachments:       nil,
	}
}
