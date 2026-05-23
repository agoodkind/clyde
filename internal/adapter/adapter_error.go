package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/adapter/errcontract"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/slogger"
)

type adapterRouteFamily string

const (
	adapterRouteOpenAI    adapterRouteFamily = "openai"
	adapterRouteAnthropic adapterRouteFamily = "anthropic"
	adapterRouteHealth    adapterRouteFamily = "health"
)

type adapterErrorClass string

const (
	adapterErrorAuthFailed              adapterErrorClass = "auth_failed"
	adapterErrorMethodNotAllowed        adapterErrorClass = "method_not_allowed"
	adapterErrorInvalidJSON             adapterErrorClass = "invalid_json"
	adapterErrorInvalidRequest          adapterErrorClass = "invalid_request"
	adapterErrorModelNotFound           adapterErrorClass = "model_not_found"
	adapterErrorModelNotSupported       adapterErrorClass = "model_not_supported"
	adapterErrorUnsupportedBackend      adapterErrorClass = "unsupported_backend"
	adapterErrorUnsupportedContent      adapterErrorClass = "unsupported_content"
	adapterErrorContextLengthExceeded   adapterErrorClass = "context_length_exceeded"
	adapterErrorRateLimited             adapterErrorClass = "rate_limited"
	adapterErrorUpstreamAuthFailed      adapterErrorClass = "upstream_auth_failed"
	adapterErrorUpstreamUnavailable     adapterErrorClass = "upstream_unavailable"
	adapterErrorUpstreamFailed          adapterErrorClass = "upstream_failed"
	adapterErrorUpstreamRateLimited     adapterErrorClass = "upstream_rate_limited"
	adapterErrorUpstreamSchemaViolation adapterErrorClass = "upstream_schema_violation"
	adapterErrorUpstreamNetworkError    adapterErrorClass = "upstream_network_error"
	adapterErrorTimeout                 adapterErrorClass = "timeout"
	adapterErrorCanceled                adapterErrorClass = "canceled"
	adapterErrorInternal                adapterErrorClass = "internal"
)

type adapterError struct {
	Class          adapterErrorClass
	HTTPStatus     int
	Message        string
	OpenAIType     string
	OpenAICode     string
	OpenAIParam    string
	AnthropicType  string
	Provider       string
	Backend        string
	ModelAlias     string
	ResolvedModel  string
	UpstreamStatus int
	Cause          error
	SafeForClient  bool
}

func (e *adapterError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return string(e.Class)
}

