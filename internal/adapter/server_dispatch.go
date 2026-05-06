package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/slogger"
)

// CountNormalizedTools counts tools that arrived without a `function` key
// and were likely sent in native alternate shape.
func CountNormalizedTools(req ChatRequest, raw []byte) int {
	if len(req.Tools) == 0 {
		return 0
	}
	var wire struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return 0
	}

	count := 0
	for _, rawTool := range wire.Tools {
		var w struct {
			Function json.RawMessage `json:"function"`
		}
		if err := json.Unmarshal(rawTool, &w); err != nil {
			continue
		}
		if len(w.Function) == 0 {
			count++
		}
	}
	return count
}

func (s *Server) handleModels(ctx context.Context, hctx *handlerCtx) error {
	w := hctx.Writer
	r := hctx.Request
	corr := hctx.Correlation
	corr.SetHTTPHeaders(w.Header())
	entries := s.registry.List()
	fingerprint := modelCatalogFingerprint(entries)
	resp := ModelsResponse{Object: "list"}
	for _, m := range entries {
		entry := modelEntryFromResolved(m)
		if m.Backend == BackendCodex {
			entry = adaptercodex.ApplyCapabilityReport(entry, adaptercodex.CapabilityReportForModel(m, adaptercodex.CapabilityMode{
				WebsocketEnabled: s.codexWebsocketEnabled(),
			}))
		}
		resp.Data = append(resp.Data, entry)
	}
	writeJSON(w, http.StatusOK, resp)
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("remote_addr", r.RemoteAddr),
		slog.String("user_agent", r.UserAgent()),
		slog.Int("model_count", len(entries)),
		slog.String("catalog_fingerprint", fingerprint),
	}
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsCatalog).LogAttrs(ctx, slog.LevelInfo, "adapter.models.listed", attrs...)
	return nil
}

func modelEntryFromResolved(m ResolvedModel) ModelEntry {
	return ModelEntry{
		ID:                               m.Alias,
		Object:                           "model",
		OwnedBy:                          "clyde",
		Context:                          m.Context,
		ContextWindow:                    m.Context,
		ContextLength:                    m.Context,
		MaxContextLength:                 m.Context,
		MaxContextTokens:                 m.Context,
		MaxModelLen:                      m.Context,
		MaxTokens:                        m.Context,
		InputTokenLimit:                  m.Context,
		MaxInputTokens:                   m.Context,
		ContextTokenLimit:                m.Context,
		ContextTokenLimitCamel:           m.Context,
		ContextTokenLimitForMaxMode:      m.Context,
		ContextTokenLimitForMaxModeCamel: m.Context,
		Efforts:                          m.Efforts,
		Backend:                          m.Backend,
		ClaudeModel:                      m.ClaudeModel,
	}
}

