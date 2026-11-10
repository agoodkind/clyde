package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

// abortedCompactionOutputText is the output text of a synthetic output item.
// Codex writes the same text after an abort.
const abortedCompactionOutputText = "aborted"

var (
	errCompactionInputMissing    = errors.New("compaction request has no input array")
	errCompactionCutOutOfRange   = errors.New("compaction cut is out of range")
	errCompactionCallKindUnknown = errors.New("compaction call kind is unknown")
)

// truncateCompactionError logs one truncation failure and returns it wrapped
// with the operation. The caller forwards the original request unmodified
// after a failure, and this log is the only record of the reason.
func truncateCompactionError(operation string, err error) error {
	slog.Warn("adapter.codex.compaction_truncate_failed",
		"concern", "adapter.providers.codex.request",
		"component", "adapter",
		"subcomponent", "codex",
		"operation", operation,
		"err", err,
	)
	return fmt.Errorf("%s: %w", operation, err)
}

// abortedCallOutput is the synthetic function_call_output or
// custom_tool_call_output item. Type selects between the two.
type abortedCallOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// abortedToolSearchOutput answers an aborted tool search call. Tools
// serializes as an empty array, never as null.
type abortedToolSearchOutput struct {
	Type   string            `json:"type"`
	CallID string            `json:"call_id"`
	Tools  []json.RawMessage `json:"tools"`
}

// TruncateCompactionInput rewrites the input array of body to end the counted
// transcript at the cut. The result contains every item before the boundary
// item, the boundary item shortened at the cut, one synthetic output per
// unanswered call in the counted range, and every item from instructionStart
// onward. Every byte outside the input array stays unchanged.
func TruncateCompactionInput(
	body []byte,
	cut CompactionCut,
	retainStart, instructionStart int,
) ([]byte, error) {
	inputStart, inputEnd, ok := jsonObjectFieldValueRange(body, "input")
	if !ok {
		return nil, truncateCompactionError("locate compaction input", errCompactionInputMissing)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(body[inputStart:inputEnd], &items); err != nil {
		return nil, truncateCompactionError("decode compaction input", err)
	}
	if err := validateCompactionCut(cut, retainStart, instructionStart, len(items)); err != nil {
		return nil, err
	}

	kept := make([]json.RawMessage, 0, len(items))
	kept = append(kept, items[:cut.MessageIndex]...)

	boundary, err := shortenBoundaryItem(items[cut.MessageIndex], cut)
	if err != nil {
		return nil, err
	}
	if boundary != nil {
		kept = append(kept, boundary)
	}

	kept = dropTrailingReasoning(kept, retainStart)

	synthetic, err := syntheticCompactionOutputs(kept[retainStart:])
	if err != nil {
		return nil, err
	}
	kept = append(kept, synthetic...)
	kept = append(kept, items[instructionStart:]...)

	encoded, err := marshalRawArray(kept)
	if err != nil {
		return nil, truncateCompactionError("encode truncated compaction input", err)
	}
	return replaceByteRange(body, inputStart, inputEnd, encoded), nil
}

func validateCompactionCut(cut CompactionCut, retainStart, instructionStart, count int) error {
	retainValid := retainStart >= 0 && retainStart <= cut.MessageIndex && cut.MessageIndex < count
	instructionValid := instructionStart > cut.MessageIndex && instructionStart <= count
	if retainValid && instructionValid {
		return nil
	}
	operation := fmt.Sprintf(
		"validate cut message index %d, retain start %d, instruction start %d against %d items",
		cut.MessageIndex, retainStart, instructionStart, count,
	)
	return truncateCompactionError(operation, errCompactionCutOutOfRange)
}

// shortenBoundaryItem returns the boundary item shortened at the cut, or nil
// when nothing of it remains. A message and a string output shorten at a rune
// boundary. Every other item is atomic.
func shortenBoundaryItem(raw json.RawMessage, cut CompactionCut) (json.RawMessage, error) {
	kind, err := rawCompactionItemKind(raw)
	if err != nil {
		return nil, err
	}
	switch kind {
	case transcript.CompactedContextItemKindMessage:
		return shortenBoundaryMessage(raw, cut.SegmentIndex, cut.HeadRunes)
	case transcript.CompactedContextItemKindFunctionCallOutput,
		transcript.CompactedContextItemKindCustomToolCallOutput:
		return shortenBoundaryOutput(raw, cut.HeadRunes)
	case transcript.CompactedContextItemKindReasoning,
		transcript.CompactedContextItemKindLocalShellCall,
		transcript.CompactedContextItemKindFunctionCall,
		transcript.CompactedContextItemKindToolSearchCall,
		transcript.CompactedContextItemKindCustomToolCall,
		transcript.CompactedContextItemKindToolSearchOutput,
		transcript.CompactedContextItemKindWebSearchCall,
		transcript.CompactedContextItemKindImageGenerationCall,
		transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindCompactionTrigger,
		transcript.CompactedContextItemKindContextCompaction,
		transcript.CompactedContextItemKindOther:
		// The segment model counts these kinds as one atomic segment.
	default:
		// An item with no recognized type is atomic too.
	}
	return keepAtomicCompactionItem(raw, cut.HeadRunes), nil
}

func rawCompactionItemKind(raw json.RawMessage) (transcript.CompactedContextItemKind, error) {
	var identity struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		return "", truncateCompactionError("decode input item type", err)
	}
	return transcript.CompactedContextItemKind(identity.Type), nil
}

func keepAtomicCompactionItem(raw json.RawMessage, headRunes int) json.RawMessage {
	if headRunes > 0 {
		return raw
	}
	return nil
}

