package adapter

import (
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"goodkind.io/clyde/internal/adapter/errcontract"
)

// upstreamCodeClass aliases the contract type so existing in-package
// callers (anthropic_provider_dispatch.go, codex_provider_dispatch.go,
// anthropic_native.go, server_passthrough_override.go) keep their
// compact local references while the boundary speaks only the
// contract enum across package edges.
type upstreamCodeClass = errcontract.UpstreamCodeClass

const (
	upstreamClassRateLimit       = errcontract.UpstreamClassRateLimit
	upstreamClassAuth            = errcontract.UpstreamClassAuth
	upstreamClassInvalidRequest  = errcontract.UpstreamClassInvalidRequest
	upstreamClassSchemaViolation = errcontract.UpstreamClassSchemaViolation
	upstreamClassServerError     = errcontract.UpstreamClassServerError
	upstreamClassNetworkError    = errcontract.UpstreamClassNetworkError
	upstreamClassUnknown         = errcontract.UpstreamClassUnknown
)

// boundaryRegistry holds the typed, per-family registries the boundary
// dispatches against. The zero value is usable; reads and writes are
// guarded by mu so init-time RegisterErrorBoundary calls are safe even
// if a future generation runs them concurrently. Provider packages
// populate this registry exactly once at process startup through their
// RegisterErrorBoundary hooks; the boundary file owns no provider
// import and constructs no provider envelope.
type boundaryRegistry struct {
	mu                   sync.RWMutex
	mappers              map[adapterRouteFamily]errcontract.UpstreamErrorMapper
	renderers            map[adapterRouteFamily]errcontract.ErrorRenderer
	streamErrorRenderers map[adapterRouteFamily]errcontract.StreamErrorRenderer
}

func newBoundaryRegistry() *boundaryRegistry {
	return &boundaryRegistry{
		mu:                   sync.RWMutex{},
		mappers:              map[adapterRouteFamily]errcontract.UpstreamErrorMapper{},
		renderers:            map[adapterRouteFamily]errcontract.ErrorRenderer{},
		streamErrorRenderers: map[adapterRouteFamily]errcontract.StreamErrorRenderer{},
	}
}

// Register stores the mapper and renderer for a family. Implements
// errcontract.BoundaryRegistrar so provider packages can call
// Register without importing the adapter package's family enum.
func (r *boundaryRegistry) Register(family errcontract.RouteFamily, mapper errcontract.UpstreamErrorMapper, renderer errcontract.ErrorRenderer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := adapterRouteFamily(family)
	if mapper != nil {
		r.mappers[key] = mapper
	}
	if renderer != nil {
		r.renderers[key] = renderer
	}
}

// RegisterStreamErrorRenderer stores the native stream error renderer
// for a family. It intentionally covers error events only; normal
// success stream chunks stay on their existing renderer path.
func (r *boundaryRegistry) RegisterStreamErrorRenderer(family errcontract.RouteFamily, renderer errcontract.StreamErrorRenderer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if renderer != nil {
		r.streamErrorRenderers[adapterRouteFamily(family)] = renderer
	}
}

// upstreamMapper returns the registered mapper for a family.
func (r *boundaryRegistry) upstreamMapper(family adapterRouteFamily) (errcontract.UpstreamErrorMapper, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.mappers[family]
	return m, ok
}

// errorRenderer returns the registered renderer for a family.
func (r *boundaryRegistry) errorRenderer(family adapterRouteFamily) (errcontract.ErrorRenderer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rd, ok := r.renderers[family]
	return rd, ok
}

// streamErrorRenderer returns the registered stream error renderer
// for a family.
func (r *boundaryRegistry) streamErrorRenderer(family adapterRouteFamily) (errcontract.StreamErrorRenderer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rd, ok := r.streamErrorRenderers[family]
	return rd, ok
}

// snapshotRenderers returns a copy of the renderer map so a Server can
// own a per-instance registry that is decoupled from later mutations.
func (r *boundaryRegistry) snapshotRenderers() map[adapterRouteFamily]errcontract.ErrorRenderer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[adapterRouteFamily]errcontract.ErrorRenderer, len(r.renderers))
	maps.Copy(out, r.renderers)
	return out
}

// snapshotStreamErrorRenderers returns a copy of the stream error
// renderer map so a Server can own per-instance boundary wiring.
func (r *boundaryRegistry) snapshotStreamErrorRenderers() map[adapterRouteFamily]errcontract.StreamErrorRenderer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[adapterRouteFamily]errcontract.StreamErrorRenderer, len(r.streamErrorRenderers))
	maps.Copy(out, r.streamErrorRenderers)
	return out
}