func (s *Server) handleChat(ctx context.Context, hctx *handlerCtx) error {
	w := hctx.Writer
	r := hctx.Request
	if r.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}
	corr := hctx.Correlation
	reqID := corr.RequestID
	corr.SetHTTPHeaders(w.Header())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		return adapterErrInvalidRequest("failed to read body", err)
	}

	bodyBytes := len(body)
	s.logChatIngress(ctx, r, corr, reqID, bodyBytes)
	discovery := DiscoverRequest(body)
	s.logChatDiscovery(ctx, r, corr, reqID, discovery)

	var req ChatRequest
	parseErr := json.Unmarshal(body, &req)
	s.emitRawChatLog(ctx, r, corr, reqID, body, bodyBytes, &req, parseErr)

	if parseErr != nil {
		s.logChatParseFailed(ctx, corr, reqID, bodyBytes, parseErr)
		return adapterErrInvalidJSON("invalid JSON: "+parseErr.Error(), parseErr)
	}
	s.logToolNormalization(ctx, corr, reqID, req, body)
	if normErr := s.normalizeRequestMessages(ctx, corr, reqID, &req); normErr != nil {
		return normErr
	}
	if len(req.Messages) == 0 {
		s.logMessagesRequired(ctx, corr, reqID, req)
		return adapterErrInvalidRequest("messages is required", nil)
	}
	if req.ReasoningEffort == "" && req.Reasoning != nil {
		req.ReasoningEffort = strings.TrimSpace(req.Reasoning.Effort)
	}
	cursorReq := adaptercursor.TranslateRequest(req)
	corr = corr.WithCursor(cursorReq.RequestID, cursorReq.ConversationID)
	if cursorReq.GenerationID != "" {
		corr = corr.WithCursorGenerationID(cursorReq.GenerationID)
	}
	corr = resolveChatKey(corr, cursorReq, req)
	ctx = correlation.WithContext(ctx, corr)
	r = r.WithContext(ctx)
	req.Model = cursorReq.NormalizedModel

	model, effort, err := s.registry.Resolve(req.Model, req.ReasoningEffort)
	if err != nil {
		s.logChatResolveFailed(ctx, corr, reqID, req, cursorReq, err)
		return adapterErrModelNotFound(err.Error())
	}
	s.logChatResolved(ctx, corr, reqID, req, cursorReq, model, effort)

	// Step D: build the typed resolver.ResolvedRequest alongside the
	// legacy resolution. Backends still use model.ResolvedModel for now;
	// this call validates the resolver in production traffic and emits a
	// telemetry event so we can confirm provider+effort+budget mapping is
	// consistent before flipping the dispatcher to use it.
	resolvedReq, resolverErr := adapterresolver.Resolve(cursorReq, adapterresolver.NewModelRegistryAdapter(s.registry))
	s.logResolverOutcome(ctx, corr, reqID, req, cursorReq, &resolvedReq, resolverErr)

	model, err = s.applyBackendOverride(r, req, model, reqID)
	if err != nil {
		return err
	}

	toolNames := chatToolNames(req)
	s.logChatReceived(ctx, corr, reqID, req, cursorReq, model, toolNames)
	if cursorReq.PathKind == adaptercursor.RequestPathSubagent && cursorReq.GenerationID == "" {
		s.logSubagentMissingGenerationID(ctx, r, corr, reqID, cursorReq, discovery)
	}

	if perr := s.preflightChat(ctx, &req, model, reqID); perr != nil {
		return perr
	}

	s.dispatchResolvedChat(w, r, req, model, effort, reqID, body, cursorReq, resolvedReq, resolverErr)
	return nil
}

// logChatIngress emits the per-request adapter ingress event.
func (s *Server) logChatIngress(ctx context.Context, r *http.Request, corr correlation.Context, reqID string, bodyBytes int) {
	ingressAttrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("remote_addr", r.RemoteAddr),
		slog.Int("body_bytes", bodyBytes),
		slog.String("user_agent", r.UserAgent()),
		slog.String("cf_ray", r.Header.Get("Cf-Ray")),
		slog.String("cf_connecting_ip", r.Header.Get("Cf-Connecting-Ip")),
	}
	ingressAttrs = append(ingressAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPIngress).LogAttrs(ctx, slog.LevelInfo, "adapter.chat.ingress", ingressAttrs...)
}

