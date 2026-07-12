package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	anthropicbackend "goodkind.io/clyde/internal/adapter/anthropic/backend"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/gklog/correlation"
	"goodkind.io/gklog/trace"
)

const maxAnthropicMessagesBodyBytes = 8 << 20

func (s *Server) handleAnthropicMessages(ctx context.Context, hctx *handlerCtx) (err error) {
	defer trace.Op(ctx, "adapter.anthropic.messages")(&err)
	w := hctx.Writer
	r := hctx.Request
	if r.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}
	if s.anthropicProvider == nil {
		err := newAdapterError(adapterErrorUpstreamUnavailable, "anthropic backend is not enabled in [adapter]")
		err.Provider = "anthropic"
		return err
	}

	corr := hctx.Correlation
	reqID := corr.RequestID
	clydeingress.SetHTTPHeaders(corr, w.Header())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAnthropicMessagesBodyBytes))
	if err != nil {
		return adapterErrInvalidRequest("failed to read body", err)
	}

	var req anthropic.Request
	if err := json.Unmarshal(body, &req); err != nil {
		return adapterErrInvalidJSON("invalid JSON: "+err.Error(), err)
	}
	if len(req.Messages) == 0 {
		return adapterErrInvalidRequest("messages is required", nil)
	}

	requestedModel := strings.TrimSpace(req.Model)
	requestedEffort := ""
	if req.OutputConfig != nil {
		requestedEffort = req.OutputConfig.Effort
	}
	resolved, resolveErr := s.anthropicNativeResolvedRequest(ctx, requestedModel, requestedEffort)
	switch {
	case resolveErr != nil:
		var invalidRequestErr *adapterresolver.InvalidRequestError
		if errors.As(resolveErr, &invalidRequestErr) {
			return adapterErrInvalidRequest(invalidRequestErr.Error(), resolveErr)
		}
		return adapterErrModelNotFound("model " + requestedModel + " does not resolve to a known backend")
	case resolved.Provider != BackendAnthropic:
		return adapterErrInvalidRequest("model does not resolve to the anthropic backend", nil)
	}
	applyAnthropicNativeResolution(&req, &resolved)

	s.logAnthropicNativeIngress(ctx, corr, reqID, r.URL.Path, req, len(body))

	prepared := anthropic.PreparedRequest{
		Request:       req,
		Resolved:      &resolved,
		RequestID:     reqID,
		Stream:        req.Stream,
		NativeIngress: true, TrackerKey: "", JSONCoercion: anthropic.
				JSONCoercion{Coerce: nil, Validate: nil},

		IncludeUsage: false,
	}
	execCtx := anthropic.WithRequestID(ctx, reqID)
	if req.Stream {
		streamWriter, streamErr := newNativeAnthropicStreamWriter(w)
		if streamErr != nil {
			return adapterErrInternal(streamErr.Error(), streamErr)
		}
		if _, err := s.anthropicProvider.ExecutePrepared(execCtx, prepared, streamWriter); err != nil {
			s.writeAnthropicIngressProviderError(w, r, err)
		}
		return nil
	}

	collector := newNativeAnthropicJSONWriter()
	if _, err := s.anthropicProvider.ExecutePrepared(execCtx, prepared, collector); err != nil {
		s.writeAnthropicIngressProviderError(w, r, err)
		return nil
	}
	if collector.empty() {
		aerr := newAdapterError(adapterErrorUpstreamUnavailable, "anthropic native collect path produced no response")
		aerr.Provider = "anthropic"
		s.writeShapedError(w, r, aerr)
		return nil
	}
	collector.writeTo(w)
	return nil
}

func applyAnthropicNativeResolution(req *anthropic.Request, resolved *adapterresolver.ResolvedRequest) {
	if req == nil || resolved == nil {
		return
	}
	req.Model = anthropicbackend.StripContextSuffix(resolved.Model)
	if effort := resolved.ProviderEffort(); effort != "" {
		if req.OutputConfig == nil {
			req.OutputConfig = &anthropic.OutputConfig{Effort: ""}
		}
		req.OutputConfig.Effort = effort.String()
	}
	prependAnthropicNativeInstructions(req, resolved.Instructions)
	anthropicbackend.ApplyThinkingConfig(req, resolved, req.Model)
	if resolved.MaxOutputTokens > 0 && req.MaxTokens > resolved.MaxOutputTokens {
		req.MaxTokens = resolved.MaxOutputTokens
	}
	req.FeatureVector = anthropic.WireFlavorFeatureVector{
		ModelID:     req.Model,
		WireProfile: resolved.WireProfile,
	}
}