// defaultBoundaryRegistry is the package-level registry every
// non-Server caller (tests, free functions like
// codexProviderAdapterError) reads from. Provider packages register
// into it from internal/adapter/error_boundary_registration.go at
// init() time. The boundary file (this file) holds no provider
// import; the registration file is the only place under
// internal/adapter/*.go (depth 1) that imports a provider package
// for boundary wiring.
var defaultBoundaryRegistry = newBoundaryRegistry()

// mapUpstreamForFamily classifies an upstream failure into the
// route-family-correct adapterError. It looks up the registered
// UpstreamErrorMapper for the family and folds the primitive
// UpstreamMapping it returns into a generic *adapterError. The
// boundary holds no per-family policy; every shape decision lives in
// the provider package's mapper.
//
// The catch-all when no mapper is registered for the family is
// HTTP 400 + invalid_request_error + upstream_failed with every known
// field folded into error.message so the operator can reconstruct
// the failure from the chat transcript without grepping daemon logs.
func mapUpstreamForFamily(
	family adapterRouteFamily,
	provider string,
	upstreamStatus int,
	codeClass upstreamCodeClass,
	upstreamCode string,
	upstreamMessage string,
) *adapterError {
	provider = strings.TrimSpace(provider)
	upstreamCode = strings.TrimSpace(upstreamCode)
	upstreamMessage = strings.TrimSpace(upstreamMessage)
	mapper, ok := defaultBoundaryRegistry.upstreamMapper(family)
	if !ok {
		return upstreamCatchAllAdapterError(provider, upstreamStatus, upstreamCode, upstreamMessage)
	}
	mapping := mapper.Map(provider, upstreamStatus, codeClass, upstreamCode, upstreamMessage)
	return adapterErrorFromMapping(family, provider, upstreamStatus, mapping)
}

// adapterErrorFromMapping folds a primitive UpstreamMapping returned
// by a provider mapper into the generic *adapterError. The boundary
// uses HTTPStatus and the neutral Code/Param/Message verbatim and
// derives a coarse Class for logs, metrics, and the renderer's
// class-to-wire-type translation. The mapper already chose the
// family-correct envelope wire type, but the generic adapterError no
// longer carries a provider wire type: each family renderer re-derives
// its own envelope `error.type` from the neutral Class, so the wire
// string never lives on the generic struct.
func adapterErrorFromMapping(_ adapterRouteFamily, provider string, upstreamStatus int, mapping errcontract.UpstreamMapping) *adapterError {
	return &adapterError{
		Class:             classForMappedCode(mapping.Info.Code),
		HTTPStatus:        mapping.HTTPStatus,
		Message:           mapping.Info.Message,
		Code:              mapping.Info.Code,
		Param:             mapping.Info.Param,
		Provider:          provider,
		Backend:           "",
		ModelAlias:        "",
		ResolvedModelName: "",
		UpstreamStatus:    upstreamStatus,
		Cause:             nil,
		SafeForClient:     true,
	}
}

// upstreamCatchAllAdapterError is the catch-all default when no
// family mapper is registered. The shape is the conservative
// invalid_request envelope with every known upstream field folded
// into error.message so the chat transcript carries the diagnostic.
func upstreamCatchAllAdapterError(provider string, upstreamStatus int, upstreamCode, upstreamMessage string) *adapterError {
	folded := upstreamFallbackMessage(provider, upstreamStatus, upstreamCode, upstreamMessage)
	return &adapterError{
		Class:             adapterErrorUpstreamFailed,
		HTTPStatus:        http.StatusBadRequest,
		Message:           folded,
		Code:              "upstream_failed",
		Param:             "",
		Provider:          provider,
		Backend:           "",
		ModelAlias:        "",
		ResolvedModelName: "",
		UpstreamStatus:    upstreamStatus,
		Cause:             nil,
		SafeForClient:     true,
	}
}

// adapterErrorClassByMappedCode is the lookup table from a mapper's
// chosen Code to the adapterErrorClass logs and metrics use as a
// class dimension. The mapping is informational only; HTTPStatus and
// envelope fields come straight from the mapper.
var adapterErrorClassByMappedCode = map[string]adapterErrorClass{
	"upstream_rate_limited":      adapterErrorUpstreamRateLimited,
	"upstream_auth_failed":       adapterErrorUpstreamAuthFailed,
	"upstream_malformed_request": adapterErrorUpstreamSchemaViolation,
	"upstream_network_error":     adapterErrorUpstreamNetworkError,
	"upstream_unavailable":       adapterErrorUpstreamUnavailable,
	"invalid_request":            adapterErrorInvalidRequest,
	"rate_limit_exceeded":        adapterErrorRateLimited,
	"upstream_failed":            adapterErrorUpstreamFailed,
}

