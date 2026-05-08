package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// Phase 10 relocation: these tests live next to the parser implementation
// because they assert backend-local ParseSSE behavior.

func TestParseSSERetainsReasoningSignalWithoutVisibleText(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"delta":"Answer."}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":7}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	got, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if !res.ReasoningSignaled {
		t.Fatalf("expected reasoning signal")
	}
	if !res.ReasoningVisible {
		t.Fatalf("expected synthetic visible reasoning marker")
	}
	if !strings.Contains(got, "Answer.") {
		t.Fatalf("missing streamed answer: %q", got)
	}
	if !strings.Contains(got, "<!--clyde-thinking") || !strings.Contains(got, "<!--/clyde-thinking-->") {
		t.Fatalf("missing synthetic thinking envelope: %q", got)
	}
}

func TestParseSSEEmitsThinkingWhenReasoningItemStarts(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","sequence_number":1,"item":{"id":"rs_1","type":"reasoning","summary":[],"content":null,"encrypted_content":"enc"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"delta":"Answer."}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":7}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	got, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if !res.ReasoningSignaled {
		t.Fatalf("expected reasoning signal")
	}
	thinking := strings.Index(got, "<!--clyde-thinking")
	answer := strings.Index(got, "Answer.")
	if thinking < 0 {
		t.Fatalf("missing synthetic thinking envelope: %q", got)
	}
	if strings.Contains(got, "\n> Thinking...") {
		t.Fatalf("thinking block should not include placeholder body: %q", got)
	}
	if answer < 0 {
		t.Fatalf("missing answer: %q", got)
	}
	if thinking > answer {
		t.Fatalf("thinking marker should precede answer, got %q", got)
	}
	if strings.Count(got, "<!--clyde-thinking") != 1 {
		t.Fatalf("thinking marker duplicated: %q", got)
	}
}

func TestParseSSEEmitsDataRefOnCodexThinkingOpenMarker(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","sequence_number":1,"item":{"id":"rs_phase5_42","type":"reasoning","summary":[],"content":null,"encrypted_content":"enc"}}`,
		"",
		"event: response.reasoning_summary_part.added",
		`data: {"type":"response.reasoning_summary_part.added","sequence_number":2,"summary_index":0,"item_id":"rs_phase5_42"}`,
		"",
		"event: response.reasoning_summary_text.delta",
		`data: {"type":"response.reasoning_summary_text.delta","sequence_number":3,"summary_index":0,"delta":"Step one.","item_id":"rs_phase5_42"}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","sequence_number":4,"delta":"Final."}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":1}}},"sequence_number":5}`,
		"",
	}, "\n") + "\n")
	got, _, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	want := `<!--clyde-thinking data-ref="rs_phase5_42"-->`
	if !strings.Contains(got, want) {
		t.Fatalf("missing data-ref open marker; got=%q", got)
	}
	if strings.Contains(got, "<!--clyde-thinking-->") {
		t.Fatalf("legacy attribute-less marker leaked alongside the data-ref form: %q", got)
	}
}

func TestParseSSEEmitsReasoningSummaryPartAddedAsVisibleThinking(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","sequence_number":1,"item":{"id":"rs_1","type":"reasoning","summary":[],"content":null,"encrypted_content":"enc"}}`,
		"",
		"event: response.reasoning_summary_part.added",
		`data: {"type":"response.reasoning_summary_part.added","sequence_number":2,"summary_index":0}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","sequence_number":3,"delta":"Answer."}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":7}}},"sequence_number":4}`,
		"",
	}, "\n") + "\n")
	got, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if !res.ReasoningSignaled || !res.ReasoningVisible {
		t.Fatalf("reasoning flags=%+v want signaled and visible", res)
	}
	if strings.Count(got, "<!--clyde-thinking") != 1 {
		t.Fatalf("thinking marker count mismatch: %q", got)
	}
	if strings.Contains(got, "\n> Thinking...") || !strings.Contains(got, "Answer.") {
		t.Fatalf("unexpected thinking placeholder or missing answer: %q", got)
	}
}

