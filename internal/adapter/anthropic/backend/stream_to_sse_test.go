package anthropicbackend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// TestStreamPipelineDeliversToolCallsAndThinking is the structural lock
// for the regression where Cursor receives only assistant text and
// neither tool_calls nor thinking blocks. The test feeds a synthetic
// Anthropic stream that contains a thinking block, a text block, and a
// tool_use block with arguments. It runs the full provider stream path
// from RunStreamExecution down through the renderer to StreamChunks
// and inspects what the renderer produced. Fail fast if any of the
// four wire signals goes missing.
//
// Signals asserted on the captured chunks:
//
//   - At least one chunk has a non-empty delta.tool_calls with a
//     function name set.
//   - At least one chunk has non-empty delta.tool_calls with arguments.
//   - At least one chunk's delta.content contains <!--clyde-thinking-->.
//   - At least one chunk's delta.content contains <!--/clyde-thinking-->.
func TestStreamPipelineDeliversToolCallsAndThinking(t *testing.T) {
	t.Parallel()

	dispatcher := &fakeResponseDispatcher{}
	dispatcher.streamEvents = func(_ context.Context, _ anthropic.Request, sink anthropic.EventSink) (anthropic.Usage, string, error) {
		emit := func(ev anthropic.StreamEvent) error {
			return sink(ev)
		}

		if err := emit(anthropic.StreamThinkingStart{BlockIndex: 0}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamThinkingDelta{BlockIndex: 0, Text: "considering the request"}); err != nil {
			return anthropic.Usage{}, "", err
		}

		if err := emit(anthropic.StreamTextDelta{BlockIndex: 1, Text: "Reading the file."}); err != nil {
			return anthropic.Usage{}, "", err
		}

		if err := emit(anthropic.StreamToolUseStart{BlockIndex: 2, ToolUseID: "tu_1", ToolUseName: "Read"}); err != nil {
			return anthropic.Usage{}, "", err
		}
		// Anthropic's first input_json_delta for a tool_use block is
		// often empty. Forward this to exercise the wire shape Cursor
		// sees in production. The forward fix in stream_parse.go must
		// drop the empty delta before it reaches the renderer.
		if err := emit(anthropic.StreamToolUseArgDelta{BlockIndex: 2, PartialJSON: ""}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamToolUseArgDelta{BlockIndex: 2, PartialJSON: `{"path":"`}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamToolUseArgDelta{BlockIndex: 2, PartialJSON: `/tmp/file.txt"}`}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamToolUseStop{BlockIndex: 2}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamStop{StopReason: "tool_use"}); err != nil {
			return anthropic.Usage{}, "", err
		}
		return anthropic.Usage{InputTokens: 100, OutputTokens: 20}, "tool_use", nil
	}

	dispatcher.sseWriter = &fakeResponseSSEWriter{}
	req := anthropic.Request{Model: "claude-sonnet-4-6"}
	resolved := resolvedForTest(adaptermodel.ResolvedAlias{Alias: "clyde-sonnet-4.6-medium-thinking", WireModel: "claude-sonnet-4-6"})

	emit := func(ev adapterrender.Event) error {
		return dispatcher.WriteEvent(ev)
	}
	_, err := RunStreamExecution(dispatcher, context.Background(), req, resolved, "req-stream-test", time.Now(), "tracker", emit)
	if err != nil {
		t.Fatalf("RunStreamExecution: %v", err)
	}
	if err := dispatcher.FlushEventWriter(); err != nil {
		t.Fatalf("FlushEventWriter: %v", err)
	}

	chunks := dispatcher.sseWriter.chunks
	if len(chunks) == 0 {
		t.Fatalf("no chunks produced; pipeline drops everything")
	}

	hasToolName := false
	hasToolArgs := false
	hasThinkingOpen := false
	hasThinkingClose := false
	hasReasoningContent := false
	emptyToolCallChunks := 0
	for _, ch := range chunks {
		for _, choice := range ch.Choices {
			if strings.Contains(choice.Delta.Content, `<!--clyde-thinking data-origin="anthropic"-->`) {
				hasThinkingOpen = true
			}
			if strings.Contains(choice.Delta.Content, "<!--/clyde-thinking-->") {
				hasThinkingClose = true
			}
			if strings.TrimSpace(choice.Delta.ReasoningContent) != "" {
				hasReasoningContent = true
			}
			for _, tc := range choice.Delta.ToolCalls {
				if strings.TrimSpace(tc.Function.Name) != "" {
					hasToolName = true
				}
				if strings.TrimSpace(tc.Function.Arguments) != "" {
					hasToolArgs = true
				}
				// Cursor's OpenAI SSE parser treats a tool_call with no
				// function name and no arguments as a finalize/reset
				// signal. The leading empty input_json_delta from
				// Anthropic must not surface as a chunk on the wire.
				if strings.TrimSpace(tc.Function.Name) == "" &&
					strings.TrimSpace(tc.Function.Arguments) == "" &&
					strings.TrimSpace(tc.ID) == "" {
					emptyToolCallChunks++
				}
			}
		}
	}

	if !hasToolName {
		t.Errorf("missing signal: no chunk with delta.tool_calls function name")
	}
	if !hasToolArgs {
		t.Errorf("missing signal: no chunk with delta.tool_calls arguments")
	}
	if !hasThinkingOpen {
		t.Errorf("missing signal: no chunk with <!--clyde-thinking--> open marker")
	}
	if !hasThinkingClose {
		t.Errorf("missing signal: no chunk with <!--/clyde-thinking--> close marker")
	}
	if !hasReasoningContent {
		t.Errorf("missing signal: no chunk with non-empty delta.reasoning_content (dual-emit probe for native Cursor BYOK reasoning render)")
	}
	if emptyToolCallChunks > 0 {
		t.Errorf("found %d empty tool_call chunks (no name, args, or id); Cursor will drop the tool call", emptyToolCallChunks)
	}

	if t.Failed() {
		for i, ch := range chunks {
			b, _ := json.Marshal(ch)
			t.Logf("chunk %d: %s", i, string(b))
		}
	}
}