func (e *adapterError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func newAdapterError(class adapterErrorClass, message string) *adapterError {
	e := &adapterError{
		Class:         class,
		Message:       strings.TrimSpace(message),
		SafeForClient: true,
	}
	e.applyDefaults()
	return e
}

func adapterErrInvalidJSON(message string, cause error) *adapterError {
	e := newAdapterError(adapterErrorInvalidJSON, message)
	e.Cause = cause
	return e
}

func adapterErrInvalidRequest(message string, cause error) *adapterError {
	e := newAdapterError(adapterErrorInvalidRequest, message)
	e.Cause = cause
	return e
}

func adapterErrModelNotFound(message string) *adapterError {
	return newAdapterError(adapterErrorModelNotFound, message)
}

func adapterErrInternal(message string, cause error) *adapterError {
	e := newAdapterError(adapterErrorInternal, message)
	e.Cause = cause
	e.SafeForClient = false
	return e
}

func adapterErrUpstreamFailed(provider, message string, cause error) *adapterError {
	e := newAdapterError(adapterErrorUpstreamFailed, message)
	e.Provider = strings.TrimSpace(provider)
	e.Cause = cause
	return e
}

// adapterErrorDefaults holds the default envelope fields for a single
// adapterErrorClass. The applyDefaults table below is the single
// source of truth for class-to-envelope mapping; new classes plug in
// by adding a row instead of growing the switch.
type adapterErrorDefaults struct {
	HTTPStatus    int
	OpenAIType    string
	OpenAICode    string
	OpenAIParam   string
	AnthropicType string
}

var adapterErrorDefaultsByClass = map[adapterErrorClass]adapterErrorDefaults{
	adapterErrorAuthFailed:              {HTTPStatus: http.StatusUnauthorized, OpenAIType: "authentication_error", OpenAICode: "unauthorized", OpenAIParam: "", AnthropicType: "authentication_error"},
	adapterErrorMethodNotAllowed:        {HTTPStatus: http.StatusMethodNotAllowed, OpenAIType: "invalid_request_error", OpenAICode: "method_not_allowed", OpenAIParam: "", AnthropicType: "invalid_request_error"},
	adapterErrorInvalidJSON:             {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "invalid_json", OpenAIParam: "", AnthropicType: "invalid_request_error"},
	adapterErrorInvalidRequest:          {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "invalid_request", OpenAIParam: "", AnthropicType: "invalid_request_error"},
	adapterErrorModelNotFound:           {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "model_not_found", OpenAIParam: "model", AnthropicType: "invalid_request_error"},
	adapterErrorModelNotSupported:       {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "model_not_supported", OpenAIParam: "model", AnthropicType: "invalid_request_error"},
	adapterErrorUnsupportedBackend:      {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "unsupported_backend", OpenAIParam: "", AnthropicType: "invalid_request_error"},
	adapterErrorUnsupportedContent:      {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "unsupported_content", OpenAIParam: "", AnthropicType: "invalid_request_error"},
	adapterErrorContextLengthExceeded:   {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "context_length_exceeded", OpenAIParam: "messages", AnthropicType: "invalid_request_error"},
	adapterErrorRateLimited:             {HTTPStatus: http.StatusTooManyRequests, OpenAIType: "rate_limit_error", OpenAICode: "rate_limit_exceeded", OpenAIParam: "", AnthropicType: "rate_limit_error"},
	adapterErrorUpstreamAuthFailed:      {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "upstream_auth_failed", OpenAIParam: "", AnthropicType: "authentication_error"},
	adapterErrorUpstreamRateLimited:     {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "upstream_rate_limited", OpenAIParam: "", AnthropicType: "rate_limit_error"},
	adapterErrorUpstreamSchemaViolation: {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "upstream_malformed_request", OpenAIParam: "", AnthropicType: "invalid_request_error"},
	adapterErrorUpstreamNetworkError:    {HTTPStatus: http.StatusBadRequest, OpenAIType: "invalid_request_error", OpenAICode: "upstream_network_error", OpenAIParam: "", AnthropicType: "api_error"},
	adapterErrorUpstreamUnavailable:     {HTTPStatus: http.StatusBadGateway, OpenAIType: "server_error", OpenAICode: "upstream_unavailable", OpenAIParam: "", AnthropicType: "api_error"},
	adapterErrorUpstreamFailed:          {HTTPStatus: http.StatusBadGateway, OpenAIType: "server_error", OpenAICode: "upstream_failed", OpenAIParam: "", AnthropicType: "api_error"},
	adapterErrorTimeout:                 {HTTPStatus: http.StatusGatewayTimeout, OpenAIType: "server_error", OpenAICode: "timeout", OpenAIParam: "", AnthropicType: "api_error"},
	adapterErrorCanceled:                {HTTPStatus: 499, OpenAIType: "server_error", OpenAICode: "canceled", OpenAIParam: "", AnthropicType: "api_error"},
	adapterErrorInternal:                {HTTPStatus: http.StatusInternalServerError, OpenAIType: "internal_error", OpenAICode: "internal_error", OpenAIParam: "", AnthropicType: "api_error"},
}

func (e *adapterError) applyDefaults() {
	explicitStatus := e.HTTPStatus
	explicitOpenAIType := e.OpenAIType
	explicitOpenAICode := e.OpenAICode
	explicitOpenAIParam := e.OpenAIParam
	explicitAnthropicType := e.AnthropicType
	if e.HTTPStatus == 0 {
		e.HTTPStatus = http.StatusInternalServerError
	}
	if defaults, ok := adapterErrorDefaultsByClass[e.Class]; ok {
		e.HTTPStatus = defaults.HTTPStatus
		e.OpenAIType = defaults.OpenAIType
		e.OpenAICode = defaults.OpenAICode
		e.OpenAIParam = defaults.OpenAIParam
		e.AnthropicType = defaults.AnthropicType
	}
	if e.Message == "" {
		e.Message = string(e.Class)
	}
	if explicitStatus > 0 {
		e.HTTPStatus = explicitStatus
	}
	if explicitOpenAIType != "" {
		e.OpenAIType = explicitOpenAIType
	}
	if explicitOpenAICode != "" {
		e.OpenAICode = explicitOpenAICode
	}
	if explicitOpenAIParam != "" {
		e.OpenAIParam = explicitOpenAIParam
	}
	if explicitAnthropicType != "" {
		e.AnthropicType = explicitAnthropicType
	}
}

func adapterRouteFamilyForPath(path string) adapterRouteFamily {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return adapterRouteAnthropic
	case path == "/healthz":
		return adapterRouteHealth
	default:
		return adapterRouteOpenAI
	}
}

