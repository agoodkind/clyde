package adapter

import (
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// init wires the canonical vendor implementations of the ingress
// boundary contract into the package-level registry. This file is
// the composition root for ingress registration: it is the only file
// under internal/adapter at depth 1 that imports a vendor ingress
// package. The boundary file (family_ingress_mapping.go) and the
// dispatcher hold no vendor ingress import and construct no vendor
// ingress type.
//
// Vendor packages expose
// RegisterIngress(ingresscontract.IngressRegistrar) so this file can
// register Cursor without speaking Cursor's request-shape conventions.
func init() {
	adaptercursor.RegisterIngress(defaultIngressRegistry)
}

// resolveCursorChatRequest is the registration-root indirection that
// hides the cursor.Request type from every other depth-1 adapter
// file. The dispatcher calls this helper to get a typed
// resolver.ResolvedRequest for an OpenAI-shaped chat request without
// importing adaptercursor. Future ingress vendors fold their typed
// resolver entry-point into this same registration root file.
func resolveCursorChatRequest(req ChatRequest, registry adapterresolver.ModelRegistry) (adapterresolver.ResolvedRequest, error) {
	cursorReq := adaptercursor.TranslateRequest(req)
	return adapterresolver.Resolve(cursorReq, registry)
}