// TestRunStreamExecutionRecoversFinishReasonAfterLateError exercises the
// late-stream-error fallback path. The fake upstream emits a complete
// text block, signals a stop reason via StreamStop, and then returns a
// non-nil error to RunStreamExecution. The result returned to the
// dispatcher must still carry the OpenAI-normalized finish_reason
// derived from stop_reason and the cumulative usage so the dispatcher
// can still send a well-formed finalize sequence (finish chunk + usage
// chunk + [DONE]) to Cursor. See research/claude-code-source-code-full/
// src/services/api/claude.ts around lines 2341-2350 and 2818-2869.
func TestRunStreamExecutionRecoversFinishReasonAfterLateError(t *testing.T) {
	t.Parallel()

	dispatcher := &fakeResponseDispatcher{}
	dispatcher.streamEvents = func(_ context.Context, _ anthropic.Request, sink anthropic.EventSink) (anthropic.Usage, string, error) {
		emit := func(ev anthropic.StreamEvent) error {
			return sink(ev)
		}
		if err := emit(anthropic.StreamTextDelta{BlockIndex: 0, Text: "The capital of France is Paris."}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamStop{StopReason: "end_turn"}); err != nil {
			return anthropic.Usage{}, "", err
		}
		// Late error after the answer has fully streamed. Caller code
		// must still be able to finalize the OpenAI SSE turn.
		return anthropic.Usage{InputTokens: 10, OutputTokens: 7}, "end_turn", errors.New("simulated late stream termination")
	}
	dispatcher.sseWriter = &fakeResponseSSEWriter{}

	req := anthropic.Request{Model: "claude-opus-4-7"}
	resolved := resolvedForTest(adaptermodel.ResolvedAlias{Alias: "clyde-opus-4-7", WireModel: "claude-opus-4-7", Context: 200_000})

	emit := func(ev adapterrender.Event) error {
		return dispatcher.WriteEvent(ev)
	}
	result, err := RunStreamExecution(dispatcher, context.Background(), req, resolved, "req-late-err", time.Now(), "tracker", emit)
	if err == nil {
		t.Fatalf("expected late error to propagate, got nil")
	}
	if result.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want \"stop\" (recovered from stop_reason=end_turn)", result.FinishReason)
	}
	if result.Usage.PromptTokens != 10 {
		t.Fatalf("prompt_tokens = %d, want 10", result.Usage.PromptTokens)
	}
	if result.Usage.CompletionTokens != 7 {
		t.Fatalf("completion_tokens = %d, want 7", result.Usage.CompletionTokens)
	}
	if result.Usage.TotalTokens != 17 {
		t.Fatalf("total_tokens = %d, want 17", result.Usage.TotalTokens)
	}
}

// TestRunStreamExecutionRecoversFinishReasonFromToolUseStopReason verifies
// the stop_reason -> finish_reason mapping for tool_use surfaces through
// the late-error fallback path. A turn that ended in a tool call but
// whose transport surfaced a non-nil error must still report
// finish_reason="tool_calls".
func TestRunStreamExecutionRecoversFinishReasonFromToolUseStopReason(t *testing.T) {
	t.Parallel()

	dispatcher := &fakeResponseDispatcher{}
	dispatcher.streamEvents = func(_ context.Context, _ anthropic.Request, sink anthropic.EventSink) (anthropic.Usage, string, error) {
		emit := func(ev anthropic.StreamEvent) error {
			return sink(ev)
		}
		if err := emit(anthropic.StreamTextDelta{BlockIndex: 0, Text: "Calling the tool now."}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamToolUseStart{BlockIndex: 1, ToolUseID: "tu_late", ToolUseName: "Read"}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamToolUseArgDelta{BlockIndex: 1, PartialJSON: `{"path":"/etc/hosts"}`}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamToolUseStop{BlockIndex: 1}); err != nil {
			return anthropic.Usage{}, "", err
		}
		if err := emit(anthropic.StreamStop{StopReason: "tool_use"}); err != nil {
			return anthropic.Usage{}, "", err
		}
		return anthropic.Usage{InputTokens: 50, OutputTokens: 12}, "tool_use", errors.New("late scanner failure")
	}
	dispatcher.sseWriter = &fakeResponseSSEWriter{}

	req := anthropic.Request{Model: "claude-opus-4-7"}
	resolved := resolvedForTest(adaptermodel.ResolvedAlias{Alias: "clyde-opus-4-7", WireModel: "claude-opus-4-7", Context: 200_000})

	emit := func(ev adapterrender.Event) error {
		return dispatcher.WriteEvent(ev)
	}
	result, err := RunStreamExecution(dispatcher, context.Background(), req, resolved, "req-late-tool", time.Now(), "tracker", emit)
	if err == nil {
		t.Fatalf("expected late error to propagate, got nil")
	}
	if result.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want \"tool_calls\"", result.FinishReason)
	}
	if result.ToolCallCount < 1 {
		t.Fatalf("tool_call_count = %d, want >= 1", result.ToolCallCount)
	}
}
