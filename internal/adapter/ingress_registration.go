package adapter

import (
	"fmt"
	"log/slog"

	adapteranthropic "goodkind.io/clyde/internal/adapter/anthropic"
	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

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
func resolveCursorChatRequest(req ChatRequest, registry adapterresolver.ModelRegistry) (adapterresolver.ResolvedRequest, error) {
	cursorReq := adaptercursor.TranslateRequest(req)
	resolved, err := adapterresolver.Resolve(cursorReq, registry)
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
