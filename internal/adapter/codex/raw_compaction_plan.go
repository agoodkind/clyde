package codex

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

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

// RawResponsesCompactionTransformer appends the wrapped injection to the
// successful compaction response.
type RawResponsesCompactionTransformer struct {
	injection         string
	stream            bool
	mutation          *rawCompactionMutation
	strictFinalAnswer bool
}

type rawCompactionContentEncoding string

const (
	rawCompactionContentEncodingGzip   rawCompactionContentEncoding = "gzip"
	rawCompactionContentEncodingBrotli rawCompactionContentEncoding = "br"
)

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

// PrepareRawResponsesCompaction splits only a v1 compaction request. Every
// failure returns the original request and no transformer. A nil splitter
// disables the split.
func PrepareRawResponsesCompaction(
	raw RawResponsesRequest,
	splitter CompactionSplitter,
) (RawResponsesRequest, *RawResponsesCompactionTransformer) {
	if splitter == nil || DetectRawResponsesCompactionProtocol(raw.Header) != RawResponsesCompactionV1 {
		return raw, nil
	}
	split, ok := splitter.Plan(raw.Body)
	if !ok {
		return raw, nil
	}
	transformed := raw
	transformed.Body = split.Forwarded
	return transformed, &RawResponsesCompactionTransformer{
		injection:         split.Injection,
		stream:            raw.Stream,
		mutation:          nil,
		strictFinalAnswer: false,
	}
}

// IsRawResponsesV1CompactionRequest reports whether request carries the exact
// local v1 compaction metadata accepted by PrepareRawResponsesCompaction.
func IsRawResponsesV1CompactionRequest(request RawResponsesRequest) bool {
	return DetectRawResponsesCompactionProtocol(request.Header) == RawResponsesCompactionV1
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
		// Codex answers an aborted tool search with an empty tools array, so
		// an empty array is a complete output.
		if item.ToolSearchOutput == nil || item.ToolSearchOutput.CallID == "" {
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