func TestParseSSEEmitsReasoningFromDoneItemContent(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","sequence_number":1,"item":{"id":"rs_1","type":"reasoning","summary":[],"content":null,"encrypted_content":"enc"}}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","sequence_number":2,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"Checked constraints."}],"content":[{"type":"reasoning_text","text":"Raw reasoning detail."},{"type":"text","text":"Additional note."}],"encrypted_content":"enc"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","sequence_number":3,"delta":"Final answer."}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":7}}},"sequence_number":4}`,
		"",
	}, "\n") + "\n")
	got, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if !res.ReasoningSignaled || !res.ReasoningVisible {
		t.Fatalf("reasoning flags=%+v want signaled and visible", res)
	}
	for _, want := range []string{"Checked constraints.", "Raw reasoning detail.", "Additional note.", "Final answer."} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, "\n> Thinking...") {
		t.Fatalf("thinking block should not include placeholder body: %q", got)
	}
	// The close marker may carry an optional `data-encrypted` attribute
	// when the reasoning item brought an encrypted_content blob; either
	// the bare or the encrypted-bearing form counts as a close.
	closeCount := strings.Count(got, "<!--/clyde-thinking-->") + strings.Count(got, "<!--/clyde-thinking data-encrypted=")
	if strings.Count(got, "<!--clyde-thinking") != 1 || closeCount != 1 {
		t.Fatalf("thinking envelope count mismatch: %q", got)
	}
}

func TestParseSSEMapsIncompleteResponseToLengthFinishReason(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"delta":"Partial answer."}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.FinishReason != "length" {
		t.Fatalf("finish_reason=%q want length", res.FinishReason)
	}
}

func TestParseSSEUsageTelemetryNonzeroCachedTokens(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":64},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":108}}}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.Usage.PromptTokens != 100 || res.Usage.CompletionTokens != 8 || res.Usage.TotalTokens != 108 {
		t.Fatalf("usage=%+v", res.Usage)
	}
	if res.Usage.PromptTokensDetails == nil || res.Usage.PromptTokensDetails.CachedTokens != 64 {
		t.Fatalf("prompt token details=%+v want cached_tokens=64", res.Usage.PromptTokensDetails)
	}
	if !res.UsageTelemetry.UsagePresent || !res.UsageTelemetry.InputTokensDetailsPresent {
		t.Fatalf("usage telemetry missing presence bits: %+v", res.UsageTelemetry)
	}
	if res.UsageTelemetry.CachedTokens != 64 || res.UsageTelemetry.ReasoningOutputTokens != 3 {
		t.Fatalf("usage telemetry=%+v", res.UsageTelemetry)
	}
	if !res.UsageTelemetry.OutputTokensDetailsPresent {
		t.Fatalf("expected output_tokens_details_present")
	}
}

func TestParseSSEUsageTelemetryExplicitZeroCachedTokens(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":0},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":108}}}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.Usage.PromptTokensDetails == nil {
		t.Fatalf("explicit zero cached_tokens should preserve prompt token details")
	}
	if res.Usage.PromptTokensDetails.CachedTokens != 0 {
		t.Fatalf("cached_tokens=%d want 0", res.Usage.PromptTokensDetails.CachedTokens)
	}
	if !res.UsageTelemetry.InputTokensDetailsPresent || res.UsageTelemetry.CachedTokens != 0 {
		t.Fatalf("usage telemetry=%+v want explicit zero details", res.UsageTelemetry)
	}
}

func TestParseSSEUsageTelemetryOmittedInputTokenDetails(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"output_tokens":8,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":108}}}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.Usage.PromptTokensDetails != nil {
		t.Fatalf("omitted input_tokens_details should not synthesize prompt details: %+v", res.Usage.PromptTokensDetails)
	}
	if !res.UsageTelemetry.UsagePresent {
		t.Fatalf("usage_present=false want true")
	}
	if res.UsageTelemetry.InputTokensDetailsPresent {
		t.Fatalf("input_tokens_details_present=true want false")
	}
	if res.UsageTelemetry.CachedTokens != 0 {
		t.Fatalf("cached_tokens=%d want 0", res.UsageTelemetry.CachedTokens)
	}
}

func TestParseSSEUsageTelemetryNullDetails(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"input_tokens_details":null,"output_tokens":8,"output_tokens_details":null,"total_tokens":108}}}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.Usage.PromptTokensDetails != nil {
		t.Fatalf("null input_tokens_details should not synthesize prompt details: %+v", res.Usage.PromptTokensDetails)
	}
	if res.UsageTelemetry.InputTokensDetailsPresent || res.UsageTelemetry.OutputTokensDetailsPresent {
		t.Fatalf("details presence should be false for null details: %+v", res.UsageTelemetry)
	}
}

func TestParseSSEUsageTelemetryOmittedUsage(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1"}}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.UsageTelemetry.UsagePresent {
		t.Fatalf("usage_present=true want false")
	}
	if res.Usage.PromptTokens != 0 || res.Usage.CompletionTokens != 0 || res.Usage.TotalTokens != 0 {
		t.Fatalf("usage=%+v want zero value", res.Usage)
	}
}

func TestParseSSEMapsContextWindowFailureToTypedError(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.failed",
		`data: {"type":"response.failed","error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}`,
		"",
	}, "\n") + "\n")
	_, _, err := collectSSE(stream)
	if err == nil {
		t.Fatalf("ParseSSE error = nil, want context window error")
	}
	var contextErr *ContextWindowError
	if !errors.As(err, &contextErr) {
		t.Fatalf("ParseSSE error type = %T, want ContextWindowError", err)
	}
	if contextErr.Error() != "Your input exceeds the context window of this model. Please adjust your input and try again." {
		t.Fatalf("context error = %q", contextErr.Error())
	}
}

