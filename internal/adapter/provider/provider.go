package provider

import (
	"context"

	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// Provider is the upstream-agnostic interface every backend
// implementation satisfies. The dispatcher routes a ResolvedRequest
// to the Provider whose ID matches ResolvedRequest.Provider, and
// calls Execute. Providers must not retain references to the writer
// after Execute returns.
type Provider interface {
	// ID returns the typed provider identity. It must match the
	// resolver.ProviderID constant the resolver assigns to this
	// upstream so the dispatcher can look the provider up.
	ID() adapterresolver.ProviderID

	// Execute runs the upstream request and emits normalized events
	// onto the supplied EventWriter. Returning a non-nil error
	// signals the dispatcher to surface a wire-level failure to the
	// client; partial output written before the error is the
	// caller's responsibility.
	Execute(ctx context.Context, req adapterresolver.ResolvedRequest, w EventWriter) (Result, error)
}
