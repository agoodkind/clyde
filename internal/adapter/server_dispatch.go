package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/logevent"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
	"goodkind.io/gklog/trace"
)

func (s *Server) handleModels(ctx context.Context, hctx *handlerCtx) error {
	w := hctx.Writer
	r := hctx.Request
	corr := hctx.Correlation
	clydeingress.SetHTTPHeaders(corr, w.Header())
	entries := s.modelRegistry().List()
	fingerprint := modelCatalogFingerprint(entries)
	resp := ModelsResponse{Object: "list", Data: nil}
	for _, m := range entries {
		entry := modelEntryFromResolved(m)
		if m.Backend == BackendCodex {
			entry = adaptercodex.ApplyCapabilityReport(entry, adaptercodex.CapabilityReportForModel(m, adaptercodex.CapabilityMode{
				WebsocketEnabled: s.codexWebsocketEnabled(),
			}))
		}
		if m.Backend == BackendAnthropic {
			entry = applyModelContextLimit(entry, m.TransportLimits[config.AdapterModelTransportAnthropic])
		}
		resp.Data = append(resp.Data, entry)
	}
	respBody, err := json.Marshal(resp)
	if err != nil {
		s.log.WarnContext(ctx, "adapter.models.marshal_failed", "concern", "adapter.models.catalog", "err", err)
		return fmt.Errorf("marshal models response: %w", err)
	}
	writeJSON(w, respBody)
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("remote_addr", r.RemoteAddr),
		slog.String("user_agent", r.UserAgent()),
		slog.Int("model_count", len(entries)),
		slog.String("catalog_fingerprint", fingerprint),
	}
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterModelsCatalog).LogAttrs(ctx, slog.LevelInfo, "adapter.models.listed", append([]slog.Attr{slog.String("concern", "adapter.models.catalog")}, attrs...)...)
	return nil
}

func modelEntryFromResolved(m adaptermodel.ResolvedAlias) ModelEntry {
	advertised := m.Context
	return ModelEntry{
		ID:                               m.Alias,
		Object:                           "model",
		OwnedBy:                          "clyde",
		Context:                          advertised,
		ContextWindow:                    advertised,
		ContextLength:                    advertised,
		MaxContextLength:                 advertised,
		MaxContextTokens:                 advertised,
		MaxModelLen:                      advertised,
		MaxTokens:                        advertised,
		InputTokenLimit:                  advertised,
		MaxInputTokens:                   advertised,
		ContextTokenLimit:                advertised,
		ContextTokenLimitCamel:           advertised,
		ContextTokenLimitForMaxMode:      advertised,
		ContextTokenLimitForMaxModeCamel: advertised,
		Efforts:                          m.Efforts,
		Backend:                          m.Backend.String(),
		ClaudeModel:                      m.WireModel,
	}
}

func applyModelContextLimit(entry ModelEntry, limit int) ModelEntry {
	if limit <= 0 {
		return entry
	}
	entry.Context = limit
	entry.ContextWindow = limit
	entry.ContextLength = limit
	entry.MaxContextLength = limit
	entry.MaxContextTokens = limit
	entry.MaxModelLen = limit
	entry.MaxTokens = limit
	entry.InputTokenLimit = limit
	entry.MaxInputTokens = limit
	entry.ContextTokenLimit = limit
	entry.ContextTokenLimitCamel = limit
	entry.ContextTokenLimitForMaxMode = limit
	entry.ContextTokenLimitForMaxModeCamel = limit
	return entry
}

