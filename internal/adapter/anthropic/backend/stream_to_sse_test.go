package anthropicbackend

import (
	"context"
	"encoding/json"
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
	model := adaptermodel.ResolvedModel{Alias: "clyde-sonnet-4.6-medium-thinking", ClaudeModel: "claude-sonnet-4-6"}

	emit := func(ev adapterrender.Event) error {
		return dispatcher.WriteEvent(ev)
	}
	_, err := RunStreamExecution(dispatcher, context.Background(), req, model, "req-stream-test", time.Now(), "tracker", emit)
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
			if strings.Contains(choice.Delta.Content, "<!--clyde-thinking-->") {
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
