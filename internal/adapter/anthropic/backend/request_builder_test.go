package anthropicbackend

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// resolvedForTest projects the legacy ResolvedAlias fields these tests
// construct into the typed ResolvedRequest the builder now consumes. The
// Alias is carried on Cursor.NormalizedModel so the builder's request
// alias derivation reproduces the prior ResolvedAlias.Alias behavior.
func resolvedForTest(model adaptermodel.ResolvedAlias) *adapterresolver.ResolvedRequest {
	resolved := &adapterresolver.ResolvedRequest{
		Provider:        adaptermodel.BackendAnthropic,
		Family:          model.FamilySlug,
		Model:           model.ClaudeModel,
		Effort:          adapterresolver.Effort(model.Effort),
		ContextBudget:   adapterresolver.ContextBudget{InputTokens: model.Context, OutputTokens: model.MaxOutputTokens, TotalTokens: model.Context},
		Thinking:        model.Thinking,
		Instructions:    model.Instructions,
		Efforts:         model.Efforts,
		Alias:           model.Alias,
		MaxOutputTokens: model.MaxOutputTokens,
	}
	resolved.Cursor.NormalizedModel = model.Alias
	resolved.OpenAI.Model = model.Alias
	return resolved
}

func anthropicID() anthropic.Identity {
	return anthropic.Identity{
		DeviceID:    "dev-1",
		AccountUUID: "acct-1",
		SessionID:   "sess-1",
	}
}

func requestBuilderConfig() BuildRequestConfig {
	return BuildRequestConfig{
		SystemPromptPrefix: "prefix",
		UserAgent:          "clyde-test/1.0.0",
		CCVersion:          "1.0.0",
		CCEntrypoint:       "test",
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func requestBuilderChatRequest() adapteropenai.ChatRequest {
	maxTokens := 4096
	return adapteropenai.ChatRequest{
		Model: "clyde-opus-4-7",
		Messages: []adapteropenai.ChatMessage{
			{Role: "user", Content: []byte(`"hello"`)},
		},
		MaxTokens: &maxTokens,
	}
}

func TestBuildRequestDerivesFeatureVector(t *testing.T) {
	type featureVectorCase struct {
		name                    string
		model                   adaptermodel.ResolvedAlias
		toolsPresent            bool
		structuredOutputPresent bool
		want                    anthropic.WireFlavorFeatureVector
	}
	cases := []featureVectorCase{
		{
			name: "one million context thinking tools and structured output",
			model: adaptermodel.ResolvedAlias{
				Alias:           "clyde-opus-4-7-1m-thinking-enabled",
				ClaudeModel:     "claude-opus-4-7[1m]",
				Context:         1_000_000,
				MaxOutputTokens: 32000,
				Thinking:        adaptermodel.ThinkingEnabled,
			},
			toolsPresent:            true,
			structuredOutputPresent: true,
			want: anthropic.WireFlavorFeatureVector{
				ModelID:                 "claude-opus-4-7",
				Context1M:               true,
				ThinkingMode:            anthropic.WireFlavorThinkingEnabled,
				StructuredOutputPresent: true,
				ToolsPresent:            true,
			},
		},
		{
			name: "standard context no optional request features",
			model: adaptermodel.ResolvedAlias{
				Alias:           "clyde-haiku-4-5",
				ClaudeModel:     "claude-haiku-4-5",
				Context:         200_000,
				MaxOutputTokens: 8192,
				Thinking:        "",
			},
			toolsPresent:            false,
			structuredOutputPresent: false,
			want: anthropic.WireFlavorFeatureVector{
				ModelID:                 "claude-haiku-4-5",
				Context1M:               false,
				ThinkingMode:            anthropic.WireFlavorThinkingNone,
				StructuredOutputPresent: false,
				ToolsPresent:            false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := requestBuilderChatRequest()
			if tc.toolsPresent {
				req.Tools = []adapteropenai.Tool{{
					Type: "function",
					Function: adapteropenai.ToolFunctionSchema{
						Name:        "ReadFile",
						Description: "read a file",
						Parameters:  []byte(`{"type":"object"}`),
					},
				}}
			}
			cfg := requestBuilderConfig()
			cfg.StructuredOutputPresent = tc.structuredOutputPresent
			if tc.structuredOutputPresent {
				cfg.JSONSystemPrompt = "Return JSON only."
			}

			out, err := BuildRequest(context.Background(), req, resolvedForTest(tc.model), "", cfg, "req-test")
			if err != nil {
				t.Fatalf("BuildRequest: %v", err)
			}
			if out.FeatureVector != tc.want {
				t.Fatalf("FeatureVector = %+v, want %+v", out.FeatureVector, tc.want)
			}
		})
	}
}

func TestBuildRequestEmitsMetadataAndContextManagement(t *testing.T) {
	req := requestBuilderChatRequest()
	stream := true
	req.Stream = stream
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7-medium-thinking-enabled",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
		Thinking:        adaptermodel.ThinkingEnabled,
	}
	cfg := requestBuilderConfig()
	cfg.Identity = anthropicID()

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), adaptermodel.EffortMedium, cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.Metadata == nil {
		t.Fatal("Metadata is nil")
	}
	if !strings.Contains(out.Metadata.UserID, `"device_id":"dev-1"`) {
		t.Errorf("Metadata.UserID missing device_id: %s", out.Metadata.UserID)
	}
	if !strings.Contains(out.Metadata.UserID, `"account_uuid":"acct-1"`) {
		t.Errorf("Metadata.UserID missing account_uuid: %s", out.Metadata.UserID)
	}
	if !strings.Contains(out.Metadata.UserID, `"session_id":"sess-1"`) {
		t.Errorf("Metadata.UserID missing session_id: %s", out.Metadata.UserID)
	}
	if out.ContextManagement == nil || len(out.ContextManagement.Edits) != 1 {
		t.Fatalf("ContextManagement missing or wrong length: %+v", out.ContextManagement)
	}
	edit := out.ContextManagement.Edits[0]
	if edit.Type != "clear_thinking_20251015" || edit.Keep != "all" {
		t.Errorf("clear_thinking edit shape wrong: %+v", edit)
	}
}

