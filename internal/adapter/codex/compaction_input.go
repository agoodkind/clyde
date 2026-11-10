package codex

import (
	"encoding/json"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/transcript"
)

// MaxCompactionInjectionBytes caps the injection a native v2 compaction stores
// in process memory between turn N and turn N+1.
const MaxCompactionInjectionBytes = 1 * 1024 * 1024

// CompactionSplitter plans one native compaction request. The daemon supplies
// the generic splitter with the Codex provider.
type CompactionSplitter interface {
	Plan(body []byte) (CompactionSplit, bool)
}

// CompactionSplit is one planned split: the truncated request and the wrapped
// text the response append inserts.
type CompactionSplit struct {
	Forwarded []byte
	Injection string
}

// CompactionSegmentKind is the content kind of one countable piece of a
// Responses input item.
type CompactionSegmentKind uint8

const (
	// CompactionSegmentText is a message text part.
	CompactionSegmentText CompactionSegmentKind = iota
	// CompactionSegmentThinking is a reasoning summary part.
	CompactionSegmentThinking
	// CompactionSegmentToolUse is a call item's arguments, input, or action.
	CompactionSegmentToolUse
	// CompactionSegmentToolResult is an output item's output or tools.
	CompactionSegmentToolResult
	// CompactionSegmentImage is a message image part.
	CompactionSegmentImage
	// CompactionSegmentOther is an item kind the split counts as empty.
	CompactionSegmentOther
)

// CompactionSegment is one countable piece of an input item. Atomic marks a
// piece the truncation cannot shorten: a JSON object, a JSON string the model
// emitted as tool arguments, or encrypted reasoning.
type CompactionSegment struct {
	Kind   CompactionSegmentKind
	Text   string
	Atomic bool
}

// CompactionRole is the role of one input item.
type CompactionRole string

const (
	// CompactionRoleUser is a user message.
	CompactionRoleUser CompactionRole = "user"
	// CompactionRoleAssistant is an assistant message or any non-message item.
	CompactionRoleAssistant CompactionRole = "assistant"
	// CompactionRoleDeveloper is a developer message.
	CompactionRoleDeveloper CompactionRole = "developer"
)

// CompactionMessage is one input item decoded into countable segments.
type CompactionMessage struct {
	Role     CompactionRole
	Segments []CompactionSegment
}

// CompactionInput is a native compaction request decoded for the split.
type CompactionInput struct {
	Messages []CompactionMessage
	// RetainStart is the first item the split counts. The v2 setup items sit
	// before it. v1 sets 0.
	RetainStart int
	// InstructionStart is the index of the compaction prompt (v1) or of the
	// first unfinished item before the trigger (v2).
	InstructionStart int
}

// CompactionCut is where the split divides the boundary item.
type CompactionCut struct {
	MessageIndex int
	SegmentIndex int
	HeadRunes    int
}

// DecodeCompactionInput decodes a Responses input array into one message per
// item. ok is false for a body the split must forward unchanged: no input,
// an incomplete call and output pairing, an unrecognized item, or a
// malformed v1 prompt or v2 layout.
func DecodeCompactionInput(body []byte) (CompactionInput, bool) {
	empty := CompactionInput{Messages: nil, RetainStart: 0, InstructionStart: 0}
	rawItems, ok := decodeCompactionInputItems(body)
	if !ok || len(rawItems) < 2 {
		return empty, false
	}
	items := codexstore.NormalizeResponseInputItems(rawItems)
	if len(items) != len(rawItems) {
		return empty, false
	}
	retainStart, instructionStart, ok := compactionLayout(items)
	if !ok {
		return empty, false
	}
	// An unrecognized item kind in the counted range has no segment model.
	// The v2 setup items before retainStart stay in the forwarded request
	// whatever their kind.
	for _, item := range items[retainStart:instructionStart] {
		if item.Kind == transcript.CompactedContextItemKindOther {
			return empty, false
		}
	}
	if !rawCompactionPairsAreComplete(items[retainStart:instructionStart]) {
		return empty, false
	}
	messages := make([]CompactionMessage, 0, len(items))
	for _, item := range items {
		messages = append(messages, compactionMessage(item))
	}
	return CompactionInput{
		Messages:         messages,
		RetainStart:      retainStart,
		InstructionStart: instructionStart,
	}, true
}

func decodeCompactionInputItems(body []byte) ([]json.RawMessage, bool) {
	inputStart, inputEnd, ok := jsonObjectFieldValueRange(body, "input")
	if !ok {
		return nil, false
	}
	var rawItems []json.RawMessage
	if json.Unmarshal(body[inputStart:inputEnd], &rawItems) != nil {
		return nil, false
	}
	return rawItems, true
}

// compactionLayout returns the counted range of the items. A trailing
// compaction_trigger item selects the v2 layout: the setup items before the
// first user message stay in the forwarded request, and the unfinished items
// after the last complete assistant answer stay with the trigger. Any other
// shape is v1: every item before the final user prompt is counted.
func compactionLayout(items []transcript.CompactedContextItem) (int, int, bool) {
	last := len(items) - 1
	if items[last].Kind == transcript.CompactedContextItemKindCompactionTrigger {
		triggerIndex, ok := rawResponsesCompactionV2TriggerIndex(items)
		if !ok {
			return 0, 0, false
		}
		transcriptStart, ok := rawResponsesCompactionV2TranscriptStart(items[:triggerIndex])
		if !ok {
			return 0, 0, false
		}
		transcriptItems := items[transcriptStart:triggerIndex]
		completeEnd, ok := rawResponsesCompactionV2CompletePrefixEnd(transcriptItems)
		if !ok {
			return 0, 0, false
		}
		if !rawResponsesCompactionV2UnfinishedSuffixIsValid(transcriptItems[completeEnd:]) {
			return 0, 0, false
		}
		return transcriptStart, transcriptStart + completeEnd, true
	}
	if !rawCompactionPromptIsValid(items[last]) {
		return 0, 0, false
	}
	return 0, last, true
}

