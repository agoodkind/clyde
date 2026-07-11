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
	if fc.CallID != "call_abc123_0" || fc.Name != "get_weather" || fc.Arguments != `{"city":"SF"}` || fc.Status != "completed" {
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

func TestBuildResponsesResponseKeepsRefusalAsTypedContentPart(t *testing.T) {
	t.Parallel()
	resp := BuildResponsesResponse(ResponsesResponseParams{
		ID:         "resp_refusal",
		Model:      "m",
		CreatedAt:  1,
		Status:     ResponsesStatusCompleted,
		Text:       "I can help with a safe alternative.",
		Reasoning:  "",
		Refusal:    "I cannot help with that request.",
		ToolCalls:  nil,
		Usage:      nil,
		ItemIDBase: "refusal",
	})

	if len(resp.Output) != 1 {
		t.Fatalf("output len=%d want 1", len(resp.Output))
	}
	if len(resp.Output[0].Content) != 2 {
		t.Fatalf("content len=%d want 2", len(resp.Output[0].Content))
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var wire struct {
		Output []struct {
			Content []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	parts := wire.Output[0].Content
	if parts[0].Type != "output_text" || parts[0].Text != "I can help with a safe alternative." {
		t.Fatalf("text part = %+v", parts[0])
	}
	if parts[1].Type != "refusal" || parts[1].Refusal != "I cannot help with that request." {
		t.Fatalf("refusal part = %+v", parts[1])
	}
}

func TestBuildResponsesResponseIncompleteCarriesReason(t *testing.T) {
	t.Parallel()
	resp := BuildResponsesResponse(ResponsesResponseParams{
		ID:         "resp_incomplete",
		Model:      "m",
		CreatedAt:  1,
		Status:     ResponsesStatusIncomplete,
		Text:       "partial",
		Reasoning:  "",
		Refusal:    "",
		ToolCalls:  nil,
		Usage:      nil,
		ItemIDBase: "incomplete",
	})
	if resp.IncompleteDetails == nil {
		t.Fatal("incomplete response omitted incomplete_details")
	}
	if resp.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete reason=%q want max_output_tokens", resp.IncompleteDetails.Reason)
	}
}

func TestBuildResponsesResponseIncompleteMarksDerivedItemsIncomplete(t *testing.T) {
	t.Parallel()
	resp := BuildResponsesResponse(ResponsesResponseParams{
		ID:        "resp_partial",
		Model:     "m",
		CreatedAt: 1,
		Status:    ResponsesStatusIncomplete,
		Text:      "partial answer",
		Reasoning: "partial reasoning",
		ToolCalls: []ToolCall{{
			Index: 0,
			Type:  "function",
			Function: ToolCallFunction{
				Name:      "lookup",
				Arguments: `{"query":`,
			},
		}},
		ItemIDBase: "partial",
	})

	if len(resp.Output) != 3 {
		t.Fatalf("output len=%d want 3", len(resp.Output))
	}
	for index, item := range resp.Output {
		if item.Status != "incomplete" {
			t.Errorf("output[%d] type=%q status=%q want incomplete", index, item.Type, item.Status)
		}
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"reasoning","id":"rs_partial","status":"incomplete"`) {
		t.Fatalf("reasoning wire omitted incomplete status: %s", raw)
	}
}

func TestResponsesTerminalForFinishReason(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		finish string
		status ResponsesStatus
		reason string
	}{
		{name: "clean", finish: "stop", status: ResponsesStatusCompleted, reason: ""},
		{name: "length", finish: "length", status: ResponsesStatusIncomplete, reason: "max_output_tokens"},
		{name: "content filter", finish: "content_filter", status: ResponsesStatusIncomplete, reason: "content_filter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, details := ResponsesTerminalForFinishReason(test.finish)
			if status != test.status {
				t.Fatalf("status=%q want %q", status, test.status)
			}
			if test.reason == "" {
				if details != nil {
					t.Fatalf("details=%+v want nil", details)
				}
				return
			}
			if details == nil || details.Reason != test.reason {
				t.Fatalf("details=%+v want reason %q", details, test.reason)
			}
		})
	}
}
