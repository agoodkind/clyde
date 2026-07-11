package adapter

import (
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
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
	writer, err := newResponsesStreamWriter(rec, "resp_test123", "clyde-codex-5.4-medium", nil, slog.Default())
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
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.reasoning_summary_text.done",
		"response.output_item.done",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
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

type responsesStreamFrame struct {
	Name           string
	Type           string                              `json:"type"`
	SequenceNumber int                                 `json:"sequence_number"`
	Response       *adapteropenai.ResponsesResponse    `json:"response"`
	ItemID         string                              `json:"item_id"`
	OutputIndex    int                                 `json:"output_index"`
	ContentIndex   int                                 `json:"content_index"`
	Part           *adapteropenai.ResponsesContentPart `json:"part"`
	Item           *adapteropenai.ResponsesOutputItem  `json:"item"`
	Delta          string                              `json:"delta"`
	Text           string                              `json:"text"`
	Refusal        string                              `json:"refusal"`
	FunctionName   string                              `json:"name"`
	Arguments      string                              `json:"arguments"`
}

func parseResponsesStreamFrames(t *testing.T, body string) []responsesStreamFrame {
	t.Helper()
	frames := make([]responsesStreamFrame, 0)
	for _, rawFrame := range strings.Split(body, "\n\n") {
		frame := strings.TrimSpace(rawFrame)
		if frame == "" {
			continue
		}
		var name string
		var payload string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				payload = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || payload == "" {
			t.Fatalf("invalid Responses frame %q", frame)
		}
		var parsed responsesStreamFrame
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			t.Fatalf("unmarshal %s: %v", name, err)
		}
		parsed.Name = name
		frames = append(frames, parsed)
	}
	return frames
}

func TestResponsesStreamWriterPreservesMixedOutputIdentityAndOrder(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_mixed", "model", nil, slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.TextDelta{Text: "safe text"}); err != nil {
		t.Fatalf("text delta: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ReasoningDelta{Text: "reasoning", ReasoningKind: "summary"}); err != nil {
		t.Fatalf("reasoning delta: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.RefusalDelta{Text: "refusal"}); err != nil {
		t.Fatalf("refusal delta: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
		Index: 0, ID: "provider_call", Type: "function", Function: adapteropenai.ToolCallFunction{Name: "lookup", Arguments: `{"q":"x"}`},
	}}}); err != nil {
		t.Fatalf("tool delta: %v", err)
	}
	if err := writer.finish(adapterprovider.Result{FinishReason: "length"}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	frames := parseResponsesStreamFrames(t, rec.Body.String())
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("Responses stream must not contain [DONE]: %s", rec.Body.String())
	}
	for index, frame := range frames {
		if frame.SequenceNumber != index {
			t.Fatalf("frame %d sequence=%d want %d", index, frame.SequenceNumber, index)
		}
	}
	if frames[0].Name != "response.created" || frames[0].Response == nil || frames[0].Response.ID != "resp_mixed" {
		t.Fatalf("created frame = %+v", frames[0])
	}
	if frames[len(frames)-1].Name != "response.incomplete" {
		t.Fatalf("terminal event=%q want response.incomplete", frames[len(frames)-1].Name)
	}
	terminal := frames[len(frames)-1].Response
	if terminal == nil || terminal.Status != adapteropenai.ResponsesStatusIncomplete {
		t.Fatalf("terminal response = %+v", terminal)
	}
	if terminal.IncompleteDetails == nil || terminal.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete details = %+v", terminal.IncompleteDetails)
	}
	if len(terminal.Output) != 3 {
		t.Fatalf("terminal output len=%d want 3", len(terminal.Output))
	}
	if terminal.Output[0].Type != "message" || terminal.Output[0].ID != "msg_mixed" {
		t.Fatalf("output[0] = %+v", terminal.Output[0])
	}
	if len(terminal.Output[0].Content) != 2 || terminal.Output[0].Content[0].Type != "output_text" || terminal.Output[0].Content[1].Type != "refusal" {
		t.Fatalf("message content = %+v", terminal.Output[0].Content)
	}
	if terminal.Output[1].Type != "reasoning" || terminal.Output[1].ID != "rs_mixed" {
		t.Fatalf("output[1] = %+v", terminal.Output[1])
	}
	if terminal.Output[2].Type != "function_call" || terminal.Output[2].ID != "fc_mixed_0" || terminal.Output[2].CallID == "provider_call" {
		t.Fatalf("output[2] = %+v", terminal.Output[2])
	}
	if strings.Contains(rec.Body.String(), "provider_call") {
		t.Fatalf("provider tool-call id escaped the Clyde Responses identity space: %s", rec.Body.String())
	}

	refusalDelta := false
	refusalDone := false
	for _, frame := range frames {
		if frame.Name == "response.refusal.delta" && frame.ItemID == "msg_mixed" && frame.OutputIndex == 0 && frame.ContentIndex == 1 && frame.Delta == "refusal" {
			refusalDelta = true
		}
		if frame.Name == "response.refusal.done" && frame.ItemID == "msg_mixed" && frame.OutputIndex == 0 && frame.ContentIndex == 1 && frame.Refusal == "refusal" {
			refusalDone = true
		}
	}
	if !refusalDelta || !refusalDone {
		t.Fatalf("missing typed refusal events: %+v", frames)
	}
}

