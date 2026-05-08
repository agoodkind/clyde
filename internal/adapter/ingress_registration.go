package adapter

import (
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
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
