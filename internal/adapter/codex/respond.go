package codex

import (
	"encoding/json"
	"strconv"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/clock"
)

// MergeEvents is part of Clyde's typed adapter surface.
func MergeEvents(reqID, modelAlias, systemFingerprint string, events []adapterrender.Event, res RunResult) adapteropenai.ChatResponse {
	collected := adapterrender.CollectMessage(events)
	msg := adapteropenai.ChatMessage{
		Role:    "assistant",
		Content: json.RawMessage(strconv.Quote(collected.Text)), Name: "", ToolCalls: nil, ToolCallID: "", Reasoning: "", ReasoningContent: "", Refusal: "", Annotations: nil,
	}
	if collected.Reasoning != "" {
		msg.Reasoning = collected.Reasoning
		msg.ReasoningContent = collected.Reasoning
	}
	if collected.Refusal != "" {
		msg.Refusal = collected.Refusal
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
			FinishReason: res.FinishReason, Logprobs: nil,
		}},
		Usage: &res.Usage,
	}
}