func TestParseSSEMapsUnsupportedModelFailureToTypedError(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.failed",
		`data: {"type":"response.failed","error":{"message":"The '5.5' model is not supported when using Codex with a ChatGPT account."}}`,
		"",
	}, "\n") + "\n")
	_, _, err := collectSSE(stream)
	if err == nil {
		t.Fatalf("ParseSSE error = nil, want unsupported model error")
	}
	var unsupportedErr *UnsupportedModelError
	if !errors.As(err, &unsupportedErr) {
		t.Fatalf("ParseSSE error type = %T, want UnsupportedModelError", err)
	}
	if unsupportedErr.Error() != "The '5.5' model is not supported when using Codex with a ChatGPT account." {
		t.Fatalf("unsupported model error = %q", unsupportedErr.Error())
	}
}

func TestParseSSEDoesNotEmitCleanupChunkForContextWindowFailure(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.reasoning_summary_text.delta",
		`data: {"delta":"thinking"}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}`,
		"",
	}, "\n") + "\n")
	chunks, _, err := parseSSEChunksForTest(stream)
	if err == nil {
		t.Fatalf("ParseSSE error = nil, want context window error")
	}
	var contextErr *ContextWindowError
	if !errors.As(err, &contextErr) {
		t.Fatalf("ParseSSE error type = %T, want ContextWindowError", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks len=%d want 1 reasoning delta only", len(chunks))
	}
}

func TestParseSSECapturesCompletedOutputItems(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.done",
		`data: {"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Assistant output."}]}}`,
		"",
		"event: response.output_item.done",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if len(res.OutputItems) != 2 {
		t.Fatalf("output_items len=%d want 2: %#v", len(res.OutputItems), res.OutputItems)
	}
	if got, _ := res.OutputItems[0]["id"].(string); got != "msg_1" {
		t.Fatalf("first output item id=%q want msg_1", got)
	}
	if got, _ := res.OutputItems[1]["type"].(string); got != "function_call" {
		t.Fatalf("second output item type=%q want function_call", got)
	}
	if got, _ := res.OutputItems[1]["name"].(string); got != "read_file" {
		t.Fatalf("second output item name=%q want read_file", got)
	}
}

func TestParseSSEStoresReconstructedFunctionCallArguments(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_shell","name":"shell_command","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"fc_1","delta":"{\"command\":\"pwd\","}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"fc_1","delta":"\"workdir\":\"/private/tmp/clyde-cursor-smoke-ws\"}"}`,
		"",
		"event: response.output_item.done",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_shell","name":"shell_command","arguments":""}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if len(res.OutputItems) != 1 {
		t.Fatalf("output_items len=%d want 1: %#v", len(res.OutputItems), res.OutputItems)
	}
	args, _ := res.OutputItems[0]["arguments"].(string)
	if !strings.Contains(args, `"command":"pwd"`) || !strings.Contains(args, `"workdir":"/private/tmp/clyde-cursor-smoke-ws"`) {
		t.Fatalf("arguments=%q", args)
	}
}

func TestParseSSERetainsResponseIDFromCreatedEvent(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.created",
		`data: {"response":{"id":"resp-created"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		"",
	}, "\n") + "\n")
	_, res, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.ResponseID != "resp-created" {
		t.Fatalf("response_id=%q want resp-created", res.ResponseID)
	}
}

