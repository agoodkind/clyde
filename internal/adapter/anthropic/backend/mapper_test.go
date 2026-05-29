package anthropicbackend

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// mustTextBlock asserts that content[idx] is a TextBlock and returns it.
func mustTextBlock(t *testing.T, content []AnthContentBlock, idx int) TextBlock {
	t.Helper()
	if idx >= len(content) {
		t.Fatalf("content index %d out of range (len=%d)", idx, len(content))
	}
	tb, ok := content[idx].(TextBlock)
	if !ok {
		t.Fatalf("content[%d] = %T, want TextBlock", idx, content[idx])
	}
	return tb
}

// mustToolUseBlock asserts that content[idx] is a ToolUseBlock and returns it.
func mustToolUseBlock(t *testing.T, content []AnthContentBlock, idx int) ToolUseBlock {
	t.Helper()
	if idx >= len(content) {
		t.Fatalf("content index %d out of range (len=%d)", idx, len(content))
	}
	tub, ok := content[idx].(ToolUseBlock)
	if !ok {
		t.Fatalf("content[%d] = %T, want ToolUseBlock", idx, content[idx])
	}
	return tub
}

// mustToolResultBlock asserts that content[idx] is a ToolResultBlock and returns it.
func mustToolResultBlock(t *testing.T, content []AnthContentBlock, idx int) ToolResultBlock {
	t.Helper()
	if idx >= len(content) {
		t.Fatalf("content index %d out of range (len=%d)", idx, len(content))
	}
	trb, ok := content[idx].(ToolResultBlock)
	if !ok {
		t.Fatalf("content[%d] = %T, want ToolResultBlock", idx, content[idx])
	}
	return trb
}

// mustThinkingBlock asserts that content[idx] is a ThinkingBlock and returns it.
func mustThinkingBlock(t *testing.T, content []AnthContentBlock, idx int) ThinkingBlock {
	t.Helper()
	if idx >= len(content) {
		t.Fatalf("content index %d out of range (len=%d)", idx, len(content))
	}
	tb, ok := content[idx].(ThinkingBlock)
	if !ok {
		t.Fatalf("content[%d] = %T, want ThinkingBlock", idx, content[idx])
	}
	return tb
}

// mustImageBlock asserts that content[idx] is an ImageBlock and returns it.
func mustImageBlock(t *testing.T, content []AnthContentBlock, idx int) ImageBlock {
	t.Helper()
	if idx >= len(content) {
		t.Fatalf("content index %d out of range (len=%d)", idx, len(content))
	}
	ib, ok := content[idx].(ImageBlock)
	if !ok {
		t.Fatalf("content[%d] = %T, want ImageBlock", idx, content[idx])
	}
	return ib
}

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
	if len(out.Messages[0].Content) != 1 {
		t.Fatalf("unexpected content: %+v", out.Messages[0].Content)
	}
	tb := mustTextBlock(t, out.Messages[0].Content, 0)
	if tb.Text != "hello" {
		t.Fatalf("text=%q want hello", tb.Text)
	}
}

func TestTranslateRequestContentPartsTextOnly(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{Model: "x", Messages: []adapteropenai.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}}}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages[0].Content) != 1 {
		t.Fatalf("got %+v", out.Messages[0].Content)
	}
	tb := mustTextBlock(t, out.Messages[0].Content, 0)
	if tb.Text != "hi" {
		t.Fatalf("text=%q want hi", tb.Text)
	}
}