func TestBuildRequestSkipsContextManagementWhenThinkingOff(t *testing.T) {
	req := requestBuilderChatRequest()
	req.Stream = true
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-haiku-4-5",
		ClaudeModel:     "claude-haiku-4-5",
		MaxOutputTokens: 4096,
	}
	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", requestBuilderConfig(), "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.ContextManagement != nil {
		t.Errorf("ContextManagement should be nil when thinking is off, got %+v", out.ContextManagement)
	}
}

func TestBuildRequestThinkingDisplaySummarizedWhenEnabled(t *testing.T) {
	req := requestBuilderChatRequest()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-haiku-4-5-thinking-enabled",
		ClaudeModel:     "claude-haiku-4-5-20251001",
		MaxOutputTokens: 32000,
		Thinking:        adaptermodel.ThinkingEnabled,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", requestBuilderConfig(), "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if out.Thinking.Type != "enabled" {
		t.Fatalf("Thinking.Type = %q want enabled", out.Thinking.Type)
	}
	if out.Thinking.Display != "summarized" {
		t.Fatalf("Thinking.Display = %q want summarized", out.Thinking.Display)
	}
}

// TestBuildRequestPassesThinkingAdaptiveThrough locks in the contract
// after the registry took ownership of per-family thinking_wire_mode
// resolution. BuildRequest is now a passthrough: whatever Thinking value
// the registry put on the ResolvedAlias is what reaches the wire. A
// family that wants adaptive declares thinking_wire_mode = "adaptive";
// the registry stamps it onto the ResolvedAlias at construction time,
// and BuildRequest does not patch it at request time.
func TestBuildRequestPassesThinkingAdaptiveThrough(t *testing.T) {
	req := requestBuilderChatRequest()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7-medium-thinking",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
		Thinking:        adaptermodel.ThinkingAdaptive,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), adaptermodel.EffortMedium, requestBuilderConfig(), "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if out.Thinking.Type != "adaptive" {
		t.Fatalf("Thinking.Type = %q want adaptive", out.Thinking.Type)
	}
	if out.Thinking.Display != "summarized" {
		t.Fatalf("Thinking.Display = %q want summarized", out.Thinking.Display)
	}
}

