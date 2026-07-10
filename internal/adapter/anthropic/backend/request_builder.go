package anthropicbackend

import (
	"context"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// requestAlias returns the caller-facing model alias for a resolved
// request. It prefers the normalized Cursor model and falls back to the
// raw OpenAI model so logs and response `model` fields carry the alias
// the client sent, matching the prior ResolvedAlias-based behavior.
func requestAlias(resolved *adapterresolver.ResolvedRequest) string {
	if resolved == nil {
		return ""
	}
	if alias := strings.TrimSpace(resolved.Cursor.NormalizedModel); alias != "" {
		return alias
	}
	return strings.TrimSpace(resolved.OpenAI.Model)
}

// anthropicBackendName is the backend label the Anthropic backend
// reports in completion telemetry. Both the OpenAI ingress and native
// ingress prepared requests resolve to the anthropic backend, so the
// label is constant; it stays a function so callers read it uniformly.
func anthropicBackendName(_ *adapterresolver.ResolvedRequest) string {
	return "anthropic"
}

// FineGrainedToolStreamingBeta is part of Clyde's typed adapter surface.
const FineGrainedToolStreamingBeta = "fine-grained-tool-streaming-2025-05-14"

// BuildRequestConfig is part of Clyde's typed adapter surface.
type BuildRequestConfig struct {
	SystemPromptPrefix              string
	UserAgent                       string
	CCVersion                       string
	CCEntrypoint                    string
	JSONSystemPrompt                string
	PromptCachingEnabled            *bool
	PromptCacheTTL                  string
	PromptCacheScope                string
	ToolResultCacheReferenceEnabled bool
	// Identity sources metadata.user_id. AccountUUID and DeviceID
	// stay constant across requests; the per-request session_id is
	// taken from Cursor's metadata.cursorConversationId via
	// BuildRequest's req.User parameter.
	Identity anthropic.Identity
	// InboundThinkingMaterialization controls how the assistant block
	// builder shapes round-tripped synthetic thinking envelopes that
	// Cursor replays back to us. Empty string falls through to
	// [adapterrender.MaterializeNativeThinkingBlock] (the Anthropic
	// default) so existing call sites stay correct without ceremony.
	InboundThinkingMaterialization adapterrender.MaterializationStrategy
	Logger                         *slog.Logger
}

// BuildRequest is part of Clyde's typed adapter surface.
func BuildRequest(ctx context.Context, req adapteropenai.ChatRequest, resolved *adapterresolver.ResolvedRequest, effort string, cfg BuildRequestConfig, reqID string) (anthropic.Request, error) {
	maxTok := ResolveMaxTokens(requestedOutputTokens(req), resolved)
	strategy := cfg.InboundThinkingMaterialization
	if strategy == "" {
		strategy = adapterrender.MaterializeNativeThinkingBlock
	}
	tr, err := TranslateRequest(req, cfg.SystemPromptPrefix, maxTok, strategy)
	if err != nil {
		return anthropic.Request{}, err
	}
	callerSystem := stripSystemPrefix(tr.System, cfg.SystemPromptPrefix)
	if instr := strings.TrimSpace(resolved.Instructions); instr != "" {
		if callerSystem == "" {
			callerSystem = instr
		} else {
			callerSystem = instr + "\n\n" + callerSystem
		}
	}
	if instr := strings.TrimSpace(cfg.JSONSystemPrompt); instr != "" {
		if callerSystem == "" {
			callerSystem = instr
		} else {
			callerSystem = callerSystem + "\n\n" + instr
		}
	}

	cliVersion := anthropic.VersionFromUserAgent(cfg.UserAgent)
	if cliVersion == "" {
		cliVersion = cfg.CCVersion
	}
	billingHeader := anthropic.BuildAttributionHeader(cliVersion, cfg.CCEntrypoint)
	billingHeader = MutateBillingForProbe(billingHeader, cliVersion, cfg.CCEntrypoint)

	cachingEnabled := true
	if cfg.PromptCachingEnabled != nil {
		cachingEnabled = *cfg.PromptCachingEnabled
	}
	tr.System = ""
	sysBlocks := BuildSystemBlocks(
		billingHeader,
		cfg.SystemPromptPrefix,
		callerSystem,
		normalizePromptCacheTTL(cfg.PromptCacheTTL),
		normalizePromptCacheScope(cfg.PromptCacheScope),
		cachingEnabled,
	)

	strippedModel := StripContextSuffix(resolved.Model)
	out, cacheStats := ToAPIRequest(tr, strippedModel, cfg.ToolResultCacheReferenceEnabled)
	if cacheStats.ToolResultCandidates > 0 && cfg.Logger != nil {
		level := slog.LevelInfo
		if cfg.ToolResultCacheReferenceEnabled {
			level = slog.LevelWarn
		}
		cfg.Logger.LogAttrs(ctx, level, "adapter.cache_breakpoints.tool_result_cache_reference", slog.String("concern", "adapter.chat.render"), slog.String("component", "adapter"),
			slog.String("subcomponent", "oauth"),
			slog.String("request_id", reqID),
			slog.String("alias", requestAlias(resolved)),
			slog.String("model", strippedModel),
			slog.Bool("enabled", cfg.ToolResultCacheReferenceEnabled),
			slog.Int("tool_result_candidates", cacheStats.ToolResultCandidates),
			slog.Int("tool_result_applied", cacheStats.ToolResultApplied),
		)
	}
	out.SystemBlocks = sysBlocks

	// Note: claude-cli does NOT send fine-grained-tool-streaming-2025-05-14
	// (verified against the local Claude Code MITM baseline). The flavor's
	// beta header is the canonical set; do not append it here.
	if effort != "" {
		out.OutputConfig = &anthropic.OutputConfig{Effort: effort}
	}
	ApplyThinkingConfig(&out, resolved, strippedModel)
	out.FeatureVector = resolvedRequestFeatureVector(out, resolved.WireProfile)
	if userID := cfg.Identity.EncodeUserID(); userID != "" {
		out.Metadata = &anthropic.RequestMetadata{UserID: userID}
	}
	// claude-cli sends context_management.clear_thinking when thinking
	// is on (adaptive or enabled). The Stream flag on out is always
	// false at this stage; response_runtime sets it on streaming
	// dispatch. We mirror claude-cli's gate (thinking-on only) so the
	// upstream cache fingerprint matches whether streamed or not.
	if out.Thinking != nil && out.Thinking.Type != "disabled" {
		out.ContextManagement = &anthropic.ContextManagement{
			Edits: []anthropic.ContextManagementEdit{{
				Type: "clear_thinking_20251015",
				Keep: "all",
			}},
		}
	}
	return out, nil
}

func requestedOutputTokens(req adapteropenai.ChatRequest) *int {
	if req.MaxOutputTokens != nil {
		return req.MaxOutputTokens
	}
	if req.MaxTokens != nil {
		return req.MaxTokens
	}
	return req.MaxComplTokens
}

func resolvedRequestFeatureVector(req anthropic.Request, wireProfile string) anthropic.WireFlavorFeatureVector {
	return anthropic.WireFlavorFeatureVector{
		ModelID:     strings.TrimSpace(req.Model),
		WireProfile: strings.TrimSpace(wireProfile),
	}
}

func stripSystemPrefix(system, prefix string) string {
	if prefix == "" || !strings.HasPrefix(system, prefix) {
		return system
	}
	out := strings.TrimPrefix(system, prefix)
	return strings.TrimPrefix(out, "\n\n")
}

func normalizePromptCacheTTL(ttl string) string {
	ttl = strings.TrimSpace(ttl)
	if ttl != "" && ttl != "5m" && ttl != "1h" {
		return ""
	}
	return ttl
}

func normalizePromptCacheScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if scope != "" && scope != "global" && scope != "org" {
		return ""
	}
	return scope
}
