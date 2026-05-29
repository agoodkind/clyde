package mitmcontrib

import "testing"

func TestRouteProviderClaimsCodexConnectHosts(t *testing.T) {
	provider := routeProvider{}
	for _, host := range []string{
		"api.openai.com",
		"chat.openai.com",
		"chatgpt.com",
		"ab.chatgpt.com",
	} {
		t.Run(host, func(t *testing.T) {
			claim := provider.ClassifyConnect(host)
			if !claim.Claimed {
				t.Fatalf("ClassifyConnect(%q).Claimed = false, want true", host)
			}
			if claim.ProviderID != providerID {
				t.Fatalf("ProviderID = %v, want %v", claim.ProviderID, providerID)
			}
		})
	}
}

func TestRouteProviderLeavesUnrelatedConnectHostsOpaque(t *testing.T) {
	provider := routeProvider{}
	for _, host := range []string{
		"api2.cursor.sh",
		"api.anthropic.com",
		"example.com",
	} {
		t.Run(host, func(t *testing.T) {
			claim := provider.ClassifyConnect(host)
			if claim.Claimed {
				t.Fatalf("ClassifyConnect(%q).Claimed = true, want false", host)
			}
		})
	}
}
