package anthropicbackend

import (
	"encoding/json"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func newTranslatorForTest() *StreamTranslator {
	return NewStreamTranslator("req1", "clyde-test")
}

func TestStreamTranslatorThinkingEmitsBlockquoteContent(t *testing.T) {
	tr := newTranslatorForTest()
	renderer := adapterrender.NewEventRenderer("req1", "clyde-test", "anthropic", nil)
	var out []adapteropenai.StreamChunk
	emit := func(name string, payload []byte) {
		events, _, _, _, err := tr.HandleEventEvents(name, payload)
		if err != nil {
			t.Fatalf("HandleEventEvents: %v", err)
		}
		for _, event := range events {
			out = append(out, renderer.HandleEvent(event)...)
		}
	}
	msgStart, err := json.Marshal(struct {
		Message struct {
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}{})
	if err != nil {
		t.Fatal(err)
	}
	emit("message_start", msgStart)

	startThinking, err := json.Marshal(struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}{Index: 0, ContentBlock: struct {
		Type string `json:"type"`
	}{Type: "thinking"}})
	if err != nil {
		t.Fatal(err)
	}
	emit("content_block_start", startThinking)

	deltaThinking, err := json.Marshal(struct {
		Index int           `json:"index"`
		Delta *AnthSSEDelta `json:"delta"`
	}{Index: 0, Delta: &AnthSSEDelta{Type: "thinking_delta", Thinking: "step one"}})
	if err != nil {
		t.Fatal(err)
	}
	emit("content_block_delta", deltaThinking)

	var seenOpener, seenText bool
	for _, ch := range out {
		if len(ch.Choices) == 0 {
			continue
		}
		c := ch.Choices[0].Delta.Content
		if strings.Contains(c, "<!--clyde-thinking-->") {
			seenOpener = true
		}
		if strings.Contains(c, "step one") {
			seenText = true
		}
	}
	if !seenOpener || !seenText {
		t.Fatalf("missing thinking envelope in %+v", out)
	}
}

func TestStreamTextTranslator(t *testing.T) {
	t.Parallel()
	tr := NewStreamTranslator("chatcmpl-s", "alias")
	renderer := adapterrender.NewEventRenderer("chatcmpl-s", "alias", "anthropic", nil)
	var all []adapteropenai.StreamChunk
	feed := func(name string, payload string) {
		events, finished, reason, _, err := tr.HandleEventEvents(name, []byte(payload))
		if err != nil {
			t.Fatalf("event %s: %v", name, err)
		}
		for _, event := range events {
			all = append(all, renderer.HandleEvent(event)...)
		}
		if name == "message_stop" {
			if !finished || reason != "stop" {
				t.Fatalf("finish finished=%v reason=%q", finished, reason)
			}
		}
	}
	feed("message_start", `{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":1,"output_tokens":0}}}`)
	feed("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	feed("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`)
	feed("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"b"}}`)
	feed("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"c"}}`)
	feed("content_block_stop", `{"type":"content_block_stop","index":0}`)
	feed("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
	feed("message_stop", `{"type":"message_stop"}`)

	if len(all) != 3 {
		t.Fatalf("chunk count %d", len(all))
	}
	if all[0].Choices[0].Delta.Role != "assistant" || all[0].Choices[0].Delta.Content != "a" {
		t.Fatalf("first chunk %+v", all[0].Choices[0].Delta)
	}
	if all[1].Choices[0].Delta.Role != "" || all[1].Choices[0].Delta.Content != "b" {
		t.Fatalf("second chunk %+v", all[1].Choices[0].Delta)
	}
	if all[2].Choices[0].Delta.Content != "c" {
		t.Fatalf("third chunk %+v", all[2].Choices[0].Delta)
	}
}

func TestStreamToolTranslator(t *testing.T) {
	t.Parallel()
	tr := NewStreamTranslator("chatcmpl-t", "alias")
	renderer := adapterrender.NewEventRenderer("chatcmpl-t", "alias", "anthropic", nil)
	var all []adapteropenai.StreamChunk
	var finishReason string
	feed := func(name string, payload string) {
		events, finished, reason, _, err := tr.HandleEventEvents(name, []byte(payload))
		if err != nil {
			t.Fatalf("event %s: %v", name, err)
		}
		for _, event := range events {
			all = append(all, renderer.HandleEvent(event)...)
		}
		if finished {
			finishReason = reason
		}
	}
	feed("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`)
	feed("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"get_weather","input":{}}}`)
	partial1 := string([]byte{'"', '{', '"', 'l', 'o', 'c'})
	partial2 := string([]byte{'"', ':', '"', 'N', 'Y', '"', '}'})
	d1, err := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": partial1}})
	if err != nil {
		t.Fatal(err)
	}
	d2, err := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": partial2}})
	if err != nil {
		t.Fatal(err)
	}
	feed("content_block_delta", string(d1))
	feed("content_block_delta", string(d2))
	feed("content_block_stop", `{"type":"content_block_stop","index":0}`)
	feed("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
	feed("message_stop", `{"type":"message_stop"}`)

	if finishReason != "tool_calls" {
		t.Fatalf("finish reason %q", finishReason)
	}
	if len(all) != 3 {
		t.Fatalf("chunks %d", len(all))
	}
	if len(all[0].Choices[0].Delta.ToolCalls) != 1 || all[0].Choices[0].Delta.ToolCalls[0].Function.Arguments != "" {
		t.Fatalf("open %+v", all[0].Choices[0].Delta.ToolCalls)
	}
	want1 := string([]byte{'"', '{', '"', 'l', 'o', 'c'})
	if all[1].Choices[0].Delta.ToolCalls[0].Function.Arguments != want1 {
		t.Fatalf("arg1 %+v want %q", all[1].Choices[0].Delta.ToolCalls, want1)
	}
	want2 := string([]byte{'"', ':', '"', 'N', 'Y', '"', '}'})
	if all[2].Choices[0].Delta.ToolCalls[0].Function.Arguments != want2 {
		t.Fatalf("arg2 %+v want %q", all[2].Choices[0].Delta.ToolCalls, want2)
	}
}

func TestStreamTranslatorEventPathEmitsToolCallEvents(t *testing.T) {
	t.Parallel()
	tr := NewStreamTranslator("chatcmpl-event-tool", "alias")

	events, _, _, _, err := tr.HandleEventEvents(
		"content_block_start",
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"get_weather","input":{}}}`),
	)
	if err != nil {
		t.Fatalf("HandleEventEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events len=%d want 1", len(events))
	}
	tc0, ok := events[0].(adapterrender.ToolCallDelta)
	if !ok {
		t.Fatalf("events[0] type=%T want ToolCallDelta", events[0])
	}
	if len(tc0.ToolCalls) != 1 {
		t.Fatalf("tool_calls len=%d want 1", len(tc0.ToolCalls))
	}
	if tc0.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool name=%q", tc0.ToolCalls[0].Function.Name)
	}
}

func TestStreamTranslatorEventPathEmitsRefusalEvent(t *testing.T) {
	t.Parallel()
	tr := NewStreamTranslator("chatcmpl-event-refusal", "alias")

	if _, _, _, _, err := tr.HandleEventEvents("message_start", []byte(`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`)); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	if _, _, _, _, err := tr.HandleEventEvents("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)); err != nil {
		t.Fatalf("content_block_start: %v", err)
	}
	if _, _, _, _, err := tr.HandleEventEvents("content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"declined"}}`)); err != nil {
		t.Fatalf("content_block_delta: %v", err)
	}
	if _, _, _, _, err := tr.HandleEventEvents("message_delta", []byte(`{"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":2}}`)); err != nil {
		t.Fatalf("message_delta: %v", err)
	}
	events, finished, reason, usage, err := tr.HandleEventEvents("message_stop", []byte(`{"type":"message_stop"}`))
	if err != nil {
		t.Fatalf("message_stop: %v", err)
	}
	if !finished || reason != "content_filter" {
		t.Fatalf("finished=%v reason=%q", finished, reason)
	}
	if usage == nil || usage.CompletionTokens != 2 {
		t.Fatalf("usage=%+v", usage)
	}
	if len(events) != 1 {
		t.Fatalf("events len=%d want 1", len(events))
	}
	rd, ok := events[0].(adapterrender.RefusalDelta)
	if !ok {
		t.Fatalf("events[0] type=%T want RefusalDelta", events[0])
	}
	if rd.Text != "declined" {
		t.Fatalf("refusal=%q want declined", rd.Text)
	}
}