func TestResponsesStreamWriterEmitsWarningsOnlyOnCreated(t *testing.T) {
	t.Parallel()
	warnings := []adaptercompat.CompatibilityWarning{{Code: "field_omitted", Param: "temperature", Disposition: "omitted", Message: "temperature omitted"}}
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_warning", "model", warnings, slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}
	if err := writer.finish(adapterprovider.Result{}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	frames := parseResponsesStreamFrames(t, rec.Body.String())
	for _, frame := range frames {
		if frame.Response == nil {
			continue
		}
		gotWarnings := 0
		if frame.Response.Clyde != nil {
			gotWarnings = len(frame.Response.Clyde.Warnings)
		}
		if frame.Name == "response.created" && gotWarnings != 1 {
			t.Fatalf("created warnings=%d want 1", gotWarnings)
		}
		if frame.Name != "response.created" && gotWarnings != 0 {
			t.Fatalf("%s warnings=%d want 0", frame.Name, gotWarnings)
		}
	}
}

func TestResponsesStreamWriterFailureUsesMappedErrorAndIsTerminal(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_failure", "model", nil, slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}
	if err := writer.fail(adapterErrUpstreamFailed("codex", "client-visible failure", nil)); err != nil {
		t.Fatalf("fail: %v", err)
	}
	frames := parseResponsesStreamFrames(t, rec.Body.String())
	terminal := frames[len(frames)-1]
	if terminal.Name != "response.failed" || terminal.Response == nil || terminal.Response.Status != adapteropenai.ResponsesStatusFailed {
		t.Fatalf("terminal failure = %+v", terminal)
	}
	if terminal.Response.Error == nil || terminal.Response.Error.Code != "upstream_failed" || terminal.Response.Error.Message != "client-visible failure" {
		t.Fatalf("terminal error = %+v", terminal.Response.Error)
	}
	if strings.Contains(rec.Body.String(), "response.completed") || strings.Contains(rec.Body.String(), "response.incomplete") || strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("failure stream emitted another terminal frame: %s", rec.Body.String())
	}
}

func TestResponsesStreamWriterUsesContentFilterIncompleteTerminal(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_filtered", "model", nil, slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}
	if err := writer.finish(adapterprovider.Result{FinishReason: "content_filter"}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	frames := parseResponsesStreamFrames(t, rec.Body.String())
	terminal := frames[len(frames)-1]
	if terminal.Name != "response.incomplete" || terminal.Response == nil {
		t.Fatalf("terminal=%+v", terminal)
	}
	if terminal.Response.Status != adapteropenai.ResponsesStatusIncomplete || terminal.Response.IncompleteDetails == nil || terminal.Response.IncompleteDetails.Reason != "content_filter" {
		t.Fatalf("incomplete response=%+v", terminal.Response)
	}
	if strings.Contains(rec.Body.String(), "response.completed") {
		t.Fatalf("content-filter stream emitted response.completed: %s", rec.Body.String())
	}
}

func TestResponsesOutputFromEventsPreservesNormalizedOutputOrder(t *testing.T) {
	t.Parallel()
	output := responsesOutputFromEvents("resp_ordered", []adapterrender.Event{
		adapterrender.TextDelta{Text: "text"},
		adapterrender.ReasoningDelta{Text: "reasoning", ReasoningKind: "summary"},
		adapterrender.RefusalDelta{Text: "refusal"},
		adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
			Index: 0, ID: "upstream-call", Type: "function", Function: adapteropenai.ToolCallFunction{Name: "lookup", Arguments: "{}"},
		}}},
	})
	if len(output) != 3 {
		t.Fatalf("output len=%d want 3", len(output))
	}
	if output[0].Type != "message" || output[1].Type != "reasoning" || output[2].Type != "function_call" {
		t.Fatalf("output types=%q,%q,%q", output[0].Type, output[1].Type, output[2].Type)
	}
	if len(output[0].Content) != 2 || output[0].Content[1].Type != "refusal" {
		t.Fatalf("message content=%+v", output[0].Content)
	}
	if output[2].CallID != "call_ordered_0" {
		t.Fatalf("call id=%q", output[2].CallID)
	}
}

