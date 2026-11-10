package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	anthropicbackend "goodkind.io/clyde/internal/adapter/anthropic/backend"
)

func TestApplyCacheBreakpointsStampsLastToolAndLastUserText(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "first"}}},
		{Role: "assistant", Content: []anthropic.ContentBlock{{Type: "text", Text: "reply"}}},
		{Role: "user", Content: []anthropic.ContentBlock{
			{Type: "text", Text: "prefix"},
			{Type: "text", Text: "tail"},
		}},
	}
	tools := []anthropic.Tool{
		{Name: "first_tool"},
		{Name: "second_tool"},
	}
	anthropicbackend.ApplyCacheBreakpoints(msgs, tools, false)

	if tools[0].CacheControl != nil {
		t.Fatalf("non-last tool marked")
	}
	if tools[1].CacheControl == nil || tools[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("last tool not marked ephemeral: %+v", tools[1].CacheControl)
	}
	if msgs[0].Content[0].CacheControl != nil {
		t.Fatalf("first user message marked (only last should be)")
	}
	if msgs[2].Content[0].CacheControl != nil {
		t.Fatalf("non-tail text block in last user message marked")
	}
	if msgs[2].Content[1].CacheControl == nil {
		t.Fatalf("tail text of last user message not marked")
	}
}

func TestApplyCacheBreakpointsStampsAssistantTailAndSkipsThinkingTail(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "question"}}},
		{Role: "assistant", Content: []anthropic.ContentBlock{
			{Type: "text", Text: "answer"},
			{Type: "thinking", Text: "private"},
		}},
	}
	tools := []anthropic.Tool{{Name: "only_tool"}}
	anthropicbackend.ApplyCacheBreakpoints(msgs, tools, false)

	if tools[0].CacheControl == nil {
		t.Fatalf("tool marker should apply even on single-message calls")
	}
	if msgs[1].Content[0].CacheControl == nil {
		t.Fatalf("assistant tail text should carry cache marker")
	}
	if msgs[1].Content[1].CacheControl != nil {
		t.Fatalf("thinking tail should not carry cache marker")
	}
}

func TestApplyCacheBreakpointsNoToolsOrMessagesIsNoop(t *testing.T) {
	anthropicbackend.ApplyCacheBreakpoints(nil, nil, false)
	anthropicbackend.ApplyCacheBreakpoints([]anthropic.Message{}, []anthropic.Tool{}, false)
}

func TestApplyCacheBreakpointsLeavesPriorToolResultsPlainByDefault(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: []anthropic.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "result 1"},
			{Type: "text", Text: "follow-up"},
		}},
		{Role: "assistant", Content: []anthropic.ContentBlock{{Type: "text", Text: "assistant turn"}}},
	}
	stats := anthropicbackend.ApplyCacheBreakpoints(msgs, nil, false)
	if msgs[0].Content[0].CacheReference != "" {
		t.Fatalf("tool_result cache_reference = %q want empty by default", msgs[0].Content[0].CacheReference)
	}
	if msgs[1].Content[0].CacheControl == nil {
		t.Fatalf("assistant boundary message should carry cache marker")
	}
	if stats.ToolResultCandidates != 1 || stats.ToolResultApplied != 0 {
		t.Fatalf("unexpected tool_result stats: %+v", stats)
	}
}

func TestApplyCacheBreakpointsCanEnableCacheReferenceOnPriorToolResults(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "user", Content: []anthropic.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_1", Content: "result 1"},
			{Type: "text", Text: "follow-up"},
		}},
		{Role: "assistant", Content: []anthropic.ContentBlock{{Type: "text", Text: "assistant turn"}}},
	}
	stats := anthropicbackend.ApplyCacheBreakpoints(msgs, nil, true)
	if msgs[0].Content[0].CacheReference != "toolu_1" {
		t.Fatalf("tool_result cache_reference = %q want toolu_1", msgs[0].Content[0].CacheReference)
	}
	if stats.ToolResultCandidates != 1 || stats.ToolResultApplied != 1 {
		t.Fatalf("unexpected tool_result stats: %+v", stats)
	}
}