func prependAnthropicNativeInstructions(req *anthropic.Request, instructions string) {
	if req == nil {
		return
	}
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return
	}
	if len(req.SystemBlocks) > 0 {
		blocks := make([]anthropic.SystemBlock, 0, len(req.SystemBlocks)+1)
		blocks = append(blocks, anthropic.SystemBlock{Type: "text", Text: instructions, CacheControl: nil})
		req.SystemBlocks = append(blocks, req.SystemBlocks...)
		return
	}
	if req.System == "" {
		req.System = instructions
		return
	}
	req.System = instructions + "\n\n" + req.System
}

func (s *Server) logAnthropicNativeIngress(
	ctx context.Context,
	corr correlation.Context,
	requestID string,
	requestPath string,
	req anthropic.Request,
	bodyBytes int,
) {
	attrs := []slog.Attr{
		slog.String("request_id", requestID),
		slog.String("path", requestPath),
		slog.String("model", req.Model),
		slog.Bool("stream", req.Stream),
		slog.Int("body_bytes", bodyBytes),
	}
	attrs = append(attrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.anthropic.ingress", append([]slog.Attr{
		slog.String("concern", "adapter.providers.anthropic.request"),
	}, attrs...)...)
}

// anthropicCountEnvVar names the environment variable holding the dedicated
// Anthropic API key used for count_tokens. This path is isolated from the
// subscription OAuth token, which does not authorize count_tokens.
const anthropicCountEnvVar = "CLYDE_ANTHROPIC_API_KEY"

// countTokensPathSuffix is appended to the configured messages URL to reach the
// sibling count_tokens endpoint.
const countTokensPathSuffix = "/count_tokens"

// handleAnthropicCountTokens answers /v1/messages/count_tokens by forwarding the
// request to Anthropic's count_tokens endpoint with x-api-key auth and returning
// the exact input_tokens. It requires a dedicated API key in the environment; the
// subscription OAuth token is never used here. Without a key it returns a typed
// error rather than a local estimate, since callers expect exact counts.
func (s *Server) handleAnthropicCountTokens(ctx context.Context, hctx *handlerCtx) (err error) {
	defer trace.Op(ctx, "adapter.anthropic.count_tokens")(&err)
	w := hctx.Writer
	r := hctx.Request
	if r.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}
	apiKey := strings.TrimSpace(os.Getenv(anthropicCountEnvVar))
	if apiKey == "" {
		aerr := newAdapterError(adapterErrorUpstreamUnavailable, "count_tokens requires an Anthropic API key in "+anthropicCountEnvVar)
		aerr.Provider = "anthropic"
		return aerr
	}
	messagesURL := s.cfg.Anthropic.OAuth.MessagesURL
	if messagesURL == "" {
		aerr := newAdapterError(adapterErrorUpstreamUnavailable, "anthropic messages endpoint is not configured")
		aerr.Provider = "anthropic"
		return aerr
	}
	body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAnthropicMessagesBodyBytes))
	if readErr != nil {
		return adapterErrInvalidRequest("failed to read body", readErr)
	}
	var req anthropic.Request
	if unmarshalErr := json.Unmarshal(body, &req); unmarshalErr != nil {
		return adapterErrInvalidJSON("invalid JSON: "+unmarshalErr.Error(), unmarshalErr)
	}
	if len(req.Messages) == 0 {
		return adapterErrInvalidRequest("messages is required", nil)
	}

	inputTokens, status, countErr := s.forwardAnthropicCount(ctx, messagesURL+countTokensPathSuffix, apiKey, body)
	if countErr != nil {
		s.log.WarnContext(ctx, "adapter.anthropic.count_tokens_failed", "concern", "adapter.providers.anthropic.request", "status", status, "err", countErr)
		codeClass := anthropicCodeClassForStatus(status)
		aerr := mapUpstreamForFamily(adapterRouteAnthropic, "anthropic", status, codeClass, "", "count_tokens upstream failed")
		s.respondAdapterError(w, r, aerr)
		return nil
	}
	clydeingress.SetHTTPHeaders(hctx.Correlation, w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if encodeErr := json.NewEncoder(w).Encode(anthropic.CountTokensResponse{InputTokens: inputTokens}); encodeErr != nil {
		s.log.WarnContext(ctx, "adapter.anthropic.count_tokens_encode_failed", "concern", "adapter.providers.anthropic.request", "err", encodeErr)
	}
	return nil
}