func TestParseSSEEmitsToolCallDeltas(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"fc_1","delta":"{\"path\":\"out.md\"}"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	got, res, err := parseSSEChunksForTest(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q want tool_calls", res.FinishReason)
	}
	var deltas []adapteropenai.ToolCall
	for _, ch := range got {
		if len(ch.Choices) == 0 {
			continue
		}
		deltas = append(deltas, ch.Choices[0].Delta.ToolCalls...)
	}
	if len(deltas) < 2 {
		t.Fatalf("tool delta len=%d want >=2", len(deltas))
	}
	if deltas[0].Function.Name != "read_file" {
		t.Fatalf("first tool name=%q want read_file", deltas[0].Function.Name)
	}
	if deltas[1].Function.Arguments != `{"path":"out.md"}` {
		t.Fatalf("second args=%q", deltas[1].Function.Arguments)
	}
}

func TestParseSSETracksSubagentToolCalls(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Task","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"fc_1","delta":"{\"prompt\":\"inspect\",\"run_in_background\":true}"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	got, res, err := parseSSEChunksForTest(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q want tool_calls", res.FinishReason)
	}
	if res.ToolCallCount != 1 {
		t.Fatalf("tool_call_count=%d want 1", res.ToolCallCount)
	}
	if !res.HasSubagentToolCall {
		t.Fatalf("HasSubagentToolCall=false want true")
	}
	if got := strings.Join(res.ToolCallNames, ","); got != "Task" {
		t.Fatalf("ToolCallNames=%q want Task", got)
	}
	calls := collectToolCallsLocal(got)
	if len(calls) == 0 || calls[0].Function.Name != "Task" {
		t.Fatalf("tool calls=%#v want first name Task", calls)
	}
}