func TestTranslateRequestImageDataURI(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{Model: "x", Messages: []adapteropenai.ChatMessage{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]`)}}}
	out, err := TranslateRequest(req, "", 64, "")
	if err != nil {
		t.Fatal(err)
	}
	ib := mustImageBlock(t, out.Messages[0].Content, 0)
	src := ib.Source
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
	ib := mustImageBlock(t, out.Messages[0].Content, 0)
	src := ib.Source
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
	tu := mustToolUseBlock(t, msg.Content, 1)
	if tu.Name != "get_weather" || string(tu.Input) != `{"loc":"NY"}` {
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
	tb0 := mustTextBlock(t, msg.Content, 0)
	if tb0.Text != "working" {
		t.Fatalf("first block text=%q want working", tb0.Text)
	}
	tu1 := mustToolUseBlock(t, msg.Content, 1)
	if tu1.Name != "get_weather" || string(tu1.Input) != `{"loc":"NY"}` {
		t.Fatalf("second block = %+v", tu1)
	}
	tu2 := mustToolUseBlock(t, msg.Content, 2)
	if tu2.Name != "write_file" || string(tu2.Input) != `{"path":"out.md"}` {
		t.Fatalf("third block = %+v", tu2)
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
	b := mustToolResultBlock(t, out.Messages[0].Content, 0)
	if b.ToolUseID != "toolu_1" || b.ResultContent != "result" {
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
	thinking := adapterrender.SyntheticContentOpenWithRef(adapterrender.SyntheticReasoning, "", adapterrender.OriginAnthropic) +
		"> I should answer 42." +
		adapterrender.SyntheticContentCloseWithAttrs(adapterrender.SyntheticReasoning, "", "OPAQUE_SIG_BASE64==")
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
	tb0 := mustThinkingBlock(t, asst.Content, 0)
	if tb0.Thinking != "I should answer 42." {
		t.Fatalf("block0 thinking=%q want %q", tb0.Thinking, "I should answer 42.")
	}
	tb1 := mustTextBlock(t, asst.Content, 1)
	if !strings.Contains(tb1.Text, "The answer is 42.") {
		t.Fatalf("block1 text=%q should contain real answer", tb1.Text)
	}
	if strings.Contains(tb1.Text, "clyde-thinking") {
		t.Fatalf("text block leaked envelope marker: %q", tb1.Text)
	}
	wire, _ := ToAPIRequest(out, "claude-x", false)
	encoded, err := json.Marshal(wire.Messages[1])
	if err != nil {
		t.Fatalf("marshal wire assistant: %v", err)
	}
	wireBytes := string(encoded)
	if !strings.Contains(wireBytes, `"thinking":"I should answer 42."`) {
		t.Fatalf("wire JSON missing thinking body, got %s", wireBytes)
	}
}

// TestTranslateRequestAssistantThinkingPropagatesAnthropicSignature
// asserts that when Cursor replays an assistant text whose synthetic
// thinking envelope close marker carries `data-signature="..."`, the
// Anthropic mapper copies that signature onto the materialized native
// thinking content block. This is the per-thinking-block opaque value
// Anthropic's signature_delta SSE event emits; without copying it, a
// later replay would fail upstream signature validation.
func TestTranslateRequestAssistantThinkingPropagatesAnthropicSignature(t *testing.T) {
	t.Parallel()
	envelope := adapterrender.SyntheticContentOpenWithRef(adapterrender.SyntheticReasoning, "", adapterrender.OriginAnthropic) +
		"> deliberation body" +
		adapterrender.SyntheticContentCloseWithAttrs(
			adapterrender.SyntheticReasoning,
			"",
			"OPAQUE_SIG_BASE64==",
		)
	assistantText := envelope + "Final answer."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, adapterrender.MaterializeNativeThinkingBlock)
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	if len(asst.Content) < 1 {
		t.Fatalf("assistant blocks empty")
	}
	tb0 := mustThinkingBlock(t, asst.Content, 0)
	if tb0.Thinking != "deliberation body" {
		t.Fatalf("thinking body=%q want %q", tb0.Thinking, "deliberation body")
	}
	if tb0.Signature != "OPAQUE_SIG_BASE64==" {
		t.Fatalf("thinking block signature=%q want OPAQUE_SIG_BASE64==", tb0.Signature)
	}
}

// TestTranslateRequestAssistantCodexThinkingInjectsAsText is the
// regression test for the cross-provider replay bug. A Cursor chat that
// started on Codex stores assistant turns whose thinking marker carries
// `data-origin="codex"`. When the user switches the model to Anthropic
// mid-conversation, the Anthropic mapper must NOT reproduce a native
// thinking block (it would lack the Anthropic signature and upstream
// rejects with `messages.N.content.0.thinking.signature: Field
// required`). Instead, the foreign-origin thinking body is injected as a
// plain text block in front of the final answer so the prior reasoning
// stays in context.
func TestTranslateRequestAssistantCodexThinkingInjectsAsText(t *testing.T) {
	t.Parallel()
	envelope := adapterrender.SyntheticContentOpenWithRef(adapterrender.SyntheticReasoning, "rs_codex_1", adapterrender.OriginCodex) +
		"> Let me see, 2+2 is 4." +
		adapterrender.SyntheticContentCloseWithAttrs(
			adapterrender.SyntheticReasoning,
			"CODEX_ENCRYPTED_BLOB==",
			"",
		)
	assistantText := envelope + "It is 4."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, adapterrender.MaterializeNativeThinkingBlock)
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	for i, blk := range asst.Content {
		if _, ok := blk.(ThinkingBlock); ok {
			t.Fatalf("block[%d] is a ThinkingBlock; Codex-origin thinking must not produce a native Anthropic thinking block: %+v", i, asst.Content)
		}
	}
	if len(asst.Content) < 1 {
		t.Fatalf("assistant blocks empty; want at least one text block carrying the injected reasoning")
	}
	tb0 := mustTextBlock(t, asst.Content, 0)
	if !strings.Contains(tb0.Text, "Let me see, 2+2 is 4.") {
		t.Fatalf("first text block should carry injected codex reasoning, got %q", tb0.Text)
	}
	var sawFinal bool
	for _, blk := range asst.Content {
		if tb, ok := blk.(TextBlock); ok && strings.Contains(tb.Text, "It is 4.") {
			sawFinal = true
			break
		}
	}
	if !sawFinal {
		t.Fatalf("final answer text missing from translated assistant content: %+v", asst.Content)
	}
}

// TestTranslateRequestAssistantRawUnsignedThinkingInjectsAsText covers
// Cursor replays that arrive as OpenAI-style content parts but already
// contain an Anthropic-shaped thinking block. Without a signature, Clyde
// cannot send the block as native Anthropic thinking, so the readable body
// is injected as ordinary assistant text.
func TestTranslateRequestAssistantRawUnsignedThinkingInjectsAsText(t *testing.T) {
	t.Parallel()
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{
				Role: "assistant",
				Content: json.RawMessage(`[
					{"type":"thinking","thinking":"raw prior thinking"},
					{"type":"text","text":"Final answer."},
					{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}
				]`),
			},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, adapterrender.MaterializeNativeThinkingBlock)
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	if len(asst.Content) != 3 {
		t.Fatalf("assistant blocks=%d want 3: %+v", len(asst.Content), asst.Content)
	}
	tb0 := mustTextBlock(t, asst.Content, 0)
	if tb0.Text != "raw prior thinking" {
		t.Fatalf("block0 text=%q want raw prior thinking", tb0.Text)
	}
	tb1 := mustTextBlock(t, asst.Content, 1)
	if tb1.Text != "Final answer." {
		t.Fatalf("block1 text=%q want Final answer.", tb1.Text)
	}
	mustToolUseBlock(t, asst.Content, 2)

	wire, _ := ToAPIRequest(out, "claude-x", false)
	encoded, err := json.Marshal(wire.Messages[1])
	if err != nil {
		t.Fatalf("marshal wire assistant: %v", err)
	}
	wireJSON := string(encoded)
	if strings.Contains(wireJSON, `"type":"thinking"`) {
		t.Fatalf("unsigned raw thinking leaked as native wire block: %s", wireJSON)
	}
	if !strings.Contains(wireJSON, `"text":"raw prior thinking"`) {
		t.Fatalf("wire JSON missing injected thinking text: %s", wireJSON)
	}
}

