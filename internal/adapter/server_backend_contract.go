package adapter

import (
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
	"goodkind.io/clyde/internal/correlation"
)

// applyBackendOverride keeps backend selection in the root adapter so request
// decode, model resolution, and final backend choice stay readable in one
// place before control passes to a backend package. Returns an *adapterError
// for unsupported overrides; callers should route the error through the
// adapter boundary (return it from an adapterHandler).
func (s *Server) applyBackendOverride(r *http.Request, req ChatRequest, model ResolvedModel, reqID string) (ResolvedModel, error) {
	override := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Clyde-Backend")))
	if override == "" {
		return model, nil
	}

	original := model.Backend
	switch override {
	case BackendAnthropic, BackendPassthroughOverride, BackendCodex:
		model.Backend = override
	default:
		return model, newAdapterError(adapterErrorUnsupportedBackend,
			"X-Clyde-Backend must be one of: anthropic, passthrough_override, codex")
	}

	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("from", original),
		slog.String("to", override),
	}
	attrs = append(attrs, correlation.AttrsFromContext(r.Context())...)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "adapter.backend.overridden", attrs...)
	return model, nil
}

// dispatchResolvedChat is the root-owned backend contract boundary. By the time
// execution reaches this method, the request has already been decoded,
// normalized, logged, and resolved to a backend.
func (s *Server) dispatchResolvedChat(
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	model ResolvedModel,
	effort string,
	reqID string,
	body []byte,
	ingressCtx ingresscontract.IngressContext,
	resolvedReq adapterresolver.ResolvedRequest,
	resolverErr error,
) {
	attrs := []slog.Attr{
		slog.String("request_id", reqID),
		slog.String("alias", req.Model),
		slog.String("backend", model.Backend),
		slog.String("resolved_model", model.ClaudeModel),
		slog.String("effort", effort),
		slog.Bool("stream", req.Stream),
	}
	attrs = append(attrs, correlation.AttrsFromContext(r.Context())...)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "adapter.backend.dispatching", attrs...)
	switch model.Backend {
	case BackendPassthroughOverride:
		s.forwardPassthroughOverride(w, r, model, body)
		return
	case BackendAnthropic:
		if s.anthropicProvider == nil {
			err := newAdapterError(adapterErrorUpstreamUnavailable, "anthropic backend is not enabled in [adapter]")
			err.Provider = "anthropic"
			err.Backend = model.Backend
			err.ModelAlias = req.Model
			s.respondAdapterError(w, r, err)
			return
		}
		if resolverErr != nil || resolvedReq.Provider != adapterresolver.ProviderAnthropic {
			err := newAdapterError(adapterErrorInvalidRequest, "resolver did not map this request to the anthropic provider")
			err.Provider = "anthropic"
			err.Backend = model.Backend
			err.ModelAlias = req.Model
			s.respondAdapterError(w, r, err)
			return
		}
		s.dispatchAnthropicProvider(w, r, effort, reqID, resolvedReq)
		return
	case BackendCodex:
		if s.codexProvider == nil {
			err := newAdapterError(adapterErrorUpstreamUnavailable, "codex backend is not enabled in [adapter.codex]")
			err.Provider = "codex"
			err.Backend = model.Backend
			err.ModelAlias = req.Model
			s.respondAdapterError(w, r, err)
			return
		}
		if resolverErr != nil || resolvedReq.Provider != adapterresolver.ProviderCodex {
			err := newAdapterError(adapterErrorInvalidRequest, "resolver did not map this request to the codex provider")
			err.Provider = "codex"
			err.Backend = model.Backend
			err.ModelAlias = req.Model
			s.respondAdapterError(w, r, err)
			return
		}
		s.dispatchCodexProvider(w, r, req, model, reqID, ingressCtx, resolvedReq)
		return
	default:
		err := newAdapterError(adapterErrorUnsupportedBackend,
			"model resolved to unsupported backend "+model.Backend+"; configure anthropic, codex, or passthrough_override explicitly")
		err.Backend = model.Backend
		err.ModelAlias = req.Model
		s.respondAdapterError(w, r, err)
		return
	}
}
