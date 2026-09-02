//revive:disable:file-length-limit
package codex

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/reorienttag"
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

// HasRawResponsesCompactionItem identifies raw requests that cannot project
// compaction state into chat messages but must still reach the raw transport.
func HasRawResponsesCompactionItem(request RawResponsesRequest) bool {
	var body struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(request.Body, &body) != nil {
		return false
	}
	for _, item := range codexstore.NormalizeResponseInputItems(body.Input) {
		if item.Kind == transcript.CompactedContextItemKindCompaction {
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
	mutation   *rawCompactionMutation
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
		mutation:   nil,
	}
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
			return renderRawResponsesCompactionNormalizedItems(normalized[units[start].start:promptIndex])
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

func selectRawCompactionStart(
	units []rawCompactionInterval,
	maxBytes int,
	targetCount int,
	render func(int) (string, bool),
) (int, string, bool) {
	if targetCount < 1 || targetCount > len(units) {
		return 0, "", false
	}
	minimumStart := len(units) - targetCount
	maximumStart := len(units) - 1
	selectedStart := -1
	selectedTranscript := ""
	for minimumStart <= maximumStart {
		candidateStart := minimumStart + (maximumStart-minimumStart)/2
		candidateTranscript, ok := render(candidateStart)
		if !ok || strings.TrimSpace(candidateTranscript) == "" {
			return 0, "", false
		}
		if maxBytes > 0 && len(candidateTranscript) > maxBytes {
			minimumStart = candidateStart + 1
			continue
		}
		selectedStart = candidateStart
		selectedTranscript = candidateTranscript
		maximumStart = candidateStart - 1
	}
	if selectedStart < 0 {
		return 0, "", false
	}
	return selectedStart, selectedTranscript, true
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
		if item.FunctionCallOutput == nil {
			return "", "", false
		}
		return item.FunctionCallOutput.CallID, rawCompactionCallFunction, true
	case transcript.CompactedContextItemKindCustomToolCallOutput:
		if item.CustomToolCallOutput == nil {
			return "", "", false
		}
		return item.CustomToolCallOutput.CallID, rawCompactionCallCustom, true
	case transcript.CompactedContextItemKindToolSearchOutput:
		if item.ToolSearchOutput == nil {
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
		if item.FunctionCallOutput == nil || item.FunctionCallOutput.CallID == "" {
			return empty, "", "", false, "", "", false
		}
		return empty, "", "", true, item.FunctionCallOutput.CallID, rawCompactionCallFunction, true
	case transcript.CompactedContextItemKindCustomToolCallOutput:
		if item.CustomToolCallOutput == nil || item.CustomToolCallOutput.CallID == "" {
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

// TransformResponse appends the removed transcript to one successful response.
// Upstream failures and response-shape failures retain their original bytes.
func (t *RawResponsesCompactionTransformer) TransformResponse(response *http.Response) *http.Response {
	if t == nil || response == nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return response
	}
	wrapped := wrappedRawCompactionTranscript(t.transcript)
	encoding, encoded := rawCompactionResponseContentEncoding(response.Header.Get("Content-Encoding"))
	if encoded {
		return t.transformEncodedResponse(response, wrapped, encoding)
	}
	if t.stream || strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		clone := *response
		clone.Header = response.Header.Clone()
		clone.Header.Del("Content-Length")
		clone.ContentLength = -1
		clone.Body = newRawCompactionSSEBody(response.Body, wrapped, t.markMutated)
		return &clone
	}
	originalBody := response.Body
	body, err := io.ReadAll(originalBody)
	if err != nil {
		response.Body = &rawCompactionReadCloser{reader: io.MultiReader(bytes.NewReader(body), originalBody), closer: originalBody}
		return response
	}
	_ = originalBody.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	transformed, ok := appendRawCompactionJSON(body, wrapped)
	if !ok || bytes.Equal(transformed, body) {
		return response
	}
	t.markMutated()
	clone := *response
	clone.Header = response.Header.Clone()
	clone.Header.Del("Content-Length")
	clone.ContentLength = -1
	clone.Body = io.NopCloser(bytes.NewReader(transformed))
	return &clone
}

func (t *RawResponsesCompactionTransformer) transformEncodedResponse(
	response *http.Response,
	transcriptText string,
	encoding rawCompactionContentEncoding,
) *http.Response {
	streamingResponse := t.stream || strings.Contains(
		strings.ToLower(response.Header.Get("Content-Type")),
		"text/event-stream",
	)
	if streamingResponse {
		decoded, ok := newRawCompactionDecodedStream(response, encoding)
		if !ok {
			return response
		}
		clone := *response
		clone.Header = response.Header.Clone()
		clone.Header.Del("Content-Length")
		clone.ContentLength = -1
		clone.Body = newRawCompactionEncodedBody(
			newRawCompactionSSEBody(decoded, transcriptText),
			encoding,
		)
		return &clone
	}

	originalBody := response.Body
	wireBody, readErr := io.ReadAll(originalBody)
	if readErr != nil {
		response.Body = &rawCompactionReadCloser{reader: io.MultiReader(bytes.NewReader(wireBody), originalBody), closer: originalBody}
		return response
	}
	_ = originalBody.Close()
	decodedBody, ok := decodeRawCompactionBody(wireBody, encoding)
	if !ok {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	transformed, ok := appendRawCompactionJSON(decodedBody, transcriptText)
	if !ok || bytes.Equal(transformed, decodedBody) {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	encodedBody, ok := encodeRawCompactionBody(transformed, encoding)
	if !ok {
		response.Body = io.NopCloser(bytes.NewReader(wireBody))
		return response
	}
	t.markMutated()
	clone := *response
	clone.Header = response.Header.Clone()
	clone.Header.Del("Content-Length")
	clone.ContentLength = -1
	clone.Body = io.NopCloser(bytes.NewReader(encodedBody))
	return &clone
}

func rawCompactionResponseContentEncoding(value string) (rawCompactionContentEncoding, bool) {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case string(rawCompactionContentEncodingGzip):
		return rawCompactionContentEncodingGzip, true
	case string(rawCompactionContentEncodingBrotli):
		return rawCompactionContentEncodingBrotli, true
	default:
		return "", false
	}
}

func decodeRawCompactionBody(body []byte, encoding rawCompactionContentEncoding) ([]byte, bool) {
	var reader io.Reader
	switch encoding {
	case rawCompactionContentEncodingGzip:
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false
		}
		defer func() { _ = gzipReader.Close() }()
		reader = gzipReader
	case rawCompactionContentEncodingBrotli:
		reader = brotli.NewReader(bytes.NewReader(body))
	default:
		return nil, false
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return nil, false
	}
	return decoded, true
}

func encodeRawCompactionBody(body []byte, encoding rawCompactionContentEncoding) ([]byte, bool) {
	var buffer bytes.Buffer
	writer, ok := newRawCompactionEncodingWriter(&buffer, encoding)
	if !ok {
		return nil, false
	}
	if _, err := writer.Write(body); err != nil {
		_ = writer.Close()
		return nil, false
	}
	if err := writer.Close(); err != nil {
		return nil, false
	}
	return buffer.Bytes(), true
}

func newRawCompactionDecodedStream(
	response *http.Response,
	encoding rawCompactionContentEncoding,
) (io.ReadCloser, bool) {
	switch encoding {
	case rawCompactionContentEncodingGzip:
		buffered := bufio.NewReader(response.Body)
		gzipReader, err := gzip.NewReader(buffered)
		if err != nil {
			response.Body = &rawCompactionReadCloser{reader: buffered, closer: response.Body}
			return nil, false
		}
		return &rawCompactionMultiCloser{
			Reader:  gzipReader,
			closers: []io.Closer{gzipReader, response.Body},
		}, true
	case rawCompactionContentEncodingBrotli:
		return &rawCompactionReadCloser{reader: brotli.NewReader(response.Body), closer: response.Body}, true
	default:
		return nil, false
	}
}

type rawCompactionReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func (b *rawCompactionReadCloser) Read(destination []byte) (int, error) {
	count, err := b.reader.Read(destination)
	if errors.Is(err, io.EOF) {
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read raw compaction response: %w", err)
	}
	return count, nil
}

func (b *rawCompactionReadCloser) Close() error {
	if err := b.closer.Close(); err != nil {
		slog.Warn("adapter.codex.raw_compaction.close_failed", "concern", "adapter.providers.codex.request", "err", err)
		return fmt.Errorf("close raw compaction response: %w", err)
	}
	return nil
}

type rawCompactionMultiCloser struct {
	io.Reader
	closers []io.Closer
}

func (b *rawCompactionMultiCloser) Close() error {
	var firstErr error
	for _, closer := range b.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type rawCompactionEncodedBody struct {
	reader *io.PipeReader
	source io.ReadCloser
}

func newRawCompactionEncodedBody(
	source io.ReadCloser,
	encoding rawCompactionContentEncoding,
) io.ReadCloser {
	reader, pipeWriter := io.Pipe()
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicErr := fmt.Errorf("encode raw compaction response: %v", recovered)
				slog.Error("adapter.codex.raw_compaction.encode_panic", "concern", "adapter.providers.codex.request", "err", panicErr)
				_ = pipeWriter.CloseWithError(panicErr)
			}
		}()
		defer func() { _ = source.Close() }()
		writer, ok := newRawCompactionEncodingWriter(pipeWriter, encoding)
		if !ok {
			_ = pipeWriter.CloseWithError(errors.New("unsupported raw compaction response encoding"))
			return
		}
		_, copyErr := io.Copy(writer, source)
		closeErr := writer.Close()
		if copyErr != nil {
			_ = pipeWriter.CloseWithError(copyErr)
			return
		}
		_ = pipeWriter.CloseWithError(closeErr)
	}()
	return &rawCompactionEncodedBody{reader: reader, source: source}
}

func (b *rawCompactionEncodedBody) Read(destination []byte) (int, error) {
	count, err := b.reader.Read(destination)
	if errors.Is(err, io.EOF) {
		return count, io.EOF
	}
	if err != nil {
		return count, fmt.Errorf("read encoded raw compaction response: %w", err)
	}
	return count, nil
}

func (b *rawCompactionEncodedBody) Close() error {
	readerErr := b.reader.Close()
	sourceErr := b.source.Close()
	if sourceErr != nil {
		slog.Warn("adapter.codex.raw_compaction.source_close_failed", "concern", "adapter.providers.codex.request", "err", sourceErr)
		return fmt.Errorf("close raw compaction response source: %w", sourceErr)
	}
	if readerErr != nil {
		slog.Warn("adapter.codex.raw_compaction.reader_close_failed", "concern", "adapter.providers.codex.request", "err", readerErr)
		return fmt.Errorf("close encoded raw compaction response: %w", readerErr)
	}
	return nil
}

func newRawCompactionEncodingWriter(
	writer io.Writer,
	encoding rawCompactionContentEncoding,
) (io.WriteCloser, bool) {
	switch encoding {
	case rawCompactionContentEncodingGzip:
		return gzip.NewWriter(writer), true
	case rawCompactionContentEncodingBrotli:
		return brotli.NewWriter(writer), true
	default:
		return nil, false
	}
}

func wrappedRawCompactionTranscript(content string) string {
	return "\n\n" + reorienttag.PreCompactionTranscriptOpen + "\n" + content + "\n" + reorienttag.PreCompactionTranscriptClose + "\n"
}

func appendRawCompactionJSON(body []byte, transcriptText string) ([]byte, bool) {
	outputStart, outputEnd, ok := jsonObjectFieldValueRange(body, "output")
	if !ok {
		return body, false
	}
	ranges, ok := jsonArrayValueRanges(body[outputStart:outputEnd])
	if !ok {
		return body, false
	}
	for index := range slices.Backward(ranges) {
		itemStart := outputStart + ranges[index].start
		itemEnd := outputStart + ranges[index].end
		mutated, matched, valid := appendRawCompactionAssistantItem(body[itemStart:itemEnd], transcriptText)
		if !valid {
			return body, false
		}
		if !matched {
			continue
		}
		if bytes.Equal(mutated, body[itemStart:itemEnd]) {
			return body, true
		}
		return replaceByteRange(body, itemStart, itemEnd, mutated), true
	}
	return body, false
}

func appendRawCompactionAssistantItem(
	item []byte,
	transcriptText string,
) ([]byte, bool, bool) {
	var identity struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if json.Unmarshal(item, &identity) != nil {
		return item, false, false
	}
	if identity.Type != "message" || identity.Role != "assistant" {
		return item, false, true
	}
	contentStart, contentEnd, hasContent := jsonObjectFieldValueRange(item, "content")
	if !hasContent {
		return appendRawCompactionAssistantContentPart(item, 0, 0, false, transcriptText)
	}
	contentRanges, ok := jsonArrayValueRanges(item[contentStart:contentEnd])
	if !ok {
		return item, false, false
	}
	var target rawCompactionInterval
	hasTarget := false
	for index := range slices.Backward(contentRanges) {
		partStart := contentStart + contentRanges[index].start
		partEnd := contentStart + contentRanges[index].end
		part := item[partStart:partEnd]
		var contentIdentity struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(part, &contentIdentity) != nil {
			return item, false, false
		}
		if contentIdentity.Type != "output_text" {
			continue
		}
		textStart, textEnd, ok := jsonObjectFieldValueRange(part, "text")
		if !ok {
			return item, false, false
		}
		var text string
		if json.Unmarshal(part[textStart:textEnd], &text) != nil {
			return item, false, false
		}
		if strings.Contains(text, transcriptText) || rawCompactionTextHasTranscriptWrapper(text) {
			return item, true, true
		}
		if !hasTarget {
			target = rawCompactionInterval{start: partStart, end: partEnd}
			hasTarget = true
		}
	}
	if !hasTarget {
		return appendRawCompactionAssistantContentPart(item, contentStart, contentEnd, true, transcriptText)
	}
	part := item[target.start:target.end]
	textStart, textEnd, ok := jsonObjectFieldValueRange(part, "text")
	if !ok {
		return item, false, false
	}
	var text string
	if json.Unmarshal(part[textStart:textEnd], &text) != nil {
		return item, false, false
	}
	encodedText, ok := marshalRawCompactionString(text + transcriptText)
	if !ok {
		return item, false, false
	}
	mutatedPart := replaceByteRange(part, textStart, textEnd, encodedText)
	return replaceByteRange(item, target.start, target.end, mutatedPart), true, true
}

func rawCompactionTextHasTranscriptWrapper(text string) bool {
	_, following, found := strings.Cut(text, reorienttag.PreCompactionTranscriptOpen)
	return found && strings.Contains(following, reorienttag.PreCompactionTranscriptClose)
}

func appendRawCompactionAssistantContentPart(
	item []byte,
	contentStart int,
	contentEnd int,
	hasContent bool,
	transcriptText string,
) ([]byte, bool, bool) {
	encodedText, ok := marshalRawCompactionString(transcriptText)
	if !ok {
		return item, false, false
	}
	part := make([]byte, 0, len(encodedText)+32)
	part = append(part, `{"type":"output_text","text":`...)
	part = append(part, encodedText...)
	part = append(part, '}')
	if hasContent {
		content := item[contentStart:contentEnd]
		if len(content) < 2 || content[0] != '[' || content[len(content)-1] != ']' {
			return item, false, false
		}
		mutatedContent := make([]byte, 0, len(content)+len(part)+1)
		mutatedContent = append(mutatedContent, content[:len(content)-1]...)
		if len(bytes.TrimSpace(content[1:len(content)-1])) > 0 {
			mutatedContent = append(mutatedContent, ',')
		}
		mutatedContent = append(mutatedContent, part...)
		mutatedContent = append(mutatedContent, ']')
		return replaceByteRange(item, contentStart, contentEnd, mutatedContent), true, true
	}
	trimmedItem := bytes.TrimSpace(item)
	if len(trimmedItem) < 2 || trimmedItem[len(trimmedItem)-1] != '}' {
		return item, false, false
	}
	itemEnd := len(item) - len(bytes.TrimRight(item, " \t\r\n"))
	if itemEnd == len(item) {
		itemEnd = len(item)
	}
	insertionIndex := len(item) - itemEnd - 1
	if insertionIndex < 0 {
		return item, false, false
	}
	field := make([]byte, 0, len(part)+11)
	field = append(field, `,"content":[`...)
	field = append(field, part...)
	field = append(field, ']')
	return replaceByteRange(item, insertionIndex, insertionIndex, field), true, true
}

func marshalRawCompactionString(value string) ([]byte, bool) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, false
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), true
}

