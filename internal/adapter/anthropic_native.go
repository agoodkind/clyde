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
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

const maxAnthropicMessagesBodyBytes = 8 << 20

func (s *Server) handleAnthropicMessages(ctx context.Context, hctx *handlerCtx) error {
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
	corr.SetHTTPHeaders(w.Header())
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

	model, _, err := s.registry.Resolve(req.Model, "")
	if err != nil {
		return adapterErrModelNotFound(err.Error())
	}
	nativeClaudeModel := isNativeClaudeModelID(req.Model)
	if !nativeClaudeModel && model.Backend != BackendAnthropic && model.Backend != BackendClaude {
		return adapterErrInvalidRequest("model does not resolve to the anthropic backend", nil)
	}
	if nativeClaudeModel && model.Backend != BackendAnthropic && model.Backend != BackendClaude {
		model = ResolvedModel{
			Alias:       strings.TrimSpace(req.Model),
			Backend:     BackendClaude,
			ClaudeModel: strings.TrimSpace(req.Model),
		}
	}
	req.Model = anthropicIngressWireModel(req.Model, model)

	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("path", r.URL.Path),
		slog.String("model", req.Model),
		slog.Bool("stream", req.Stream),
		slog.Int("body_bytes", len(body)),
	}
	attrs = append(attrs, corr.Attrs()...)
	s.log.LogAttrs(ctx, slog.LevelInfo, "adapter.anthropic.ingress", attrs...)

	prepared := anthropic.PreparedRequest{
		Request:       req,
		Model:         anthropicIngressResolvedModel(model),
		RequestID:     reqID,
		Stream:        req.Stream,
		NativeIngress: true,
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

func isNativeClaudeModelID(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
}

func anthropicIngressWireModel(requested string, model ResolvedModel) string {
	if isNativeClaudeModelID(requested) {
		return anthropicbackend.StripContextSuffix(requested)
	}
	return anthropicbackend.StripContextSuffix(model.ClaudeModel)
}

func anthropicIngressResolvedModel(model ResolvedModel) adaptermodel.ResolvedModel {
	return adaptermodel.ResolvedModel{
		Alias:           model.Alias,
		Backend:         adaptermodel.BackendAnthropic,
		ClaudeModel:     model.ClaudeModel,
		Context:         model.Context,
		Effort:          "",
		MaxOutputTokens: model.MaxOutputTokens,
	}
}

func (s *Server) writeAnthropicIngressProviderError(w http.ResponseWriter, r *http.Request, err error) {
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
		header: make(http.Header),
	}
}

func (w *nativeAnthropicJSONWriter) WriteEvent(adapterrender.Event) error {
	return nil
}

func (w *nativeAnthropicJSONWriter) Flush() error {
	return nil
}

func (w *nativeAnthropicJSONWriter) capture(status int, header http.Header, body []byte) error {
	if w == nil {
		return fmt.Errorf("native anthropic writer missing")
	}
	w.status = status
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
		flusher: flusher,
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
		return err
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
		return err
	}
}

var (
	_ adapterprovider.EventWriter = (*nativeAnthropicJSONWriter)(nil)
	_ adapterprovider.EventWriter = (*nativeAnthropicStreamWriter)(nil)
)
