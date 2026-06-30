package anthropicbackend

import (
	"encoding/json"
	"strconv"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/clock"
)

// JSONCoercion captures the optional JSON-coercion contract a caller
// may provide when merging the buffered Anthropic stream chunks into a
// final OpenAI ChatResponse. The Anthropic backend does not know about
// the root-side JSONResponseSpec type, so callers translate to this
// neutral form (or nil-coercion when the caller did not request
// structured output).
type JSONCoercion struct {
	// Coerce returns a JSON-safe variant of the buffered assistant
	// text. Set to nil when the caller did not request structured
	// output. Coerce should not mutate the input.
	Coerce func(text string) string
	// Validate reports whether the coerced text is valid JSON. The
	// merger only swaps in the coerced text when Validate returns
	// true. Set to nil when no validation is required.
	Validate func(text string) bool
}

// MergeCollectedEvents reconstructs the final ChatResponse from a
// buffer of normalized events emitted by the Anthropic translator.
//
// The merger reassembles assistant text, reasoning, refusal text, and
// tool-call deltas into the OpenAI non-streaming response shape.
// Anthropic's distinct `stop_reason == "refusal"` is mapped onto
// `message.refusal` when no explicit refusal event arrived.
//
// JSON coercion is opt-in: callers that requested a structured
// `response_format` pass a JSONCoercion with both Coerce and Validate
// set; otherwise the merger leaves assistant text untouched.
func MergeCollectedEvents(
	reqID, modelAlias, systemFingerprint string,
	events []adapterrender.Event,
	usage adapteropenai.Usage,
	finishReason string,
	json JSONCoercion,
	anthropicStopReason string,
) adapteropenai.ChatResponse {
	collected := adapterrender.CollectMessage(events)
	outText := collected.Text
	if json.Coerce != nil {
		coerced := json.Coerce(outText)
		if json.Validate == nil || json.Validate(coerced) {
			outText = coerced
		}
	}

	msg := adapteropenai.ChatMessage{
		Role:    "assistant",
		Content: jsonRawQuoted(outText), Name: "", ToolCalls: nil, ToolCallID: "", Reasoning: "", ReasoningContent: "", Refusal: "", Annotations: nil,
	}
	if collected.Reasoning != "" {
		msg.Reasoning = collected.Reasoning
		msg.ReasoningContent = collected.Reasoning
	}
	if collected.Refusal != "" {
		msg.Refusal = collected.Refusal
	} else if strings.EqualFold(anthropicStopReason, "refusal") && strings.TrimSpace(outText) != "" {
		msg.Refusal = outText
	}
	msg.ToolCalls = append(msg.ToolCalls, collected.ToolCalls...)

	return adapteropenai.ChatResponse{
		ID:                reqID,
		Object:            "chat.completion",
		Created:           clock.Now().Unix(),
		Model:             modelAlias,
		SystemFingerprint: systemFingerprint,
		Choices: []adapteropenai.ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason, Logprobs: nil,
		}},
		Usage: &usage,
	}
}

func jsonRawQuoted(s string) json.RawMessage {
	return json.RawMessage(strconv.Quote(s))
}