func marshalRawArray(items []json.RawMessage) ([]byte, error) {
	var buffer bytes.Buffer
	buffer.WriteByte('[')
	for index, item := range items {
		if !json.Valid(item) {
			return nil, errors.New("raw compaction input item is invalid JSON")
		}
		if index > 0 {
			buffer.WriteByte(',')
		}
		buffer.Write(item)
	}
	buffer.WriteByte(']')
	return buffer.Bytes(), nil
}

func jsonObjectFieldValueRange(raw []byte, field string) (int, int, bool) {
	if !json.Valid(raw) {
		return 0, 0, false
	}
	index := skipJSONSpace(raw, 0)
	if index >= len(raw) || raw[index] != '{' {
		return 0, 0, false
	}
	index++
	for {
		index = skipJSONSpace(raw, index)
		if index >= len(raw) || raw[index] == '}' {
			return 0, 0, false
		}
		keyStart := index
		keyEnd, ok := scanJSONStringEnd(raw, keyStart)
		if !ok {
			return 0, 0, false
		}
		key, err := strconv.Unquote(string(raw[keyStart:keyEnd]))
		if err != nil {
			return 0, 0, false
		}
		index = skipJSONSpace(raw, keyEnd)
		if index >= len(raw) || raw[index] != ':' {
			return 0, 0, false
		}
		valueStart := skipJSONSpace(raw, index+1)
		valueEnd, ok := scanJSONValueEnd(raw, valueStart)
		if !ok {
			return 0, 0, false
		}
		if key == field {
			return valueStart, valueEnd, true
		}
		index = skipJSONSpace(raw, valueEnd)
		if index < len(raw) && raw[index] == ',' {
			index++
			continue
		}
		return 0, 0, false
	}
}

