package adapter

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// parseSSEEventNames extracts the ordered `event:` names from a Responses
// SSE body. Each frame is `event: <name>\ndata: <json>\n\n`.
func parseSSEEventNames(t *testing.T, body string) []string {
	t.Helper()
	var names []string
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				names = append(names, strings.TrimPrefix(line, "event: "))
			}
		}
	}
	return names
}

func firstSSEDataObject(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
					t.Fatalf("unmarshal first data frame: %v", err)
				}
				return obj
			}
		}
	}
	t.Fatalf("no data frame found")
	return nil
}

func TestResponsesStreamWriterEventOrder(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_test123", "clyde-codex-5.4-medium", slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}

	writer.begin()
	if err := writer.WriteEvent(adapterrender.ReasoningDelta{
		Text: "pondering", ReasoningKind: "summary", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: "",
	}); err != nil {
		t.Fatalf("reasoning delta: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.TextDelta{Text: "Hi "}); err != nil {
		t.Fatalf("text delta 1: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.TextDelta{Text: "there"}); err != nil {
		t.Fatalf("text delta 2: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
		Index: 0, ID: "call_1", Type: "function", Function: adapteropenai.ToolCallFunction{Name: "f", Arguments: `{"a":1}`},
	}}}); err != nil {
		t.Fatalf("tool call delta: %v", err)
	}
	writer.finish(adapterprovider.Result{
		Usage:                      adapteropenai.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0},
		FinalResponse:              nil,
		FinishReason:               "tool_calls",
		SystemFingerprint:          "",
		ReasoningSignaled:          true,
		ReasoningVisible:           true,
		ReasoningSummary:           "",
		DerivedCacheCreationTokens: 0,
		UpstreamResponseID:         "",
		ToolCallCount:              1,
		ToolCallNames:              []string{"f"},
		HasSubagentToolCall:        false,
		UsageNoticeWindows:         nil,
		UsageNotices:               nil,
	})

	body := rec.Body.String()

	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Responses stream must NOT emit data: [DONE]; body=%s", body)
	}

	names := parseSSEEventNames(t, body)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(names) != len(want) {
		t.Fatalf("event count=%d want %d\ngot=%v", len(names), len(want), names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("event[%d]=%q want %q\nfull=%v", i, names[i], want[i], names)
		}
	}

	first := firstSSEDataObject(t, body)
	var seq int
	if err := json.Unmarshal(first["sequence_number"], &seq); err != nil {
		t.Fatalf("sequence_number: %v", err)
	}
	if seq != 0 {
		t.Errorf("first sequence_number=%d want 0", seq)
	}
}

func TestResponsesCollectorBuildsResponseObject(t *testing.T) {
	t.Parallel()
	collector := newProviderCollectorWriter()
	_ = collector.WriteEvent(adapterrender.ReasoningDelta{Text: "hmm", ReasoningKind: "summary", SummaryIndex: nil, Signature: "", RedactedData: "", ItemID: "", ItemType: ""})
	_ = collector.WriteEvent(adapterrender.TextDelta{Text: "answer"})

	collected := adapterrender.CollectMessage(collector.events)
	usage := adapteropenai.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10, PromptTokensDetails: &adapteropenai.PromptTokensDetails{CachedTokens: 2}, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0}
	resp := adapteropenai.BuildResponsesResponse(adapteropenai.ResponsesResponseParams{
		ID:         "resp_collect",
		Model:      "m",
		CreatedAt:  42,
		Status:     adapteropenai.ResponsesStatusCompleted,
		Text:       collected.Text,
		Reasoning:  collected.Reasoning,
		Refusal:    collected.Refusal,
		ToolCalls:  collected.ToolCalls,
		Usage:      &usage,
		ItemIDBase: "collect",
	})
	if resp.Status != adapteropenai.ResponsesStatusCompleted {
		t.Errorf("status=%q", resp.Status)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output len=%d want 2 (reasoning, message)", len(resp.Output))
	}
	if resp.Output[0].Type != "reasoning" || resp.Output[1].Type != "message" {
		t.Errorf("output types=%q,%q", resp.Output[0].Type, resp.Output[1].Type)
	}
}
