package adapter

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"goodkind.io/clyde/internal/adapter/anthropic"
	anthropicbackend "goodkind.io/clyde/internal/adapter/anthropic/backend"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

var (
	anthropicProcessSessionOnce sync.Once
	anthropicProcessSessionVal  string
)

// anthropicProcessSessionID returns a stable per-daemon-process
// session UUID used as a final fallback for metadata.user_id when
// neither cursorConversationId nor req.User is present.
func anthropicProcessSessionID() string {
	anthropicProcessSessionOnce.Do(func() {
		anthropicProcessSessionVal = uuid.NewString()
	})
	return anthropicProcessSessionVal
}

// buildAnthropicWire maps the OpenAI chat request to a native messages body,
// then applies thinking and effort knobs that are not part of the OpenAI wire
// shape.
func (s *Server) buildAnthropicWire(ctx context.Context, req ChatRequest, resolved *adapterresolver.ResolvedRequest, effort string, jsonSpec JSONResponseSpec, reqID string) (anthropic.Request, error) {
	wire, err := anthropicbackend.BuildRequest(ctx, req, resolved, effort, anthropicbackend.BuildRequestConfig{
		SystemPromptPrefix:              s.anthr.SystemPromptPrefix(),
		UserAgent:                       s.anthr.UserAgent(),
		CCVersion:                       s.anthr.CCVersion(),
		CCEntrypoint:                    s.anthr.CCEntrypoint(),
		JSONSystemPrompt:                jsonSpec.SystemPrompt(false),
		PromptCachingEnabled:            s.cfg.ClientIdentity.PromptCachingEnabled,
		PromptCacheTTL:                  s.cfg.ClientIdentity.PromptCacheTTL,
		PromptCacheScope:                s.cfg.ClientIdentity.PromptCacheScope,
		ToolResultCacheReferenceEnabled: s.cfg.Anthropic.OAuth.ToolResultCacheReferenceEnabled,
		PerContextBetas:                 s.cfg.ClientIdentity.PerContextBetas,
		StructuredOutputPresent:         jsonSpec.Mode != "",
		Identity:                        s.anthropicIdentity(req),
		InboundThinkingMaterialization:  adapterrender.MaterializationStrategy(s.cfg.Anthropic.Reasoning.ResolvedInboundThinking()),
		Logger:                          s.log,
	}, reqID)
	if err != nil {
		slog.WarnContext(ctx, "adapter.oauth.build_anthropic_wire_failed", "concern", "adapter.providers.anthropic.oauth", "request_id", reqID, "err", err)
		return anthropic.Request{}, fmt.Errorf("build anthropic wire request: %w", err)
	}
	return wire, nil
}

// anthropicIdentity assembles the metadata.user_id payload claude-cli
// emits. DeviceID is persisted across runs; AccountUUID is read from
// claude-cli's ~/.claude.json (Anthropic OAuth tokens are opaque so
// JWT extraction is not viable). SessionID prefers the inbound
// metadata.cursorConversationId, falls back to req.User, then to a
// per-daemon-process UUID so the field is never empty (claude-cli
// always sends a non-empty session_id).
func (s *Server) anthropicIdentity(req ChatRequest) anthropic.Identity {
	id := anthropic.Identity{DeviceID: "", AccountUUID: "", SessionID: ""}
	if dev, err := anthropic.DeviceID(); err == nil {
		id.DeviceID = dev
	}
	if uuid, err := anthropic.AccountUUIDFromClaudeConfig(); err == nil && uuid != "" {
		id.AccountUUID = uuid
	}
	switch {
	case metadataString(req.Metadata, "cursorConversationId") != "":
		id.SessionID = metadataString(req.Metadata, "cursorConversationId")
	case req.User != "":
		id.SessionID = req.User
	default:
		id.SessionID = anthropicProcessSessionID()
	}
	return id
}
