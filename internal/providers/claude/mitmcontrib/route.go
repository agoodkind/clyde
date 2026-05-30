// Package mitmcontrib registers Claude MITM route classification.
package mitmcontrib

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

// anthropicUpstream is the canonical Anthropic upstream the Claude
// provider routes its plain-HTTP MITM traffic to. The constant is
// provider-owned and lives in this package so the generic
// internal/mitm package never names a provider upstream.
const anthropicUpstream = "https://api.anthropic.com"

const (
	anthropicAPIHost     = "api.anthropic.com"
	anthropicConsoleHost = "console.anthropic.com"
	claudeAIHost         = "claude.ai"
	anthropicSuffix      = ".anthropic.com"
)

// routeProvider satisfies the [mitm.Provider] contract for the
// Claude (Anthropic) upstream. It claims plain-HTTP paths under
// `/v1/messages` and `/v1/models`, and claims Anthropic and
// claude.ai CONNECT hosts for TLS interception.
type routeProvider struct{}

// providerID is the typed provider id this package registers with
// the MITM provider registry.
const providerID mitm.ProviderID = mitm.ProviderIDClaude

// ID returns the typed provider id used in the per-provider
// concern path and emitted event provider field.
func (routeProvider) ID() mitm.ProviderID { return providerID }

// ClassifyConnect claims Anthropic and claude.ai CONNECT hosts for
// TLS interception so the MITM proxy can decrypt and capture the
// inner HTTP requests instead of opaque-byte splicing.
func (routeProvider) ClassifyConnect(host string) mitm.ConnectClaim {
	if isClaudeConnectHost(host) {
		return mitm.ConnectClaim{
			Claimed:    true,
			Host:       host,
			ProviderID: providerID,
		}
	}
	return mitm.ConnectClaim{
		Claimed:    false,
		Host:       host,
		ProviderID: providerID,
	}
}

func isClaudeConnectHost(host string) bool {
	normalizedHost := strings.Trim(strings.ToLower(host), ".")
	switch normalizedHost {
	case anthropicAPIHost, anthropicConsoleHost, claudeAIHost:
		return true
	}
	return strings.HasSuffix(normalizedHost, anthropicSuffix)
}

// ClassifyPlain claims the Anthropic plain-HTTP routes the MITM
// proxy forwards. The proxy concatenates the request URI verbatim
// onto the returned UpstreamURL.
func (routeProvider) ClassifyPlain(path string) mitm.PlainRouteClaim {
	switch {
	case strings.HasPrefix(path, "/v1/messages"), strings.HasPrefix(path, "/v1/models"):
		return mitm.PlainRouteClaim{
			Claimed:     true,
			Provider:    providerID.String(),
			UpstreamURL: anthropicUpstream,
		}
	}
	return mitm.PlainRouteClaim{
		Claimed:     false,
		Provider:    "",
		UpstreamURL: "",
	}
}

// ExtractIdentity returns the zero typed contribution. Claude
// traffic over plain-HTTP MITM does not currently contribute a
// provider identity facet through this contract.
func (routeProvider) ExtractIdentity(headers http.Header) mitm.IdentityContribution {
	return mitm.IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		Facet:                      nil,
	}
}

func init() {
	mitm.RegisterProvider(routeProvider{})
}
