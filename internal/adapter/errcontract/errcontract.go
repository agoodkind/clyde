// Package errcontract declares the dependency-inversion interfaces
// the adapter error boundary uses to render and classify upstream
// failures without importing any provider envelope type. The boundary
// declares what it needs (primitives in, primitives out), and each
// provider package supplies the concrete renderer and mapper that
// know how to construct that family's envelope. Keeping the boundary
// blind to provider envelope shapes is what enforces the layering
// rule: the generic adapter never speaks OpenAI or Anthropic
// envelope syntax, only primitives.
package errcontract

import (
	"net/http"
)

// ErrorInfo is the primitive payload the boundary hands to a family
// renderer. Every field is a primitive string so the boundary never
// imports a provider envelope type. An empty Code means omit the
// field from the rendered envelope.
//
// Class is the boundary's neutral error classification (the adapter's
// own adapterErrorClass values such as "auth_failed" or
// "upstream_failed"). The boundary never names a provider's wire Type;
// it hands the neutral Class through, and each family renderer derives
// its own envelope Type from that Class when Type is empty. The
// upstream-mapper path fills Type directly because the mapper already
// chose the family-correct envelope type, so renderers prefer a
// non-empty Type and fall back to the Class-derived type otherwise.
type ErrorInfo struct {
	Type    string
	Class   string
	Code    string
	Message string
	Param   string
	// UpstreamStatus is the neutral upstream HTTP status (0 when the
	// failure did not originate from an upstream HTTP response). A
	// family renderer may use it to derive a spec-correct envelope type
	// when the status carries the classification (e.g. the Anthropic
	// family maps 401 to authentication_error and 429 to
	// rate_limit_error). It is a plain integer, not a provider wire
	// string, so the generic boundary stays blind to envelope syntax.
	UpstreamStatus int
	Diagnostics    *ErrorDiagnostics
}

