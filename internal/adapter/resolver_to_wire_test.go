package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"goodkind.io/clyde/codexwire"
	"goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/config"
)

// resolverToWireConfig is the test fixture for the resolver-to-wire
// integration tests. It configures both an Anthropic family
// (opus-4-7 with thinking_wire_mode=adaptive in modelMatrixConfig)
// and a Codex family (gpt-5.4 with effort tiers) so a single registry
// exercises both providers.
func resolverToWireConfig() config.AdapterConfig {
	cfg := modelMatrixConfig()
	opusProfile := cfg.ModelProfiles["opus"]
	opusProfile.ThinkingProfiles["enabled"] = config.AdapterModelThinkingProfile{
		Mode:         config.AdapterThinkingModeEnabled,
		BudgetTokens: 7000,
	}
	cfg.ModelProfiles["opus"] = opusProfile
	opus := cfg.Models["clyde-opus-4.7-medium"]
	opus.Instructions = "family base instructions"
	cfg.Models["clyde-opus-4.7-medium"] = opus
	cfg.Anthropic.OAuth = config.AdapterOAuth{
		MessagesURL:      "https://example.test/v1/messages",
		AnthropicBeta:    "test-beta",
		AnthropicVersion: "2023-06-01",
		KeychainService:  "test-keychain",
	}
	cfg.Codex.Enabled = true
	cfg.Codex.AuthFile = "~/.codex/auth.json"
	codex := cfg.Models["gpt-5.4"]
	codex.Instructions = "model base instructions"
	cfg.Models["gpt-5.4"] = codex
	return cfg
}

// TestResolverToAnthropicWirePropagatesThinking is the end-to-end
// regression lock for the "thinking dropped at a layer boundary"
// class. It resolves a clyde-opus-4.7-thinking alias through the live
// Registry, projects it through resolver.Resolve, then runs the full
// prepareAnthropicProviderRequest path. The test fails fast if any of
// these layer boundaries silently drop the thinking field:
//
//   - Registry.Resolve to ResolvedModelView (registry_bridge.go)
//   - ResolvedModelView to ResolvedRequest (resolve.go)
//   - ResolvedRequest to anthropic.Request.Thinking
//     (BuildRequest -> ApplyThinkingConfig)
func TestResolverToAnthropicWirePropagatesThinking(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry(resolverToWireConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	bridge := adapterresolver.NewModelRegistryAdapter(registry)
	server := newAnthropicWireServer(t)

	cases := []struct {
		alias              string
		wantThinking       string // anthropic.Thinking.Type
		wantThinkingBudget int
	}{
		{
			// modelMatrixConfig declares thinking_wire_mode = "adaptive"
			// explicitly on the opus-4-7 family, so the resolved wire mode
			// is adaptive (there is no implicit per-model remap).
			alias:        "clyde-opus-4.7-medium-thinking",
			wantThinking: "adaptive",
		},
		{
			alias:        "clyde-opus-4.7-medium",
			wantThinking: "",
		},
		{
			// opus-4-6 declares no thinking_wire_mode, so the empty-default
			// resolves to "enabled".
			alias:              "clyde-opus-4.6-medium-thinking",
			wantThinking:       "enabled",
			wantThinkingBudget: 7000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			cursorReq := buildCursorRequest(tc.alias)
			resolved, err := adapterresolver.Resolve(adapterresolver.IngressCursor, cursorReq, bridge)
			if err != nil {
				t.Fatalf("resolver.Resolve(%s): %v", tc.alias, err)
			}
			prepared, err := server.prepareAnthropicProviderRequest(context.Background(), resolved, "req-resolver-wire-"+tc.alias)
			if err != nil {
				t.Fatalf("prepareAnthropicProviderRequest: %v", err)
			}
			if tc.wantThinking == "" && prepared.Request.Thinking == nil {
				return
			}
			if prepared.Request.Thinking == nil {
				t.Fatalf("Thinking is nil; expected Type=%q", tc.wantThinking)
			}
			if prepared.Request.Thinking.Type != tc.wantThinking {
				t.Fatalf("Thinking.Type = %q want %q (resolver or provider dropped the field)", prepared.Request.Thinking.Type, tc.wantThinking)
			}
			if prepared.Request.Thinking.BudgetTokens != tc.wantThinkingBudget {
				t.Fatalf("Thinking.BudgetTokens = %d want %d (resolver or provider dropped the configured budget)", prepared.Request.Thinking.BudgetTokens, tc.wantThinkingBudget)
			}
		})
	}
}