// TestTranslateRequestAssistantUnknownOriginThinkingInjectsAsText covers
// pre-upgrade transcripts: a saved assistant turn whose synthetic-thinking
// envelope has no `data-origin` attribute resolves to OriginUnknown. The
// mapper must treat this the same as a foreign-origin piece and inject
// the body as plain text rather than emit an unsigned native thinking
// block.
func TestTranslateRequestAssistantUnknownOriginThinkingInjectsAsText(t *testing.T) {
	t.Parallel()
	envelope := adapterrender.SyntheticContentOpen(adapterrender.SyntheticReasoning) +
		"> legacy thinking body" +
		adapterrender.SyntheticContentClose(adapterrender.SyntheticReasoning)
	assistantText := envelope + "Final."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, adapterrender.MaterializeNativeThinkingBlock)
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	for i, blk := range asst.Content {
		if _, ok := blk.(ThinkingBlock); ok {
			t.Fatalf("block[%d] is a ThinkingBlock; unknown-origin thinking must inject as text: %+v", i, asst.Content)
		}
	}
	tb0 := mustTextBlock(t, asst.Content, 0)
	if !strings.Contains(tb0.Text, "legacy thinking body") {
		t.Fatalf("first text block should carry injected legacy reasoning, got %q", tb0.Text)
	}
}

