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

// openAITypeByClass maps the boundary's neutral error classes to the
// OpenAI family wire `error.type`. This map is the OpenAI package's
// own source of truth for the class-to-type translation; the generic
// adapter never names these wire strings. Classes absent from the map
// fall back to invalid_request_error, the Cursor-safe default.
//
// Every upstream_* class resolves to invalid_request_error because the
// OpenAI-compatible route family rule (documented in docs/cursor.md)
// requires every non-2xx upstream to surface as HTTP 400 +
// invalid_request_error + a typed upstream_* code: Cursor BYOK swaps
// in generic vendor fallback chrome on server_error/rate_limit_error
// and erases the chosen error.message. The OpenAI package owns that
// rule here, so deriving the wire type from the neutral class produces
// the same Cursor-safe shape the upstream mapper produces.
var openAITypeByClass = map[string]string{
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
	"upstream_auth_failed":      "invalid_request_error",
	"upstream_rate_limited":     "invalid_request_error",
	"upstream_schema_violation": "invalid_request_error",
	"upstream_network_error":    "invalid_request_error",
	"upstream_unavailable":      "invalid_request_error",
	"upstream_failed":           "invalid_request_error",
	"baseline_missing":          "invalid_request_error",
	"timeout":                   "invalid_request_error",
	"canceled":                  "invalid_request_error",
	"internal":                  "internal_error",
}

// openAITypeForClass derives the OpenAI wire `error.type` from the
// boundary's neutral class. Unknown classes resolve to the Cursor-safe
// invalid_request_error default so an unseen class can never escape as
// a server_error or rate_limit_error that triggers Cursor BYOK chrome.
func openAITypeForClass(class string) string {
	if t, ok := openAITypeByClass[strings.TrimSpace(class)]; ok {
		return t
	}
	return "invalid_request_error"
}

// documentedTypeByStatus maps a documented OpenAI HTTP status to the
// error.type the OpenAI API returns with it. The error code guide
// documents invalid_request_error for 400, rate_limit_error for 429, and
// service_unavailable_error for 503. The remaining entries follow the
// status classes the official SDKs raise.
var documentedTypeByStatus = map[int]string{
	http.StatusBadRequest:          "invalid_request_error",
	http.StatusUnauthorized:        "authentication_error",
	http.StatusForbidden:           "permission_error",
	http.StatusNotFound:            "invalid_request_error",
	http.StatusMethodNotAllowed:    "invalid_request_error",
	http.StatusConflict:            "invalid_request_error",
	http.StatusUnprocessableEntity: "invalid_request_error",
	http.StatusTooManyRequests:     "rate_limit_error",
	http.StatusServiceUnavailable:  "service_unavailable_error",
}

// documentedTypeForStatus derives the documented OpenAI error.type from
// the HTTP status. Other client errors are invalid_request_error, and
// other server errors are server_error.
func documentedTypeForStatus(status int) string {
	if envelopeType, ok := documentedTypeByStatus[status]; ok {
		return envelopeType
	}
	if status >= http.StatusInternalServerError {
		return "server_error"
	}
	return "invalid_request_error"
}

// envelopeTypeFor selects the wire error.type. The documented contract
// derives it from the status. The compatibility contract prefers an
// explicit Type and otherwise derives it from the neutral Class.
func envelopeTypeFor(status int, info errcontract.ErrorInfo) string {
	if info.Contract == errcontract.ClientContractDocumented {
		return documentedTypeForStatus(status)
	}
	if info.Type != "" {
		return info.Type
	}
	return openAITypeForClass(info.Class)
}

// Render serializes a canonical OpenAI error envelope. The envelope type
// comes from envelopeTypeFor. An empty Code defaults to the envelope
// type, which gives Cursor's BYOK error renderer a code to dispatch on.
// An encoding failure writes a deterministic constant envelope instead.
func (ErrorRenderer) Render(w http.ResponseWriter, code int, info errcontract.ErrorInfo) error {
	envelopeType := envelopeTypeFor(code, info)
	body := ErrorBody{
		Message: info.Message,
		Type:    envelopeType,
		Code:    info.Code,
		Param:   info.Param,
		Clyde:   info.Diagnostics,
	}
	if body.Code == "" {
		body.Code = body.Type
	}
	log := slog.Default()
	payload, err := json.Marshal(ErrorResponse{Error: body})
	if err != nil {
		const fallback = `{"error":{"message":"failed to encode error envelope","type":"internal_error","code":"internal_error"}}`
		writeErr := erroring.WriteJSONStatus(w, http.StatusInternalServerError, []byte(fallback))
		log.Warn("adapter.openai_error_envelope.render_failed", "concern", "adapter.http.errors", "event", "marshal_failed",
			"err", err.Error(),
		)
		if writeErr != nil {
			return fmt.Errorf("write openai error fallback: %w", writeErr)
		}
		return fmt.Errorf("marshal openai error envelope: %w", err)
	}
	if writeErr := erroring.WriteJSONStatus(w, code, payload); writeErr != nil {
		log.Warn("adapter.openai_error_envelope.render_failed", "concern", "adapter.http.errors", "event", "write_failed",
			"err", writeErr.Error(),
		)
		return fmt.Errorf("write openai error envelope: %w", writeErr)
	}
	return nil
}

// UpstreamErrorMapper implements the OpenAI family compatibility shape.
// Cursor BYOK never renders HTTP 5xx + server_error correctly, and
// HTTP 429 + rate_limit_error triggers Cursor's generic BYOK chrome
// instead of the upstream message. Both show opaque UI text that hides
// the real diagnostic. The mapper therefore maps every non-2xx upstream
// to HTTP 400 + invalid_request_error + a typed upstream_* code. The
// boundary keeps that shape on the Cursor BYOK listener. On the generic
// OpenAI listener the boundary restores the documented status from the
// preserved upstream status, and the renderer derives the documented
// error.type from it.
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
			Type:           "invalid_request_error",
			Class:          "",
			Code:           code,
			Message:        message,
			Param:          "",
			UpstreamStatus: 0,
			Diagnostics:    nil,
			Contract:       errcontract.ClientContractCompatibility,
			Status:         http.StatusBadRequest,
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