// TestBuildRequestPassesThinkingEnabledThrough locks in that an
// operator who explicitly sets thinking_wire_mode = "enabled" on a
// family (including claude-opus-4-7) gets the typed enabled wire shape
// with budget_tokens. The registry honors the explicit choice and the
// request builder no longer rewrites it.
func TestBuildRequestPassesThinkingEnabledThrough(t *testing.T) {
	req := requestBuilderChatRequest()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7-medium-thinking",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
		Thinking:        adaptermodel.ThinkingEnabled,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), adaptermodel.EffortMedium, requestBuilderConfig(), "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if out.Thinking.Type != "enabled" {
		t.Fatalf("Thinking.Type = %q want enabled", out.Thinking.Type)
	}
	if out.Thinking.BudgetTokens != 31999 {
		t.Fatalf("Thinking.BudgetTokens = %d want 31999", out.Thinking.BudgetTokens)
	}
}

func TestBuildRequestHaikuEnabledStaysManual(t *testing.T) {
	req := requestBuilderChatRequest()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-haiku-4-5-thinking-enabled",
		ClaudeModel:     "claude-haiku-4-5-20251001",
		MaxOutputTokens: 16000,
		Thinking:        adaptermodel.ThinkingEnabled,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", requestBuilderConfig(), "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if out.Thinking.Type != "enabled" {
		t.Fatalf("Thinking.Type = %q want enabled", out.Thinking.Type)
	}
	if out.Thinking.BudgetTokens != 15999 {
		t.Fatalf("Thinking.BudgetTokens = %d want 15999", out.Thinking.BudgetTokens)
	}
}

func TestBuildRequestThinkingAdaptiveKeepsSummarizedDisplay(t *testing.T) {
	req := requestBuilderChatRequest()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7-thinking-adaptive",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
		Thinking:        adaptermodel.ThinkingAdaptive,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", requestBuilderConfig(), "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if out.Thinking.Type != "adaptive" {
		t.Fatalf("Thinking.Type = %q want adaptive", out.Thinking.Type)
	}
	if out.Thinking.Display != "summarized" {
		t.Fatalf("Thinking.Display = %q want summarized", out.Thinking.Display)
	}
}

func TestBuildRequestThinkingDisabledHasNoDisplay(t *testing.T) {
	req := requestBuilderChatRequest()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7-thinking-disabled",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
		Thinking:        adaptermodel.ThinkingDisabled,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", requestBuilderConfig(), "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Fatal("Thinking is nil")
	}
	if out.Thinking.Type != "disabled" {
		t.Fatalf("Thinking.Type = %q want disabled", out.Thinking.Type)
	}
	if out.Thinking.Display != "" {
		t.Fatalf("Thinking.Display = %q want empty", out.Thinking.Display)
	}
}

func TestBuildRequestAddsModelInstructionsBeforeCallerSystem(t *testing.T) {
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
		Instructions:    "model base instructions",
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 3 {
		t.Fatalf("SystemBlocks len = %d want 3", len(out.SystemBlocks))
	}
	if got := out.SystemBlocks[2].Text; got != "model base instructions" {
		t.Fatalf("caller system block = %q want model instructions prepended", got)
	}
}

