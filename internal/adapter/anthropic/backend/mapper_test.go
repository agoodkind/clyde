package anthropicbackend

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

func TestTranslateRequestSimpleUserText(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{Model: "x", Messages: []adapteropenai.ChatMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("unexpected messages: %+v", out.Messages)
	}
	if len(out.Messages[0].Content) != 1 || out.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("unexpected content: %+v", out.Messages[0].Content)
	}
}

func TestTranslateRequestContentPartsTextOnly(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{Model: "x", Messages: []adapteropenai.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}}}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages[0].Content) != 1 || out.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("got %+v", out.Messages[0].Content)
	}
}

func TestTranslateRequestImageDataURI(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{Model: "x", Messages: []adapteropenai.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]`)}}}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	src := out.Messages[0].Content[0].Source
	if src == nil || src.Type != "base64" || src.MediaType != "image/png" || src.Data != "iVBOR" {
		t.Fatalf("unexpected source: %+v", src)
	}
}

func TestTranslateRequestImageHTTPSURL(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{Model: "x", Messages: []adapteropenai.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]`)}}}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	src := out.Messages[0].Content[0].Source
	if src == nil || src.Type != "url" || src.URL != "https://x/y.png" {
		t.Fatalf("unexpected source: %+v", src)
	}
}

func TestTranslateRequestAudioRejected(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{Model: "x", Messages: []adapteropenai.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"input_audio","input_audio":{"data":"qqq"}}]`)}}}
	_, err := TranslateRequest(req, "", 64, "")
	if err == nil || !errors.Is(err, ErrAudioUnsupported) {
		t.Fatalf("expected ErrAudioUnsupported, got %v", err)
	}
}

func TestTranslateRequestToolsTranslated(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hi"`),
		}},
		Tools: []adapteropenai.Tool{
			{Type: "function", Function: adapteropenai.ToolFunctionSchema{Name: "a", Parameters: json.RawMessage(`{"type":"object"}`)}},
			{Type: "function", Function: adapteropenai.ToolFunctionSchema{Name: "b", Description: "d", Parameters: json.RawMessage(`{"a":1}`)}},
		},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 {
		t.Fatalf("tools: %+v", out.Tools)
	}
	if string(out.Tools[0].InputSchema) != `{"type":"object"}` || out.Tools[1].Name != "b" {
		t.Fatalf("unexpected tools: %+v", out.Tools)
	}
}

func TestTranslateRequestToolChoiceVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want *AnthToolChoice
	}{
		{"none", `"none"`, &AnthToolChoice{Type: "none"}},
		// "auto" is the Anthropic default; claude-cli omits tool_choice
		// in this case so we do too (CLYDE-124 parity).
		{"auto", `"auto"`, nil},
		{"required", `"required"`, &AnthToolChoice{Type: "any"}},
		{"named", `{"type":"function","function":{"name":"X"}}`, &AnthToolChoice{Type: "tool", Name: "X"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := adapteropenai.ChatRequest{
				Model: "x",
				Messages: []adapteropenai.ChatMessage{{
					Role:    "user",
					Content: json.RawMessage(`"hi"`),
				}},
				ToolChoice: json.RawMessage(tc.raw),
			}
			out, err := TranslateRequest(req, "", 64, "")
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil:
				if out.ToolChoice != nil {
					t.Fatalf("got %+v want <nil>", out.ToolChoice)
				}
			case out.ToolChoice == nil || out.ToolChoice.Type != tc.want.Type || out.ToolChoice.Name != tc.want.Name:
				t.Fatalf("got %+v want %+v", out.ToolChoice, tc.want)
			}
		})
	}
}

func TestTranslateRequestParallelToolCallsFalse(t *testing.T) {
	t.Parallel()
	f := false
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hi"`),
		}},
		ParallelTools: &f,
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.ToolChoice == nil || out.ToolChoice.Type != "auto" || !out.ToolChoice.DisableParallelToolUse {
		t.Fatalf("got %+v", out.ToolChoice)
	}
}

func TestTranslateRequestAssistantWithToolCalls(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{{
			Role:    "assistant",
			Content: json.RawMessage(`"ok"`),
			ToolCalls: []adapteropenai.ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: adapteropenai.ToolCallFunction{
					Name:      "get_weather",
					Arguments: `{"loc":"NY"}`,
				},
			}},
		}},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	msg := out.Messages[0]
	if msg.Role != "assistant" || len(msg.Content) != 2 {
		t.Fatalf("unexpected assistant: %+v", msg)
	}
	tu := msg.Content[1]
	if tu.Type != "tool_use" || tu.Name != "get_weather" || string(tu.Input) != `{"loc":"NY"}` {
		t.Fatalf("unexpected tool block: %+v", tu)
	}
}

