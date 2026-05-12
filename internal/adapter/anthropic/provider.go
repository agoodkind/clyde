package anthropic

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

type requestIDContextKey struct{}

// WithRequestID binds the adapter-generated request id into ctx so the
// provider can preserve the existing response IDs and log correlation
// fields while the root dispatcher delegates Anthropic execution through
// Provider.Execute.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(requestID) == "" {
		return ctx
	}
	return context.WithValue(ctx, requestIDContextKey{}, requestID)
}

func requestIDFromContext(ctx context.Context, defaultRequestID string) string {
	if ctx != nil {
		if value, ok := ctx.Value(requestIDContextKey{}).(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return strings.TrimSpace(defaultRequestID)
}

// ExecuteError preserves the existing collect-path HTTP status/code
// decisions for pre-wire failures now that Provider.Execute no longer
// writes directly to the response writer.
type ExecuteError struct {
	Status  int
	Code    string
	Message string
	Cause   error
}

func (e *ExecuteError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "anthropic provider execution failed"
}

func (e *ExecuteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// Provider is the Anthropic direct-OAuth implementation of the
// upstream-agnostic adapter/provider.Provider contract.
type Provider struct {
	log             *slog.Logger
	prepare         func(context.Context, adapterresolver.ResolvedRequest, string) (PreparedRequest, error)
	executePrepared func(context.Context, PreparedRequest, adapterprovider.EventWriter) (adapterprovider.Result, error)
}

// ProviderOptions binds the construction-time Anthropic-only
// dependencies the root adapter currently supplies for the OpenAI
// ingress adapter.
type ProviderOptions struct {
	Prepare         func(context.Context, adapterresolver.ResolvedRequest, string) (PreparedRequest, error)
	ExecutePrepared func(context.Context, PreparedRequest, adapterprovider.EventWriter) (adapterprovider.Result, error)
	// FileLog controls rotation for the dedicated anthropic.jsonl
	// sidecar sink. Mirrors the Codex provider's FileLog wiring so the
	// resolved logpolicy SinkAnthropicSidecar rotation flows through one
	// public entry point.
	FileLog FileLogRotationConfig
}

func NewProvider(deps adapterprovider.Deps, opts ProviderOptions) *Provider {
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	ConfigureAnthropicFileLogger(opts.FileLog)
	return &Provider{
		log:             log.With("component", "adapter", "subcomponent", "anthropic_provider"),
		prepare:         opts.Prepare,
		executePrepared: opts.ExecutePrepared,
	}
}

// ID matches the resolver.ProviderID for the Anthropic backend.
func (p *Provider) ID() adapterresolver.ProviderID {
	return adapterresolver.ProviderAnthropic
}

func (p *Provider) Execute(ctx context.Context, req adapterresolver.ResolvedRequest, w adapterprovider.EventWriter) (adapterprovider.Result, error) {
	if p == nil || p.prepare == nil {
		return adapterprovider.Result{}, &ExecuteError{
			Status:  http.StatusInternalServerError,
			Code:    "anthropic_prepare_unconfigured",
			Message: "adapter built without anthropic request preparation; set adapter.direct_oauth=true and restart",
		}
	}
	reqID := requestIDFromContext(ctx, req.Cursor.RequestID)
	prepared, err := p.prepare(ctx, req, reqID)
	if err != nil {
		return adapterprovider.Result{}, err
	}
	return p.ExecutePrepared(ctx, prepared, w)
}

// ExecutePrepared runs a prebuilt native Anthropic request through the
// provider-owned execution path. Future native `/v1/messages` ingress
// can call this directly after it decodes and validates the public
// Anthropic wire shape.
func (p *Provider) ExecutePrepared(ctx context.Context, req PreparedRequest, w adapterprovider.EventWriter) (adapterprovider.Result, error) {
	if p == nil || p.executePrepared == nil {
		return adapterprovider.Result{}, &ExecuteError{
			Status:  http.StatusInternalServerError,
			Code:    "oauth_unconfigured",
			Message: "adapter built without anthropic execution provider; set adapter.direct_oauth=true and restart",
		}
	}
	return p.executePrepared(ctx, req, w)
}