func (s *Server) handleChat(ctx context.Context, hctx *handlerCtx) (err error) {
	defer trace.Op(ctx, "adapter.openai.chat_completions")(&err)
	started := clock.Now()
	w := hctx.Writer
	r := hctx.Request
	if r.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}
	corr := hctx.Correlation
	reqID := corr.RequestID
	clydeingress.SetHTTPHeaders(corr, w.Header())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		return adapterErrInvalidRequest("failed to read body", err)
	}

	bodyBytes := len(body)
	ingress := s.ingress
	if ingress == nil {
		return adapterErrInternal("ingress contract not registered", nil)
	}
	if capW := s.beginIngressCapture(hctx); capW != nil {
		w = hctx.Writer
		defer func() {
			s.finishIngressCapture(capW, hctx.Correlation, r, body, started, err)
		}()
	}
	ctx, r, corr, headerFacets := applyHeaderIngressContext(ctx, r, corr, ingress)

	recorder := s.beginChatLogRecorder(r, corr)
	defer s.completeChatLogRecorder(ctx, recorder)
	s.emitChatRequestLeg(ctx, recorder, logevent.LegAdapterIngress, logevent.PhaseStarted, logevent.StatusOK, headerFacets)
	discovery := DiscoverRequest(body)

	req, err := s.prepareChatRequest(ctx, corr, reqID, body, bodyBytes, recorder)
	if err != nil {
		return err
	}
	ctx, r, corr, ingressCtx, bodyFacets := s.applyBodyChatIdentity(ctx, r, corr, recorder, ingress, &req)

	// The resolver is the authoritative single resolution path. It maps
	// the alias and reasoning effort to a typed ResolvedRequest carrying
	// provider identity, effort, budget, and the per-provider knobs the
	// dispatcher and backends consume directly.
	resolvedReq, resolverErr := resolveCursorChatRequest(
		openAIIngressSurface(ctx),
		req,
		adapterresolver.NewModelRegistryAdapter(s.modelRegistry()),
	)
	if resolverErr != nil {
		s.logChatResolveFailed(ctx, corr, reqID, req, ingressCtx, ingress, resolverErr)
		recorder.EmitError(ctx, "model_resolve_failed", resolverErr.Error())
		var invalidRequestErr *adapterresolver.InvalidRequestError
		if errors.As(resolverErr, &invalidRequestErr) {
			return adapterErrInvalidRequest(invalidRequestErr.Error(), resolverErr)
		}
		return adapterErrModelNotFound(resolverErr.Error())
	}
	resolvedReq.RequestID = reqID
	resolvedReq.Correlation = corr
	effort := resolvedReq.Effort.String()
	s.logChatResolved(ctx, corr, reqID, req, ingressCtx, ingress, &resolvedReq, effort)
	s.emitChatModelResolveLeg(ctx, recorder, corr, &resolvedReq, effort, bodyFacets)
	s.logResolverOutcome(ctx, corr, reqID, req, ingressCtx, &resolvedReq, nil)

	if err := s.applyBackendOverride(r, req, &resolvedReq, reqID); err != nil {
		recorder.EmitError(ctx, "backend_override_failed", err.Error())
		return err
	}

	toolNames := chatToolNames(req)
	s.logChatReceived(ctx, corr, reqID, req, ingressCtx, ingress, &resolvedReq, toolNames)
	if ingressCtx.PathKind == ingresscontract.PathKindSubagent && ingressCtx.GenerationID == "" {
		s.logSubagentMissingGenerationID(ctx, r, corr, reqID, ingressCtx, ingress, discovery)
	}

	if perr := s.preflightChat(ctx, &req, &resolvedReq, reqID); perr != nil {
		recorder.EmitError(ctx, "preflight_failed", perr.Error())
		return perr
	}

	s.emitChatProviderSendStartedLeg(ctx, recorder, corr, req, &resolvedReq, effort, bodyFacets)
	s.dispatchResolvedChat(w, r, req, effort, reqID, body, ingressCtx, resolvedReq)
	s.completeChatDispatchLegs(ctx, recorder, corr, req, &resolvedReq, effort, bodyFacets)
	return nil
}

func openAIIngressSurface(ctx context.Context) adapterresolver.IngressSurface {
	if ingressLabelFromContext(ctx) == string(adapterresolver.IngressCursor) {
		return adapterresolver.IngressCursor
	}
	return adapterresolver.IngressOpenAI
}

func applyHeaderIngressContext(ctx context.Context, r *http.Request, corr correlation.Context, ingress ingresscontract.IngressContract) (context.Context, *http.Request, correlation.Context, []logevent.Facet) {
	headerIngressCtx := ingress.TranslateHeaders(r.Header)
	if headerIngressCtx.ConversationID != "" && clydeingress.ChatKey(corr) == "" {
		corr = clydeingress.WithChatIdentity(corr, headerIngressCtx.ConversationID, "native", headerIngressCtx.ConversationID, "")
	}
	corr = corr.WithIdentityAttributes(ingress.CorrelationAttrs(headerIngressCtx)...)
	ctx = correlation.WithContext(ctx, corr)
	return ctx, r.WithContext(ctx), corr, ingress.RequestFacets(headerIngressCtx)
}

// applyBodyChatIdentity folds the body-derived ingress translation, chat
// identity resolution, and correlation enrichment into ctx, r, and corr. It
// normalizes req.Model, emits the client-metadata leg, and returns the
// translated ingress context and body facets the dispatch path consumes.
func (s *Server) applyBodyChatIdentity(ctx context.Context, r *http.Request, corr correlation.Context, recorder *logevent.Recorder, ingress ingresscontract.IngressContract, req *ChatRequest) (context.Context, *http.Request, correlation.Context, ingresscontract.IngressContext, []logevent.Facet) {
	ingressCtx := ingress.Translate(ingresscontract.ChatRequestPrimitive{Body: *req})
	corr = corr.WithIdentityAttributes(ingress.CorrelationAttrs(ingressCtx)...)
	identity := ingress.ResolveIdentity(corr, ingressCtx, ingresscontract.ChatRequestPrimitive{Body: *req})
	// Body-derived backfill must not overwrite a header-resolved ChatKey.
	// WithChatKey is a first-wins setter; only the source/root/branch fields
	// get rewritten as a unit when the body has the canonical identity.
	corr = clydeingress.WithChatKey(corr, identity.ChatKey)
	if identity.ChatKeySource != "" || identity.ChatRootKey != "" || identity.ChatBranchKey != "" {
		corr = clydeingress.WithChatIdentity(corr, clydeingress.ChatKey(corr), identity.ChatKeySource, identity.ChatRootKey, identity.ChatBranchKey)
	}
	ctx = correlation.WithContext(ctx, corr)
	r = r.WithContext(ctx)
	bodyFacets := ingress.RequestFacets(ingressCtx)
	s.emitChatClientMetadataLeg(ctx, recorder, corr, bodyFacets)
	req.Model = ingressCtx.NormalizedModel
	s.logChatForkDetected(ctx, corr, identity)
	return ctx, r, corr, ingressCtx, bodyFacets
}