func compactionMessage(item transcript.CompactedContextItem) CompactionMessage {
	switch item.Kind {
	case transcript.CompactedContextItemKindMessage:
		return CompactionMessage{Role: compactionRole(item.Message.Role), Segments: compactionMessageSegments(item.Message)}
	case transcript.CompactedContextItemKindReasoning:
		return CompactionMessage{Role: CompactionRoleAssistant, Segments: compactionReasoningSegments(item.Reasoning)}
	case transcript.CompactedContextItemKindFunctionCall:
		return atomicMessage(CompactionSegmentToolUse, item.FunctionCall.Arguments)
	case transcript.CompactedContextItemKindCustomToolCall:
		return atomicMessage(CompactionSegmentToolUse, item.CustomToolCall.Input)
	case transcript.CompactedContextItemKindLocalShellCall:
		return atomicMessage(CompactionSegmentToolUse, string(item.LocalShellCall.ActionRaw))
	case transcript.CompactedContextItemKindToolSearchCall:
		return atomicMessage(CompactionSegmentToolUse, string(item.ToolSearchCall.ArgumentsRaw))
	case transcript.CompactedContextItemKindFunctionCallOutput:
		return compactionOutputMessage(item.FunctionCallOutput.OutputRaw)
	case transcript.CompactedContextItemKindCustomToolCallOutput:
		return compactionOutputMessage(item.CustomToolCallOutput.OutputRaw)
	case transcript.CompactedContextItemKindToolSearchOutput:
		return atomicMessage(CompactionSegmentToolResult, toolSearchOutputText(item.ToolSearchOutput.ToolsRaw))
	case transcript.CompactedContextItemKindWebSearchCall,
		transcript.CompactedContextItemKindImageGenerationCall,
		transcript.CompactedContextItemKindCompaction,
		transcript.CompactedContextItemKindCompactionTrigger,
		transcript.CompactedContextItemKindContextCompaction,
		transcript.CompactedContextItemKindOther:
		return atomicMessage(CompactionSegmentOther, "")
	default:
		return atomicMessage(CompactionSegmentOther, "")
	}
}

func compactionRole(role string) CompactionRole {
	switch role {
	case string(CompactionRoleUser):
		return CompactionRoleUser
	case string(CompactionRoleDeveloper):
		return CompactionRoleDeveloper
	default:
		return CompactionRoleAssistant
	}
}

// compactionMessageSegments returns one segment per content part, in part
// order. The truncation relies on that one-to-one mapping.
func compactionMessageSegments(message *transcript.CompactedMessageItem) []CompactionSegment {
	if message == nil {
		return nil
	}
	segments := make([]CompactionSegment, 0, len(message.Content))
	for _, part := range message.Content {
		switch rawCompactionMessageContentType(part.Type) {
		case rawCompactionContentInputText, rawCompactionContentOutputText, rawCompactionContentText:
			segments = append(segments, CompactionSegment{Kind: CompactionSegmentText, Text: part.Text, Atomic: false})
		default:
			segments = append(segments, CompactionSegment{Kind: CompactionSegmentImage, Text: "", Atomic: true})
		}
	}
	return segments
}

// compactionReasoningSegments returns one atomic segment per summary part.
// The encrypted content is not counted and never shortened.
func compactionReasoningSegments(reasoning *transcript.CompactedReasoningItem) []CompactionSegment {
	if reasoning == nil {
		return nil
	}
	segments := make([]CompactionSegment, 0, len(reasoning.Summary))
	for _, summary := range reasoning.Summary {
		segments = append(segments, CompactionSegment{Kind: CompactionSegmentThinking, Text: summary.Text, Atomic: true})
	}
	if len(segments) == 0 {
		segments = append(segments, CompactionSegment{Kind: CompactionSegmentThinking, Text: "", Atomic: true})
	}
	return segments
}

// toolSearchOutputText joins the raw tool entries of a tool_search_output
// item into the text the counter measures.
func toolSearchOutputText(tools []json.RawMessage) string {
	encoded, err := marshalRawArray(tools)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func atomicMessage(kind CompactionSegmentKind, text string) CompactionMessage {
	return CompactionMessage{
		Role:     CompactionRoleAssistant,
		Segments: []CompactionSegment{{Kind: kind, Text: text, Atomic: true}},
	}
}

// compactionOutputMessage counts a string output as text the truncation can
// shorten, and any other output shape as one atomic segment.
func compactionOutputMessage(raw json.RawMessage) CompactionMessage {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return CompactionMessage{
			Role:     CompactionRoleAssistant,
			Segments: []CompactionSegment{{Kind: CompactionSegmentToolResult, Text: text, Atomic: false}},
		}
	}
	return atomicMessage(CompactionSegmentToolResult, string(raw))
}