func adapterErrorFrom(err error) *adapterError {
	if err == nil {
		return nil
	}
	var aerr *adapterError
	if errors.As(err, &aerr) {
		aerr.applyDefaults()
		return aerr
	}
	return adapterErrUpstreamFailed("", err.Error(), err)
}

// respondAdapterError is a deprecated alias for writeShapedError. New
// callers should use writeShapedError; this wrapper exists so the
// PR1 boundary helpers can land without rewriting every existing call
// site in one sweep.
func (s *Server) respondAdapterError(w http.ResponseWriter, r *http.Request, err error) {
	s.writeShapedError(w, r, err)
}

// writeShapedError renders a non-2xx response by looking up the
// route family's registered ErrorRenderer and handing it the
// primitive ErrorInfo derived from the adapterError. The boundary
// never imports a provider envelope type and never constructs an
// envelope literal; provider packages own those shapes through the
// errcontract.ErrorRenderer interface.
func (s *Server) writeShapedError(w http.ResponseWriter, r *http.Request, err error) {
	aerr := adapterErrorFrom(err)
	if aerr == nil {
		aerr = adapterErrInternal("adapter internal error", nil)
	}
	family := adapterRouteFamilyForPath(r.URL.Path)
	aerr = applyFamilyShape(family, aerr)
	corr := correlationForRequest(r)
	message := aerr.Message
	if !aerr.SafeForClient {
		message = "adapter internal error"
		if corr.RequestID != "" {
			message += "; see Clyde logs with request_id " + corr.RequestID
		}
	}
	message = maybeAppendClydeRequestID(family, aerr, message, corr.RequestID)
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.String("route_family", string(family)),
		slog.Int("status", aerr.HTTPStatus),
		slog.String("error_class", string(aerr.Class)),
		slog.String("openai_type", aerr.OpenAIType),
		slog.String("openai_code", aerr.OpenAICode),
		slog.String("anthropic_type", aerr.AnthropicType),
		slog.String("model", aerr.ModelAlias),
		slog.String("backend", aerr.Backend),
		slog.String("provider", aerr.Provider),
		slog.Int("upstream_status", aerr.UpstreamStatus),
		slog.Bool("response_started", false),
		slog.Bool("safe_for_client", aerr.SafeForClient),
		slog.String("err", aerr.Error()),
	}
	attrs = append(attrs, corr.Attrs()...)
	slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPErrors).LogAttrs(r.Context(), slog.LevelWarn, "adapter.error.responded", attrs...)
	info := adapterErrorInfoForRequest(family, aerr, message, corr, r)
	renderer, ok := s.lookupErrorRenderer(family)
	if !ok {
		s.writeFallbackError(r.Context(), w, family, aerr.HTTPStatus, info)
		return
	}
	if writeErr := renderer.Render(w, aerr.HTTPStatus, info); writeErr != nil {
		s.log.LogAttrs(r.Context(), slog.LevelWarn, "adapter.error_boundary.render_failed",
			slog.String("route_family", string(family)),
			slog.Any("err", writeErr),
		)
	}
}

