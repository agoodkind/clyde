package adapter

import (
	"context"
	"strings"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	adapterruntime "goodkind.io/clyde/internal/adapter/runtime"
	"goodkind.io/gklog/correlation"
)

func providerName(req *adapterresolver.ResolvedRequest, path string) string {
	if req == nil {
		return "claude-cli"
	}
	switch req.Provider {
	case BackendClaude, BackendAnthropic:
		return "anthropic-oauth"
	case BackendCodex:
		if path == "app" {
			return "openai-codex-app"
		}
		return "openai-codex"
	case BackendPassthroughOverride:
		if strings.TrimSpace(req.OpenAICompatPassthrough.BaseURL) != "" {
			return "openai-compat-passthrough"
		}
		name := strings.TrimSpace(req.PassthroughOverrideName)
		if name == "" {
			return "passthrough-override-unknown"
		}
		return "passthrough-override-" + name
	default:
		return "claude-cli"
	}
}

// resolvedRequestAlias derives the caller-facing alias from a resolved
// request for telemetry. It prefers the normalized Cursor model, falls
// back to the raw OpenAI model, then the resolved snapshot alias.
func resolvedRequestAlias(req *adapterresolver.ResolvedRequest) string {
	if req == nil {
		return ""
	}
	if alias := strings.TrimSpace(req.Cursor.NormalizedModel); alias != "" {
		return alias
	}
	if alias := strings.TrimSpace(req.OpenAI.Model); alias != "" {
		return alias
	}
	return req.Alias
}

func (s *Server) emitRequestStarted(ctx context.Context, req *adapterresolver.ResolvedRequest, path, reqID, modelID string, stream bool) {
	backend := ""
	if req != nil {
		backend = req.Provider.String()
	}
	adapterruntime.LogStarted(s.log, ctx, s.deps.RequestEvents, adapterruntime.StartedAttrs{
		Provider:  providerName(req, path),
		Backend:   backend,
		RequestID: reqID,
		Alias:     resolvedRequestAlias(req),
		ModelID:   modelID,
		Stream:    stream, Correlation: correlation.
				Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	})
}

func (s *Server) emitRequestStreamOpened(ctx context.Context, req *adapterresolver.ResolvedRequest, path, reqID, modelID string, stream bool) {
	backend := ""
	if req != nil {
		backend = req.Provider.String()
	}
	adapterruntime.LogStreamOpened(s.log, ctx, s.deps.RequestEvents, adapterruntime.StreamOpenedAttrs{
		Provider:  providerName(req, path),
		Backend:   backend,
		RequestID: reqID,
		Alias:     resolvedRequestAlias(req),
		ModelID:   modelID,
		Stream:    stream, Correlation: correlation.
				Context{TraceID: "", SpanID: "", ParentSpanID: "", RequestID: "", IdentityAttributes: nil},
	})
}