func TestParseSSETracksCursorSubagentToolCalls(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.done",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Subagent","arguments":"{\"prompt\":\"inspect\",\"run_in_background\":true}"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	got, res, err := parseSSEChunksForTest(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if !res.HasSubagentToolCall {
		t.Fatalf("HasSubagentToolCall=false want true")
	}
	if got := strings.Join(res.ToolCallNames, ","); got != "Subagent" {
		t.Fatalf("ToolCallNames=%q want Subagent", got)
	}
	calls := collectToolCallsLocal(got)
	if len(calls) == 0 || calls[0].Function.Name != "Subagent" {
		t.Fatalf("tool calls=%#v want first name Subagent", calls)
	}
}

func TestParseSSEPreservesLegacySpawnAgentName(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"fc_1","delta":"{\"prompt\":\"inspect\",\"run_in_background\":true}"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	got, res, err := parseSSEChunksForTest(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if !res.HasSubagentToolCall {
		t.Fatalf("HasSubagentToolCall=false want true")
	}
	if got := strings.Join(res.ToolCallNames, ","); got != "spawn_agent" {
		t.Fatalf("ToolCallNames=%q want spawn_agent", got)
	}
	calls := collectToolCallsLocal(got)
	if len(calls) == 0 || calls[0].Function.Name != "spawn_agent" {
		t.Fatalf("tool calls=%#v want first name spawn_agent", calls)
	}
}

func TestParseSSEDoesNotMarkOrdinaryToolCallsAsSubagent(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"Read","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"fc_1","delta":"{\"path\":\"README.md\"}"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	_, res, err := parseSSEChunksForTest(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.HasSubagentToolCall {
		t.Fatalf("HasSubagentToolCall=true want false")
	}
	if got := strings.Join(res.ToolCallNames, ","); got != "Read" {
		t.Fatalf("ToolCallNames=%q want Read", got)
	}
}

func TestParseSSEMapsNativeLocalShellToCursorShell(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.done",
		`data: {"item":{"id":"ls_1","type":"local_shell_call","call_id":"call_shell","status":"completed","action":{"type":"exec","command":["zsh","-lc","pwd"],"working_directory":"/repo","timeout_ms":1000}}}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		"",
	}, "\n") + "\n")
	got, res, err := parseSSEChunksForTest(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q", res.FinishReason)
	}
	calls := collectToolCallsLocal(got)
	if len(calls) != 2 {
		t.Fatalf("tool call chunks=%d want 2: %#v", len(calls), calls)
	}
	if calls[0].Function.Name != "Shell" {
		t.Fatalf("tool name=%q want Shell", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[1].Function.Arguments), &args); err != nil {
		t.Fatalf("args JSON: %v", err)
	}
	if args["command"] != "pwd" || args["working_directory"] != "/repo" || args["block_until_ms"].(float64) != 1000 {
		t.Fatalf("args=%v", args)
	}
}

func TestParseSSEMapsNativeApplyPatchToCursorApplyPatch(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: out.md\n+ok\n*** End Patch\n"
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"ct_1","type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""}}`,
		"",
		"event: response.custom_tool_call_input.delta",
		`data: {"item_id":"ct_1","call_id":"call_patch","delta":"*** Begin Patch\n"}`,
		"",
		"event: response.custom_tool_call_input.delta",
		`data: {"item_id":"ct_1","call_id":"call_patch","delta":"*** Add File: out.md\n+ok\n*** End Patch\n"}`,
		"",
		"event: response.output_item.done",
		`data: {"item":{"id":"ct_1","type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""}}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		"",
	}, "\n") + "\n")
	got, res, err := parseSSEChunksForTest(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q", res.FinishReason)
	}
	calls := collectToolCallsLocal(got)
	if len(calls) != 3 {
		t.Fatalf("tool call chunks=%d want 3: %#v", len(calls), calls)
	}
	if calls[0].Function.Name != "ApplyPatch" {
		t.Fatalf("tool name=%q want ApplyPatch", calls[0].Function.Name)
	}
	if gotPatch := calls[1].Function.Arguments + calls[2].Function.Arguments; gotPatch != patch {
		t.Fatalf("patch args=%q want %q", gotPatch, patch)
	}
}

func TestParseSSELogsSuccessfulNativeLocalShellItem(t *testing.T) {
	var logBuffer bytes.Buffer
	restore := captureDefaultDebugLogs(t, &logBuffer)
	defer restore()

	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.done",
		`data: {"item":{"id":"ls_1","type":"local_shell_call","call_id":"call_shell","status":"completed","action":{"type":"exec","command":["zsh","-lc","pwd"],"working_directory":"/repo","timeout_ms":1000}}}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		"",
	}, "\n") + "\n")
	if _, _, err := parseSSEChunksForTestWithLog(stream, sseInstrumentationContext{RequestID: "req_native_shell"}); err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}

	logs := nativeToolParsedLogs(t, logBuffer.Bytes())
	if len(logs) != 1 {
		t.Fatalf("native parsed logs=%d want 1: %#v", len(logs), logs)
	}
	got := logs[0]
	if got.RequestID != "req_native_shell" || got.SSEEvent != "response.output_item.done" ||
		got.ItemType != "local_shell_call" || got.ItemID != "ls_1" ||
		got.CallID != "call_shell" || got.ToolName != "Shell" {
		t.Fatalf("native parsed log=%+v", got)
	}
}

