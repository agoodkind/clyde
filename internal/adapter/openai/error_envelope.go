package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"goodkind.io/clyde/internal/adapter/errcontract"
	"goodkind.io/clyde/internal/adapter/internal/erroring"
)

// ErrorRenderer renders the OpenAI family error envelope. The Cursor
// BYOK error renderer dispatches on `error.type` plus `error.code`,
// so the constant `invalid_request_error` type is what keeps Cursor
// off its generic rate-limit fallback chrome and on a renderer that
// surfaces our chosen `error.message` verbatim. The renderer takes
// primitive ErrorInfo from the boundary and constructs the
// envelope literal here, in the provider package, so the boundary
// never imports ErrorBody or ErrorResponse.
type ErrorRenderer struct{}

// NewErrorRenderer returns the canonical OpenAI ErrorRenderer.
func NewErrorRenderer() ErrorRenderer { return ErrorRenderer{} }

// Render serializes a canonical OpenAI error envelope. Code defaults
// to Type when empty so Cursor's BYOK error renderer always has
// something to dispatch on. Encoding failure falls back to a
// deterministic constant envelope so the response writer is never
// left dangling without a body.
func (ErrorRenderer) Render(w http.ResponseWriter, code int, info errcontract.ErrorInfo) error {
	body := ErrorBody{
		Message: info.Message,
		Type:    info.Type,
		Code:    info.Code,
		Param:   info.Param,
	}
	body.Clyde = info.Diagnostics
	if body.Code == "" {
		body.Code = body.Type
	}
	log := slog.Default()
	payload, err := json.Marshal(ErrorResponse{Error: body})
	if err != nil {
		const fallback = `{"error":{"message":"failed to encode error envelope","type":"internal_error","code":"internal_error"}}`
		writeErr := erroring.WriteJSONStatus(w, http.StatusInternalServerError, []byte(fallback))
		log.Warn("adapter.openai_error_envelope.render_failed",
			"event", "marshal_failed",
			"err", err.Error(),
		)
		if writeErr != nil {
			return fmt.Errorf("write openai error fallback: %w", writeErr)
		}
		return fmt.Errorf("marshal openai error envelope: %w", err)
	}
	if writeErr := erroring.WriteJSONStatus(w, code, payload); writeErr != nil {
		log.Warn("adapter.openai_error_envelope.render_failed",
			"event", "write_failed",
			"err", writeErr.Error(),
		)
		return fmt.Errorf("write openai error envelope: %w", writeErr)
	}
	return nil
}

// UpstreamErrorMapper carries the OpenAI family safe-shape policy.
// Cursor BYOK never renders HTTP 5xx + server_error correctly, and
// HTTP 429 + rate_limit_error triggers Cursor's generic BYOK chrome
// rather than the upstream message; both fall back to opaque UI text
// that hides the real diagnostic. Cursor BYOK is the canonical
// client today, but Continue, Aider, and raw curl all hit the same
// surface, so the rule must be true regardless of which OpenAI
// family client connects: every non-2xx upstream maps to HTTP 400 +
// invalid_request_error + a typed upstream_* code so the chosen
// error.message renders verbatim in the chat transcript.
type UpstreamErrorMapper struct{}

// NewUpstreamErrorMapper returns the canonical OpenAI mapper.
func NewUpstreamErrorMapper() UpstreamErrorMapper { return UpstreamErrorMapper{} }

// RegisterErrorBoundary plugs the OpenAI family renderer and mapper
// into the adapter's error boundary through the inversion seam in
// errcontract. The boundary file holds no provider import; callers
// invoke this from the composition root so wiring is explicit and
// the family-to-implementation map is owned outside the boundary.
func RegisterErrorBoundary(reg errcontract.BoundaryRegistrar) {
	reg.Register(errcontract.RouteFamilyOpenAI, NewUpstreamErrorMapper(), NewErrorRenderer())
	reg.RegisterStreamErrorRenderer(errcontract.RouteFamilyOpenAI, NewStreamErrorRenderer())
}

// Map classifies an upstream failure into the OpenAI family safe
// shape. Return value is primitive (HTTPStatus + ErrorInfo) so the
// boundary never imports an OpenAI envelope type.
func (UpstreamErrorMapper) Map(
	provider string,
	status int,
	class errcontract.UpstreamCodeClass,
	code, message string,
) errcontract.UpstreamMapping {
	trimmedProvider := strings.TrimSpace(provider)
	trimmedMsg := strings.TrimSpace(message)
	trimmedCode := strings.TrimSpace(code)
	resolvedMessage := trimmedMsg
	if resolvedMessage == "" {
		resolvedMessage = upstreamFallbackMessage(trimmedProvider, status, trimmedCode, trimmedMsg)
	}
	switch class {
	case errcontract.UpstreamClassRateLimit:
		return openAIInvalidRequestMapping("upstream_rate_limited", resolvedMessage)
	case errcontract.UpstreamClassAuth:
		return openAIInvalidRequestMapping("upstream_auth_failed", resolvedMessage)
	case errcontract.UpstreamClassSchemaViolation:
		return openAIInvalidRequestMapping("upstream_malformed_request", resolvedMessage)
	case errcontract.UpstreamClassNetworkError:
		return openAIInvalidRequestMapping("upstream_network_error", resolvedMessage)
	case errcontract.UpstreamClassInvalidRequest:
		return openAIInvalidRequestMapping("invalid_request", resolvedMessage)
	case errcontract.UpstreamClassServerError:
		return openAIInvalidRequestMapping("upstream_failed", resolvedMessage)
	case errcontract.UpstreamClassUnknown:
		fallthrough
	default:
		folded := upstreamFallbackMessage(trimmedProvider, status, trimmedCode, trimmedMsg)
		return openAIInvalidRequestMapping("upstream_failed", folded)
	}
}

// openAIInvalidRequestMapping returns a Cursor-safe invalid-request
// mapping with the provided code and message. The HTTPStatus is
// pinned to 400 so Cursor BYOK renders error.message verbatim
// instead of falling back to its generic chrome.
func openAIInvalidRequestMapping(code, message string) errcontract.UpstreamMapping {
	return errcontract.UpstreamMapping{
		HTTPStatus: http.StatusBadRequest,
		Info: errcontract.ErrorInfo{
			Type:    "invalid_request_error",
			Code:    code,
			Message: message,
			Param:   "",
		},
	}
}

// upstreamFallbackMessage folds every available upstream field into a
// single string so callers that lack an upstream message body still
// surface a useful diagnostic to the chat transcript. Provider is
// passed empty here because the boundary holds the provider name and
// stitches it into adapterError.Provider separately.
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
