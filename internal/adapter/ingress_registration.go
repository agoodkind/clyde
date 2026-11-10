package adapter

import (
	"fmt"
	"log/slog"

	adapteranthropic "goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	"goodkind.io/clyde/internal/adapter/ingresscontract"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// newServerIngress builds a fresh ingress contract owned by one adapter
// Server. The Cursor ingress carries a per-daemon ChatIdentityResolver
// holding branch/fork state, so each Server must own its own instance:
// the package-level defaultIngressRegistry is a single shared instance,
// and two Servers in one process (every parallel test, or any future
// multi-tenant case) would otherwise cross-pollute that branch state and
// drop fork-detection events. The global registry stays for stateless
// boundary lookups (header/body translation) that hold no resolver
// state. This file is the composition root, the only depth-1 adapter
// file allowed to construct a vendor ingress type.
func newServerIngress() ingresscontract.IngressContract {
	return adaptercursor.NewIngress()
}

// init wires canonical vendor implementations into package-level
// registries. This file is the adapter composition root: it is the
// only file under internal/adapter at depth 1 that imports vendor
// packages for ingress and backend-facet registration. The dispatcher
// and request logger hold no vendor facet import and construct no
// vendor facet type.
//
// Vendor packages expose
// RegisterIngress(ingresscontract.IngressRegistrar) so this file can
// register Cursor without speaking Cursor's request-shape conventions.
func init() {
	adaptercursor.RegisterIngress(defaultIngressRegistry)
	adaptercodex.RegisterBackendFacet(defaultBackendFacetRegistry)
	adapteranthropic.RegisterBackendFacet(defaultBackendFacetRegistry)
}

// resolveCursorChatRequest is the registration-root indirection that
// hides the cursor.Request type from every other depth-1 adapter
// file. The dispatcher calls this helper to get a typed
// resolver.ResolvedRequest for an OpenAI-shaped chat request without
// importing adaptercursor. Future ingress vendors fold their typed
// resolver entry-point into this same registration root file.
func resolveCursorChatRequest(
	surface adapterresolver.IngressSurface,
	req ChatRequest,
	registry adapterresolver.ModelRegistry,
) (adapterresolver.ResolvedRequest, error) {
	cursorReq := adaptercursor.TranslateRequest(req)
	resolved, err := adapterresolver.Resolve(surface, cursorReq, registry)
	if err != nil {
		slog.Warn("adapter: resolve cursor chat request failed",
			slog.String("concern", "adapter.chat.preflight"),
			slog.String("model", cursorReq.OpenAI.Model),
			slog.String("normalized_model", cursorReq.NormalizedModel),
			slog.String("err", err.Error()),
		)
		return resolved, fmt.Errorf("adapter: resolve cursor chat request: %w", err)
	}
	return resolved, nil
}

// resolveResponsesRequest leaves generic OpenAI Responses requests untouched.
// Cursor translation is applied only when the listener explicitly selected
// Cursor ingress; generic OpenAI metadata must not mutate model identity.
func resolveResponsesRequest(surface adapterresolver.IngressSurface, req ChatRequest, registry adapterresolver.ModelRegistry) (adapterresolver.ResolvedRequest, error) {
	if surface == adapterresolver.IngressCursor {
		return resolveCursorChatRequest(surface, req, registry)
	}
	resolved, err := adapterresolver.Resolve(surface, adaptercursor.NewUntranslatedRequest(req), registry)
	if err != nil {
		slog.Warn("adapter: resolve generic OpenAI Responses request failed",
			slog.String("concern", "adapter.chat.preflight"),
			slog.String("model", req.Model),
			slog.String("err", err.Error()),
		)
		return adapterresolver.ResolvedRequest{}, fmt.Errorf("resolve generic OpenAI Responses request: %w", err)
	}
	return resolved, nil
}
