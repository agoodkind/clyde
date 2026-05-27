package adapter

import (
	"context"
	"log/slog"
	"net/http"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
)

// This file holds every dispatch-time logging helper that consumes a
// vendor IngressContract or IngressContext. Keeping these in one
// place makes the ingress boundary visible: anything that needs
// vendor-typed log attributes goes through ingress.LogAttrs in this
// file, and the rest of the dispatcher in server_dispatch.go speaks
// only ChatRequest, ResolvedModel, and the typed adapter types.

// logChatResolveFailed records a model lookup failure with vendor
// ingress context. Vendor-specific log attrs come back through the
// registered IngressContract so the dispatcher carries no Cursor type.
func (s *Server) logChatResolveFailed(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, ingressCtx ingresscontract.IngressContext, ingress ingresscontract.IngressContract, err error) {
	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("model", req.Model),
		slog.String("err", err.Error()),
	}
	attrs = append(attrs, ingress.LogAttrs(ingressCtx, req.Model, nil)...)
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsResolve).LogAttrs(ctx, slog.LevelWarn, "adapter.model.resolve_failed", attrs...)
}

// logChatResolved records the successful resolution of a chat alias.
func (s *Server) logChatResolved(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, ingressCtx ingresscontract.IngressContext, ingress ingresscontract.IngressContract, model ResolvedModel, effort string) {
	resolveAttrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("backend", model.Backend),
		slog.String("resolved_model", model.ClaudeModel),
		slog.String("effort", effort),
		slog.Int("context_window", model.Context),
		slog.Bool("stream", req.Stream),
	}
	resolveAttrs = append(resolveAttrs, ingress.LogAttrs(ingressCtx, req.Model, nil)...)
	resolveAttrs = append(resolveAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsResolve).LogAttrs(ctx, slog.LevelInfo, "adapter.model.resolved", resolveAttrs...)
}

// logResolverOutcome emits the typed-resolver shadow event and stamps
// request id and correlation onto the resolved request when it succeeded.
func (s *Server) logResolverOutcome(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, ingressCtx ingresscontract.IngressContext, resolvedReq *adapterresolver.ResolvedRequest, resolverErr error) {
	if resolverErr != nil {
		slogger.WithConcern(s.log, slogger.ConcernAdapterModelsResolve).LogAttrs(ctx, slog.LevelDebug, "adapter.resolver.unresolved",
			slog.String("request_id", reqID),
			slog.String("alias", req.Model),
			slog.String("err", resolverErr.Error()),
		)
		return
	}
	resolvedReq.RequestID = reqID
	resolvedReq.Correlation = corr
	resolverAttrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("provider", resolvedReq.Provider.String()),
		slog.String("family", resolvedReq.Family),
		slog.String("model", resolvedReq.Model),
		slog.String("effort", resolvedReq.Effort.String()),
		slog.Int("input_tokens_budget", resolvedReq.ContextBudget.InputTokens),
		slog.Int("output_tokens_budget", resolvedReq.ContextBudget.OutputTokens),
		slog.String("conversation_id", ingressCtx.ConversationID),
		slog.Bool("subagent_tool_available", ingressCtx.HasSubagentTool),
		slog.Bool("has_create_plan_tool", ingressCtx.HasCreatePlanTool),
		slog.Bool("has_apply_patch_tool", ingressCtx.HasApplyPatchTool),
		slog.Int("mcp_tool_count", len(ingressCtx.MCPToolNames)),
	}
	resolverAttrs = append(resolverAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsResolve).LogAttrs(ctx, slog.LevelInfo, "adapter.resolver.resolved", resolverAttrs...)
}

// logChatReceived records the dispatch-time view of a chat request.
func (s *Server) logChatReceived(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, ingressCtx ingresscontract.IngressContext, ingress ingresscontract.IngressContract, model ResolvedModel, toolNames []string) {
	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("model", model.ClaudeModel),
		slog.String("backend", model.Backend),
		slog.Int("message_count", len(req.Messages)),
		slog.Int("tool_count", len(req.Tools)+len(req.Functions)),
		slog.Any("tool_names", toolNames),
		slog.Bool("stream", req.Stream),
	}
	attrs = append(attrs, ingress.LogAttrs(ingressCtx, req.Model, toolNames)...)
	attrs = appendFacetSlogAttrs(attrs, ingress.RequestFacets(ingressCtx))
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatDispatch).LogAttrs(ctx, slog.LevelInfo, "adapter.chat.received", attrs...)
}

// logSubagentMissingGenerationID flags a vendor subagent request that
// arrived without the expected generation_id metadata.
func (s *Server) logSubagentMissingGenerationID(ctx context.Context, r *http.Request, corr correlation.Context, reqID string, ingressCtx ingresscontract.IngressContext, ingress ingresscontract.IngressContract, discovery RequestDiscovery) {
	missingAttrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.Bool("subagent_tool_available", ingressCtx.HasSubagentTool),
		slog.Any("metadata_keys", discovery.MetadataKeys),
		slog.Any("header_names", HeaderNames(r.Header)),
	}
	missingAttrs = append(missingAttrs, ingress.LogAttrs(ingressCtx, "", ingressCtx.RawToolNames)...)
	missingAttrs = append(missingAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsCursor).LogAttrs(ctx, slog.LevelInfo, "adapter.cursor.generation_id_missing", missingAttrs...)
}

// logChatForkDetected records the per-daemon branch fork event when
// the registered IngressContract reports one for this request.
func (s *Server) logChatForkDetected(ctx context.Context, corr correlation.Context, identity ingresscontract.ChatIdentityPrimitive) {
	if !identity.Fork.Detected {
		return
	}
	attrs := []slog.Attr{
		slog.String("chat_root_key", identity.ChatRootKey),
		slog.String("chat_branch_key", identity.ChatBranchKey),
		slog.String("prior_branch_key", identity.Fork.PriorBranchKey),
		slog.Int("message_count", identity.MessageCount),
		slog.Int("tool_count", identity.ToolCount),
		slog.Int("common_prefix_len", identity.Fork.CommonPrefixLen),
	}
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatDispatch).LogAttrs(ctx, slog.LevelInfo, "adapter.chat.fork_detected", attrs...)
}
