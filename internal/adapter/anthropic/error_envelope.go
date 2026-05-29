package anthropic

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/adapter/errcontract"
	"goodkind.io/clyde/internal/adapter/internal/erroring"
)

// ErrorRenderer renders the spec-correct Anthropic error envelope.
// Native Anthropic ingress (claude-cli) parses this shape verbatim,
// so the Cursor-style remap to invalid_request_error must not apply
// here. The renderer takes primitive ErrorInfo from the boundary and
// constructs the envelope literal in the provider package so the
// boundary never imports ErrorEnvelope or ErrorDetail.
type ErrorRenderer struct{}

// NewErrorRenderer returns the canonical Anthropic ErrorRenderer.
func NewErrorRenderer() ErrorRenderer { return ErrorRenderer{} }

// anthropicTypeByClass maps the boundary's neutral error classes to
// the spec-correct Anthropic family wire `error.type`. This map is the
// Anthropic package's own source of truth for the class-to-type
// translation; the generic adapter never names these wire strings.
// Classes absent from the map fall back to api_error, the Anthropic
// catch-all envelope type.
var anthropicTypeByClass = map[string]string{
	"auth_failed":               "authentication_error",
	"method_not_allowed":        "invalid_request_error",
	"invalid_json":              "invalid_request_error",
	"invalid_request":           "invalid_request_error",
	"model_not_found":           "invalid_request_error",
	"model_not_supported":       "invalid_request_error",
	"unsupported_backend":       "invalid_request_error",
	"unsupported_content":       "invalid_request_error",
	"context_length_exceeded":   "invalid_request_error",
	"rate_limited":              "rate_limit_error",
	"upstream_auth_failed":      "authentication_error",
	"upstream_rate_limited":     "rate_limit_error",
	"upstream_schema_violation": "invalid_request_error",
	"upstream_network_error":    "api_error",
	"upstream_unavailable":      "api_error",
	"upstream_failed":           "api_error",
	"baseline_missing":          "api_error",
	"timeout":                   "api_error",
	"canceled":                  "api_error",
	"internal":                  "api_error",
}

// anthropicTypeForClass derives the spec-correct Anthropic wire
// `error.type` from the boundary's neutral class. Unknown classes
// resolve to api_error, the Anthropic catch-all envelope type.
func anthropicTypeForClass(class string) string {
	if t, ok := anthropicTypeByClass[strings.TrimSpace(class)]; ok {
		return t
	}
	return "api_error"
}

