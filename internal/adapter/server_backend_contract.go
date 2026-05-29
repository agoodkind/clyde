package adapter

import (
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/gklog/correlation"
)

// applyBackendOverride keeps backend selection in the root adapter so request
// decode, model resolution, and final backend choice stay readable in one
// place before control passes to a backend package. Returns an *adapterError
// for unsupported overrides; callers should route the error through the
// adapter boundary (return it from an adapterHandler).
func (s *Server) applyBackendOverride(r *http.Request, req ChatRequest, resolved *adapterresolver.ResolvedRequest, reqID string) error {
	override := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Clyde-Backend")))
	if override == "" {
		return nil
	}

	original := resolved.Provider
	overrideBackend := adaptermodel.BackendID(override)
	switch overrideBackend {
	case BackendClaude, BackendAnthropic, BackendPassthroughOverride, BackendCodex:
		resolved.Provider = overrideBackend
	default:
		return newAdapterError(adapterErrorUnsupportedBackend,
			"X-Clyde-Backend must be one of: anthropic, passthrough_override, codex")
	}

	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("from", original.String()),
		slog.String("to", override),
	}
	attrs = append(attrs, correlation.AttrsFromContext(r.Context())...)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "adapter.backend.overridden", append([]slog.Attr{slog.String("concern", "adapter.chat.dispatch")}, attrs...)...)
	return nil
}

// dispatchResolvedChat is the root-owned backend contract boundary. By the time
// execution reaches this method, the request has already been decoded,
// normalized, logged, and resolved to a backend.
func (s *Server) dispatchResolvedChat(
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	effort string,
	reqID string,
	body []byte,
	ingressCtx ingresscontract.IngressContext,
	resolvedReq adapterresolver.ResolvedRequest,
) {
	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("backend", resolvedReq.Provider.String()),
		slog.String("resolved_model", resolvedReq.Model),
		slog.String("effort", effort),
		slog.Bool("stream", req.Stream),
	}
	attrs = append(attrs, correlation.AttrsFromContext(r.Context())...)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "adapter.backend.dispatching", append([]slog.Attr{slog.String("concern", "adapter.chat.dispatch")}, attrs...)...)

	// Passthrough is a raw HTTP forward, not an Execute(EventWriter)
	// provider: forwardPassthroughOverride generates its own request ID,
	// mutates and coerces the raw body, calls the upstream directly, and
	// writes the upstream response verbatim rather than emitting normalized
	// events onto a provider.EventWriter. It cannot satisfy the
	// Provider.Execute contract without contortion, so it stays as the one
	// explicit branch handled before the registry lookup.
	if resolvedReq.Provider == adapterresolver.ProviderPassthrough {
		s.forwardPassthroughOverride(w, r, &resolvedReq, body)
		return
	}

	// Canonicalize the resolved backend to the provider identity the
	// registry is keyed on. The anthropic provider serves both the claude
	// and anthropic backends (the model registry rewrites claude to
	// anthropic only under direct_oauth), so a BackendClaude resolution
	// without that rewrite must still resolve to the anthropic provider.
	lookupID, known := canonicalProviderID(resolvedReq.Provider)
	if !known {
		// The resolved backend is not a known provider family at all.
		s.respondAdapterError(w, r, unsupportedBackendError(&resolvedReq, req.Model))
		return
	}

	provider, lookupErr := s.providerRegistry.Lookup(lookupID)
	if lookupErr != nil || provider == nil {
		// A known provider family with no registered (or a nil) provider
		// means the upstream is not enabled in config.
		s.respondAdapterError(w, r, upstreamUnavailableForProvider(lookupID, &resolvedReq, req.Model))
		return
	}

	// TODO(BackendID dispatch): route by the looked-up provider's ID()
	// instead of unifying the execution path. The anthropic and codex
	// dispatch helpers carry genuinely provider-specific wrapping that a
	// single generic Execute path cannot absorb without behavior change:
	// the anthropic helper wraps the context with anthropic.WithRequestID,
	// owns its own headers-written stream-runerr finalize fallback, and
	// maps errors through anthropicProviderAdapterError; the codex helper
	// registers a parent egress session with per-attempt child hooks via
	// codexEgressContext, emits codex-specific completed/terminal telemetry,
	// and maps errors through codexProviderAdapterError plus context-window
	// classification. Unifying them belongs in a later phase.
	switch provider.ID() {
	case adapterresolver.ProviderAnthropic:
		s.dispatchAnthropicProvider(w, r, effort, reqID, resolvedReq)
	case adapterresolver.ProviderCodex:
		s.dispatchCodexProvider(w, r, req, reqID, ingressCtx, resolvedReq)
	default:
		s.respondAdapterError(w, r, unsupportedBackendError(&resolvedReq, req.Model))
	}
}

// canonicalProviderID maps a resolved backend identity to the provider
// identity the registry is keyed on, and reports whether the backend
// belongs to a known provider family. BackendClaude and BackendAnthropic
// both map to ProviderAnthropic because the anthropic provider serves
// both; BackendCodex maps to ProviderCodex. Passthrough is handled before
// the lookup and any other backend is unknown.
func canonicalProviderID(id adapterresolver.ProviderID) (adapterresolver.ProviderID, bool) {
	switch id {
	case BackendClaude, BackendAnthropic:
		return adapterresolver.ProviderAnthropic, true
	case BackendCodex:
		return adapterresolver.ProviderCodex, true
	case BackendPassthroughOverride:
		// Passthrough is dispatched before this lookup; treat it as unknown
		// here so a stray passthrough id cannot reach the provider switch.
		return id, false
	}
	return id, false
}

// unsupportedBackendError builds the typed unsupported-backend adapter
// error the dispatcher returns when the resolver produced no usable
// provider mapping or the resolved backend is not a known provider
// family. The message and code match the arms the previous switch
// produced.
func unsupportedBackendError(resolved *adapterresolver.ResolvedRequest, alias string) *adapterError {
	err := newAdapterError(adapterErrorUnsupportedBackend,
		"model resolved to unsupported backend "+resolved.Provider.String()+"; configure anthropic, codex, or passthrough_override explicitly")
	err.Backend = resolved.Provider.String()
	err.ModelAlias = alias
	return err
}

// upstreamUnavailableForProvider builds the typed upstream-unavailable
// adapter error for a known provider family whose provider is not
// registered (or registered as nil), meaning the upstream is not enabled
// in config. The provider-scoped message matches what the previous
// per-backend arms produced.
func upstreamUnavailableForProvider(id adapterresolver.ProviderID, resolved *adapterresolver.ResolvedRequest, alias string) *adapterError {
	message := "backend " + id.String() + " is not enabled"
	provider := id.String()
	switch id {
	case adapterresolver.ProviderAnthropic:
		message = "anthropic backend is not enabled in [adapter]"
		provider = "anthropic"
	case adapterresolver.ProviderCodex:
		message = "codex backend is not enabled in [adapter.codex]"
		provider = "codex"
	}
	err := newAdapterError(adapterErrorUpstreamUnavailable, message)
	err.Provider = provider
	err.Backend = resolved.Provider.String()
	err.ModelAlias = alias
	return err
}
