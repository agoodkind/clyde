package mitmcontrib

import "testing"

func TestRouteProviderClaimsClaudeConnectHosts(t *testing.T) {
	provider := routeProvider{}
	for _, host := range []string{
		"api.anthropic.com",
		"console.anthropic.com",
		"claude.ai",
		"auth.anthropic.com",
		"foo.anthropic.com",
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
		"github.com",
		"anthropic.com.evil.example",
		"notanthropic.com",
		"api.openai.com",
		"chatgpt.com",
		"anthropic.com",
	} {
		t.Run(host, func(t *testing.T) {
			claim := provider.ClassifyConnect(host)
			if claim.Claimed {
				t.Fatalf("ClassifyConnect(%q).Claimed = true, want false", host)
			}
		})
	}
}

func TestIsClaudeConnectHostNormalizesInput(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"API.ANTHROPIC.COM", true},
		{"api.anthropic.com.", true},
		{".api.anthropic.com", true},
		{"Console.Anthropic.Com", true},
		{"CLAUDE.AI", true},
		{"FOO.ANTHROPIC.COM", true},
		{"example.com", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			got := isClaudeConnectHost(c.host)
			if got != c.want {
				t.Fatalf("isClaudeConnectHost(%q) = %v, want %v", c.host, got, c.want)
			}
		})
	}
}
