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

// Render serializes a spec-correct Anthropic error envelope. Falls
// back to a deterministic constant envelope when the encoder fails
// so the response writer never sees a plain-text body.
func (ErrorRenderer) Render(w http.ResponseWriter, code int, info errcontract.ErrorInfo) error {
	env := ErrorEnvelope{
		Type: "error",
		Error: ErrorDetail{
			Type:    info.Type,
			Message: info.Message,
		},
	}
	log := slog.Default()
	payload, err := json.Marshal(env)
	if err != nil {
		const fallback = `{"type":"error","error":{"type":"api_error","message":"failed to encode error envelope"}}`
		writeErr := erroring.WriteJSONStatus(w, http.StatusInternalServerError, []byte(fallback))
		log.Warn("adapter.anthropic_error_envelope.render_failed",
			"event", "marshal_failed",
			"err", err.Error(),
		)
		if writeErr != nil {
			return fmt.Errorf("write anthropic error fallback: %w", writeErr)
		}
		return fmt.Errorf("marshal anthropic error envelope: %w", err)
	}
	if writeErr := erroring.WriteJSONStatus(w, code, payload); writeErr != nil {
		log.Warn("adapter.anthropic_error_envelope.render_failed",
			"event", "write_failed",
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
			Type:    envelopeType,
			Code:    trimmedCode,
			Message: resolvedMessage,
			Param:   "",
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