func TestParseSSELogsSuccessfulNativeCustomToolItem(t *testing.T) {
	var logBuffer bytes.Buffer
	restore := captureDefaultDebugLogs(t, &logBuffer)
	defer restore()

	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"ct_1","type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""}}`,
		"",
		"event: response.custom_tool_call_input.delta",
		`data: {"item_id":"ct_1","call_id":"call_patch","delta":"*** Begin Patch\n"}`,
		"",
		"event: response.custom_tool_call_input.delta",
		`data: {"item_id":"ct_1","call_id":"call_patch","delta":"*** Add File: out.md\n+ok\n*** End Patch\n"}`,
		"",
		"event: response.output_item.done",
		`data: {"item":{"id":"ct_1","type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":""}}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		"",
	}, "\n") + "\n")
	if _, _, err := parseSSEChunksForTestWithLog(stream, sseInstrumentationContext{RequestID: "req_native_custom"}); err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}

	logs := nativeToolParsedLogs(t, logBuffer.Bytes())
	if len(logs) != 1 {
		t.Fatalf("native parsed logs=%d want 1: %#v", len(logs), logs)
	}
	got := logs[0]
	if got.RequestID != "req_native_custom" || got.SSEEvent != "response.output_item.done" ||
		got.ItemType != "custom_tool_call" || got.ItemID != "ct_1" ||
		got.CallID != "call_patch" || got.ToolName != "ApplyPatch" {
		t.Fatalf("native parsed log=%+v", got)
	}
}

func TestParseSSEDoesNotLogNormalFunctionCallItemParsing(t *testing.T) {
	var logBuffer bytes.Buffer
	restore := captureDefaultDebugLogs(t, &logBuffer)
	defer restore()

	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":""}}`,
		"",
		"event: response.output_item.done",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"README.md\"}"}}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}}`,
		"",
	}, "\n") + "\n")
	if _, _, err := parseSSEChunksForTestWithLog(stream, sseInstrumentationContext{}); err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if logs := nativeToolParsedLogs(t, logBuffer.Bytes()); len(logs) != 0 {
		t.Fatalf("native parsed logs=%d want 0: %#v", len(logs), logs)
	}
}

func TestParseSSESeparatesSummaryFromReasoningBody(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.reasoning_summary_text.delta",
		`data: {"type":"response.reasoning_summary_text.delta","delta":"Exploring pet-color constraints","summary_index":0}`,
		"",
		"event: response.reasoning_text.delta",
		`data: {"type":"response.reasoning_text.delta","delta":"I am checking combinations.","content_index":0}`,
		"",
		"event: response.output_text.delta",
		`data: {"delta":"Final answer."}`,
		"",
		"event: response.completed",
		`data: {"response":{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":7}}}}`,
		"",
	}, "\n") + "\n")
	got, _, err := collectSSE(stream)
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if !strings.Contains(got, "Exploring pet-color constraints\n> \n> I am checking combinations.") {
		t.Fatalf("expected blank-line-separated reasoning sections, got %q", got)
	}
	if !strings.Contains(got, "Final answer.") {
		t.Fatalf("missing final answer: %q", got)
	}
}

func TestParseSSEEventsEmitsNormalizedSequence(t *testing.T) {
	stream := strings.NewReader(strings.Join([]string{
		"event: response.output_item.added",
		`data: {"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"read_file","arguments":""}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"item_id":"fc_1","delta":"{\"path\":\"out.md\"}"}`,
		"",
		"event: response.reasoning_text.delta",
		`data: {"delta":"thinking..."}`,
		"",
		"event: response.output_text.delta",
		`data: {"delta":"done"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":1}}},"sequence_number":10}`,
		"",
	}, "\n") + "\n")
	var events []adapterrender.Event
	res, err := ParseSSEEventsWithOptions(context.Background(), stream, func(ev adapterrender.Event) error {
		events = append(events, ev)
		return nil
	}, sseInstrumentationContext{}, SSEParseOptions{DropEncryptedContent: false})
	if err != nil {
		t.Fatalf("ParseSSEEvents: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason=%q want tool_calls", res.FinishReason)
	}
	if !res.ReasoningSignaled || !res.ReasoningVisible {
		t.Fatalf("reasoning flags = %+v", res)
	}
	if len(events) != 5 {
		t.Fatalf("events len=%d want 5", len(events))
	}
	tc0, ok0 := events[0].(adapterrender.ToolCallDelta)
	if !ok0 {
		t.Fatalf("events[0] type=%T want ToolCallDelta", events[0])
	}
	if tc0.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("events[0] tool name=%q want read_file", tc0.ToolCalls[0].Function.Name)
	}
	tc1, ok1 := events[1].(adapterrender.ToolCallDelta)
	if !ok1 || tc1.ToolCalls[0].Function.Arguments != `{"path":"out.md"}` {
		t.Fatalf("events[1]=%+v", events[1])
	}
	if _, ok := events[2].(adapterrender.ReasoningDelta); !ok {
		t.Fatalf("events[2] type=%T want ReasoningDelta", events[2])
	}
	td3, ok3 := events[3].(adapterrender.TextDelta)
	if !ok3 || td3.Text != "done" {
		t.Fatalf("events[3]=%+v", events[3])
	}
	if _, ok := events[4].(adapterrender.ReasoningFinished); !ok {
		t.Fatalf("events[4] type=%T want ReasoningFinished", events[4])
	}
}