func TestBuildRequestAddsJSONPromptWithoutDuplicatingPrefix(t *testing.T) {
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	cfg.JSONSystemPrompt = "Return JSON only."
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 3 {
		t.Fatalf("SystemBlocks len = %d want 3", len(out.SystemBlocks))
	}
	if out.SystemBlocks[1].Text != "prefix" {
		t.Fatalf("prefix block = %q want prefix", out.SystemBlocks[1].Text)
	}
	if strings.Contains(out.SystemBlocks[2].Text, "prefix") {
		t.Fatalf("caller system should not duplicate prefix: %q", out.SystemBlocks[2].Text)
	}
	if !strings.Contains(out.SystemBlocks[2].Text, "Return JSON only.") {
		t.Fatalf("caller system missing JSON prompt: %q", out.SystemBlocks[2].Text)
	}
}

// CLYDE-217: validate Anthropic system-prefix behavior.
//
// The tests below lock in the exact request prompt shape Clyde emits
// to Anthropic /v1/messages when [adapter.client_identity].system_prompt_prefix
// is configured. They cover: where the prefix lands in SystemBlocks,
// how cache_control and ttl/scope markers attach, the empty-prefix
// fallthrough, the no-system caller case, the prefix-already-prepended
// dedupe, prompt-cache TTL/scope normalization, the caching-disabled
// gate, and the JSON response-format coercion path. Together they
// document the system shaping Clyde owns on Anthropic traffic without
// changing any production behavior.

func TestBuildRequestSystemPrefixBlockOrderingAndCache(t *testing.T) {
	// Anthropic typed system blocks arrive in a stable order:
	// billing, prefix, caller. The prefix block carries an ephemeral
	// cache_control with the configured ttl and scope; the caller
	// block carries an ephemeral cache_control without scope.
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	cfg.PromptCacheTTL = "1h"
	cfg.PromptCacheScope = "global"
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}
	systemMsg := adapteropenai.ChatMessage{Role: "system", Content: []byte(`"caller system text"`)}
	req.Messages = append([]adapteropenai.ChatMessage{systemMsg}, req.Messages...)

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 3 {
		t.Fatalf("SystemBlocks len = %d want 3 (billing, prefix, caller)", len(out.SystemBlocks))
	}
	if out.SystemBlocks[0].Text == "" {
		t.Fatalf("billing block missing: %+v", out.SystemBlocks[0])
	}
	if out.SystemBlocks[0].CacheControl != nil {
		t.Fatalf("billing block must not carry cache_control: %+v", out.SystemBlocks[0].CacheControl)
	}
	if out.SystemBlocks[1].Text != "prefix" {
		t.Fatalf("prefix block text = %q want %q", out.SystemBlocks[1].Text, "prefix")
	}
	if out.SystemBlocks[1].CacheControl == nil {
		t.Fatal("prefix block missing cache_control")
	}
	if out.SystemBlocks[1].CacheControl.Type != "ephemeral" {
		t.Errorf("prefix cache_control.type = %q want ephemeral", out.SystemBlocks[1].CacheControl.Type)
	}
	if out.SystemBlocks[1].CacheControl.TTL != "1h" {
		t.Errorf("prefix cache_control.ttl = %q want 1h", out.SystemBlocks[1].CacheControl.TTL)
	}
	if out.SystemBlocks[1].CacheControl.Scope != "global" {
		t.Errorf("prefix cache_control.scope = %q want global", out.SystemBlocks[1].CacheControl.Scope)
	}
	if out.SystemBlocks[2].Text != "caller system text" {
		t.Fatalf("caller block text = %q want caller system text", out.SystemBlocks[2].Text)
	}
	if out.SystemBlocks[2].CacheControl == nil {
		t.Fatal("caller block missing cache_control")
	}
	if out.SystemBlocks[2].CacheControl.Type != "ephemeral" {
		t.Errorf("caller cache_control.type = %q want ephemeral", out.SystemBlocks[2].CacheControl.Type)
	}
	if out.SystemBlocks[2].CacheControl.TTL != "1h" {
		t.Errorf("caller cache_control.ttl = %q want 1h", out.SystemBlocks[2].CacheControl.TTL)
	}
	if out.SystemBlocks[2].CacheControl.Scope != "" {
		t.Errorf("caller cache_control.scope = %q want empty (scope only on prefix)", out.SystemBlocks[2].CacheControl.Scope)
	}
}