func (s *Server) prepareChatRequest(ctx context.Context, corr correlation.Context, reqID string, body []byte, bodyBytes int, recorder *logevent.Recorder) (ChatRequest, error) {
	var req ChatRequest
	parseErr := json.Unmarshal(body, &req)
	s.emitChatPayloadLeg(ctx, recorder, body, parseErr)
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
	return req, nil
}

// logChatParseFailed records a body that did not parse as JSON.
func (s *Server) logChatParseFailed(ctx context.Context, corr correlation.Context, reqID string, bodyBytes int, parseErr error) {
	slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPErrors).LogAttrs(ctx, slog.LevelWarn, "adapter.chat.parse_failed", append([]slog.Attr{slog.String("concern", "adapter.http.errors")}, correlation.AppendAttrs([]slog.Attr{
		slog.String("request_id", reqID),
		slog.String("err", parseErr.Error()),
		slog.Int("body_bytes", bodyBytes),
	}, corr)...)...,
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
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.chat.validation_failed", append([]slog.Attr{slog.String("concern", "adapter.chat.preflight")}, correlation.AppendAttrs([]slog.Attr{
		slog.String("request_id", reqID),
		slog.String("model", req.Model),
		slog.String("reason", "messages_required"),
	}, corr)...)...,
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
		slog.Warn("adapter.server_dispatch.input_payload_invalid", "concern", "adapter.chat.dispatch", "err", err)
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
			Content: content, Name: "", ToolCalls: nil, ToolCallID: "", Reasoning: "", ReasoningContent: "", Refusal: "", Annotations: nil,
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
			slog.Warn("adapter.server_dispatch.input_content_invalid", "concern", "adapter.chat.dispatch", "shape", "object", "err", err)
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

// responsesInputPartType enumerates the OpenAI Responses-API input
// content-part type strings the adapter normalizes into the OpenAI
// chat-completion content-part shape.
type responsesInputPartType string

const (
	responsesInputPartText       responsesInputPartType = "text"
	responsesInputPartInputText  responsesInputPartType = "input_text"
	responsesInputPartOutputText responsesInputPartType = "output_text"
	responsesInputPartImageURL   responsesInputPartType = "image_url"
	responsesInputPartInputImage responsesInputPartType = "input_image"
)

func parseInputParts(parts []responsesInputContentPart) (json.RawMessage, error) {
	out := make([]openAIChatContentPart, 0, len(parts))
	for _, p := range parts {
		switch responsesInputPartType(p.Type) {
		case responsesInputPartText, responsesInputPartInputText, responsesInputPartOutputText:
			out = append(out, openAIChatContentPart{
				Type:     "text",
				Text:     p.Text,
				ImageURL: nil,
			})
		case responsesInputPartImageURL:
			if len(p.ImageURL) == 0 {
				continue
			}
			out = append(out, openAIChatContentPart{
				Type:     "image_url",
				Text:     "",
				ImageURL: p.ImageURL,
			})
		case responsesInputPartInputImage:
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
		slog.Warn("adapter.server_dispatch.input_content_marshal_failed", "concern", "adapter.chat.dispatch", "err", err)
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
			Content: json.RawMessage(strconv.Quote(legacy.Prompt)), Name: "", ToolCalls: nil, ToolCallID: "", Reasoning: "", ReasoningContent: "", Refusal: "", Annotations: nil,
		}}, Input: nil, StreamOptions: nil, Reasoning: nil, Tools: nil, ToolChoice: nil, Functions: nil, FunctionCall: nil, N: 0, User: "", Temperature: nil, TopP: nil, MaxTokens: nil, MaxComplTokens: nil, MaxOutputTokens: nil, PresencePenalty: nil, FrequencyPenalty: nil, LogitBias:

		// forceStreamUsageOptIn applies the generic OpenAI route family policy that
		// every streaming completion emits the trailing usage chunk regardless of what
		// the client requested via `stream_options.include_usage`.
		nil, Logprobs: nil, TopLogprobs: nil, Stop: nil, Seed: nil, ResponseFormat: nil, Audio: nil, Modalities: nil, ParallelTools: nil, Store: nil, Metadata: nil, Include: nil, ServiceTier: "", Text: nil, Truncation: "", PromptCacheRetention: "",
	}
	body, err := json.Marshal(synthetic)
	if err != nil {
		return adapterErrInternal("serialize legacy completion request", err)
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Type", "application/json")
	hctx.Request = r
	return s.handleChat(ctx, hctx)
}

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
