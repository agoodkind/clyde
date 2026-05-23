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
	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/slogger"
)

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
	recorder := s.beginChatLogRecorder(r, corr)
	defer s.completeChatLogRecorder(ctx, recorder)
	s.emitChatRequestLeg(ctx, recorder, logevent.LegAdapterIngress, logevent.PhaseStarted, logevent.StatusOK)
	discovery := DiscoverRequest(body)

	req, err := s.prepareChatRequest(ctx, r, corr, reqID, body, bodyBytes, recorder)
	if err != nil {
		return err
	}
	ingress, ok := activeIngressContract()
	if !ok || ingress == nil {
		recorder.EmitError(ctx, "ingress_contract_missing", "ingress contract not registered")
		return adapterErrInternal("ingress contract not registered", nil)
	}
	ingressCtx := ingress.Translate(ingresscontract.ChatRequestPrimitive{Body: req})
	corr = corr.WithCursor(ingressCtx.RequestID, ingressCtx.ConversationID)
	if ingressCtx.GenerationID != "" {
		corr = corr.WithCursorGenerationID(ingressCtx.GenerationID)
	}
	identity := ingress.ResolveIdentity(corr, ingressCtx, ingresscontract.ChatRequestPrimitive{Body: req})
	corr = corr.WithChatIdentity(identity.ChatKey, identity.ChatKeySource, identity.ChatRootKey, identity.ChatBranchKey)
	s.emitChatCursorMetadataLeg(ctx, recorder, corr)
	ctx = correlation.WithContext(ctx, corr)
	r = r.WithContext(ctx)
	req.Model = ingressCtx.NormalizedModel
	s.logChatForkDetected(ctx, corr, identity)

	model, effort, err := s.registry.Resolve(req.Model, req.ReasoningEffort)
	if err != nil {
		s.logChatResolveFailed(ctx, corr, reqID, req, ingressCtx, ingress, err)
		recorder.EmitError(ctx, "model_resolve_failed", err.Error())
		return adapterErrModelNotFound(err.Error())
	}
	s.logChatResolved(ctx, corr, reqID, req, ingressCtx, ingress, model, effort)
	s.emitChatModelResolveLeg(ctx, recorder, corr, model, effort)

	// Step D: build the typed resolver.ResolvedRequest alongside the
	// legacy resolution. Backends still use model.ResolvedModel for now;
	// this call validates the resolver in production traffic and emits a
	// telemetry event so we can confirm provider+effort+budget mapping is
	// consistent before flipping the dispatcher to use it.
	resolvedReq, resolverErr := resolveCursorChatRequest(req, adapterresolver.NewModelRegistryAdapter(s.registry))
	s.logResolverOutcome(ctx, corr, reqID, req, ingressCtx, &resolvedReq, resolverErr)

	model, err = s.applyBackendOverride(r, req, model, reqID)
	if err != nil {
		recorder.EmitError(ctx, "backend_override_failed", err.Error())
		return err
	}

	toolNames := chatToolNames(req)
	s.logChatReceived(ctx, corr, reqID, req, ingressCtx, ingress, model, toolNames)
	if ingressCtx.PathKind == ingresscontract.PathKindSubagent && ingressCtx.GenerationID == "" {
		s.logSubagentMissingGenerationID(ctx, r, corr, reqID, ingressCtx, discovery)
	}

	if perr := s.preflightChat(ctx, &req, model, reqID); perr != nil {
		recorder.EmitError(ctx, "preflight_failed", perr.Error())
		return perr
	}

	s.emitChatProviderSendStartedLeg(ctx, recorder, corr, req, model, effort)
	s.dispatchResolvedChat(w, r, req, model, effort, reqID, body, ingressCtx, resolvedReq, resolverErr)
	s.completeChatDispatchLegs(ctx, recorder, corr, req, model, effort)
	return nil
}

