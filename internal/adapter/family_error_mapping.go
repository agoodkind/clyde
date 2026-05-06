package adapter

import (
	"net/http"
	"strconv"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/adapter/errcontract"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
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

// defaultUpstreamMappers is the package-default registry of
// UpstreamErrorMapper implementations keyed by route family. Each
// mapper lives in its own provider package; the boundary holds only
// references through the errcontract interface so it never imports
// a provider envelope type. Server.New copies these into per-Server
// state; tests and any non-Server path use the defaults directly.
var defaultUpstreamMappers = defaultUpstreamMapperRegistry()

// defaultErrorRenderers is the package-default registry of
// ErrorRenderer implementations keyed by route family. Symmetric to
// defaultUpstreamMappers above.
var defaultErrorRenderers = defaultErrorRendererRegistry()

// defaultUpstreamMapperRegistry constructs a fresh map of the
// canonical per-family UpstreamErrorMapper implementations. The
// helper exists so Server.New stays under the funlen budget.
func defaultUpstreamMapperRegistry() map[adapterRouteFamily]errcontract.UpstreamErrorMapper {
	return map[adapterRouteFamily]errcontract.UpstreamErrorMapper{
		adapterRouteOpenAI:    adapteropenai.NewUpstreamErrorMapper(),
		adapterRouteAnthropic: anthropic.NewUpstreamErrorMapper(),
	}
}

// defaultErrorRendererRegistry constructs a fresh map of the
// canonical per-family ErrorRenderer implementations. Symmetric to
// defaultUpstreamMapperRegistry above.
func defaultErrorRendererRegistry() map[adapterRouteFamily]errcontract.ErrorRenderer {
	return map[adapterRouteFamily]errcontract.ErrorRenderer{
		adapterRouteOpenAI:    adapteropenai.NewErrorRenderer(),
		adapterRouteAnthropic: anthropic.NewErrorRenderer(),
	}
}

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
	mapper, ok := defaultUpstreamMappers[family]
	if !ok {
		return upstreamCatchAllAdapterError(provider, upstreamStatus, upstreamCode, upstreamMessage)
	}
	mapping := mapper.Map(provider, upstreamStatus, codeClass, upstreamCode, upstreamMessage)
	return adapterErrorFromMapping(family, provider, upstreamStatus, mapping)
}

// adapterErrorFromMapping folds a primitive UpstreamMapping returned
// by a provider mapper into the generic *adapterError. The boundary
// uses HTTPStatus and ErrorInfo verbatim, derives a coarse class for
// logs/metrics, and stitches in the AnthropicType only when the
// family is native Anthropic.
func adapterErrorFromMapping(family adapterRouteFamily, provider string, upstreamStatus int, mapping errcontract.UpstreamMapping) *adapterError {
	return &adapterError{
		Class:          classForMappedCode(mapping.Info.Code),
		HTTPStatus:     mapping.HTTPStatus,
		Message:        mapping.Info.Message,
		OpenAIType:     mapping.Info.Type,
		OpenAICode:     mapping.Info.Code,
		OpenAIParam:    mapping.Info.Param,
		AnthropicType:  anthropicEnvelopeTypeFromMapping(family, mapping.Info.Type),
		Provider:       provider,
		Backend:        "",
		ModelAlias:     "",
		ResolvedModel:  "",
		UpstreamStatus: upstreamStatus,
		Cause:          nil,
		SafeForClient:  true,
	}
}

// upstreamCatchAllAdapterError is the catch-all default when no
// family mapper is registered. The shape is the conservative
// invalid_request envelope with every known upstream field folded
// into error.message so the chat transcript carries the diagnostic.
func upstreamCatchAllAdapterError(provider string, upstreamStatus int, upstreamCode, upstreamMessage string) *adapterError {
	folded := upstreamFallbackMessage(provider, upstreamStatus, upstreamCode, upstreamMessage)
	return &adapterError{
		Class:          adapterErrorUpstreamFailed,
		HTTPStatus:     http.StatusBadRequest,
		Message:        folded,
		OpenAIType:     "invalid_request_error",
		OpenAICode:     "upstream_failed",
		OpenAIParam:    "",
		AnthropicType:  "api_error",
		Provider:       provider,
		Backend:        "",
		ModelAlias:     "",
		ResolvedModel:  "",
		UpstreamStatus: upstreamStatus,
		Cause:          nil,
		SafeForClient:  true,
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

// anthropicEnvelopeTypeFromMapping returns the AnthropicType field on
// adapterError when the mapping came from the Anthropic family. The
// generic boundary only ever uses AnthropicType for native ingress
// rendering; the OpenAI family path leaves the field empty so
// applyDefaults still picks up its row.
func anthropicEnvelopeTypeFromMapping(family adapterRouteFamily, infoType string) string {
	if family != adapterRouteAnthropic {
		return ""
	}
	return infoType
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
// up the family-correct envelope shape before rendering.
//
// For the OpenAI family the existing OpenAI envelope fields are
// flipped into the invalid_request shape when they would otherwise
// land on a non-renderable status. For the Anthropic family the
// spec-correct envelope is preserved verbatim.
func applyFamilyShape(family adapterRouteFamily, aerr *adapterError) *adapterError {
	if aerr == nil {
		return nil
	}
	if family != adapterRouteOpenAI {
		return aerr
	}
	if aerr.HTTPStatus == http.StatusBadRequest && aerr.OpenAIType == "invalid_request_error" {
		return aerr
	}
	if aerr.Class == adapterErrorInternal {
		return aerr
	}
	if aerr.Class == adapterErrorAuthFailed || aerr.Class == adapterErrorMethodNotAllowed {
		return aerr
	}
	folded := aerr.Message
	if folded == "" {
		folded = upstreamFallbackMessage(aerr.Provider, aerr.UpstreamStatus, aerr.OpenAICode, "")
	}
	aerr.HTTPStatus = http.StatusBadRequest
	aerr.OpenAIType = "invalid_request_error"
	if aerr.OpenAICode == "" || aerr.OpenAICode == "server_error" {
		aerr.OpenAICode = "upstream_failed"
	}
	aerr.Message = folded
	return aerr
}
