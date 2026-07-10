package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

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

func (s *Server) handleAnthropicCountTokens(_ context.Context, hctx *handlerCtx) error {
	if hctx.Request.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}
	err := newAdapterError(adapterErrorModelNotSupported, "/v1/messages/count_tokens is not implemented yet on the adapter Anthropic ingress")
	err.HTTPStatus = http.StatusNotImplemented
	// The neutral Code carries the not_supported_error reason; the
	// Anthropic renderer derives the spec-correct envelope type from it,
	// so the generic adapter never names the wire type.
	err.Code = "not_supported_error"
	return err
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