// ErrorDiagnostics carries primitive, client-visible breadcrumbs that let an
// operator or follow-up LLM jump from a displayed provider error back to the
// exact Clyde log record. Header values must already be redacted before they
// reach this struct.
type ErrorDiagnostics struct {
	RequestID          string            `json:"request_id,omitempty"`
	TraceID            string            `json:"trace_id,omitempty"`
	SpanID             string            `json:"span_id,omitempty"`
	ParentSpanID       string            `json:"parent_span_id,omitempty"`
	ChatKey            string            `json:"chat_key,omitempty"`
	ChatKeySource      string            `json:"chat_key_source,omitempty"`
	ChatRootKey        string            `json:"chat_root_key,omitempty"`
	ChatBranchKey      string            `json:"chat_branch_key,omitempty"`
	IdentityAttributes []DiagnosticField `json:"identity_attrs,omitempty"`
	UpstreamRequestID  string            `json:"upstream_request_id,omitempty"`
	UpstreamResponseID string            `json:"upstream_response_id,omitempty"`
	Provider           string            `json:"provider,omitempty"`
	Backend            string            `json:"backend,omitempty"`
	ModelAlias         string            `json:"model_alias,omitempty"`
	ResolvedModelName  string            `json:"resolved_model,omitempty"`
	ErrorClass         string            `json:"error_class,omitempty"`
	RouteFamily        string            `json:"route_family,omitempty"`
	Method             string            `json:"method,omitempty"`
	Path               string            `json:"path,omitempty"`
	UserAgent          string            `json:"user_agent,omitempty"`
	HeaderNames        []string          `json:"header_names,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
	LogHint            string            `json:"log_hint,omitempty"`
}

// DiagnosticField is a provider-owned identity field included in
// diagnostics without teaching the generic error contract the
// provider's field names.
type DiagnosticField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// UpstreamCodeClass classifies an upstream failure across providers
// without naming a specific provider's error type.
type UpstreamCodeClass string

// UpstreamClassRateLimit is the class of upstream rate-limit
// failures (HTTP 429 plus typed code rate_limit_exceeded or similar).
const UpstreamClassRateLimit UpstreamCodeClass = "rate_limit"

// UpstreamClassAuth is the class of upstream authentication or
// authorization failures (HTTP 401 or 403).
const UpstreamClassAuth UpstreamCodeClass = "auth"

// UpstreamClassInvalidRequest is the class of upstream client errors
// other than auth and rate-limit (HTTP 4xx not 401/403/429).
const UpstreamClassInvalidRequest UpstreamCodeClass = "invalid_request"

// UpstreamClassSchemaViolation is the class of upstream client
// errors caused by malformed request bodies the adapter produced
// (e.g. missing_required_parameter on Reasoning items).
const UpstreamClassSchemaViolation UpstreamCodeClass = "schema_violation"

// UpstreamClassServerError is the class of upstream server-side
// failures (HTTP 5xx other than 502 network paths).
const UpstreamClassServerError UpstreamCodeClass = "server_error"

// UpstreamClassNetworkError is the class of network or IO failures
// reaching the upstream (DNS, TCP, TLS, or transport timeouts).
const UpstreamClassNetworkError UpstreamCodeClass = "network_error"

// UpstreamClassUnknown is the catch-all class for upstream failures
// that did not match a more specific class. Mappers route this to
// the family's safest fallback envelope.
const UpstreamClassUnknown UpstreamCodeClass = "unknown"

// ErrorRenderer renders a non-2xx HTTP response in a single route
// family's envelope shape. Implementations live in provider packages.
type ErrorRenderer interface {
	Render(w http.ResponseWriter, code int, info ErrorInfo) error
}

// StreamErrorWriter is the primitive SSE surface a stream error
// renderer needs after HTTP headers have already been committed.
// Implementations own framing and flushing, but not envelope shape.
type StreamErrorWriter interface {
	WriteStreamEvent(payload []byte) error
	WriteStreamDone() error
}

// StreamErrorRenderer writes a single route family's native error
// event on an already-open stream. Implementations live in provider
// packages and decide the JSON envelope shape for error events only.
type StreamErrorRenderer interface {
	WriteStreamError(w StreamErrorWriter, info ErrorInfo) error
}

// UpstreamMapping carries the typed result of classifying an upstream
// failure. The generic boundary uses HTTPStatus and ErrorInfo to
// drive the renderer and to populate the adapterError fields it
// already owns. Implementations live in provider packages.
type UpstreamMapping struct {
	HTTPStatus int
	Info       ErrorInfo
}

// UpstreamErrorMapper classifies an upstream failure into a family
// safe HTTP shape. Implementations live in provider packages and
// know the family's safe shape policy. Provider is the upstream
// label (e.g. "anthropic", "codex") that the mapper folds into
// fallback messages so the chat transcript carries enough diagnostic
// to reconstruct the failure.
type UpstreamErrorMapper interface {
	Map(provider string, status int, class UpstreamCodeClass, code, message string) UpstreamMapping
}

// RouteFamily names a route family at the boundary registration
// surface. The string values match the adapter package's
// adapterRouteFamily values so provider packages can register against
// the boundary without importing the adapter package.
type RouteFamily string

// RouteFamilyOpenAI is the OpenAI-compatible ingress family.
const RouteFamilyOpenAI RouteFamily = "openai"

// RouteFamilyAnthropic is the native Anthropic ingress family.
const RouteFamilyAnthropic RouteFamily = "anthropic"

// BoundaryRegistrar is the inversion seam the adapter error boundary
// exposes to provider packages. Each provider package's
// RegisterErrorBoundary function calls Register with its family's
// renderer and mapper at startup. The boundary holds these by family
// and dispatches to them when a non-2xx response must be rendered.
type BoundaryRegistrar interface {
	Register(family RouteFamily, mapper UpstreamErrorMapper, renderer ErrorRenderer)
	RegisterStreamErrorRenderer(family RouteFamily, renderer StreamErrorRenderer)
}