// forwardAnthropicCount POSTs body to the Anthropic count_tokens endpoint with
// x-api-key auth and returns the decoded input_tokens, the upstream status, and
// an error. The status is returned separately so the caller can map it through
// the error boundary.
func (s *Server) forwardAnthropicCount(ctx context.Context, url, apiKey string, body []byte) (int, int, error) {
	client := s.httpClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if reqErr != nil {
		return 0, 0, errors.New("count_tokens: build request failed")
	}
	httpReq.Header.Set("X-Api-Key", apiKey)
	httpReq.Header.Set("Anthropic-Version", s.cfg.Anthropic.OAuth.AnthropicVersion)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, doErr := client.Do(httpReq)
	if doErr != nil {
		return 0, http.StatusBadGateway, errors.New("count_tokens: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, resp.StatusCode, fmt.Errorf("count_tokens: status %d", resp.StatusCode)
	}
	var decoded anthropic.CountTokensResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&decoded); decodeErr != nil {
		return 0, resp.StatusCode, errors.New("count_tokens: decode response failed")
	}
	return decoded.InputTokens, resp.StatusCode, nil
}

// anthropicNativeResolvedRequest resolves a native `/v1/messages` model
// through the same typed resolver and canonical projection used by the
// OpenAI-shaped surfaces. This keeps every catalog field available to the
// native provider path without a second manual projection to maintain.
func (s *Server) anthropicNativeResolvedRequest(
	ctx context.Context,
	requestedModel string,
	requestedEffort string,
) (adapterresolver.ResolvedRequest, error) {
	var request ChatRequest
	request.Model = requestedModel
	request.ReasoningEffort = requestedEffort
	resolved, err := resolveCursorChatRequest(
		adapterresolver.IngressAnthropic,
		request,
		adapterresolver.NewModelRegistryAdapter(s.modelRegistry()),
	)
	if err != nil {
		s.log.WarnContext(ctx, "adapter.anthropic.native_resolve_failed", "concern", "adapter.providers.anthropic.request", "model", requestedModel, "err", err)
		var empty adapterresolver.ResolvedRequest
		return empty, err
	}
	return resolved, nil
}

func (s *Server) writeAnthropicIngressProviderError(w http.ResponseWriter, r *http.Request, err error) {
	if aerr := anthropicWireBaselineAdapterError(err); aerr != nil {
		s.respondAdapterError(w, r, aerr)
		return
	}
	var execErr *anthropic.ExecuteError
	if errors.As(err, &execErr) {
		// Native Anthropic ingress preserves the spec-correct
		// Anthropic envelope so claude-cli parses it natively.
		codeClass := anthropicCodeClassForStatus(execErr.Status)
		// The Anthropic mapper classifies status and code into the
		// neutral Code/Class; the Anthropic renderer derives the
		// spec-correct envelope type from those neutral fields, so the
		// generic adapter never assigns a provider wire type here.
		aerr := mapUpstreamForFamily(adapterRouteAnthropic, "anthropic", execErr.Status, codeClass, execErr.Code, execErr.Message)
		aerr.Cause = err
		s.respondAdapterError(w, r, aerr)
		return
	}
	if upstreamErr, ok := anthropic.AsUpstreamError(err); ok {
		status := upstreamErr.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		message := upstreamErr.Message
		if message == "" {
			message = upstreamErr.Error()
		}
		codeClass := anthropicCodeClassForStatus(status)
		aerr := mapUpstreamForFamily(adapterRouteAnthropic, "anthropic", status, codeClass, "", message)
		aerr.Cause = err
		s.respondAdapterError(w, r, aerr)
		return
	}
	s.respondAdapterError(w, r, adapterErrUpstreamFailed("anthropic", err.Error(), err))
}