func decodeCompactionItemFields(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, truncateCompactionError("decode boundary item fields", err)
	}
	return fields, nil
}

func shortenBoundaryMessage(raw json.RawMessage, segmentIndex, headRunes int) (json.RawMessage, error) {
	message, err := decodeCompactionItemFields(raw)
	if err != nil {
		return nil, err
	}
	var parts []map[string]json.RawMessage
	if rawContent, ok := message["content"]; ok {
		if err := json.Unmarshal(rawContent, &parts); err != nil {
			return nil, truncateCompactionError("decode boundary message content", err)
		}
	}
	if segmentIndex < 0 || segmentIndex > len(parts) {
		return nil, truncateCompactionError("validate cut segment index", errCompactionCutOutOfRange)
	}
	kept := make([]map[string]json.RawMessage, 0, segmentIndex+1)
	kept = append(kept, parts[:segmentIndex]...)
	if segmentIndex < len(parts) {
		keep, err := shortenCompactionTextField(parts[segmentIndex], "text", headRunes)
		if err != nil {
			return nil, err
		}
		if keep {
			kept = append(kept, parts[segmentIndex])
		}
	}
	if len(kept) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, truncateCompactionError("encode boundary message content", err)
	}
	message["content"] = encoded
	return marshalCompactionItem(message)
}

func shortenBoundaryOutput(raw json.RawMessage, headRunes int) (json.RawMessage, error) {
	output, err := decodeCompactionItemFields(raw)
	if err != nil {
		return nil, err
	}
	keep, err := shortenCompactionTextField(output, "output", headRunes)
	if err != nil || !keep {
		return nil, err
	}
	return marshalCompactionItem(output)
}

// shortenCompactionTextField cuts the string at field to its first headRunes
// in place and reports whether fields survives. A value of any other JSON
// type is atomic: fields survives whole for a positive headRunes.
func shortenCompactionTextField(
	fields map[string]json.RawMessage,
	field string,
	headRunes int,
) (bool, error) {
	rawValue, ok := fields[field]
	if !ok {
		return headRunes > 0, nil
	}
	text, isString := compactionStringValue(rawValue)
	if !isString {
		return headRunes > 0, nil
	}
	head := compactionHeadRunes(text, headRunes)
	if head == "" {
		return false, nil
	}
	encoded, err := json.Marshal(head)
	if err != nil {
		return false, truncateCompactionError("encode "+field+" head", err)
	}
	fields[field] = encoded
	return true, nil
}

// compactionStringValue decodes a JSON string value. The second result is
// false for every other JSON type.
func compactionStringValue(raw json.RawMessage) (string, bool) {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

func marshalCompactionItem(fields map[string]json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, truncateCompactionError("encode shortened item", err)
	}
	return encoded, nil
}

func compactionHeadRunes(text string, count int) string {
	if count <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= count {
		return text
	}
	return string(runes[:count])
}

// dropTrailingReasoning removes reasoning items from the end of the kept
// prefix, down to retainStart. A reasoning item precedes the message or call
// it belongs to, and the cut may have deleted that message.
func dropTrailingReasoning(kept []json.RawMessage, retainStart int) []json.RawMessage {
	for len(kept) > retainStart {
		tailKind, err := rawCompactionItemKind(kept[len(kept)-1])
		if err != nil || tailKind != transcript.CompactedContextItemKindReasoning {
			return kept
		}
		kept = kept[:len(kept)-1]
	}
	return kept
}

func syntheticCompactionOutputs(items []json.RawMessage) ([]json.RawMessage, error) {
	normalized := codexstore.NormalizeResponseInputItems(items)
	answered := make(map[string]bool)
	for _, item := range normalized {
		if outputID, _, ok := rawCompactionOutput(item); ok && outputID != "" {
			answered[outputID] = true
		}
	}
	out := make([]json.RawMessage, 0)
	for _, item := range normalized {
		callID, kind, ok := rawCompactionCall(item)
		if !ok || callID == "" || answered[callID] {
			continue
		}
		encoded, err := abortedCompactionOutput(callID, kind)
		if err != nil {
			return nil, err
		}
		answered[callID] = true
		out = append(out, encoded)
	}
	return out, nil
}

func abortedCompactionOutput(callID string, kind rawCompactionCallKind) (json.RawMessage, error) {
	switch kind {
	case rawCompactionCallFunction, rawCompactionCallLocalShell:
		return marshalAbortedCallOutput(transcript.CompactedContextItemKindFunctionCallOutput, callID)
	case rawCompactionCallCustom:
		return marshalAbortedCallOutput(transcript.CompactedContextItemKindCustomToolCallOutput, callID)
	case rawCompactionCallToolSearch:
		encoded, err := json.Marshal(abortedToolSearchOutput{
			Type:   string(transcript.CompactedContextItemKindToolSearchOutput),
			CallID: callID,
			Tools:  []json.RawMessage{},
		})
		if err != nil {
			return nil, truncateCompactionError("encode aborted tool search output", err)
		}
		return encoded, nil
	default:
		return nil, truncateCompactionError("synthesize output for call kind "+string(kind), errCompactionCallKindUnknown)
	}
}

func marshalAbortedCallOutput(outputKind transcript.CompactedContextItemKind, callID string) (json.RawMessage, error) {
	encoded, err := json.Marshal(abortedCallOutput{
		Type:   string(outputKind),
		CallID: callID,
		Output: abortedCompactionOutputText,
	})
	if err != nil {
		return nil, truncateCompactionError("encode aborted call output", err)
	}
	return encoded, nil
}