// rendererTypeForFamily picks the envelope Type primitive the
// renderer should use. The Anthropic family wants the spec-correct
// envelope type that lives on AnthropicType; the OpenAI family wants
// the OpenAIType stored on the adapterError.
func rendererTypeForFamily(family adapterRouteFamily, aerr *adapterError) string {
	if family == adapterRouteAnthropic {
		if aerr.AnthropicType != "" {
			return aerr.AnthropicType
		}
		return "api_error"
	}
	return aerr.OpenAIType
}

func adapterErrorInfoForFamily(family adapterRouteFamily, aerr *adapterError, message string) errcontract.ErrorInfo {
	if aerr == nil {
		return errcontract.ErrorInfo{
			Type:        "internal_error",
			Code:        "internal_error",
			Message:     "adapter internal error",
			Param:       "",
			Diagnostics: nil,
		}
	}
	aerr.applyDefaults()
	if message == "" {
		message = aerr.Message
	}
	return errcontract.ErrorInfo{
		Type:        rendererTypeForFamily(family, aerr),
		Code:        aerr.OpenAICode,
		Message:     message,
		Param:       aerr.OpenAIParam,
		Diagnostics: nil,
	}
}

func adapterErrorInfoForRequest(
	family adapterRouteFamily,
	aerr *adapterError,
	message string,
	corr correlation.Context,
	r *http.Request,
) errcontract.ErrorInfo {
	info := adapterErrorInfoForFamily(family, aerr, message)
	info.Diagnostics = errorDiagnosticsForRequest(family, aerr, corr, r)
	return info
}

func maybeAppendClydeRequestID(family adapterRouteFamily, aerr *adapterError, message string, requestID string) string {
	if family != adapterRouteOpenAI || aerr == nil || strings.TrimSpace(requestID) == "" {
		return message
	}
	if !strings.HasPrefix(string(aerr.Class), "upstream_") {
		return message
	}
	needle := "Clyde request_id="
	if strings.Contains(message, needle) {
		return message
	}
	return strings.TrimSpace(message) + " (" + needle + requestID + ")"
}

func errorDiagnosticsForRequest(
	family adapterRouteFamily,
	aerr *adapterError,
	corr correlation.Context,
	r *http.Request,
) *errcontract.ErrorDiagnostics {
	if aerr == nil {
		aerr = adapterErrInternal("adapter internal error", nil)
	}
	headers := map[string]string(nil)
	headerNames := []string(nil)
	method := ""
	path := ""
	userAgent := ""
	if r != nil {
		headers = redactedHeaders(r.Header)
		headerNames = HeaderNames(r.Header)
		method = r.Method
		if r.URL != nil {
			path = r.URL.Path
		}
		userAgent = r.UserAgent()
	}
	return &errcontract.ErrorDiagnostics{
		RequestID:          corr.RequestID,
		TraceID:            string(corr.TraceID),
		SpanID:             string(corr.SpanID),
		ParentSpanID:       string(corr.ParentSpanID),
		ChatKey:            corr.ChatKey,
		ChatKeySource:      corr.ChatKeySource,
		ChatRootKey:        corr.ChatRootKey,
		ChatBranchKey:      corr.ChatBranchKey,
		IdentityAttributes: diagnosticIdentityAttributes(corr),
		UpstreamRequestID:  corr.UpstreamRequestID,
		UpstreamResponseID: corr.UpstreamResponseID,
		Provider:           aerr.Provider,
		Backend:            aerr.Backend,
		ModelAlias:         aerr.ModelAlias,
		ResolvedModel:      aerr.ResolvedModel,
		ErrorClass:         string(aerr.Class),
		RouteFamily:        string(family),
		Method:             method,
		Path:               path,
		UserAgent:          userAgent,
		HeaderNames:        headerNames,
		Headers:            headers,
		LogHint:            errorLogHint(corr.RequestID),
	}
}

