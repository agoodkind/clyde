package antigravity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/errcontract"
)

// UpstreamError records the status, classification, and retry instructions
// from an Antigravity HTTP error.
type UpstreamError struct {
	Status     int
	Code       string
	Message    string
	Class      errcontract.UpstreamCodeClass
	Retryable  bool
	RetryAfter time.Duration
}

// Error returns the upstream status and message.
//
// The message can include an account validation link.
//
//	message := upstreamErr.Error()
func (upstream *UpstreamError) Error() string {
	return fmt.Sprintf("antigravity HTTP %d: %s", upstream.Status, upstream.Message)
}

type errorEnvelope struct {
	Error errorStatus `json:"error"`
}

type errorStatus struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Status  string        `json:"status"`
	Details []errorDetail `json:"details"`
}

type errorDetail struct {
	Type       string           `json:"@type"`
	Reason     errorReason      `json:"reason"`
	RetryDelay string           `json:"retryDelay"`
	Metadata   errorMetadata    `json:"metadata"`
	Violations []quotaViolation `json:"violations"`
}

type errorMetadata struct {
	QuotaResetDelay string `json:"quotaResetDelay"`
	QuotaID         string `json:"quotaId"`
	QuotaIDSnake    string `json:"quota_id"`
	ValidationLink  string `json:"validation_link"`
}

type quotaViolation struct {
	QuotaID string `json:"quotaId"`
}

// ParseUpstreamError reads an Antigravity error envelope.
//
// A malformed or missing envelope falls back to the HTTP status without
// copying the raw response body into the error.
//
//	upstreamErr := ParseUpstreamError(response.StatusCode, body)
func ParseUpstreamError(status int, body []byte) *UpstreamError {
	var envelope errorEnvelope

	if err := json.Unmarshal(body, &envelope); err != nil {
		envelope = errorEnvelope{Error: errorStatus{Code: 0, Message: "", Status: "", Details: nil}}
	}

	payload := envelope.Error
	if status == 0 {
		status = payload.Code
	}

	code := strings.TrimSpace(payload.Status)
	message := strings.TrimSpace(payload.Message)
	if message == "" {
		message = http.StatusText(status)
	}

	details := classifyDetails(payload.Details)
	if details.reason != "" {
		code = string(details.reason)
	}

	if status == http.StatusForbidden && details.link != "" {
		message += " Validate the Antigravity account at " + details.link
	}

	class := classifyHTTPStatus(status)

	return &UpstreamError{
		Status:  status,
		Code:    code,
		Message: message,
		Class:   class,
		Retryable: !details.terminal && (class == errcontract.UpstreamClassServerError ||
			(class == errcontract.UpstreamClassRateLimit && details.retryable)),
		RetryAfter: details.retryDelay,
	}
}

func validationLink(raw string) string {
	link, err := url.Parse(raw)
	if err != nil || link.Scheme != "https" || link.Hostname() == "" || link.User != nil {
		return ""
	}

	return raw
}