func TestApplyCacheBreakpointsSkipsNewestToolResultWhenCacheReferenceEnabled(t *testing.T) {
	msgs := []anthropic.Message{
		{Role: "assistant", Content: []anthropic.ContentBlock{{Type: "text", Text: "call tool"}}},
		{Role: "user", Content: []anthropic.ContentBlock{
			{Type: "tool_result", ToolUseID: "toolu_latest", Content: "latest result"},
			{Type: "text", Text: "follow-up"},
		}},
	}
	stats := anthropicbackend.ApplyCacheBreakpoints(msgs, nil, true)
	if msgs[1].Content[0].CacheReference != "" {
		t.Fatalf("newest tool_result cache_reference = %q want empty", msgs[1].Content[0].CacheReference)
	}
	if msgs[1].Content[1].CacheControl == nil {
		t.Fatalf("latest user tail text should carry cache marker")
	}
	if stats.ToolResultCandidates != 0 || stats.ToolResultApplied != 0 {
		t.Fatalf("unexpected tool_result stats: %+v", stats)
	}
}

func TestToAnthropicAPIRequestSerializesCacheControl(t *testing.T) {
	tr := anthropicbackend.AnthRequest{
		System: "sys",
		Messages: []anthropicbackend.AnthMessage{
			{Role: "user", Content: []anthropicbackend.AnthContentBlock{anthropicbackend.TextBlock{Text: "a"}}},
			{Role: "assistant", Content: []anthropicbackend.AnthContentBlock{anthropicbackend.TextBlock{Text: "b"}}},
			{Role: "user", Content: []anthropicbackend.AnthContentBlock{anthropicbackend.TextBlock{Text: "c"}}},
		},
		Tools: []anthropicbackend.AnthTool{
			{Name: "t1", InputSchema: json.RawMessage(`{}`)},
		},
		MaxTokens: 64,
	}
	req, _ := anthropicbackend.ToAPIRequest(tr, "claude-model", false)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	// Tool side marker must be present.
	if !strings.Contains(body, `"name":"t1"`) {
		t.Fatalf("missing tool name: %s", body)
	}
	if strings.Count(body, `"cache_control":{"type":"ephemeral"}`) < 2 {
		t.Fatalf("expected two ephemeral cache_control markers, got body:\n%s", body)
	}
}

func TestToAnthropicAPIRequestOmitsCacheReferenceOnPriorToolResultByDefault(t *testing.T) {
	tr := anthropicbackend.AnthRequest{
		System: "sys",
		Messages: []anthropicbackend.AnthMessage{
			{Role: "user", Content: []anthropicbackend.AnthContentBlock{
				anthropicbackend.ToolResultBlock{ToolUseID: "toolu_1", ResultContent: "result"},
				anthropicbackend.TextBlock{Text: "follow-up"},
			}},
			{Role: "assistant", Content: []anthropicbackend.AnthContentBlock{anthropicbackend.TextBlock{Text: "answer"}}},
		},
		MaxTokens: 64,
	}
	req, _ := anthropicbackend.ToAPIRequest(tr, "claude-model", false)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, `"cache_reference":"toolu_1"`) {
		t.Fatalf("unexpected cache_reference on prior tool_result by default: %s", body)
	}
}