func TestResponsesStreamWriterFunctionArgsDoneCarriesFinalNameAndArguments(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_tool_done", "model", nil, slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
		Index: 0, Type: "function", Function: adapteropenai.ToolCallFunction{Name: "draft_name", Arguments: `{"city":`},
	}}}); err != nil {
		t.Fatalf("tool delta 1: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
		Index: 0, Type: "function", Function: adapteropenai.ToolCallFunction{Name: "get_weather", Arguments: `"Paris"}`},
	}}}); err != nil {
		t.Fatalf("tool delta 2: %v", err)
	}
	if err := writer.finish(adapterprovider.Result{}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	frames := parseResponsesStreamFrames(t, rec.Body.String())
	for _, frame := range frames {
		if frame.Name != adapteropenai.ResponsesEventFunctionArgsDone {
			continue
		}
		if frame.ItemID != "fc_tool_done_0" || frame.OutputIndex != 0 {
			t.Fatalf("function done identity=%+v", frame)
		}
		if frame.FunctionName != "get_weather" || frame.Arguments != `{"city":"Paris"}` {
			t.Fatalf("function done payload=%+v", frame)
		}
		return
	}
	t.Fatalf("missing function_call_arguments.done: %s", rec.Body.String())
}

func TestResponsesStreamWriterToolOnlyFailureKeepsUnfinishedItemInProgress(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_tool_failure", "model", nil, slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
		Index: 0, Type: "function", Function: adapteropenai.ToolCallFunction{Name: "lookup", Arguments: `{"q":"x"}`},
	}}}); err != nil {
		t.Fatalf("tool delta: %v", err)
	}
	if err := writer.fail(adapterErrUpstreamFailed("codex", "provider failed", nil)); err != nil {
		t.Fatalf("fail: %v", err)
	}

	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("failure stream emitted [DONE]: %s", body)
	}
	frames := parseResponsesStreamFrames(t, body)
	for _, frame := range frames {
		if frame.Name == adapteropenai.ResponsesEventFunctionArgsDone || frame.Name == adapteropenai.ResponsesEventOutputItemDone {
			t.Fatalf("failure stream emitted tool completion frame %q: %s", frame.Name, body)
		}
	}
	terminal := frames[len(frames)-1]
	if terminal.Name != adapteropenai.ResponsesEventFailed || terminal.Response == nil {
		t.Fatalf("terminal=%+v", terminal)
	}
	if len(terminal.Response.Output) != 1 || terminal.Response.Output[0].Type != "function_call" || terminal.Response.Output[0].Status != "in_progress" {
		t.Fatalf("failed output=%+v", terminal.Response.Output)
	}
}

func TestResponsesStreamWriterSuccessfulToolCompletionClosesItem(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	writer, err := newResponsesStreamWriter(rec, "resp_tool_success", "model", nil, slog.Default())
	if err != nil {
		t.Fatalf("new responses stream writer: %v", err)
	}
	if err := writer.WriteEvent(adapterrender.ToolCallDelta{ToolCalls: []adapteropenai.ToolCall{{
		Index: 0, Type: "function", Function: adapteropenai.ToolCallFunction{Name: "lookup", Arguments: "{}"},
	}}}); err != nil {
		t.Fatalf("tool delta: %v", err)
	}
	if err := writer.finish(adapterprovider.Result{}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	body := rec.Body.String()
	frames := parseResponsesStreamFrames(t, body)
	functionDone := false
	itemDone := false
	for _, frame := range frames {
		if frame.Name == adapteropenai.ResponsesEventFunctionArgsDone {
			functionDone = true
		}
		if frame.Name == adapteropenai.ResponsesEventOutputItemDone {
			itemDone = true
		}
	}
	if !functionDone || !itemDone {
		t.Fatalf("successful tool stream omitted completion frames: %s", body)
	}
	terminal := frames[len(frames)-1]
	if terminal.Name != adapteropenai.ResponsesEventCompleted || terminal.Response == nil || len(terminal.Response.Output) != 1 || terminal.Response.Output[0].Status != "completed" {
		t.Fatalf("successful terminal=%+v", terminal)
	}
}
