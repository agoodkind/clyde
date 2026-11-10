// Package finishreason maps provider terminal states to OpenAI finish_reason.
// Unknown provider reasons map to "stop", except provider refusal/content-filter
// signals which map to "content_filter".
package finishreason

import "strings"

// anthropicStopReason enumerates the values the Anthropic streaming
// surface emits on the message stop reason field.
type anthropicStopReason string

const (
	anthropicStopReasonMaxTokens    anthropicStopReason = "max_tokens"
	anthropicStopReasonToolUse      anthropicStopReason = "tool_use"
	anthropicStopReasonRefusal      anthropicStopReason = "refusal"
	anthropicStopReasonEndTurn      anthropicStopReason = "end_turn"
	anthropicStopReasonStopSequence anthropicStopReason = "stop_sequence"
	anthropicStopReasonUnset        anthropicStopReason = ""
)

// codexFinishReason enumerates the values the Codex Responses surface
// emits on the terminal status or incomplete reason field, along with
// already-normalized OpenAI finish_reason values the mapper accepts
// as a pass-through.
type codexFinishReason string

const (
	codexFinishReasonToolCalls       codexFinishReason = "tool_calls"
	codexFinishReasonToolCall        codexFinishReason = "tool_call"
	codexFinishReasonFunctionCall    codexFinishReason = "function_call"
	codexFinishReasonRequiresAction  codexFinishReason = "requires_action"
	codexFinishReasonMaxTokens       codexFinishReason = "max_tokens"
	codexFinishReasonMaxOutputTokens codexFinishReason = "max_output_tokens"
	codexFinishReasonLength          codexFinishReason = "length"
	codexFinishReasonContentFilter   codexFinishReason = "content_filter"
	codexFinishReasonRefusal         codexFinishReason = "refusal"
	codexFinishReasonStop            codexFinishReason = "stop"
	codexFinishReasonCompleted       codexFinishReason = "completed"
	codexFinishReasonComplete        codexFinishReason = "complete"
	codexFinishReasonEndTurn         codexFinishReason = "end_turn"
	codexFinishReasonUnset           codexFinishReason = ""
)

// FromAnthropicStream maps Anthropic stop_reason for streaming; unknown and empty
// values become "stop".
func FromAnthropicStream(s string) string {
	switch anthropicStopReason(s) {
	case anthropicStopReasonMaxTokens:
		return "length"
	case anthropicStopReasonToolUse:
		return "tool_calls"
	case anthropicStopReasonRefusal:
		return "content_filter"
	case anthropicStopReasonEndTurn, anthropicStopReasonStopSequence, anthropicStopReasonUnset:
		return "stop"
	default:
		return "stop"
	}
}

// FromCodex maps Codex Responses terminal reasons or already-normalized values
// to OpenAI finish_reason. Unknown values normalize to "stop".
func FromCodex(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch codexFinishReason(s) {
	case codexFinishReasonToolCalls, codexFinishReasonToolCall, codexFinishReasonFunctionCall, codexFinishReasonRequiresAction:
		return "tool_calls"
	case codexFinishReasonMaxTokens, codexFinishReasonMaxOutputTokens, codexFinishReasonLength:
		return "length"
	case codexFinishReasonContentFilter, codexFinishReasonRefusal:
		return "content_filter"
	case codexFinishReasonStop, codexFinishReasonCompleted, codexFinishReasonComplete, codexFinishReasonEndTurn, codexFinishReasonUnset:
		return "stop"
	default:
		return "stop"
	}
}

// FromCodexResponse maps the final Codex Responses status plus optional
// incomplete reason to OpenAI finish_reason.
func FromCodexResponse(status, incompleteReason string) string {
	if incompleteReason != "" {
		return FromCodex(incompleteReason)
	}
	return FromCodex(status)
}
