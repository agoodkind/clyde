package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// The documented contract always sends choices[].logprobs, message.content,
// and message.refusal. These wire types write JSON null for an absent value
// instead of omitting the member.
type documentedChatResponseWire struct {
	ID                string                     `json:"id"`
	Object            string                     `json:"object"`
	Created           int64                      `json:"created"`
	Model             string                     `json:"model"`
	Choices           []documentedChatChoiceWire `json:"choices"`
	Usage             *Usage                     `json:"usage,omitempty"`
	SystemFingerprint string                     `json:"system_fingerprint,omitempty"`
}

type documentedChatChoiceWire struct {
	Index        int                       `json:"index"`
	Message      documentedChatMessageWire `json:"message"`
	Logprobs     *LogprobsResult           `json:"logprobs"`
	FinishReason string                    `json:"finish_reason"`
}

type documentedChatMessageWire struct {
	Role             string              `json:"role"`
	Content          json.RawMessage     `json:"content"`
	Refusal          *string             `json:"refusal"`
	Name             string              `json:"name,omitempty"`
	ToolCalls        []ToolCall          `json:"tool_calls,omitempty"`
	ToolCallID       string              `json:"tool_call_id,omitempty"`
	Reasoning        string              `json:"reasoning,omitempty"`
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	Annotations      []MessageAnnotation `json:"annotations,omitempty"`
}

// MarshalDocumentedChatResponse writes JSON null for an empty content,
// refusal, or logprobs value instead of omitting the member.
func MarshalDocumentedChatResponse(resp ChatResponse) ([]byte, error) {
	choices := make([]documentedChatChoiceWire, 0, len(resp.Choices))
	for _, choice := range resp.Choices {
		choices = append(choices, documentedChatChoiceWire{
			Index:        choice.Index,
			Message:      documentedChatMessage(choice.Message),
			Logprobs:     choice.Logprobs,
			FinishReason: choice.FinishReason,
		})
	}
	encoded, err := json.Marshal(documentedChatResponseWire{
		ID:                resp.ID,
		Object:            resp.Object,
		Created:           resp.Created,
		Model:             resp.Model,
		Choices:           choices,
		Usage:             resp.Usage,
		SystemFingerprint: resp.SystemFingerprint,
	})
	if err != nil {
		slog.Warn("adapter.openai.documented_chat_response_marshal_failed", "concern", "adapter.chat.render", "err", err)
		return nil, fmt.Errorf("marshal documented chat response: %w", err)
	}
	return encoded, nil
}

func documentedChatMessage(message ChatMessage) documentedChatMessageWire {
	var refusal *string
	if message.Refusal != "" {
		value := message.Refusal
		refusal = &value
	}
	return documentedChatMessageWire{
		Role:             message.Role,
		Content:          message.Content,
		Refusal:          refusal,
		Name:             message.Name,
		ToolCalls:        message.ToolCalls,
		ToolCallID:       message.ToolCallID,
		Reasoning:        message.Reasoning,
		ReasoningContent: message.ReasoningContent,
		Annotations:      message.Annotations,
	}
}