// classForMappedCode picks an adapterErrorClass from the mapper's
// chosen Code, falling back to upstream_failed when the code is not
// in the lookup table.
func classForMappedCode(code string) adapterErrorClass {
	if class, ok := adapterErrorClassByMappedCode[strings.TrimSpace(code)]; ok {
		return class
	}
	return adapterErrorUpstreamFailed
}

// upstreamFallbackMessage folds every available upstream field into a
// single string. The boundary uses this for the catch-all and for
// adapterError instances built outside the mapper path.
func upstreamFallbackMessage(provider string, upstreamStatus int, upstreamCode, upstreamMessage string) string {
	parts := []string{}
	if provider != "" {
		parts = append(parts, "provider="+provider)
	}
	if upstreamStatus > 0 {
		parts = append(parts, "upstream_status="+strconv.Itoa(upstreamStatus))
	}
	if upstreamCode != "" {
		parts = append(parts, "upstream_code="+upstreamCode)
	}
	if upstreamMessage != "" {
		parts = append(parts, "upstream_message="+upstreamMessage)
	}
	if len(parts) == 0 {
		return "upstream call failed without diagnostic detail"
	}
	return "upstream call failed: " + strings.Join(parts, " ")
}

// applyFamilyShape coerces an adapterError that arrived without
// going through mapUpstreamForFamily into the family's safe shape.
// The boundary calls into the registered renderer to write the
// final response; this helper exists so adapterErrors built directly
// (panics, validation failures, internal-only flows) can still pick
// up the family-correct shape before rendering.
//
// The helper speaks only neutral fields: HTTPStatus, Class, Code, and
// Message. Each family renderer derives its own envelope `error.type`
// from the neutral Class, so this helper never names a provider wire
// type. For the OpenAI family it pins the status to 400 and the code
// to a typed upstream_* value when an upstream class would otherwise
// land on a non-renderable status, so the renderer's class-to-type
// map produces invalid_request_error per the OpenAI route family rule.
// For the Anthropic family the spec-correct shape is preserved
// verbatim.
func applyFamilyShape(family adapterRouteFamily, aerr *adapterError) *adapterError {
	if aerr == nil {
		return nil
	}
	if family != adapterRouteOpenAI {
		return aerr
	}
	// auth_failed, method_not_allowed, and internal are local
	// adapter-side rejections, not upstream failures; their status and
	// renderer-derived envelope type are already client-correct, so the
	// Cursor upstream rule does not apply to them.
	if aerr.Class == adapterErrorInternal {
		return aerr
	}
	if aerr.Class == adapterErrorAuthFailed || aerr.Class == adapterErrorMethodNotAllowed {
		return aerr
	}
	// baseline_missing is a local configuration failure: the daemon-owned
	// MITM wire baseline is absent or invalid, so there is no upstream
	// call to fold. Its HTTP 503 and operator-actionable message must
	// survive to the client unchanged rather than be flipped to the
	// Cursor-safe upstream shape.
	if aerr.Class == adapterErrorBaselineMissing {
		return aerr
	}
	// Anything already on HTTP 400 whose class is not the rate-limit
	// class renders as invalid_request_error under the OpenAI renderer's
	// class-to-type map, so it is already Cursor-safe. The rate-limit
	// class maps to rate_limit_error and must be flipped even at 400.
	if aerr.HTTPStatus == http.StatusBadRequest && aerr.Class != adapterErrorRateLimited {
		return aerr
	}
	folded := aerr.Message
	if folded == "" {
		folded = upstreamFallbackMessage(aerr.Provider, aerr.UpstreamStatus, aerr.Code, "")
	}
	aerr.HTTPStatus = http.StatusBadRequest
	// Normalize the neutral class to upstream_failed so the renderer
	// derives invalid_request_error. An already-typed upstream_* class
	// is preserved so its specific upstream_* code survives.
	if !strings.HasPrefix(string(aerr.Class), "upstream_") {
		aerr.Class = adapterErrorUpstreamFailed
	}
	if aerr.Code == "" || aerr.Code == "server_error" {
		aerr.Code = "upstream_failed"
	}
	aerr.Message = folded
	return aerr
}
