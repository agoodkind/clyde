package model

import "errors"

// ResolveErrorKind classifies model resolution failures that ingress handlers
// must distinguish without inspecting error text.
type ResolveErrorKind string

const (
	// ResolveErrorInvalidRequest identifies an invalid option for a known exact model.
	ResolveErrorInvalidRequest ResolveErrorKind = "invalid_request"
	// ResolveErrorModelNotFound identifies a model or alias absent from the catalog and routes.
	ResolveErrorModelNotFound ResolveErrorKind = "model_not_found"
)

// ResolveError carries a typed model resolution failure and its stable message.
type ResolveError struct {
	Kind    ResolveErrorKind
	Message string
}

// Error returns the model resolution failure message.
func (err *ResolveError) Error() string {
	if err == nil {
		return ""
	}
	return err.Message
}

// ResolveErrorKindOf returns the typed resolution kind carried anywhere in err.
func ResolveErrorKindOf(err error) (ResolveErrorKind, bool) {
	var resolveErr *ResolveError
	if !errors.As(err, &resolveErr) {
		return "", false
	}
	return resolveErr.Kind, true
}

func newResolveError(kind ResolveErrorKind, message string) error {
	return &ResolveError{Kind: kind, Message: message}
}