// logChatDiscovery emits the structural discovery event for a chat body.
func (s *Server) logChatDiscovery(ctx context.Context, r *http.Request, corr correlation.Context, reqID string, discovery RequestDiscovery) {
	discoveryAttrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.Int("body_bytes", discovery.BodyBytes),
		slog.Any("top_level_keys", discovery.TopLevelKeys),
		slog.Any("unknown_keys", discovery.UnknownKeys),
		slog.Any("metadata_keys", discovery.MetadataKeys),
		slog.Bool("metadata_is_object", discovery.MetadataIsObject),
		slog.Any("input_item_keys", discovery.InputItemKeys),
		slog.Any("input_item_roles", discovery.InputItemRoles),
		slog.Any("input_item_types", discovery.InputItemTypes),
		slog.Any("input_content_types", discovery.InputContentTypes),
		slog.Int("tool_count", discovery.ToolCount),
		slog.Any("tool_kinds", discovery.ToolKinds),
		slog.Any("tool_function_top_keys", discovery.ToolFunctionTopKeys),
		slog.Any("tool_custom_top_keys", discovery.ToolCustomTopKeys),
		slog.Any("tool_custom_format_keys", discovery.ToolCustomFormatKeys),
		slog.Any("tool_function_names", discovery.ToolFunctionNames),
		slog.Any("tool_custom_names", discovery.ToolCustomNames),
		slog.Any("mcp_tool_names", discovery.MCPToolNames),
		slog.Bool("has_mcp_like_fields", discovery.HasMCPLikeFields),
		slog.Any("mcp_like_field_names", discovery.MCPLikeFieldNames),
		slog.Any("header_names", HeaderNames(r.Header)),
	}
	discoveryAttrs = append(discoveryAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatDiscovery).LogAttrs(ctx, slog.LevelInfo, "adapter.chat.discovery", discoveryAttrs...)
}

// emitRawChatLog assembles the optional raw-body log payload per the
// configured body-logging mode and writes it when logging is enabled.
func (s *Server) emitRawChatLog(ctx context.Context, r *http.Request, corr correlation.Context, reqID string, body []byte, bodyBytes int, req *ChatRequest, parseErr error) {
	rawAttrs := rawChatLogEvent{
		RequestID:   reqID,
		Method:      r.Method,
		Path:        r.URL.Path,
		RemoteAddr:  r.RemoteAddr,
		Headers:     redactedHeaders(r.Header),
		BodyBytes:   bodyBytes,
		Correlation: corr,
	}
	bodyLogging := s.bodyLogging()
	bodyLimit := bodyLogging.MaxKB * 1024

	switch bodyLogging.Mode {
	case "summary":
		if parseErr == nil {
			bodySummary := SummarizeChatRequest(*req)
			rawAttrs.BodySummary = &bodySummary
		}
	case "whitelist":
		if parseErr == nil {
			bodySummary := SummarizeChatRequest(*req)
			rawAttrs.BodySummary = &bodySummary
			rawAttrs.BodyRaw, rawAttrs.BodyTruncated = buildWhitelistBody(*req, bodyLimit)
		} else {
			rawAttrs.BodyRaw, rawAttrs.BodyTruncated = truncateBody(body, bodyLimit)
		}
	case "raw":
		if parseErr == nil {
			bodySummary := SummarizeChatRequest(*req)
			rawAttrs.BodySummary = &bodySummary
		}
		rawAttrs.BodyRaw, rawAttrs.BodyTruncated = truncateBody(body, bodyLimit)
		rawAttrs.BodyB64 = encodeBodyB64(body, bodyLimit)
	case "off", "":
	default:
		rawAttrs.BodyRaw, rawAttrs.BodyTruncated = truncateBody(body, bodyLimit)
	}
	if bodyLogging.Mode != "off" {
		slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPRaw).LogAttrs(ctx, slog.LevelDebug, "adapter.chat.raw", rawAttrs.asAttrs()...)
	}
}

// logToolNormalization records when tools arrived in non-OpenAI shape.
func (s *Server) logToolNormalization(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, body []byte) {
	n := CountNormalizedTools(req, body)
	if n == 0 {
		return
	}
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatDiscovery).LogAttrs(ctx, slog.LevelInfo, "adapter.tools.normalized",
		correlation.AppendAttrs([]slog.Attr{
			slog.String("request_id", reqID),
			slog.String("from_shape", "anthropic_native"),
			slog.Int("count", n),
		}, corr)...,
	)
}