func diagnosticIdentityAttributes(corr correlation.Context) []errcontract.DiagnosticField {
	if len(corr.IdentityAttributes) == 0 {
		return nil
	}
	fields := make([]errcontract.DiagnosticField, 0, len(corr.IdentityAttributes))
	for _, attr := range corr.IdentityAttributes {
		if attr.Key == "" || attr.Value == "" {
			continue
		}
		fields = append(fields, errcontract.DiagnosticField{
			Key:   attr.Key,
			Value: attr.Value,
		})
	}
	return fields
}

func errorLogHint(requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return "adapter/http/errors.jsonl"
	}
	return "adapter/http/errors.jsonl request_id=" + requestID
}

// lookupErrorRenderer returns the registered renderer for a family,
// preferring the per-Server registry when populated and falling back
// to the package defaults so non-Server call paths and tests still
// resolve the canonical provider renderer.
func (s *Server) lookupErrorRenderer(family adapterRouteFamily) (errcontract.ErrorRenderer, bool) {
	if s != nil && s.errorRenderers != nil {
		if r, ok := s.errorRenderers[family]; ok {
			return r, true
		}
	}
	return defaultBoundaryRegistry.errorRenderer(family)
}

func (s *Server) lookupStreamErrorRenderer(family adapterRouteFamily) (errcontract.StreamErrorRenderer, bool) {
	if s != nil && s.streamErrorRenderers != nil {
		if r, ok := s.streamErrorRenderers[family]; ok {
			return r, true
		}
	}
	return defaultBoundaryRegistry.streamErrorRenderer(family)
}

// writeFallbackError handles the unregistered-family path so
// the boundary is still guaranteed to emit a parseable JSON body
// instead of a bare WriteHeader. The chosen shape is a
// minimal-on-purpose JSON string body; callers add the warning log so
// regressions in registration get noticed.
func (s *Server) writeFallbackError(ctx context.Context, w http.ResponseWriter, family adapterRouteFamily, status int, info errcontract.ErrorInfo) {
	s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.error_boundary.no_renderer_for_family",
		slog.String("route_family", string(family)),
		slog.Int("status", status),
		slog.String("error_type", info.Type),
		slog.String("error_code", info.Code),
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	const fallback = `{"error":{"message":"no error renderer registered for route family","type":"internal_error","code":"internal_error"}}`
	_, _ = fmt.Fprint(w, fallback)
}

// respondAdapterStreamError emits one Cursor-safe SSE error frame
// followed by the [DONE] terminator. Mid-stream errors must use this
// helper because the response headers have already been committed and
// switching to a JSON envelope would corrupt the SSE channel.
func (s *Server) respondAdapterStreamError(ctx context.Context, sse *adapteropenai.SSEWriter, err error) error {
	if sse == nil {
		return nil
	}
	aerr := adapterErrorFrom(err)
	if aerr == nil {
		aerr = adapterErrInternal("adapter internal error", nil)
	}
	aerr = applyFamilyShape(adapterRouteOpenAI, aerr)
	message := aerr.Message
	if !aerr.SafeForClient {
		message = "adapter internal error"
	}
	corr := correlation.FromContext(ctx)
	message = maybeAppendClydeRequestID(adapterRouteOpenAI, aerr, message, corr.RequestID)
	info := adapterErrorInfoForFamily(adapterRouteOpenAI, aerr, message)
	info.Diagnostics = errorDiagnosticsForRequest(adapterRouteOpenAI, aerr, corr, nil)
	renderer, ok := s.lookupStreamErrorRenderer(adapterRouteOpenAI)
	if !ok {
		return fmt.Errorf("no stream error renderer registered for route family %q", adapterRouteOpenAI)
	}
	if writeErr := renderer.WriteStreamError(sse, info); writeErr != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_error_write_failed",
			slog.String("openai_type", info.Type),
			slog.String("openai_code", info.Code),
			slog.Any("err", writeErr),
		)
		return fmt.Errorf("render stream error: %w", writeErr)
	}
	return nil
}