func TestBuildRequestEmptyPrefixOmitsPrefixBlock(t *testing.T) {
	// When system_prompt_prefix is empty, BuildSystemBlocks must not
	// emit a prefix block. The wire shape collapses to billing plus
	// caller (when caller system is non-empty).
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	cfg.SystemPromptPrefix = ""
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}
	systemMsg := adapteropenai.ChatMessage{Role: "system", Content: []byte(`"only caller"`)}
	req.Messages = append([]adapteropenai.ChatMessage{systemMsg}, req.Messages...)

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 2 {
		t.Fatalf("SystemBlocks len = %d want 2 (billing, caller)", len(out.SystemBlocks))
	}
	for i, block := range out.SystemBlocks {
		if block.Text == "prefix" {
			t.Errorf("SystemBlocks[%d] unexpectedly contains prefix text with empty prefix configured", i)
		}
	}
	if out.SystemBlocks[1].Text != "only caller" {
		t.Fatalf("caller block text = %q want only caller", out.SystemBlocks[1].Text)
	}
}

func TestBuildRequestPrefixBlockEmittedWithoutCallerSystem(t *testing.T) {
	// With prefix configured but no caller-supplied system, the wire
	// shape is billing plus prefix only. There must be no empty
	// caller block.
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 2 {
		t.Fatalf("SystemBlocks len = %d want 2 (billing, prefix)", len(out.SystemBlocks))
	}
	if out.SystemBlocks[1].Text != "prefix" {
		t.Fatalf("prefix block text = %q want prefix", out.SystemBlocks[1].Text)
	}
	if out.SystemBlocks[1].CacheControl == nil || out.SystemBlocks[1].CacheControl.Type != "ephemeral" {
		t.Errorf("prefix block must carry ephemeral cache_control: %+v", out.SystemBlocks[1].CacheControl)
	}
}

func TestBuildRequestStripsPrefixWhenCallerAlreadyPrepended(t *testing.T) {
	// If the caller-provided system message already begins with the
	// configured prefix, the prefix must appear once (in its own
	// block) and the caller block must not duplicate it.
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}
	// Caller smuggled the prefix into their system content; Clyde
	// must dedupe so the wire only carries it once.
	callerSystem := "prefix\n\nthe rest of the caller system"
	body, err := encodeChatContent(callerSystem)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	systemMsg := adapteropenai.ChatMessage{Role: "system", Content: body}
	req.Messages = append([]adapteropenai.ChatMessage{systemMsg}, req.Messages...)

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 3 {
		t.Fatalf("SystemBlocks len = %d want 3", len(out.SystemBlocks))
	}
	if out.SystemBlocks[1].Text != "prefix" {
		t.Fatalf("prefix block text = %q want prefix", out.SystemBlocks[1].Text)
	}
	if got := out.SystemBlocks[2].Text; strings.HasPrefix(got, "prefix") {
		t.Fatalf("caller block must not start with prefix after dedupe: %q", got)
	}
	if got := out.SystemBlocks[2].Text; got != "the rest of the caller system" {
		t.Fatalf("caller block text = %q want %q", got, "the rest of the caller system")
	}
}

func TestBuildRequestCachingDisabledDropsCacheMarkers(t *testing.T) {
	// When PromptCachingEnabled is false, neither the prefix block nor
	// the caller block carries cache_control. The block shapes still
	// match the order (billing, prefix, caller).
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	disabled := false
	cfg.PromptCachingEnabled = &disabled
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}
	systemMsg := adapteropenai.ChatMessage{Role: "system", Content: []byte(`"caller text"`)}
	req.Messages = append([]adapteropenai.ChatMessage{systemMsg}, req.Messages...)

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 3 {
		t.Fatalf("SystemBlocks len = %d want 3", len(out.SystemBlocks))
	}
	if out.SystemBlocks[1].CacheControl != nil {
		t.Errorf("prefix block must not carry cache_control with caching disabled: %+v", out.SystemBlocks[1].CacheControl)
	}
	if out.SystemBlocks[2].CacheControl != nil {
		t.Errorf("caller block must not carry cache_control with caching disabled: %+v", out.SystemBlocks[2].CacheControl)
	}
}