// anthropicTypeForInfo derives the spec-correct Anthropic wire
// `error.type` from the neutral ErrorInfo when the boundary did not
// pre-fill a type. The native Anthropic ingress emits a
// not_supported_error envelope for unimplemented endpoints (e.g.
// /v1/messages/count_tokens), carried on the neutral Code. Upstream
// failures carry their HTTP status on the neutral UpstreamStatus, and
// the Anthropic spec maps 401/403 to authentication_error and 429 to
// rate_limit_error, so this derivation reproduces the status-correct
// envelope without the generic adapter ever naming a wire type.
func anthropicTypeForInfo(info errcontract.ErrorInfo) string {
	if strings.TrimSpace(info.Code) == "not_supported_error" {
		return "not_supported_error"
	}
	switch info.UpstreamStatus {
	case http.StatusBadRequest, http.StatusMethodNotAllowed:
		return "invalid_request_error"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	return anthropicTypeForClass(info.Class)
}

// Render serializes a spec-correct Anthropic error envelope. The
// envelope Type is derived from the neutral Class when the boundary
// did not already pick one (the upstream-mapper path fills Type
// directly). Falls back to a deterministic constant envelope when the
// encoder fails so the response writer never sees a plain-text body.
func (ErrorRenderer) Render(w http.ResponseWriter, code int, info errcontract.ErrorInfo) error {
	envelopeType := info.Type
	if envelopeType == "" {
		envelopeType = anthropicTypeForInfo(info)
	}
	env := ErrorEnvelope{
		Type: "error",
		Error: ErrorDetail{
			Type:    envelopeType,
			Message: info.Message,
		},
	}
	log := slog.Default()
	payload, err := json.Marshal(env)
	if err != nil {
		const fallback = `{"type":"error","error":{"type":"api_error","message":"failed to encode error envelope"}}`
		writeErr := erroring.WriteJSONStatus(w, http.StatusInternalServerError, []byte(fallback))
		log.Warn("adapter.anthropic_error_envelope.render_failed", "concern", "adapter.providers.anthropic.errors", "event", "marshal_failed",
			"err", err.Error(),
		)
		if writeErr != nil {
			return fmt.Errorf("write anthropic error fallback: %w", writeErr)
		}
		return fmt.Errorf("marshal anthropic error envelope: %w", err)
	}
	if writeErr := erroring.WriteJSONStatus(w, code, payload); writeErr != nil {
		log.Warn("adapter.anthropic_error_envelope.render_failed", "concern", "adapter.providers.anthropic.errors", "event", "write_failed",
			"err", writeErr.Error(),
		)
		return fmt.Errorf("write anthropic error envelope: %w", writeErr)
	}
	return nil
}

// UpstreamErrorMapper preserves the spec-correct Anthropic envelope
// for upstream failures. claude-cli reads native Anthropic shapes and
// would break under any Cursor-style remap, so the mapper preserves
// upstream HTTP status and picks an Anthropic envelope type matching
// the status family.
type UpstreamErrorMapper struct{}

// NewUpstreamErrorMapper returns the canonical Anthropic mapper.
func NewUpstreamErrorMapper() UpstreamErrorMapper { return UpstreamErrorMapper{} }

// RegisterErrorBoundary plugs the Anthropic family renderer and
// mapper into the adapter's error boundary through the inversion seam
// in errcontract. The boundary file holds no provider import; callers
// invoke this from the composition root so wiring is explicit and the
// family-to-implementation map is owned outside the boundary.
func RegisterErrorBoundary(reg errcontract.BoundaryRegistrar) {
	reg.Register(errcontract.RouteFamilyAnthropic, NewUpstreamErrorMapper(), NewErrorRenderer())
}

// Map classifies an upstream failure into the Anthropic family safe
// shape. Returns primitives so the boundary never imports an
// Anthropic envelope type.
func (UpstreamErrorMapper) Map(
	provider string,
	status int,
	class errcontract.UpstreamCodeClass,
	code, message string,
) errcontract.UpstreamMapping {
	_ = provider // anthropic envelope folds upstream context into message via Type/Code
	trimmedMsg := strings.TrimSpace(message)
	trimmedCode := strings.TrimSpace(code)
	resolvedMessage := trimmedMsg
	if resolvedMessage == "" {
		resolvedMessage = anthropicFallbackMessage(status, trimmedCode, trimmedMsg)
	}
	httpStatus := status
	if httpStatus <= 0 {
		httpStatus = mapClassToStatus(class)
	}
	envelopeType := anthropicErrorTypeForClass(httpStatus, trimmedCode, class)
	return errcontract.UpstreamMapping{
		HTTPStatus: httpStatus,
		Info: errcontract.ErrorInfo{
			Type:           envelopeType,
			Class:          "",
			Code:           trimmedCode,
			Message:        resolvedMessage,
			Param:          "",
			UpstreamStatus: httpStatus,
			Diagnostics:    nil,
		},
	}
}

// mapClassToStatus picks a default HTTP status when the upstream did
// not surface one. The defaults match the Anthropic spec mapping
// between class and HTTP status.
func mapClassToStatus(class errcontract.UpstreamCodeClass) int {
	switch class {
	case errcontract.UpstreamClassRateLimit:
		return http.StatusTooManyRequests
	case errcontract.UpstreamClassAuth:
		return http.StatusUnauthorized
	case errcontract.UpstreamClassInvalidRequest, errcontract.UpstreamClassSchemaViolation:
		return http.StatusBadRequest
	case errcontract.UpstreamClassServerError, errcontract.UpstreamClassNetworkError:
		return http.StatusBadGateway
	case errcontract.UpstreamClassUnknown:
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
}

// anthropicErrorTypeForClass picks the spec-correct Anthropic
// envelope type for an upstream status, code, and class. Code wins
// when it is a known Anthropic envelope type so passthrough preserves
// the upstream classification verbatim.
func anthropicErrorTypeForClass(status int, code string, class errcontract.UpstreamCodeClass) string {
	if strings.TrimSpace(code) == "not_supported_error" {
		return "not_supported_error"
	}
	switch status {
	case http.StatusBadRequest, http.StatusMethodNotAllowed:
		return "invalid_request_error"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	switch class {
	case errcontract.UpstreamClassRateLimit:
		return "rate_limit_error"
	case errcontract.UpstreamClassAuth:
		return "authentication_error"
	case errcontract.UpstreamClassInvalidRequest, errcontract.UpstreamClassSchemaViolation:
		return "invalid_request_error"
	case errcontract.UpstreamClassServerError, errcontract.UpstreamClassNetworkError, errcontract.UpstreamClassUnknown:
		return "api_error"
	}
	return "api_error"
}

func anthropicFallbackMessage(status int, code, message string) string {
	parts := []string{}
	if status > 0 {
		parts = append(parts, fmt.Sprintf("upstream_status=%d", status))
	}
	if code != "" {
		parts = append(parts, "upstream_code="+code)
	}
	if message != "" {
		parts = append(parts, "upstream_message="+message)
	}
	if len(parts) == 0 {
		return "upstream call failed without diagnostic detail"
	}
	return "upstream call failed: " + strings.Join(parts, " ")
}