// logChatParseFailed records a body that did not parse as JSON.
func (s *Server) logChatParseFailed(ctx context.Context, corr correlation.Context, reqID string, bodyBytes int, parseErr error) {
	slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPErrors).LogAttrs(ctx, slog.LevelWarn, "adapter.chat.parse_failed",
		correlation.AppendAttrs([]slog.Attr{
			slog.String("request_id", reqID),
			slog.String("err", parseErr.Error()),
			slog.Int("body_bytes", bodyBytes),
		}, corr)...,
	)
}

// normalizeRequestMessages folds Responses-style input into req.Messages.
// Returns a non-nil adapter error when the caller should abort.
func (s *Server) normalizeRequestMessages(ctx context.Context, corr correlation.Context, reqID string, req *ChatRequest) error {
	if len(req.Messages) != 0 || len(req.Input) == 0 {
		return nil
	}
	count, nerr := parseMessagesFromInput(req)
	if nerr != nil {
		slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.messages.normalize_failed",
			correlation.AppendAttrs([]slog.Attr{
				slog.String("request_id", reqID),
				slog.String("model", req.Model),
				slog.String("err", nerr.Error()),
			}, corr)...,
		)
		return adapterErrInvalidRequest(nerr.Error(), nerr)
	}
	if count > 0 {
		slogger.WithConcern(s.log, slogger.ConcernAdapterChatDiscovery).LogAttrs(ctx, slog.LevelInfo, "adapter.messages.normalized",
			correlation.AppendAttrs([]slog.Attr{
				slog.String("request_id", reqID),
				slog.String("from_shape", "responses_input"),
				slog.Int("count", count),
			}, corr)...,
		)
	}
	return nil
}

// logMessagesRequired records a request that arrived with no messages.
func (s *Server) logMessagesRequired(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest) {
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.chat.validation_failed",
		correlation.AppendAttrs([]slog.Attr{
			slog.String("request_id", reqID),
			slog.String("model", req.Model),
			slog.String("reason", "messages_required"),
		}, corr)...,
	)
}

// logChatResolveFailed records a model lookup failure with cursor context.
func (s *Server) logChatResolveFailed(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, cursorReq adaptercursor.Request, err error) {
	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("model", req.Model),
		slog.String("err", err.Error()),
	}
	attrs = append(attrs, adaptercursor.BoundaryLogAttrs(cursorReq, cursorReq.OpenAI.Model, nil)...)
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsResolve).LogAttrs(ctx, slog.LevelWarn, "adapter.model.resolve_failed", attrs...)
}

// logChatResolved records the successful resolution of a chat alias.
func (s *Server) logChatResolved(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, cursorReq adaptercursor.Request, model ResolvedModel, effort string) {
	resolveAttrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("backend", model.Backend),
		slog.String("resolved_model", model.ClaudeModel),
		slog.String("effort", effort),
		slog.Int("context_window", model.Context),
		slog.Bool("stream", req.Stream),
	}
	resolveAttrs = append(resolveAttrs, adaptercursor.BoundaryLogAttrs(cursorReq, cursorReq.OpenAI.Model, nil)...)
	resolveAttrs = append(resolveAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsResolve).LogAttrs(ctx, slog.LevelInfo, "adapter.model.resolved", resolveAttrs...)
}

// logResolverOutcome emits the typed-resolver shadow event and stamps
// request id and correlation onto the resolved request when it succeeded.
func (s *Server) logResolverOutcome(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, cursorReq adaptercursor.Request, resolvedReq *adapterresolver.ResolvedRequest, resolverErr error) {
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
		slog.String("conversation_id", cursorReq.ConversationID),
		slog.Bool("subagent_tool_available", cursorReq.HasSubagentTool),
		slog.Bool("has_create_plan_tool", cursorReq.HasCreatePlanTool),
		slog.Bool("has_apply_patch_tool", cursorReq.HasApplyPatchTool),
		slog.Int("mcp_tool_count", len(cursorReq.MCPToolNames)),
	}
	resolverAttrs = append(resolverAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsResolve).LogAttrs(ctx, slog.LevelInfo, "adapter.resolver.resolved", resolverAttrs...)
}

