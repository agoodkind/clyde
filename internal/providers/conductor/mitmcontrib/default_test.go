package mitmcontrib

import "testing"

func TestRouteProviderClaimsConductorConnectHosts(t *testing.T) {
	t.Parallel()

	provider := routeProvider{}
	claim := provider.ClassifyConnect("api.conductor.build")
	if !claim.Claimed {
		t.Fatal("expected api.conductor.build claim")
	}
	if claim.ProviderID != providerID {
		t.Fatalf("provider id = %v want %v", claim.ProviderID, providerID)
	}
}

func TestRouteProviderLeavesUnrelatedConnectHostsOpaque(t *testing.T) {
	t.Parallel()

	provider := routeProvider{}
	claim := provider.ClassifyConnect("example.com")
	if claim.Claimed {
		t.Fatal("expected non-Conductor host to fall through")
	}
}
