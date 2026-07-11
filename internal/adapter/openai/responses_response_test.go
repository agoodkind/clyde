package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// verifyResponsesResponse is a typed mirror of the Responses response
// object JSON the builder emits. The test unmarshals into it rather
// than a loose map so the shape assertions stay type-checked.
type verifyResponsesResponse struct {
	ID      string                `json:"id"`
	Object  string                `json:"object"`
	Created int64                 `json:"created_at"`
	Status  string                `json:"status"`
	Model   string                `json:"model"`
	Output  []verifyResponsesItem `json:"output"`
	Usage   verifyResponsesUsage  `json:"usage"`
}

type verifyResponsesItem struct {
	Type      string                       `json:"type"`
	ID        string                       `json:"id"`
	Status    string                       `json:"status"`
	Role      string                       `json:"role"`
	Content   []verifyResponsesContentPart `json:"content"`
	Summary   []verifyResponsesSummaryPart `json:"summary"`
	CallID    string                       `json:"call_id"`
	Name      string                       `json:"name"`
	Arguments string                       `json:"arguments"`
}

type verifyResponsesContentPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Annotations []json.RawMessage `json:"annotations"`
}

type verifyResponsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type verifyResponsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func TestBuildResponsesResponseNonStreamingShape(t *testing.T) {
	t.Parallel()
	usage := Usage{
		PromptTokens:        120,
		CompletionTokens:    34,
		TotalTokens:         154,
		PromptTokensDetails: &PromptTokensDetails{CachedTokens: 80},
		InputTokens:         0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0,
	}
	resp := BuildResponsesResponse(ResponsesResponseParams{
		ID:        "resp_abc123",
		Model:     "clyde-codex-5.4-medium",
		CreatedAt: 1700000000,
		Status:    ResponsesStatusCompleted,
		Text:      "Hello world",
		Reasoning: "thinking about it",
		Refusal:   "",
		ToolCalls: []ToolCall{{
			Index:    0,
			ID:       "call_xyz",
			Type:     "function",
			Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"SF"}`},
		}},
		Usage:      &usage,
		ItemIDBase: "abc123",
	})

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `"incomplete_details":null`) {
		t.Errorf("expected incomplete_details null, body=%s", body)
	}
	if !strings.Contains(body, `"error":null`) {
		t.Errorf("expected error null, body=%s", body)
	}
	if !strings.Contains(body, `"metadata":{}`) {
		t.Errorf("expected metadata {}, body=%s", body)
	}

	var got verifyResponsesResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !strings.HasPrefix(got.ID, "resp_") {
		t.Errorf("id=%q want resp_ prefix", got.ID)
	}
	if got.Object != "response" {
		t.Errorf("object=%q want response", got.Object)
	}
	if got.Status != "completed" {
		t.Errorf("status=%q want completed", got.Status)
	}
	if got.Model != "clyde-codex-5.4-medium" {
		t.Errorf("model=%q", got.Model)
	}
	if len(got.Output) != 3 {
		t.Fatalf("output len=%d want 3 (reasoning, message, function_call): %s", len(got.Output), body)
	}

	reasoning := got.Output[0]
	if reasoning.Type != "reasoning" {
		t.Errorf("output[0].type=%q want reasoning", reasoning.Type)
	}
	if !strings.HasPrefix(reasoning.ID, "rs_") {
		t.Errorf("reasoning id=%q want rs_ prefix", reasoning.ID)
	}
	if len(reasoning.Summary) != 1 || reasoning.Summary[0].Type != "summary_text" || reasoning.Summary[0].Text != "thinking about it" {
		t.Errorf("reasoning summary=%+v", reasoning.Summary)
	}

	message := got.Output[1]
	if message.Type != "message" {
		t.Errorf("output[1].type=%q want message", message.Type)
	}
	if !strings.HasPrefix(message.ID, "msg_") {
		t.Errorf("message id=%q want msg_ prefix", message.ID)
	}
	if message.Status != "completed" || message.Role != "assistant" {
		t.Errorf("message status=%q role=%q", message.Status, message.Role)
	}
	if len(message.Content) != 1 || message.Content[0].Type != "output_text" || message.Content[0].Text != "Hello world" {
		t.Fatalf("message content=%+v", message.Content)
	}
	if message.Content[0].Annotations == nil {
		t.Errorf("message annotations should be empty array, not null")
	}

	fc := got.Output[2]
	if fc.Type != "function_call" {
		t.Errorf("output[2].type=%q want function_call", fc.Type)
	}
	if !strings.HasPrefix(fc.ID, "fc_") {
		t.Errorf("function_call id=%q want fc_ prefix", fc.ID)
	}
	if fc.CallID != "call_xyz" || fc.Name != "get_weather" || fc.Arguments != `{"city":"SF"}` || fc.Status != "completed" {
		t.Errorf("function_call=%+v", fc)
	}

	if got.Usage.InputTokens != 120 || got.Usage.OutputTokens != 34 || got.Usage.TotalTokens != 154 {
		t.Errorf("usage tokens=%+v", got.Usage)
	}
	if got.Usage.InputTokensDetails.CachedTokens != 80 {
		t.Errorf("cached_tokens=%d want 80", got.Usage.InputTokensDetails.CachedTokens)
	}
}

func TestBuildResponsesResponseOmitsMessageWhenOnlyToolCall(t *testing.T) {
	t.Parallel()
	resp := BuildResponsesResponse(ResponsesResponseParams{
		ID:         "resp_zzz",
		Model:      "m",
		CreatedAt:  1,
		Status:     ResponsesStatusCompleted,
		Text:       "",
		Reasoning:  "",
		Refusal:    "",
		ToolCalls:  []ToolCall{{Index: 0, ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}},
		Usage:      nil,
		ItemIDBase: "zzz",
	})
	if len(resp.Output) != 1 {
		t.Fatalf("output len=%d want 1 (function_call only)", len(resp.Output))
	}
	if resp.Output[0].Type != "function_call" {
		t.Errorf("output[0].type=%q want function_call", resp.Output[0].Type)
	}
}