// chatToolNames flattens tool and legacy function names from a chat request.
func chatToolNames(req ChatRequest) []string {
	toolNames := make([]string, 0, len(req.Tools)+len(req.Functions))
	for _, t := range req.Tools {
		toolNames = append(toolNames, t.Function.Name)
	}
	for _, f := range req.Functions {
		toolNames = append(toolNames, f.Name)
	}
	return toolNames
}

// logChatReceived records the dispatch-time view of a chat request.
func (s *Server) logChatReceived(ctx context.Context, corr correlation.Context, reqID string, req ChatRequest, cursorReq adaptercursor.Request, model ResolvedModel, toolNames []string) {
	cursor := cursorReq.Context()
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
	if cursor.ConversationID != "" {
		attrs = append(attrs, slog.String("cursor_conversation_id", cursor.ConversationID))
	}
	if cursor.RequestID != "" {
		attrs = append(attrs, slog.String("cursor_request_id", cursor.RequestID))
	}
	attrs = append(attrs, adaptercursor.BoundaryLogAttrs(cursorReq, cursorReq.OpenAI.Model, toolNames)...)
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatDispatch).LogAttrs(ctx, slog.LevelInfo, "adapter.chat.received", attrs...)
}

// logSubagentMissingGenerationID flags a Cursor subagent request that
// arrived without the expected generation_id metadata.
func (s *Server) logSubagentMissingGenerationID(ctx context.Context, r *http.Request, corr correlation.Context, reqID string, cursorReq adaptercursor.Request, discovery RequestDiscovery) {
	missingAttrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("cursor_conversation_id", cursorReq.ConversationID),
		slog.String("cursor_request_path", string(cursorReq.PathKind)),
		slog.Bool("subagent_tool_available", cursorReq.HasSubagentTool),
		slog.Any("metadata_keys", discovery.MetadataKeys),
		slog.Any("header_names", HeaderNames(r.Header)),
	}
	missingAttrs = append(missingAttrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsCursor).LogAttrs(ctx, slog.LevelInfo, "adapter.cursor.generation_id_missing", missingAttrs...)
}

func truncateBody(body []byte, maxBytes int) (string, bool) {
	if maxBytes <= 0 {
		return "", false
	}
	if len(body) <= maxBytes {
		return string(body), false
	}
	return string(body[:maxBytes]), true
}

func buildWhitelistBody(req ChatRequest, maxBytes int) (string, bool) {
	type whitelistTool struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	type whitelistMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	payloadMessages := make([]whitelistMessage, 0, len(req.Messages))
	payload := map[string]any{
		"model":    req.Model,
		"stream":   req.Stream,
		"messages": payloadMessages,
	}

	for _, msg := range req.Messages {
		content := FlattenContent(msg.Content)
		if len(content) > 2048 {
			content = content[:2048]
		}
		payloadMessages = append(payloadMessages, whitelistMessage{
			Role:    msg.Role,
			Content: content,
		})
	}
	payload["messages"] = payloadMessages
	if len(req.Tools) > 0 {
		tools := make([]whitelistTool, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, whitelistTool{
				Type: "function",
				Function: struct {
					Name string `json:"name"`
				}{Name: tool.Function.Name},
			})
		}
		payload["tools"] = tools
	}
	if len(req.Functions) > 0 {
		functions := make([]whitelistTool, 0, len(req.Functions))
		for _, fn := range req.Functions {
			functions = append(functions, whitelistTool{
				Type: "function",
				Function: struct {
					Name string `json:"name"`
				}{Name: fn.Name},
			})
		}
		payload["functions"] = functions
	}
	if req.ToolChoice != nil {
		payload["tool_choice"] = req.ToolChoice
	}
	if req.ParallelTools != nil {
		payload["parallel_tool_calls"] = req.ParallelTools
	}
	if req.Logprobs != nil {
		payload["logprobs"] = req.Logprobs
	}
	if req.Temperature != nil {
		payload["temperature"] = req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = req.TopP
	}
	if req.MaxTokens != nil {
		payload["max_tokens"] = req.MaxTokens
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}

	logBody, bodyTruncated := truncateBody(raw, maxBytes)
	return logBody, bodyTruncated
}