func TestTranslateRequestAssistantWithMultipleToolCallsPreservesOrder(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{{
			Role:    "assistant",
			Content: json.RawMessage(`"working"`),
			ToolCalls: []adapteropenai.ToolCall{
				{
					ID:   "call_1",
					Type: "function",
					Function: adapteropenai.ToolCallFunction{
						Name:      "get_weather",
						Arguments: `{"loc":"NY"}`,
					},
				},
				{
					ID:   "call_2",
					Type: "function",
					Function: adapteropenai.ToolCallFunction{
						Name:      "write_file",
						Arguments: `{"path":"out.md"}`,
					},
				},
			},
		}},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	msg := out.Messages[0]
	if len(msg.Content) != 3 {
		t.Fatalf("content len = %d want 3 (%+v)", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != "text" || msg.Content[0].Text != "working" {
		t.Fatalf("first block = %+v", msg.Content[0])
	}
	if msg.Content[1].Type != "tool_use" || msg.Content[1].Name != "get_weather" || string(msg.Content[1].Input) != `{"loc":"NY"}` {
		t.Fatalf("second block = %+v", msg.Content[1])
	}
	if msg.Content[2].Type != "tool_use" || msg.Content[2].Name != "write_file" || string(msg.Content[2].Input) != `{"path":"out.md"}` {
		t.Fatalf("third block = %+v", msg.Content[2])
	}
}

func TestTranslateRequestToolRoleMessage(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{{
			Role:       "tool",
			ToolCallID: "toolu_1",
			Content:    json.RawMessage(`"result"`),
		}},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	b := out.Messages[0].Content[0]
	if b.Type != "tool_result" || b.ToolUseID != "toolu_1" || b.ResultContent != "result" {
		t.Fatalf("unexpected block: %+v", b)
	}
}

func TestTranslateRequestRoleAlternationMerge(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"a"`)},
			{Role: "user", Content: json.RawMessage(`"b"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("expected merged user, got %d", len(out.Messages))
	}
	if len(out.Messages[0].Content) != 2 {
		t.Fatalf("content blocks: %+v", out.Messages[0].Content)
	}
}

func TestTranslateRequestDropsTrailingTextAssistantPrefill(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"please inspect"`)},
			{Role: "assistant", Content: json.RawMessage(`"I will inspect the code."`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages len = %d want 1: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[0].Role != "user" {
		t.Fatalf("last role = %q want user", out.Messages[0].Role)
	}
}

func TestTranslateRequestKeepsTrailingAssistantToolUse(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"please inspect"`)},
			{
				Role:    "assistant",
				Content: json.RawMessage(`"I will inspect the code."`),
				ToolCalls: []adapteropenai.ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: adapteropenai.ToolCallFunction{
						Name:      "Read",
						Arguments: `{"path":"main.go"}`,
					},
				}},
			},
		},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("messages len = %d want 2: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[1].Role != "assistant" {
		t.Fatalf("last role = %q want assistant", out.Messages[1].Role)
	}
	if !assistantHasToolUse(out.Messages[1]) {
		t.Fatalf("expected trailing assistant tool_use to be preserved: %+v", out.Messages[1])
	}
}

func TestTranslateRequestSystemPrefixIdempotent(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{{
			Role:    "system",
			Content: json.RawMessage(`"SYS\n\nalready"`),
		}},
	}
	out, err := TranslateRequest(req, "SYS", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.System != "SYS\n\nalready" {
		t.Fatalf("system: %q", out.System)
	}
}

func TestTranslateRequestLegacyFunctions(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`"hi"`),
		}},
		Functions: []adapteropenai.Function{{
			Name:        "legacy",
			Description: "d",
			Parameters:  json.RawMessage(`{"x":1}`),
		}},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "legacy" {
		t.Fatalf("tools: %+v", out.Tools)
	}
}

