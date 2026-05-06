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
type ErrorInfo struct {
	Type    string
	Code    string
	Message string
	Param   string
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

// StreamErrorRenderer renders a single route family's native error
// event on an already-open stream. Implementations live in provider
// packages and decide the JSON envelope shape for error events only.
type StreamErrorRenderer interface {
	RenderStreamError(w StreamErrorWriter, info ErrorInfo) error
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