func TestToAnthropicAPIRequestCanSerializeCacheReferenceOnPriorToolResult(t *testing.T) {
	tr := anthropicbackend.AnthRequest{
		System: "sys",
		Messages: []anthropicbackend.AnthMessage{
			{Role: "user", Content: []anthropicbackend.AnthContentBlock{
				anthropicbackend.ToolResultBlock{ToolUseID: "toolu_1", ResultContent: "result"},
				anthropicbackend.TextBlock{Text: "follow-up"},
			}},
			{Role: "assistant", Content: []anthropicbackend.AnthContentBlock{anthropicbackend.TextBlock{Text: "answer"}}},
		},
		MaxTokens: 64,
	}
	req, stats := anthropicbackend.ToAPIRequest(tr, "claude-model", true)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"cache_reference":"toolu_1"`) {
		t.Fatalf("missing cache_reference on prior tool_result: %s", body)
	}
	if stats.ToolResultCandidates != 1 || stats.ToolResultApplied != 1 {
		t.Fatalf("unexpected tool_result stats: %+v", stats)
	}
}

func TestUsageFromAnthropicMapsCacheRead(t *testing.T) {
	in := anthropic.Usage{
		InputTokens:              120,
		OutputTokens:             30,
		CacheCreationInputTokens: 800,
		CacheReadInputTokens:     4000,
	}
	u := anthropicbackend.UsageFromAnthropic(in)
	// prompt_tokens must include the cached + cache-creation portions
	// to match OpenAI semantics: 120 uncached + 800 written + 4000 read.
	if u.PromptTokens != 4920 || u.CompletionTokens != 30 || u.TotalTokens != 4950 {
		t.Fatalf("unexpected core usage: %+v", u)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 4000 {
		t.Fatalf("cached_tokens not mapped: %+v", u.PromptTokensDetails)
	}
}

func TestUsageFromAnthropicOmitsDetailsWhenNoCache(t *testing.T) {
	u := anthropicbackend.UsageFromAnthropic(anthropic.Usage{InputTokens: 10, OutputTokens: 2})
	if u.PromptTokensDetails != nil {
		t.Fatalf("PromptTokensDetails should be nil when no cache read: %+v", u.PromptTokensDetails)
	}
}

func TestBuildSystemBlocksEnabledStampsPrefixAndCaller(t *testing.T) {
	blocks := anthropicbackend.BuildSystemBlocks("x-anthropic-billing-header: v=1", "cli-prefix", "caller sys text", "", "", true)
	if len(blocks) != 3 {
		t.Fatalf("want 3 blocks, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].CacheControl != nil {
		t.Fatalf("billing block must not carry cache_control")
	}
	if blocks[1].CacheControl == nil || blocks[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("prefix block missing ephemeral cache marker: %+v", blocks[1].CacheControl)
	}
	if blocks[1].CacheControl.TTL != "" {
		t.Fatalf("default TTL should be empty (Anthropic 5m default), got %q", blocks[1].CacheControl.TTL)
	}
	if blocks[2].CacheControl == nil || blocks[2].CacheControl.Type != "ephemeral" {
		t.Fatalf("caller block missing ephemeral cache marker: %+v", blocks[2].CacheControl)
	}
}

func TestBuildSystemBlocksStampsScopeOnPrefixOnly(t *testing.T) {
	blocks := anthropicbackend.BuildSystemBlocks("billing", "prefix", "caller", "", "global", true)
	if len(blocks) != 3 {
		t.Fatalf("want 3 blocks, got %d", len(blocks))
	}
	if blocks[1].CacheControl == nil || blocks[1].CacheControl.Scope != "global" {
		t.Fatalf("prefix block missing scope=global: %+v", blocks[1].CacheControl)
	}
	if blocks[2].CacheControl == nil || blocks[2].CacheControl.Scope != "" {
		t.Fatalf("caller block should not carry scope: %+v", blocks[2].CacheControl)
	}
}

func TestBuildSystemBlocksHonorsExplicit1hTTL(t *testing.T) {
	blocks := anthropicbackend.BuildSystemBlocks("billing", "prefix", "caller", "1h", "", true)
	for i, b := range blocks[1:] {
		if b.CacheControl == nil || b.CacheControl.TTL != "1h" {
			t.Fatalf("block %d missing 1h TTL: %+v", i+1, b.CacheControl)
		}
	}
}

func TestBuildSystemBlocksSkipsEmptyInputs(t *testing.T) {
	blocks := anthropicbackend.BuildSystemBlocks("", "cli-prefix", "", "", "", true)
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d: %+v", len(blocks), blocks)
	}
	if blocks[0].Text != "cli-prefix" || blocks[0].CacheControl == nil {
		t.Fatalf("unexpected single block: %+v", blocks[0])
	}
}

func TestBuildSystemBlocksDisabledStripsMarkers(t *testing.T) {
	blocks := anthropicbackend.BuildSystemBlocks("billing", "prefix", "caller", "", "", false)
	for i, b := range blocks {
		if b.CacheControl != nil {
			t.Fatalf("block %d carried cache_control when caching disabled: %+v", i, b)
		}
	}
}

func TestRequestMarshalEmitsSystemArray(t *testing.T) {
	r := anthropic.Request{
		Model:     "claude-model",
		MaxTokens: 64,
		SystemBlocks: []anthropic.SystemBlock{
			{Type: "text", Text: "billing"},
			{Type: "text", Text: "prefix", CacheControl: &anthropic.CacheControl{Type: "ephemeral", TTL: "1h"}},
		},
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"system":[`) {
		t.Fatalf("system not emitted as array: %s", body)
	}
	if !strings.Contains(body, `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Fatalf("missing 1h ttl marker: %s", body)
	}
	if strings.Count(body, `"system"`) != 1 {
		t.Fatalf("system emitted twice: %s", body)
	}
}

func TestRequestMarshalFallsBackToStringSystem(t *testing.T) {
	r := anthropic.Request{
		Model:     "claude-model",
		MaxTokens: 64,
		System:    "plain string",
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"system":"plain string"`) {
		t.Fatalf("string system not emitted: %s", body)
	}
}