// TestTranslateRequestAssistantThinkingRoundTripsAsNativeBlock asserts the
// Phase B P0 fix: when Cursor replays a prior assistant turn whose text was
// wrapped by [adapterrender.FormatSyntheticContent] with kind
// [adapterrender.SyntheticReasoning], the Anthropic mapper materializes the
// envelope body as a native `{type:"thinking", thinking:"..."}` content
// block followed by the surrounding `{type:"text", text:"..."}` blocks in
// order. This is the round-trip path that lets the model retain its own
// prior reasoning chain across turns.
func TestTranslateRequestAssistantThinkingRoundTripsAsNativeBlock(t *testing.T) {
	t.Parallel()
	thinking := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "I should answer 42.")
	assistantText := thinking + "\n\nThe answer is 42."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"What is the answer?"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"Now multiply by 2."`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages count=%d, want 3", len(out.Messages))
	}
	asst := out.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("role=%q want assistant", asst.Role)
	}
	if len(asst.Content) != 2 {
		t.Fatalf("assistant blocks=%d, want 2 (thinking + text); got %+v", len(asst.Content), asst.Content)
	}
	if asst.Content[0].Type != "thinking" {
		t.Fatalf("block0 type=%q want thinking", asst.Content[0].Type)
	}
	if asst.Content[0].Thinking != "I should answer 42." {
		t.Fatalf("block0 thinking=%q want %q", asst.Content[0].Thinking, "I should answer 42.")
	}
	if asst.Content[0].Text != "" {
		t.Fatalf("block0 text should be empty on a thinking block, got %q", asst.Content[0].Text)
	}
	if asst.Content[1].Type != "text" {
		t.Fatalf("block1 type=%q want text", asst.Content[1].Type)
	}
	if !strings.Contains(asst.Content[1].Text, "The answer is 42.") {
		t.Fatalf("block1 text=%q should contain real answer", asst.Content[1].Text)
	}
	if strings.Contains(asst.Content[1].Text, "clyde-thinking") {
		t.Fatalf("text block leaked envelope marker: %q", asst.Content[1].Text)
	}
}

// TestTranslateRequestAssistantNoticeIsDroppedFromUpstream asserts that
// notice envelopes (UI-only quota / runtime warnings) are not forwarded to
// the upstream provider; they would otherwise be re-billed as input tokens.
func TestTranslateRequestAssistantNoticeIsDroppedFromUpstream(t *testing.T) {
	t.Parallel()
	notice := adapterrender.FormatSyntheticContent(adapterrender.SyntheticNotice, "⚠️ 95% used")
	assistantText := "Real answer.\n\n" + notice
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"go"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"more"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	for _, blk := range asst.Content {
		if strings.Contains(blk.Text, "clyde-notice") || strings.Contains(blk.Text, "95% used") {
			t.Fatalf("notice envelope leaked upstream: %+v", blk)
		}
		if blk.Type == "thinking" {
			t.Fatalf("notice should not become a thinking block: %+v", blk)
		}
	}
	if len(asst.Content) != 1 || asst.Content[0].Type != "text" || !strings.Contains(asst.Content[0].Text, "Real answer.") {
		t.Fatalf("expected single text block with real answer, got %+v", asst.Content)
	}
}

// TestTranslateRequestAssistantThinkingDropStrategyOptsOut asserts that when
// the operator flips Anthropic's inbound_thinking_materialization from the
// default `native_thinking_block` to `drop`, the round-tripped thinking
// content is removed and only the surrounding text survives. The lever is
// documented at config.AdapterSyntheticContent.Anthropic.InboundThinkingMaterialization.
func TestTranslateRequestAssistantThinkingDropStrategyOptsOut(t *testing.T) {
	t.Parallel()
	thinking := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "I should answer 42.")
	assistantText := thinking + "\n\nThe answer is 42."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"What is the answer?"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"Now multiply by 2."`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, adapterrender.MaterializeDrop)
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	for _, blk := range asst.Content {
		if blk.Type == "thinking" {
			t.Fatalf("drop strategy should not produce thinking blocks: %+v", blk)
		}
		if strings.Contains(blk.Text, "clyde-thinking") || strings.Contains(blk.Text, "I should answer 42.") {
			t.Fatalf("thinking content leaked under drop strategy: %+v", blk)
		}
	}
	if len(asst.Content) != 1 || !strings.Contains(asst.Content[0].Text, "The answer is 42.") {
		t.Fatalf("expected single text block with answer, got %+v", asst.Content)
	}
}

// TestTranslateRequestAssistantThinkingPlainTextConcatPreservesBody asserts
// that plain_text_concat preserves the thinking content as plain prose for
// upstreams that cannot accept native thinking blocks.
func TestTranslateRequestAssistantThinkingPlainTextConcatPreservesBody(t *testing.T) {
	t.Parallel()
	thinking := adapterrender.FormatSyntheticContent(adapterrender.SyntheticReasoning, "deliberation body")
	assistantText := thinking + "\n\nFinal."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"go"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, adapterrender.MaterializePlainTextConcat)
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	for _, blk := range asst.Content {
		if blk.Type == "thinking" {
			t.Fatalf("plain_text_concat must not emit native thinking blocks: %+v", blk)
		}
	}
	joined := ""
	for _, blk := range asst.Content {
		joined += blk.Text
	}
	if !strings.Contains(joined, "deliberation body") {
		t.Fatalf("plain_text_concat should preserve thinking content: %q", joined)
	}
	if strings.Contains(joined, "clyde-thinking") {
		t.Fatalf("envelope marker leaked under plain_text_concat: %q", joined)
	}
}