func TestCanonicalContinuationPreservesFunctionCallName(t *testing.T) {
	item := map[string]any{
		"type":      "function_call",
		"call_id":   "call_1",
		"name":      " read_file ",
		"arguments": `{"path":"README.md"}`,
	}
	event, ok := canonicalContinuationEvent(item)
	if !ok {
		t.Fatalf("canonical event not recognized")
	}
	if event.Name != "read_file" {
		t.Fatalf("event name=%q want read_file", event.Name)
	}
	canonical := canonicalContinuationItem(item)
	if got, _ := canonical["name"].(string); got != "read_file" {
		t.Fatalf("canonical item name=%q want read_file", got)
	}
}

func TestCanonicalContinuationDoesNotEquateMappedToolNames(t *testing.T) {
	readFile := map[string]any{
		"type":      "function_call",
		"name":      "read_file",
		"arguments": `{"path":"README.md"}`,
	}
	read := map[string]any{
		"type":      "function_call",
		"name":      "Read",
		"arguments": `{"path":"README.md"}`,
	}
	if continuationItemEqual(readFile, read) {
		t.Fatalf("read_file and Read should not compare equal")
	}
}

func collectSSE(stream *strings.Reader) (string, RunResult, error) {
	r := adapterrender.NewEventRenderer("req", "alias", "codex", nil)
	var got strings.Builder
	res, err := ParseSSEEventsWithOptions(context.Background(), stream, func(ev adapterrender.Event) error {
		for _, ch := range r.HandleEvent(ev) {
			if len(ch.Choices) > 0 {
				got.WriteString(ch.Choices[0].Delta.Content)
			}
		}
		return nil
	}, sseInstrumentationContext{}, SSEParseOptions{DropEncryptedContent: false})
	return got.String(), res, err
}

func parseSSEChunksForTest(stream *strings.Reader) ([]adapteropenai.StreamChunk, RunResult, error) {
	return parseSSEChunksForTestWithLog(stream, sseInstrumentationContext{})
}

func parseSSEChunksForTestWithLog(stream *strings.Reader, logCtx sseInstrumentationContext) ([]adapteropenai.StreamChunk, RunResult, error) {
	r := adapterrender.NewEventRenderer("req", "alias", "codex", nil)
	var got []adapteropenai.StreamChunk
	res, err := ParseSSEEventsWithOptions(context.Background(), stream, func(ev adapterrender.Event) error {
		got = append(got, r.HandleEvent(ev)...)
		return nil
	}, logCtx, SSEParseOptions{DropEncryptedContent: false})
	return got, res, err
}

func collectToolCallsLocal(chunks []adapteropenai.StreamChunk) []adapteropenai.ToolCall {
	var out []adapteropenai.ToolCall
	for _, ch := range chunks {
		if len(ch.Choices) == 0 {
			continue
		}
		out = append(out, ch.Choices[0].Delta.ToolCalls...)
	}
	return out
}

type nativeToolParsedLog struct {
	Message   string `json:"msg"`
	Event     string `json:"event"`
	RequestID string `json:"request_id"`
	SSEEvent  string `json:"sse_event"`
	ItemType  string `json:"item_type"`
	ItemID    string `json:"item_id"`
	CallID    string `json:"call_id"`
	ToolName  string `json:"tool_name"`
}

func captureDefaultDebugLogs(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() {
		slog.SetDefault(previous)
	}
}

func nativeToolParsedLogs(t *testing.T, data []byte) []nativeToolParsedLog {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	var out []nativeToolParsedLog
	for {
		var record nativeToolParsedLog
		if err := dec.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				return out
			}
			t.Fatalf("decode log record: %v\n%s", err, string(data))
		}
		if record.Message == "adapter.codex.tooling.event" && record.Event == "native_tool_item.parsed" {
			out = append(out, record)
		}
	}
}
