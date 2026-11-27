package antigravity_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/adapter/render"
)

type streamRecorder []render.Event

func (r *streamRecorder) WriteEvent(event render.Event) error {
	*r = append(*r, event)

	return nil
}

func (*streamRecorder) Flush() error { return nil }

func TestGeminiResponseEvents(t *testing.T) {
	input, err := os.Open("testdata/gemini.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var recorder streamRecorder
	result, err := antigravity.ReadStream(t.Context(), input, &recorder)
	if err != nil {
		t.Fatal(err)
	}

	if len(recorder) != 4 {
		t.Fatalf("events = %#v", recorder)
	}

	if recorder[0].(render.ReasoningDelta).Text != "plan" ||
		recorder[1].(render.TextDelta).Text != "answer" {
		t.Fatalf("text events = %#v", recorder[:2])
	}

	first := recorder[2].(render.ToolCallDelta).ToolCalls[0]
	second := recorder[3].(render.ToolCallDelta).ToolCalls[0]
	if first.Index != 0 || first.ID == "" || first.Function.Name != "read" ||
		first.Function.Arguments != `{"key":1}` ||
		second.Index != 1 ||
		second.ID != "second" ||
		result.FinishReason != "tool_calls" ||
		result.ToolCallCount != 2 {
		t.Fatalf("tool events = %#v, %#v; result = %+v", first, second, result)
	}

	usage := result.Usage
	if usage.PromptTokens != 5 || usage.CompletionTokens != 9 || usage.TotalTokens != 14 ||
		usage.CompletionTokensDetails == nil || usage.CompletionTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestGeminiStreamErrors(t *testing.T) {
	for _, data := range []string{"data: not-json", "data: {}"} {
		var recorder streamRecorder
		if _, err := antigravity.ReadStream(
			t.Context(),
			strings.NewReader(data),
			&recorder,
		); err == nil {
			t.Errorf("accepted %q", data)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var recorder streamRecorder
	if _, err := antigravity.ReadStream(ctx, strings.NewReader(""), &recorder); !errors.Is(
		err,
		context.Canceled,
	) {
		t.Errorf("canceled stream error = %v", err)
	}
}
