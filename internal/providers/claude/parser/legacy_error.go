package parser

import "strings"

// legacyResolutionError stores the subject, reason, and underlying errors for
// a failed resolution of a summary, request, reference, or line.
type legacyResolutionError struct {
	subject string
	reason  string
	causes  []error
}

func newResolutionError(subject string, reason string, causes ...error) *legacyResolutionError {
	return &legacyResolutionError{subject: subject, reason: reason, causes: causes}
}

// Error returns the subject, reason, and each cause.
func (resolutionError *legacyResolutionError) Error() string {
	var builder strings.Builder
	builder.WriteString(resolutionError.subject)
	builder.WriteString(": ")
	builder.WriteString(resolutionError.reason)
	for _, cause := range resolutionError.causes {
		builder.WriteString("; ")
		builder.WriteString(cause.Error())
	}
	return builder.String()
}

// Unwrap returns the underlying errors.
func (resolutionError *legacyResolutionError) Unwrap() []error {
	return resolutionError.causes
}