// TestResolverToAnthropicWirePropagatesEffort locks in that the
// resolver's Effort field reaches the wire as anthropic.OutputConfig
// when the family declares effort tiers. Mirrors the Thinking test
// but for the second field most likely to silently drop.
func TestResolverToAnthropicWirePropagatesEffort(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry(resolverToWireConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	bridge := adapterresolver.NewModelRegistryAdapter(registry)
	server := newAnthropicWireServer(t)

	cases := []struct {
		alias            string
		wantEffort       string
		wantInstructions string
	}{
		{alias: "clyde-opus-4.7-medium-thinking", wantEffort: "medium"},
		{alias: "clyde-opus-4.7-high-thinking", wantEffort: "high"},
	}

	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			cursorReq := buildCursorRequest(tc.alias)
			resolved, err := adapterresolver.Resolve(adapterresolver.IngressCursor, cursorReq, bridge)
			if err != nil {
				t.Fatalf("resolver.Resolve(%s): %v", tc.alias, err)
			}
			prepared, err := server.prepareAnthropicProviderRequest(context.Background(), resolved, "req-effort-"+tc.alias)
			if err != nil {
				t.Fatalf("prepareAnthropicProviderRequest: %v", err)
			}
			if prepared.Request.OutputConfig == nil {
				t.Fatalf("OutputConfig is nil; expected effort=%q", tc.wantEffort)
			}
			if prepared.Request.OutputConfig.Effort != tc.wantEffort {
				t.Fatalf("OutputConfig.Effort = %q want %q", prepared.Request.OutputConfig.Effort, tc.wantEffort)
			}
		})
	}
}