// anthropicCodeClassForStatus picks the upstreamCodeClass for a known
// upstream HTTP status, defaulting to upstreamClassUnknown so the
// catch-all default applies.
func anthropicCodeClassForStatus(status int) upstreamCodeClass {
	switch {
	case status == http.StatusTooManyRequests:
		return upstreamClassRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return upstreamClassAuth
	case status >= 500:
		return upstreamClassServerError
	case status >= 400:
		return upstreamClassInvalidRequest
	default:
		return upstreamClassUnknown
	}
}

type nativeAnthropicJSONWriter struct {
	status int
	header http.Header
	body   []byte
}

func newNativeAnthropicJSONWriter() *nativeAnthropicJSONWriter {
	return &nativeAnthropicJSONWriter{
		status: http.StatusOK,
		header: make(http.Header), body: nil,
	}
}

func (w *nativeAnthropicJSONWriter) WriteEvent(adapterrender.Event) error {
	return nil
}

func (w *nativeAnthropicJSONWriter) Flush() error {
	return nil
}

func (w *nativeAnthropicJSONWriter) capture(header http.Header, body []byte) error {
	if w == nil {
		return fmt.Errorf("native anthropic writer missing")
	}
	w.status = http.StatusOK
	w.body = append(w.body[:0], body...)
	w.header = make(http.Header, len(header))
	for key, values := range header {
		cloned := append([]string(nil), values...)
		w.header[key] = cloned
	}
	return nil
}

// empty reports whether the collector captured a response body. The
// caller routes the empty case through the boundary (writeShapedError)
// instead of this writer because rendering the error envelope is the
// boundary's responsibility, not the collector's.
func (w *nativeAnthropicJSONWriter) empty() bool {
	return w == nil || len(w.body) == 0
}

func (w *nativeAnthropicJSONWriter) writeTo(dst http.ResponseWriter) {
	if w == nil {
		return
	}
	for key, values := range w.header {
		for _, value := range values {
			dst.Header().Add(key, value)
		}
	}
	if dst.Header().Get("Content-Type") == "" {
		dst.Header().Set("Content-Type", "application/json")
	}
	dst.WriteHeader(w.status)
	_, _ = dst.Write(w.body)
}

type nativeAnthropicStreamWriter struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	committed bool
}

func newNativeAnthropicStreamWriter(w http.ResponseWriter) (*nativeAnthropicStreamWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support streaming")
	}
	return &nativeAnthropicStreamWriter{
		w:       w,
		flusher: flusher, committed: false,
	}, nil
}

func (w *nativeAnthropicStreamWriter) WriteEvent(adapterrender.Event) error {
	return nil
}

func (w *nativeAnthropicStreamWriter) Flush() error {
	if w != nil && w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

func (w *nativeAnthropicStreamWriter) commit(header http.Header) {
	if w == nil || w.committed {
		return
	}
	for key, values := range header {
		for _, value := range values {
			w.w.Header().Add(key, value)
		}
	}
	if w.w.Header().Get("Content-Type") == "" {
		w.w.Header().Set("Content-Type", "text/event-stream")
	}
	w.w.WriteHeader(http.StatusOK)
	w.committed = true
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

func (w *nativeAnthropicStreamWriter) write(chunk []byte) error {
	if w == nil {
		return fmt.Errorf("native anthropic stream writer missing")
	}
	if !w.committed {
		w.commit(http.Header{"Content-Type": {"text/event-stream"}})
	}
	if _, err := w.w.Write(chunk); err != nil {
		slog.Warn("adapter.anthropic_native.write_chunk_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return fmt.Errorf("write native anthropic stream chunk: %w", err)
	}
	return w.Flush()
}

func (w *nativeAnthropicStreamWriter) relay(resp *http.Response) error {
	if w == nil {
		return fmt.Errorf("native anthropic stream writer missing")
	}
	w.commit(resp.Header.Clone())
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if writeErr := w.write(buf[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		slog.Warn("adapter.anthropic_native.relay_read_failed", "concern", "adapter.providers.anthropic.request", "err", err)
		return fmt.Errorf("read native anthropic stream response: %w", err)
	}
}

var (
	_ adapterprovider.EventWriter = (*nativeAnthropicJSONWriter)(nil)
	_ adapterprovider.EventWriter = (*nativeAnthropicStreamWriter)(nil)
)
