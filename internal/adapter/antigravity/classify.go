package antigravity

import (
	"net/http"
	"strings"
	"time"

	"goodkind.io/clyde/internal/adapter/errcontract"
)

type errorReason string

const (
	reasonRateLimit  errorReason = "RATE_LIMIT_EXCEEDED"
	reasonQuota      errorReason = "QUOTA_EXHAUSTED"
	reasonCapacity   errorReason = "MODEL_CAPACITY_EXHAUSTED"
	reasonValidation errorReason = "VALIDATION_REQUIRED"

	errorInfoType    = "type.googleapis.com/google.rpc.ErrorInfo"
	retryInfoType    = "type.googleapis.com/google.rpc.RetryInfo"
	quotaFailureType = "type.googleapis.com/google.rpc.QuotaFailure"
)

type classifiedDetails struct {
	reason     errorReason
	link       string
	retryDelay time.Duration
	retryable  bool
	terminal   bool
}

func classifyDetails(details []errorDetail) classifiedDetails {
	var result classifiedDetails

	var quotaReset time.Duration

	for _, detail := range details {
		switch detail.Type {
		case retryInfoType:
			if result.retryDelay <= 0 {
				result.retryDelay = positiveDuration(detail.RetryDelay)
			}
		case errorInfoType:
			result.applyErrorInfo(detail)
			if quotaReset <= 0 {
				quotaReset = positiveDuration(detail.Metadata.QuotaResetDelay)
			}
		case quotaFailureType:
			for _, violation := range detail.Violations {
				result.terminal = result.terminal || isDailyQuota(violation.QuotaID)
			}
		}
	}

	if result.retryDelay <= 0 {
		result.retryDelay = quotaReset
	}

	return result
}

func (result *classifiedDetails) applyErrorInfo(detail errorDetail) {
	metadata := detail.Metadata
	result.terminal = result.terminal || isDailyQuota(metadata.QuotaID) ||
		isDailyQuota(metadata.QuotaIDSnake)

	switch detail.Reason {
	case reasonRateLimit:
		if result.reason != reasonQuota && result.reason != reasonCapacity {
			result.reason = detail.Reason
		}

		result.retryable = true
	case reasonQuota, reasonCapacity:
		result.reason = detail.Reason
		result.terminal = true
	case reasonValidation:
		result.reason = detail.Reason
		result.link = validationLink(metadata.ValidationLink)
	default:
		if result.reason == "" {
			result.reason = detail.Reason
		}
	}
}

func isDailyQuota(quotaID string) bool {
	return strings.Contains(strings.ToLower(quotaID), "perday")
}

func positiveDuration(raw string) time.Duration {
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0
	}

	return value
}

func classifyHTTPStatus(status int) errcontract.UpstreamCodeClass {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errcontract.UpstreamClassAuth
	case status == http.StatusTooManyRequests:
		return errcontract.UpstreamClassRateLimit
	case status >= http.StatusBadRequest && status < http.StatusInternalServerError:
		return errcontract.UpstreamClassInvalidRequest
	case status >= http.StatusInternalServerError && status <= 599:
		return errcontract.UpstreamClassServerError
	default:
		return errcontract.UpstreamClassUnknown
	}
}