// TestTranslateRequestAssistantAnthropicThinkingMissingSignatureDropped
// asserts the malformed-anthropic case: a piece tagged `data-origin="anthropic"`
// without a signature is dropped outright. Injecting it as text would
// leak content the upstream chose to sign rather than show.
func TestTranslateRequestAssistantAnthropicThinkingMissingSignatureDropped(t *testing.T) {
	t.Parallel()
	envelope := adapterrender.SyntheticContentOpenWithRef(adapterrender.SyntheticReasoning, "", adapterrender.OriginAnthropic) +
		"> deliberation without signature" +
		adapterrender.SyntheticContentClose(adapterrender.SyntheticReasoning)
	assistantText := envelope + "Final answer."
	body, err := json.Marshal(assistantText)
	if err != nil {
		t.Fatal(err)
	}
	req := adapteropenai.ChatRequest{
		Model: "x",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: json.RawMessage(`"q"`)},
			{Role: "assistant", Content: json.RawMessage(body)},
			{Role: "user", Content: json.RawMessage(`"continue"`)},
		},
	}
	out, err := TranslateRequest(req, "", 64, adapterrender.MaterializeNativeThinkingBlock)
	if err != nil {
		t.Fatal(err)
	}
	asst := out.Messages[1]
	for i, blk := range asst.Content {
		if _, ok := blk.(ThinkingBlock); ok {
			t.Fatalf("block[%d] is a ThinkingBlock; unsigned anthropic-origin thinking must be dropped: %+v", i, asst.Content)
		}
		if tb, ok := blk.(TextBlock); ok && strings.Contains(tb.Text, "deliberation without signature") {
			t.Fatalf("block[%d] leaked unsigned anthropic body into text: %+v", i, asst.Content)
		}
	}
	var sawFinal bool
	for _, blk := range asst.Content {
		if tb, ok := blk.(TextBlock); ok && strings.Contains(tb.Text, "Final answer.") {
			sawFinal = true
			break
		}
	}
	if !sawFinal {
		t.Fatalf("final answer text missing from translated assistant content: %+v", asst.Content)
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
		if tb, ok := blk.(TextBlock); ok {
			if strings.Contains(tb.Text, "clyde-notice") || strings.Contains(tb.Text, "95% used") {
				t.Fatalf("notice envelope leaked upstream: %+v", blk)
			}
		}
		if _, ok := blk.(ThinkingBlock); ok {
			t.Fatalf("notice should not become a thinking block: %+v", blk)
		}
	}
	if len(asst.Content) != 1 {
		t.Fatalf("expected single text block with real answer, got %+v", asst.Content)
	}
	tb := mustTextBlock(t, asst.Content, 0)
	if !strings.Contains(tb.Text, "Real answer.") {
		t.Fatalf("text=%q want Real answer.", tb.Text)
	}
}

// TestTranslateRequestAssistantThinkingDropStrategyOptsOut asserts that when
// the operator flips Anthropic's inbound_thinking from the default
// `native_thinking_block` to `drop`, the round-tripped thinking content is
// removed and only the surrounding text survives. The lever is documented at
// config.AdapterAnthropicReasoning.InboundThinking.
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
		if _, ok := blk.(ThinkingBlock); ok {
			t.Fatalf("drop strategy should not produce thinking blocks: %+v", blk)
		}
		if tb, ok := blk.(TextBlock); ok {
			if strings.Contains(tb.Text, "clyde-thinking") || strings.Contains(tb.Text, "I should answer 42.") {
				t.Fatalf("thinking content leaked under drop strategy: %+v", blk)
			}
		}
	}
	if len(asst.Content) != 1 {
		t.Fatalf("expected single text block with answer, got %+v", asst.Content)
	}
	tb := mustTextBlock(t, asst.Content, 0)
	if !strings.Contains(tb.Text, "The answer is 42.") {
		t.Fatalf("text=%q want The answer is 42.", tb.Text)
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
		if _, ok := blk.(ThinkingBlock); ok {
			t.Fatalf("plain_text_concat must not emit native thinking blocks: %+v", blk)
		}
	}
	joined := ""
	for _, blk := range asst.Content {
		if tb, ok := blk.(TextBlock); ok {
			joined += tb.Text
		}
	}
	if !strings.Contains(joined, "deliberation body") {
		t.Fatalf("plain_text_concat should preserve thinking content: %q", joined)
	}
	if strings.Contains(joined, "clyde-thinking") {
		t.Fatalf("envelope marker leaked under plain_text_concat: %q", joined)
	}
}
