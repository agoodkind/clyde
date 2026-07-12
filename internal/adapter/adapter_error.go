package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	"goodkind.io/clyde/internal/adapter/errcontract"
	"goodkind.io/clyde/internal/clydeingress"
	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
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
	adapterErrorBaselineMissing         adapterErrorClass = "baseline_missing"
	adapterErrorTimeout                 adapterErrorClass = "timeout"
	adapterErrorCanceled                adapterErrorClass = "canceled"
	adapterErrorInternal                adapterErrorClass = "internal"
)

// adapterError is the single generic error type every non-2xx adapter
// response becomes. Its fields are neutral: Class is the boundary's
// own classification, Code and Param are Clyde's neutral wire-agnostic
// values that the OpenAI renderer passes through and the Anthropic
// renderer ignores, and Message is the chosen client-visible text. The
// provider-specific envelope Type lives in the provider renderers,
// which derive it from Class. The generic adapter never holds a
// provider envelope wire string such as "authentication_error" or
// "api_error".
type adapterError struct {
	Class             adapterErrorClass
	HTTPStatus        int
	Message           string
	Code              string
	Param             string
	Provider          string
	Backend           string
	ModelAlias        string
	ResolvedModelName string
	UpstreamStatus    int
	Cause             error
	SafeForClient     bool
	Warnings          []adaptercompat.CompatibilityWarning
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
		Class:             class,
		HTTPStatus:        0,
		Message:           strings.TrimSpace(message),
		Code:              "",
		Param:             "",
		Provider:          "",
		Backend:           "",
		ModelAlias:        "",
		ResolvedModelName: "",
		UpstreamStatus:    0,
		Cause:             nil,
		SafeForClient:     true,
		Warnings:          nil,
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

// adapterErrBaselineMissing reports that a provider's daemon-owned MITM
// wire baseline is absent or invalid, so the adapter cannot build a
// correctly-shaped outbound request. It maps to HTTP 503 with an
// operator-actionable message rather than sending a wrong-shaped
// request or panicking.
func adapterErrBaselineMissing(provider, message string, cause error) *adapterError {
	e := newAdapterError(adapterErrorBaselineMissing, message)
	e.Provider = strings.TrimSpace(provider)
	e.Cause = cause
	return e
}

// adapterErrorDefaults holds the neutral default fields for a single
// adapterErrorClass. The applyDefaults table below is the single
// source of truth for class-to-HTTP-status and class-to-neutral-code
// mapping; new classes plug in by adding a row. The table holds no
// provider wire type string: the OpenAI and Anthropic renderers each
// derive their own envelope `error.type` from the neutral class.
type adapterErrorDefaults struct {
	HTTPStatus int
	Code       string
	Param      string
}

var adapterErrorDefaultsByClass = map[adapterErrorClass]adapterErrorDefaults{
	adapterErrorAuthFailed:              {HTTPStatus: http.StatusUnauthorized, Code: "unauthorized", Param: ""},
	adapterErrorMethodNotAllowed:        {HTTPStatus: http.StatusMethodNotAllowed, Code: "method_not_allowed", Param: ""},
	adapterErrorInvalidJSON:             {HTTPStatus: http.StatusBadRequest, Code: "invalid_json", Param: ""},
	adapterErrorInvalidRequest:          {HTTPStatus: http.StatusBadRequest, Code: "invalid_request", Param: ""},
	adapterErrorModelNotFound:           {HTTPStatus: http.StatusBadRequest, Code: "model_not_found", Param: "model"},
	adapterErrorModelNotSupported:       {HTTPStatus: http.StatusBadRequest, Code: "model_not_supported", Param: "model"},
	adapterErrorUnsupportedBackend:      {HTTPStatus: http.StatusBadRequest, Code: "unsupported_backend", Param: ""},
	adapterErrorUnsupportedContent:      {HTTPStatus: http.StatusBadRequest, Code: "unsupported_content", Param: ""},
	adapterErrorContextLengthExceeded:   {HTTPStatus: http.StatusBadRequest, Code: "context_length_exceeded", Param: "messages"},
	adapterErrorRateLimited:             {HTTPStatus: http.StatusTooManyRequests, Code: "rate_limit_exceeded", Param: ""},
	adapterErrorUpstreamAuthFailed:      {HTTPStatus: http.StatusBadRequest, Code: "upstream_auth_failed", Param: ""},
	adapterErrorUpstreamRateLimited:     {HTTPStatus: http.StatusBadRequest, Code: "upstream_rate_limited", Param: ""},
	adapterErrorUpstreamSchemaViolation: {HTTPStatus: http.StatusBadRequest, Code: "upstream_malformed_request", Param: ""},
	adapterErrorUpstreamNetworkError:    {HTTPStatus: http.StatusBadRequest, Code: "upstream_network_error", Param: ""},
	adapterErrorUpstreamUnavailable:     {HTTPStatus: http.StatusBadGateway, Code: "upstream_unavailable", Param: ""},
	adapterErrorUpstreamFailed:          {HTTPStatus: http.StatusBadGateway, Code: "upstream_failed", Param: ""},
	adapterErrorBaselineMissing:         {HTTPStatus: http.StatusServiceUnavailable, Code: "wire_baseline_unavailable", Param: ""},
	adapterErrorTimeout:                 {HTTPStatus: http.StatusGatewayTimeout, Code: "timeout", Param: ""},
	adapterErrorCanceled:                {HTTPStatus: 499, Code: "canceled", Param: ""},
	adapterErrorInternal:                {HTTPStatus: http.StatusInternalServerError, Code: "internal_error", Param: ""},
}

func (e *adapterError) applyDefaults() {
	explicitStatus := e.HTTPStatus
	explicitCode := e.Code
	explicitParam := e.Param
	if e.HTTPStatus == 0 {
		e.HTTPStatus = http.StatusInternalServerError
	}
	if defaults, ok := adapterErrorDefaultsByClass[e.Class]; ok {
		e.HTTPStatus = defaults.HTTPStatus
		e.Code = defaults.Code
		e.Param = defaults.Param
	}
	if e.Message == "" {
		e.Message = string(e.Class)
	}
	if explicitStatus > 0 {
		e.HTTPStatus = explicitStatus
	}
	if explicitCode != "" {
		e.Code = explicitCode
	}
	if explicitParam != "" {
		e.Param = explicitParam
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
		slog.String("error_code", aerr.Code),
		slog.String("error_param", aerr.Param),
		slog.String("model", aerr.ModelAlias),
		slog.String("backend", aerr.Backend),
		slog.String("provider", aerr.Provider),
		slog.Int("upstream_status", aerr.UpstreamStatus),
		slog.Bool("response_started", false),
		slog.Bool("safe_for_client", aerr.SafeForClient),
		slog.String("err", aerr.Error()),
	}
	attrs = append(attrs, corr.Attrs()...)
	s.adapterErrorLog().LogAttrs(r.Context(), slog.LevelWarn, "adapter.error.responded", attrs...)
	info := adapterErrorInfoForRequest(family, aerr, message, corr, r)
	renderer, ok := s.lookupErrorRenderer(family)
	if !ok {
		s.writeFallbackError(r.Context(), w, family, aerr.HTTPStatus, info)
		return
	}
	if writeErr := renderer.Render(w, aerr.HTTPStatus, info); writeErr != nil {
		s.adapterErrorLog().LogAttrs(r.Context(), slog.LevelWarn, "adapter.error_boundary.render_failed", slog.String("route_family", string(family)),
			slog.Any("err", writeErr),
		)
	}
}

func (s *Server) adapterErrorLog() *slog.Logger {
	if s == nil || s.errorLog == nil {
		return slogger.For(slogger.ConcernAdapterHTTPErrors)
	}
	return s.errorLog
}

func (s *Server) adapterChatDispatchLog() *slog.Logger {
	if s == nil || s.dispatchLog == nil {
		return slogger.For(slogger.ConcernAdapterChatDispatch)
	}
	return s.dispatchLog
}

// adapterErrorInfoForFamily builds the primitive ErrorInfo the
// boundary hands to a family renderer. It passes the neutral Class,
// Code, Param, and Message through and leaves Type empty so each
// renderer derives its own family-correct envelope `error.type` from
// the neutral Class. The upstream-mapper path is the only caller that
// fills Type directly, because the provider mapper already chose the
// family-correct type; this direct-construction path never names a
// provider wire type.
func adapterErrorInfoForFamily(_ adapterRouteFamily, aerr *adapterError, message string) errcontract.ErrorInfo {
	if aerr == nil {
		return errcontract.ErrorInfo{
			Type:           "",
			Class:          string(adapterErrorInternal),
			Code:           "internal_error",
			Message:        "adapter internal error",
			Param:          "",
			UpstreamStatus: 0,
			Diagnostics:    nil,
		}
	}
	aerr.applyDefaults()
	if message == "" {
		message = aerr.Message
	}
	return errcontract.ErrorInfo{
		Type:           "",
		Class:          string(aerr.Class),
		Code:           aerr.Code,
		Message:        message,
		Param:          aerr.Param,
		UpstreamStatus: aerr.UpstreamStatus,
		Diagnostics:    nil,
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
		ChatKey:            clydeingress.ChatKey(corr),
		ChatKeySource:      clydeingress.ChatKeySource(corr),
		ChatRootKey:        clydeingress.ChatRootKey(corr),
		ChatBranchKey:      clydeingress.ChatBranchKey(corr),
		IdentityAttributes: diagnosticIdentityAttributes(corr),
		UpstreamRequestID:  clydeingress.UpstreamRequestID(corr),
		UpstreamResponseID: clydeingress.UpstreamResponseID(corr),
		Warnings:           aerr.Warnings,
		Provider:           aerr.Provider,
		Backend:            aerr.Backend,
		ModelAlias:         aerr.ModelAlias,
		ResolvedModelName:  aerr.ResolvedModelName,
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
		// Skip clyde-owned identity attribute keys that the diagnostics
		// envelope already exposes as named fields. Vendor metadata
		// (cursor_request_id, etc.) flows through unchanged.
		if diagnosticReservedIdentityKey(attr.Key) {
			continue
		}
		fields = append(fields, errcontract.DiagnosticField{
			Key:   attr.Key,
			Value: attr.Value,
		})
	}
	return fields
}

func diagnosticReservedIdentityKey(key string) bool {
	switch key {
	case clydeingress.AttrKeyChatKey,
		clydeingress.AttrKeyChatKeySource,
		clydeingress.AttrKeyChatRootKey,
		clydeingress.AttrKeyChatBranchKey,
		clydeingress.AttrKeyUpstreamRequestID,
		clydeingress.AttrKeyUpstreamResponseID:
		return true
	}
	return false
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
	s.adapterErrorLog().LogAttrs(ctx, slog.LevelWarn, "adapter.error_boundary.no_renderer_for_family", slog.String("route_family", string(family)),
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
func (s *Server) respondAdapterStreamError(ctx context.Context, sse errcontract.StreamErrorWriter, err error) error {
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
		s.log.LogAttrs(ctx, slog.LevelWarn, "adapter.chat.stream_error_write_failed", slog.String("concern", "adapter.chat.render"), slog.String("openai_type", info.Type),
			slog.String("openai_code", info.Code),
			slog.Any("err", writeErr),
		)
		return fmt.Errorf("render stream error: %w", writeErr)
	}
	return nil
}