func jsonArrayValueRanges(raw []byte) ([]rawCompactionInterval, bool) {
	if !json.Valid(raw) {
		return nil, false
	}
	index := skipJSONSpace(raw, 0)
	if index >= len(raw) || raw[index] != '[' {
		return nil, false
	}
	index++
	ranges := make([]rawCompactionInterval, 0)
	for {
		index = skipJSONSpace(raw, index)
		if index >= len(raw) {
			return nil, false
		}
		if raw[index] == ']' {
			return ranges, true
		}
		valueEnd, ok := scanJSONValueEnd(raw, index)
		if !ok {
			return nil, false
		}
		ranges = append(ranges, rawCompactionInterval{start: index, end: valueEnd})
		index = skipJSONSpace(raw, valueEnd)
		if index < len(raw) && raw[index] == ',' {
			index++
			continue
		}
		if index < len(raw) && raw[index] == ']' {
			return ranges, true
		}
		return nil, false
	}
}

func scanJSONValueEnd(raw []byte, start int) (int, bool) {
	if start >= len(raw) {
		return 0, false
	}
	switch raw[start] {
	case '"':
		return scanJSONStringEnd(raw, start)
	case '{', '[':
		return scanJSONContainerEnd(raw, start)
	default:
		return scanJSONPrimitiveEnd(raw, start)
	}
}

