package adapter

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

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
		alias        string
		wantThinking string // anthropic.Thinking.Type
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
			alias:        "clyde-opus-4.6-medium-thinking",
			wantThinking: "enabled",
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