func (s *Server) prepareChatRequest(ctx context.Context, r *http.Request, corr correlation.Context, reqID string, body []byte, bodyBytes int, recorder *logevent.Recorder) (ChatRequest, error) {
	var req ChatRequest
	parseErr := json.Unmarshal(body, &req)
	s.emitChatPayloadLeg(ctx, recorder, body, r.Header.Get("Content-Type"), parseErr)
	if parseErr != nil {
		recorder.EmitError(ctx, "invalid_json", parseErr.Error())
		s.logChatParseFailed(ctx, corr, reqID, bodyBytes, parseErr)
		return ChatRequest{}, adapterErrInvalidJSON("invalid JSON: "+parseErr.Error(), parseErr)
	}
	forceStreamUsageOptIn(&req)
	if normErr := normalizeRequestMessages(&req); normErr != nil {
		recorder.EmitError(ctx, "message_normalization_failed", normErr.Error())
		return ChatRequest{}, normErr
	}
	if len(req.Messages) == 0 {
		s.logMessagesRequired(ctx, corr, reqID, req)
		recorder.EmitError(ctx, "messages_required", "messages is required")
		return ChatRequest{}, adapterErrInvalidRequest("messages is required", nil)
	}
	if req.ReasoningEffort == "" && req.Reasoning != nil {
		req.ReasoningEffort = strings.TrimSpace(req.Reasoning.Effort)
	}
	return req, nil
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
func normalizeRequestMessages(req *ChatRequest) error {
	if len(req.Messages) != 0 || len(req.Input) == 0 {
		return nil
	}
	if _, err := parseMessagesFromInput(req); err != nil {
		return adapterErrInvalidRequest(err.Error(), err)
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

type responsesInputContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

type openAIChatContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

type imageURLObject struct {
	URL string `json:"url"`
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
		var part responsesInputContentPart
		if err := json.Unmarshal(raw, &part); err != nil {
			return nil, fmt.Errorf("invalid input content: %w", err)
		}
		return parseInputParts([]responsesInputContentPart{part})
	case '[':
		var parts []responsesInputContentPart
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, fmt.Errorf("invalid input content: %w", err)
		}
		return parseInputParts(parts)
	default:
		return nil, fmt.Errorf("invalid input content type")
	}
}

func parseInputParts(parts []responsesInputContentPart) (json.RawMessage, error) {
	out := make([]openAIChatContentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text", "output_text":
			out = append(out, openAIChatContentPart{
				Type:     "text",
				Text:     p.Text,
				ImageURL: nil,
			})
		case "image_url":
			if len(p.ImageURL) == 0 {
				continue
			}
			out = append(out, openAIChatContentPart{
				Type:     "image_url",
				Text:     "",
				ImageURL: p.ImageURL,
			})
		case "input_image":
			image, ok := normalizeResponsesInputImageURL(p.ImageURL)
			if !ok {
				continue
			}
			out = append(out, openAIChatContentPart{
				Type:     "image_url",
				Text:     "",
				ImageURL: image,
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

func normalizeResponsesInputImageURL(raw json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, false
	}
	if trimmed[0] == '{' {
		return json.RawMessage(trimmed), true
	}
	if trimmed[0] != '"' {
		return nil, false
	}
	var imageURL string
	if err := json.Unmarshal(raw, &imageURL); err != nil {
		return nil, false
	}
	encoded, err := json.Marshal(imageURLObject{URL: imageURL})
	if err != nil {
		return nil, false
	}
	return encoded, true
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

// forceStreamUsageOptIn applies the generic OpenAI route family
// policy that every streaming completion emits the trailing usage
// chunk regardless of what the client requested via
// `stream_options.include_usage`. Clyde owns the auto-compact signal
// path for Cursor BYOK and other OpenAI-SDK clients, and Cursor in
// particular only sets the opt-in for model aliases it recognizes as
// OpenAI-shaped (`clyde-codex-*`) and not for Anthropic-shaped
// aliases (`clyde-opus-*`). Honoring the opt-in per client surface
// leaves Anthropic chats without the usage signal Cursor needs to
// trigger auto-compact, so they grow until Anthropic per-request-
// rate-limits the oversized turn. Forcing the opt-in at the generic
// boundary keeps the dispatchers backend-agnostic and prevents any
// client from opting out (CLYDE-438).
func forceStreamUsageOptIn(req *ChatRequest) {
	if req == nil {
		return
	}
	if req.StreamOptions == nil {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
		return
	}
	req.StreamOptions.IncludeUsage = true
}