func TestResolverToAnthropicWirePropagatesInstructions(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry(resolverToWireConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	bridge := adapterresolver.NewModelRegistryAdapter(registry)
	server := newAnthropicWireServer(t)

	cursorReq := buildCursorRequest("clyde-opus-4.7-medium")
	resolved, err := adapterresolver.Resolve(adapterresolver.IngressCursor, cursorReq, bridge)
	if err != nil {
		t.Fatalf("resolver.Resolve: %v", err)
	}
	prepared, err := server.prepareAnthropicProviderRequest(context.Background(), resolved, "req-instructions")
	if err != nil {
		t.Fatalf("prepareAnthropicProviderRequest: %v", err)
	}
	if len(prepared.Request.SystemBlocks) < 3 {
		t.Fatalf("SystemBlocks len = %d want at least 3", len(prepared.Request.SystemBlocks))
	}
	if got := prepared.Request.SystemBlocks[2].Text; got != "family base instructions\n\ncaller system instructions" {
		t.Fatalf("SystemBlocks[2].Text = %q want family instructions prepended", got)
	}
}

// TestResolverToCodexWirePropagatesEffort is the parallel of the
// Anthropic test for the Codex provider. It locks in that the Effort
// resolved from a declared clyde-gpt effort alias reaches
// codex.HTTPTransportRequest as the Reasoning.Effort field. If the
// resolver-to-codex layer boundary ever silently drops Effort, this
// test fails immediately. The aliases are the config-declared
// effort-qualified codex aliases (clyde-gpt-5.4-1m-<effort>); there is
// no alias-suffix effort guessing.
func TestResolverToCodexWirePropagatesEffort(t *testing.T) {
	t.Parallel()

	registry, err := NewRegistry(resolverToWireConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	bridge := adapterresolver.NewModelRegistryAdapter(registry)

	cases := []struct {
		alias            string
		wantEffort       string
		wantInstructions string
	}{
		{
			alias:            "clyde-gpt-5.4-1m-medium",
			wantEffort:       "medium",
			wantInstructions: "model base instructions\n\ncaller system instructions",
		},
		{
			alias:            "clyde-gpt-5.4-1m-high",
			wantEffort:       "high",
			wantInstructions: "model base instructions\n\ncaller system instructions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.alias, func(t *testing.T) {
			cursorReq := buildCursorRequest(tc.alias)
			resolved, err := adapterresolver.Resolve(adapterresolver.IngressCursor, cursorReq, bridge)
			if err != nil {
				t.Fatalf("resolver.Resolve(%s): %v", tc.alias, err)
			}
			built := adaptercodex.BuildRequestWithConfig(resolved.OpenAI, &resolved, resolved.Effort.String(), adaptercodex.RequestBuilderConfig{})
			if built.Reasoning == nil {
				t.Fatalf("Reasoning is nil; expected effort=%q", tc.wantEffort)
			}
			if built.Reasoning.Effort != tc.wantEffort {
				t.Fatalf("Reasoning.Effort = %q want %q", built.Reasoning.Effort, tc.wantEffort)
			}
			if built.Instructions != tc.wantInstructions {
				t.Fatalf("Instructions = %q want %q", built.Instructions, tc.wantInstructions)
			}
		})
	}
}

func TestResolverToCodexWireUsesConfiguredEffortValue(t *testing.T) {
	cfg := resolverToWireConfig()
	profile := cfg.ModelProfiles["codex"]
	profile.ReasoningEfforts = append(profile.ReasoningEfforts, config.AdapterReasoningEffort("ultra"))
	profile.ReasoningEffortWireValues = map[config.AdapterReasoningEffort]config.AdapterReasoningEffort{
		"ultra": "max",
	}
	cfg.ModelProfiles["codex"] = profile

	registry, err := NewRegistry(cfg)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	request := buildCursorRequest("gpt-5.4")
	request.OpenAI.ReasoningEffort = "ultra"
	resolved, err := adapterresolver.Resolve(
		adapterresolver.IngressCursor,
		request,
		adapterresolver.NewModelRegistryAdapter(registry),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Effort != "ultra" || resolved.WireEffort != "max" {
		t.Fatalf("resolved effort/wire = %q/%q, want ultra/max", resolved.Effort, resolved.WireEffort)
	}
	built := adaptercodex.BuildRequestWithConfig(
		resolved.OpenAI,
		&resolved,
		resolved.WireEffort.String(),
		adaptercodex.RequestBuilderConfig{},
	)
	if built.Reasoning == nil || built.Reasoning.Effort != "max" {
		t.Fatalf("Reasoning = %+v, want wire effort max", built.Reasoning)
	}
}

func TestUndeclaredCodexWildcardReachesTypedProviderRequest(t *testing.T) {
	registry, err := NewRegistry(wildcardResolverToWireConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	request := wildcardProviderRequest("gpt-undeclared")
	resolved, err := adapterresolver.Resolve(
		adapterresolver.IngressCursor,
		adaptercursor.TranslateRequest(request),
		adapterresolver.NewModelRegistryAdapter(registry),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertWildcardCapabilitiesUnknown(t, resolved)
	server := newAnthropicWireServer(t)
	if preflightErr := server.preflightChat(
		context.Background(),
		&resolved.OpenAI,
		&resolved,
		"req-gpt-wildcard",
	); preflightErr != nil {
		t.Fatalf("preflightChat: %v", preflightErr)
	}
	if resolved.OpenAI.MaxTokens == nil || *resolved.OpenAI.MaxTokens != 250000 {
		t.Fatalf("resolved max_tokens = %v, want 250000", resolved.OpenAI.MaxTokens)
	}

	built := adaptercodex.BuildRequestWithConfig(
		resolved.OpenAI,
		&resolved,
		resolved.Effort.String(),
		adaptercodex.RequestBuilderConfig{},
	)
	if built.Model != "gpt-undeclared" {
		t.Fatalf("Model = %q, want gpt-undeclared", built.Model)
	}
	if built.Reasoning == nil || built.Reasoning.Effort != "invented-provider-tier" {
		t.Fatalf("Reasoning = %+v, want invented-provider-tier", built.Reasoning)
	}
	builtEncoded, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal Codex request: %v", err)
	}
	var builtFields struct {
		MaxTokens           json.RawMessage `json:"max_tokens"`
		MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
		MaxOutputTokens     json.RawMessage `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(builtEncoded, &builtFields); err != nil {
		t.Fatalf("unmarshal Codex request: %v", err)
	}
	for _, tokenCap := range []struct {
		name  string
		value json.RawMessage
	}{
		{"max_tokens", builtFields.MaxTokens},
		{"max_completion_tokens", builtFields.MaxCompletionTokens},
		{"max_output_tokens", builtFields.MaxOutputTokens},
	} {
		if len(tokenCap.value) != 0 {
			t.Fatalf("Codex egress carried %s: %s", tokenCap.name, builtEncoded)
		}
	}
	if len(built.Tools) != 1 || built.Tools[0].Name != "inspect_image" {
		t.Fatalf("Tools = %+v, want inspect_image", built.Tools)
	}
	if string(built.Text) != `{"verbosity":"high"}` {
		t.Fatalf("Text = %s, want verbosity control", built.Text)
	}
	foundImage := false
	for _, item := range built.Input {
		for _, part := range item.Content {
			if part.Type == codexwire.ContentItemInputImage && part.ImageURL == "https://example.invalid/image.png" {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatalf("Codex input dropped image: %+v", built.Input)
	}
}

func TestUndeclaredAnthropicWildcardReachesTypedProviderRequest(t *testing.T) {
	registry, err := NewRegistry(wildcardResolverToWireConfig())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	request := wildcardProviderRequest("claude-undeclared")
	resolved, err := adapterresolver.Resolve(
		adapterresolver.IngressCursor,
		adaptercursor.TranslateRequest(request),
		adapterresolver.NewModelRegistryAdapter(registry),
	)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertWildcardCapabilitiesUnknown(t, resolved)
	server := newAnthropicWireServer(t)
	if preflightErr := server.preflightChat(
		context.Background(),
		&resolved.OpenAI,
		&resolved,
		"req-anthropic-wildcard",
	); preflightErr != nil {
		t.Fatalf("preflightChat: %v", preflightErr)
	}
	prepared, err := server.prepareAnthropicProviderRequest(
		context.Background(),
		resolved,
		"req-anthropic-wildcard",
	)
	if err != nil {
		t.Fatalf("prepareAnthropicProviderRequest: %v", err)
	}
	out := prepared.Request
	if out.Model != "claude-undeclared" || out.MaxTokens != 250000 {
		t.Fatalf("model/max_tokens = %q/%d, want claude-undeclared/250000", out.Model, out.MaxTokens)
	}
	if out.OutputConfig == nil || out.OutputConfig.Effort != "invented-provider-tier" {
		t.Fatalf("OutputConfig = %+v, want invented-provider-tier", out.OutputConfig)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "inspect_image" {
		t.Fatalf("Tools = %+v, want inspect_image", out.Tools)
	}
	foundImage := false
	for _, message := range out.Messages {
		for _, block := range message.Content {
			if block.Type == "image" && block.Source != nil && block.Source.URL == "https://example.invalid/image.png" {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatalf("Anthropic messages dropped image: %+v", out.Messages)
	}
	if out.FeatureVector.WireProfile != "learned-wildcard-default" {
		t.Fatalf("WireProfile = %q, want learned-wildcard-default", out.FeatureVector.WireProfile)
	}
}

func wildcardResolverToWireConfig() config.AdapterConfig {
	cfg := resolverToWireConfig()
	cfg.Anthropic.DefaultWireProfile = "learned-wildcard-default"
	cfg.ModelRoutes = []config.AdapterModelRoute{
		{
			Match:            "gpt-*",
			Surfaces:         []config.AdapterIngressSurface{config.AdapterIngressCursor},
			Provider:         config.AdapterModelProviderCodex,
			WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
			CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
		},
		{
			Match:            "claude-*",
			Surfaces:         []config.AdapterIngressSurface{config.AdapterIngressCursor},
			Provider:         config.AdapterModelProviderAnthropic,
			WireModelPolicy:  config.AdapterWireModelPolicyPreserve,
			CapabilityPolicy: config.AdapterWildcardCapabilityPolicyPassthrough,
		},
	}
	return cfg
}

func wildcardProviderRequest(model string) adapteropenai.ChatRequest {
	maxTokens := 250000
	parallelTools := true
	return adapteropenai.ChatRequest{
		Model: model,
		Messages: []adapteropenai.ChatMessage{{
			Role: "user",
			Content: json.RawMessage(`[
				{"type":"text","text":"inspect this image"},
				{"type":"image_url","image_url":{"url":"https://example.invalid/image.png"}}
			]`),
		}},
		Reasoning: &adapteropenai.Reasoning{Effort: "invented-provider-tier"},
		Tools: []adapteropenai.Tool{{
			Type: "function",
			Function: adapteropenai.ToolFunctionSchema{
				Name:        "inspect_image",
				Description: "Inspect an image.",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		}},
		MaxTokens:     &maxTokens,
		ParallelTools: &parallelTools,
		Text:          json.RawMessage(`{"verbosity":"high"}`),
	}
}

func assertWildcardCapabilitiesUnknown(t *testing.T, resolved adapterresolver.ResolvedRequest) {
	t.Helper()
	if resolved.ToolsCapability != nil || resolved.VisionCapability != nil {
		t.Fatalf("wildcard capabilities = tools:%v vision:%v, want unknown", resolved.ToolsCapability, resolved.VisionCapability)
	}
	if resolved.ContextBudget.InputTokens != 0 || resolved.MaxOutputTokens != 0 {
		t.Fatalf("wildcard context/output = %d/%d, want unknown", resolved.ContextBudget.InputTokens, resolved.MaxOutputTokens)
	}
}

// buildCursorRequest is the minimum-viable inbound shape
// resolver.Resolve consumes. It exercises the same TranslateRequest
// path the live HTTP handler uses, so the test catches anything the
// production resolver entry would catch.
func buildCursorRequest(alias string) adaptercursor.Request {
	return adaptercursor.TranslateRequest(adapteropenai.ChatRequest{
		Model: alias,
		Messages: []adapteropenai.ChatMessage{{
			Role:    "system",
			Content: json.RawMessage(`"caller system instructions"`),
		}, {
			Role:    "user",
			Content: json.RawMessage(`"Say ok."`),
		}},
	})
}

// newAnthropicWireServer builds a minimal *Server with just enough
// state for prepareAnthropicProviderRequest to construct an outbound
// wire request. It omits all live IO; the test only exercises field
// propagation, not network calls.
func newAnthropicWireServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		anthr: anthropic.New(nil, nil, anthropic.Config{
			UserAgent:          "claude-cli/2.1.123",
			SystemPromptPrefix: "You are Claude Code.",
			CCVersion:          "2.1.123",
			CCEntrypoint:       "sdk-cli",
		}),
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}
