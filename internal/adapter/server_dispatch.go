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
	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
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
	if listenerFollowsDocumentedContract(ctx) && r.Method != http.MethodGet {
		return newAdapterError(adapterErrorMethodNotAllowed, "GET required")
	}
	corr := hctx.Correlation
	clydeingress.SetHTTPHeaders(corr, w.Header())
	registry := s.modelRegistry()
	entries := registry.List()
	fingerprint := modelCatalogFingerprint(entries)
	resp := ModelsResponse{Object: "list", Data: nil}
	for _, m := range entries {
		resp.Data = append(resp.Data, s.advertisedModelEntry(ctx, registry, m))
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

// On the OpenAI listener, advertisedModelEntry sets created to the catalog
// load time.
func (s *Server) advertisedModelEntry(ctx context.Context, registry *Registry, m adaptermodel.ResolvedAlias) ModelEntry {
	entry := modelEntryFromResolved(m)
	if m.Backend == BackendCodex {
		entry = adaptercodex.ApplyCapabilityReport(entry, adaptercodex.CapabilityReportForModel(m, adaptercodex.CapabilityMode{
			WebsocketEnabled: s.codexWebsocketEnabled(),
		}))
	}
	if m.Backend == BackendAnthropic {
		entry = applyModelContextLimit(entry, m.TransportLimits[config.AdapterModelTransportAnthropic])
	}
	if listenerFollowsDocumentedContract(ctx) {
		entry.Created = registry.LoadedUnix()
	}
	return entry
}

// handleModel returns an advertised model or a model that a route rule
// resolves for the ingress surface. For a fallback-only model, handleModel
// requests its metadata from the OpenAI-compatible upstream. Any other
// identifier returns the documented model_not_found error.
func (s *Server) handleModel(ctx context.Context, hctx *handlerCtx) error {
	w := hctx.Writer
	r := hctx.Request
	if r.Method != http.MethodGet {
		return newAdapterError(adapterErrorMethodNotAllowed, "GET required")
	}
	clydeingress.SetHTTPHeaders(hctx.Correlation, w.Header())
	requestedModel := strings.TrimSpace(r.PathValue("model"))
	registry := s.modelRegistry()
	entry, found := s.lookupModelEntry(ctx, registry, requestedModel)
	if !found {
		if fallback, ok := fallbackOnlyModel(ctx, registry, requestedModel); ok {
			return s.forwardPassthroughModel(ctx, w, fallback, requestedModel)
		}
		return adapterErrModelNotFound("The model '" + requestedModel + "' does not exist")
	}
	body, err := json.Marshal(entry)
	if err != nil {
		s.log.WarnContext(ctx, "adapter.models.retrieve_marshal_failed", "concern", "adapter.models.catalog", "err", err)
		return fmt.Errorf("marshal model response: %w", err)
	}
	writeJSON(w, body)
	return nil
}

func fallbackOnlyModel(ctx context.Context, registry *Registry, requestedModel string) (adaptermodel.ResolvedAlias, bool) {
	var none adaptermodel.ResolvedAlias
	if requestedModel == "" {
		return none, false
	}
	resolved, _, err := registry.Resolve(openAIIngressSurface(ctx), requestedModel, "")
	if err != nil || resolved.Backend != adaptermodel.BackendPassthroughOverride {
		return none, false
	}
	return resolved, true
}

// lookupModelEntry returns false for a fallback-only model. handleModel
// requests that model from the OpenAI-compatible upstream.
func (s *Server) lookupModelEntry(ctx context.Context, registry *Registry, requestedModel string) (ModelEntry, bool) {
	var notFound ModelEntry
	if requestedModel == "" {
		return notFound, false
	}
	for _, m := range registry.List() {
		if m.Alias == requestedModel {
			return s.advertisedModelEntry(ctx, registry, m), true
		}
	}
	resolved, _, err := registry.Resolve(openAIIngressSurface(ctx), requestedModel, "")
	if err != nil || resolved.Backend == adaptermodel.BackendPassthroughOverride {
		return notFound, false
	}
	entry := s.advertisedModelEntry(ctx, registry, resolved)
	entry.ID = requestedModel
	return entry, true
}

func modelEntryFromResolved(m adaptermodel.ResolvedAlias) ModelEntry {
	advertised := m.Context
	return ModelEntry{
		ID:                               m.Alias,
		Object:                           "model",
		Created:                          0,
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
	// The ingress capture defer reads hctx.Correlation when it runs, after the
	// response completes. Publishing the body-derived chat identity here is what
	// lets the captured row carry the conversation it belongs to; the header-only
	// correlation built at handler construction does not have it yet.
	hctx.Correlation = corr

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

	if rejectErr := rejectUndocumentedChatFields(ctx, recorder, body, req, discovery, &resolvedReq); rejectErr != nil {
		return rejectErr
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

func rejectUndocumentedChatFields(ctx context.Context, recorder *logevent.Recorder, body []byte, req ChatRequest, discovery RequestDiscovery, resolvedReq *adapterresolver.ResolvedRequest) error {
	if !listenerFollowsDocumentedContract(ctx) {
		return nil
	}
	rejectErr := documentedChatRejection(body, req, discovery, resolvedReq)
	if rejectErr == nil {
		return nil
	}
	recorder.EmitError(ctx, "unsupported_parameter", rejectErr.Error())
	return rejectErr
}

// handleChat runs documentedChatRejection on the generic OpenAI listener
// before any provider request starts.
func documentedChatRejection(body []byte, req ChatRequest, discovery RequestDiscovery, resolvedReq *adapterresolver.ResolvedRequest) *adapterError {
	fields, err := adapteropenai.DecodeFieldSet(body)
	if err != nil {
		return adapterErrInvalidJSON("invalid JSON: "+err.Error(), err)
	}
	var n *int
	if req.N != 0 {
		n = &req.N
	}
	values := adaptercompat.ChatRequestValues{
		Stream:           req.Stream,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		PresencePenalty:  req.PresencePenalty,
		FrequencyPenalty: req.FrequencyPenalty,
		N:                n,
		Logprobs:         req.Logprobs,
		TopLogprobs:      req.TopLogprobs,
		Store:            req.Store,
		ServiceTier:      req.ServiceTier,
		ToolChoice:       req.ToolChoice,
		FunctionCall:     req.FunctionCall,
		Modalities:       req.Modalities,
		ResponseFormat:   req.ResponseFormat,
		UnknownKeys:      discovery.UnknownKeys,
	}
	presenceFor := func(param string) int { return int(fields.Presence(param)) }
	rejection, rejected := adaptercompat.ChatRejection(presenceFor, values, resolvedReq.Provider)
	if !rejected {
		return nil
	}
	return adapterErrRejectedParameter(rejection)
}

func openAIIngressSurface(ctx context.Context) adapterresolver.IngressSurface {
	if ingressLabelFromContext(ctx) == string(adapterresolver.IngressCursor) {
		return adapterresolver.IngressCursor
	}
	return adapterresolver.IngressOpenAI
}

// On the generic OpenAI listener, marshalChatResponseForListener writes
// logprobs, content, and refusal on every choice. On other listeners it
// applies CompatibilityUsage and the compatibility encoding.
func marshalChatResponseForListener(ctx context.Context, resp ChatResponse) ([]byte, error) {
	if listenerFollowsDocumentedContract(ctx) {
		encoded, err := adapteropenai.MarshalDocumentedChatResponse(resp)
		if err != nil {
			slog.WarnContext(ctx, "adapter.chat.documented_response_marshal_failed", "concern", "adapter.chat.render", "err", err)
			return nil, fmt.Errorf("marshal documented chat response: %w", err)
		}
		return encoded, nil
	}
	if resp.Usage != nil {
		compatibilityUsage := adapteropenai.CompatibilityUsage(*resp.Usage)
		resp.Usage = &compatibilityUsage
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		slog.WarnContext(ctx, "adapter.chat.response_marshal_failed", "concern", "adapter.chat.render", "err", err)
		return nil, fmt.Errorf("marshal chat response: %w", err)
	}
	return encoded, nil
}

// listenerFollowsDocumentedContract returns true only for the "openai"
// label. [Server.StartOnListeners] labels each accepted connection "openai"
// or "cursor". An unlabeled in-process request uses the compatibility
// behavior.
func listenerFollowsDocumentedContract(ctx context.Context) bool {
	return ingressLabelFromContext(ctx) == string(adapterresolver.IngressOpenAI)
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
	if !listenerFollowsDocumentedContract(ctx) {
		forceStreamUsageOptIn(&req)
	}
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
	if listenerFollowsDocumentedContract(ctx) {
		return s.handleDocumentedLegacy(ctx, hctx)
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

// handleDocumentedLegacy runs a legacy prompt as a one-message Chat
// Completions request. LegacyCompletionWriter rewrites the Chat output into
// text_completion objects and chunks.
func (s *Server) handleDocumentedLegacy(ctx context.Context, hctx *handlerCtx) error {
	r := hctx.Request
	body, err := io.ReadAll(http.MaxBytesReader(hctx.Writer, r.Body, 8<<20))
	if err != nil {
		return adapterErrInvalidRequest("failed to read body", err)
	}
	fields, err := adapteropenai.DecodeFieldSet(body)
	if err != nil {
		return adapterErrInvalidJSON("invalid JSON: "+err.Error(), err)
	}
	var legacy adapteropenai.CompletionRequest
	if err := json.Unmarshal(body, &legacy); err != nil {
		return adapterErrInvalidJSON("invalid JSON: "+err.Error(), err)
	}
	prompt, rejectErr := legacyCompletionPrompt(legacy, fields)
	if rejectErr != nil {
		return rejectErr
	}
	chatBody, err := json.Marshal(legacyChatRequest(legacy, prompt))
	if err != nil {
		return adapterErrInternal("serialize legacy completion request", err)
	}
	r.Body = io.NopCloser(strings.NewReader(string(chatBody)))
	r.ContentLength = int64(len(chatBody))
	r.Header.Set("Content-Type", "application/json")
	hctx.Request = r
	includeUsage := legacy.StreamOptions != nil && legacy.StreamOptions.IncludeUsage
	writer := adapteropenai.NewLegacyCompletionWriter(hctx.Writer, includeUsage)
	hctx.Writer = writer
	if chatErr := s.handleChat(ctx, hctx); chatErr != nil {
		return chatErr
	}
	if finishErr := writer.Finish(); finishErr != nil {
		return adapterErrInternal("write legacy completion", finishErr)
	}
	return nil
}

func legacyCompletionPrompt(legacy adapteropenai.CompletionRequest, fields adapteropenai.ResponsesFieldSet) (string, *adapterError) {
	if unknown := fields.UnknownCompletionKeys(); len(unknown) > 0 {
		return "", adapterErrRejectedParameter(adaptercompat.Rejection{
			Code:    adaptercompat.RejectionCodeUnknownParameter,
			Param:   unknown[0],
			Message: "Unrecognized request argument supplied: " + unknown[0],
		})
	}
	unsupported := []struct {
		param   string
		present bool
	}{
		{param: "suffix", present: legacy.Suffix != nil && *legacy.Suffix != ""},
		{param: "echo", present: legacy.Echo != nil && *legacy.Echo},
		{param: "logprobs", present: legacy.Logprobs != nil},
		{param: "best_of", present: legacy.BestOf != nil && *legacy.BestOf > 1},
		{param: "n", present: legacy.N != nil && *legacy.N > 1},
	}
	for _, field := range unsupported {
		if field.present {
			return "", adapterErrRejectedParameter(adaptercompat.Rejection{
				Code:    adaptercompat.RejectionCodeUnsupportedParameter,
				Param:   field.param,
				Message: "Unsupported parameter: '" + field.param + "' is not supported by Clyde.",
			})
		}
	}
	prompt, err := legacy.PromptText()
	if err != nil {
		return "", adapterErrRejectedParameter(adaptercompat.Rejection{
			Code:    adaptercompat.RejectionCodeUnsupportedParameter,
			Param:   "prompt",
			Message: err.Error(),
		})
	}
	return prompt, nil
}

// handleChat rejects each copied field that the resolved provider cannot
// honor.
func legacyChatRequest(legacy adapteropenai.CompletionRequest, prompt string) ChatRequest {
	var chat ChatRequest
	chat.Model = legacy.Model
	chat.Messages = []ChatMessage{{
		Role:    "user",
		Content: json.RawMessage(strconv.Quote(prompt)), Name: "", ToolCalls: nil, ToolCallID: "", Reasoning: "", ReasoningContent: "", Refusal: "", Annotations: nil,
	}}
	chat.Stream = legacy.Stream
	chat.StreamOptions = legacy.StreamOptions
	chat.MaxTokens = legacy.MaxTokens
	chat.Temperature = legacy.Temperature
	chat.TopP = legacy.TopP
	chat.Stop = legacy.Stop
	chat.PresencePenalty = legacy.PresencePenalty
	chat.FrequencyPenalty = legacy.FrequencyPenalty
	chat.LogitBias = legacy.LogitBias
	chat.Seed = legacy.Seed
	chat.User = legacy.User
	chat.ReasoningEffort = legacy.ReasoningEffort
	return chat
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