func TestBuildRequestNormalizesPromptCacheTTLAndScope(t *testing.T) {
	// Out-of-spec ttl ("90m") and scope ("team") values must
	// normalize to empty strings on the prefix block. This protects
	// the upstream cache fingerprint from drifting on misconfiguration.
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	cfg.PromptCacheTTL = "90m"
	cfg.PromptCacheScope = "team"
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) < 2 {
		t.Fatalf("SystemBlocks len = %d want >= 2", len(out.SystemBlocks))
	}
	prefixBlock := out.SystemBlocks[1]
	if prefixBlock.Text != "prefix" {
		t.Fatalf("expected prefix block at index 1, got %q", prefixBlock.Text)
	}
	if prefixBlock.CacheControl == nil {
		t.Fatal("prefix block missing cache_control")
	}
	if prefixBlock.CacheControl.TTL != "" {
		t.Errorf("ttl = %q want empty after normalization of out-of-spec value", prefixBlock.CacheControl.TTL)
	}
	if prefixBlock.CacheControl.Scope != "" {
		t.Errorf("scope = %q want empty after normalization of out-of-spec value", prefixBlock.CacheControl.Scope)
	}
}

func TestBuildRequestJSONPromptAppendsAfterCallerSystemBelowPrefix(t *testing.T) {
	// JSONSystemPrompt covers Clyde's response-format coercion path.
	// The injected JSON instruction must land in the caller block,
	// not the prefix block, and must be ordered after any caller-
	// supplied system content.
	req := requestBuilderChatRequest()
	cfg := requestBuilderConfig()
	cfg.JSONSystemPrompt = "Return JSON only."
	model := adaptermodel.ResolvedAlias{
		Alias:           "clyde-opus-4-7",
		ClaudeModel:     "claude-opus-4-7",
		MaxOutputTokens: 32000,
	}
	callerSystem := "caller policy"
	body, err := encodeChatContent(callerSystem)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	systemMsg := adapteropenai.ChatMessage{Role: "system", Content: body}
	req.Messages = append([]adapteropenai.ChatMessage{systemMsg}, req.Messages...)

	out, err := BuildRequest(context.Background(), req, resolvedForTest(model), "", cfg, "req-test")
	if err != nil {
		t.Fatalf("BuildRequest: %v", err)
	}
	if len(out.SystemBlocks) != 3 {
		t.Fatalf("SystemBlocks len = %d want 3", len(out.SystemBlocks))
	}
	if out.SystemBlocks[1].Text != "prefix" {
		t.Fatalf("prefix block text = %q want prefix", out.SystemBlocks[1].Text)
	}
	if strings.Contains(out.SystemBlocks[1].Text, "Return JSON only.") {
		t.Fatalf("JSON prompt must not be merged into prefix block: %q", out.SystemBlocks[1].Text)
	}
	caller := out.SystemBlocks[2].Text
	if !strings.Contains(caller, callerSystem) {
		t.Errorf("caller block missing caller system %q: %q", callerSystem, caller)
	}
	if !strings.Contains(caller, "Return JSON only.") {
		t.Errorf("caller block missing JSON prompt: %q", caller)
	}
	if idxCaller := strings.Index(caller, callerSystem); idxCaller >= 0 {
		idxJSON := strings.Index(caller, "Return JSON only.")
		if idxJSON < idxCaller {
			t.Errorf("JSON prompt appears before caller system in caller block: %q", caller)
		}
	}
}

// encodeChatContent is a typed test helper that wraps a plain text
// system prompt as the JSON-encoded ChatMessage.Content shape the
// translator expects. Keeping this helper local avoids reaching for
// loose map[string]any test fixtures in production-shaped tests.
func encodeChatContent(text string) ([]byte, error) {
	return json.Marshal(text)
}