func parseMessagesFromInput(req *ChatRequest) (int, error) {
	var inputItems []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(req.Input, &inputItems); err != nil {
		return 0, fmt.Errorf("invalid input payload: %w", err)
	}
	if len(inputItems) == 0 {
		return 0, nil
	}

	messages := make([]ChatMessage, 0, len(inputItems))
	for _, item := range inputItems {
		role := strings.TrimSpace(item.Role)
		if role == "" {
			continue
		}
		content, err := parseInputContent(item.Content)
		if err != nil {
			return 0, err
		}
		messages = append(messages, ChatMessage{
			Role:    role,
			Content: content,
		})
	}
	if len(messages) == 0 {
		return 0, nil
	}
	req.Messages = messages
	return len(messages), nil
}

func parseInputContent(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage(`""`), nil
	}

	switch trimmed[0] {
	case '"':
		// Plain OpenAI string content.
		return json.RawMessage(trimmed), nil
	case '{':
		var part map[string]any
		if err := json.Unmarshal(raw, &part); err != nil {
			return nil, fmt.Errorf("invalid input content: %w", err)
		}
		return parseInputParts([]map[string]any{part})
	case '[':
		var parts []map[string]any
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, fmt.Errorf("invalid input content: %w", err)
		}
		return parseInputParts(parts)
	default:
		return nil, fmt.Errorf("invalid input content type")
	}
}

func parseInputParts(parts []map[string]any) (json.RawMessage, error) {
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		typ, _ := p["type"].(string)
		switch typ {
		case "text", "input_text", "output_text":
			text, _ := p["text"].(string)
			out = append(out, map[string]any{
				"type": "text",
				"text": text,
			})
		case "image_url":
			out = append(out, map[string]any{
				"type":      "image_url",
				"image_url": p["image_url"],
			})
		case "input_image":
			image := map[string]any{}
			switch v := p["image_url"].(type) {
			case map[string]any:
				image = v
			case string:
				image["url"] = v
			}
			if len(image) == 0 {
				continue
			}
			out = append(out, map[string]any{
				"type":      "image_url",
				"image_url": image,
			})
		}
	}
	if len(out) == 0 {
		return json.RawMessage(`""`), nil
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize input content: %w", err)
	}
	return buf, nil
}

func (s *Server) handleLegacy(ctx context.Context, hctx *handlerCtx) error {
	r := hctx.Request
	if r.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}
	var legacy struct {
		Model           string `json:"model"`
		Prompt          string `json:"prompt"`
		Stream          bool   `json:"stream,omitempty"`
		ReasoningEffort string `json:"reasoning_effort,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&legacy); err != nil {
		return adapterErrInvalidJSON(err.Error(), err)
	}
	synthetic := ChatRequest{
		Model:           legacy.Model,
		Stream:          legacy.Stream,
		ReasoningEffort: legacy.ReasoningEffort,
		Messages: []ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(strconv.Quote(legacy.Prompt)),
		}},
	}
	body, _ := json.Marshal(synthetic)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/json")
	hctx.Request = r
	return s.handleChat(ctx, hctx)
}

// resolveChatKey returns corr with ChatKey populated if a key is available.
// Header-set keys win because WithChatKey is a no-op when ChatKey is already
// non-empty. The fallback derivation runs only after both the header path and
// the parsed cursor metadata path failed to surface a key.
func resolveChatKey(corr correlation.Context, cursorReq adaptercursor.Request, req ChatRequest) correlation.Context {
	corr = corr.WithChatKey(cursorReq.ConversationID)
	if corr.ChatKey != "" {
		return corr
	}
	return corr.WithChatKey(adaptercursor.DeriveChatKey(req))
}