func scanJSONContainerEnd(raw []byte, start int) (int, bool) {
	opening := raw[start]
	closing := byte('}')
	if opening == '[' {
		closing = ']'
	}
	depth := 0
	for index := start; index < len(raw); index++ {
		if raw[index] == '"' {
			stringEnd, ok := scanJSONStringEnd(raw, index)
			if !ok {
				return 0, false
			}
			index = stringEnd - 1
			continue
		}
		if raw[index] == opening {
			depth++
			continue
		}
		if raw[index] != closing {
			continue
		}
		depth--
		if depth == 0 {
			return index + 1, true
		}
	}
	return 0, false
}

func scanJSONPrimitiveEnd(raw []byte, start int) (int, bool) {
	index := start
	for index < len(raw) && !strings.ContainsRune(",}] \t\r\n", rune(raw[index])) {
		index++
	}
	return index, index > start
}

func scanJSONStringEnd(raw []byte, start int) (int, bool) {
	if start >= len(raw) || raw[start] != '"' {
		return 0, false
	}
	escaped := false
	for index := start + 1; index < len(raw); index++ {
		if escaped {
			escaped = false
			continue
		}
		if raw[index] == '\\' {
			escaped = true
			continue
		}
		if raw[index] == '"' {
			return index + 1, true
		}
	}
	return 0, false
}

func skipJSONSpace(raw []byte, index int) int {
	for index < len(raw) {
		switch raw[index] {
		case ' ', '\t', '\r', '\n':
			index++
		default:
			return index
		}
	}
	return index
}

func replaceByteRange(raw []byte, start, end int, replacement []byte) []byte {
	out := make([]byte, 0, len(raw)-(end-start)+len(replacement))
	out = append(out, raw[:start]...)
	out = append(out, replacement...)
	out = append(out, raw[end:]...)
	return out
}
