// Package mitmcontrib registers Codex MITM route classification.
package mitmcontrib

import (
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/mitm"
)

// openAIUpstream and chatGPTUpstream are the canonical OpenAI
// upstreams the Codex provider routes its plain-HTTP MITM traffic
// to. These values are provider-owned and live in this package so
// the generic internal/mitm package never names a provider upstream.
const (
	openAIUpstream  = "https://api.openai.com"
	chatGPTUpstream = "https://chatgpt.com"
)

const (
	openAIAPIHost   = "api.openai.com"
	openAIChatHost  = "chat.openai.com"
	chatGPTApexHost = "chatgpt.com"
	chatGPTSuffix   = ".chatgpt.com"
)

// providerID is the typed provider id this package registers with
// the MITM provider registry.
const providerID mitm.ProviderID = mitm.ProviderIDCodex

// routeProvider satisfies the [mitm.Provider] contract for the
// Codex (OpenAI) upstream. It claims plain-HTTP paths under
// `/backend-api/` and `/v1/`.
type routeProvider struct{}

// ID returns the typed provider id used in the per-provider
// concern path and emitted event provider field.
func (routeProvider) ID() mitm.ProviderID { return providerID }

// ClassifyConnect claims Codex's OpenAI and ChatGPT CONNECT hosts
// for TLS interception.
func (routeProvider) ClassifyConnect(host string) mitm.ConnectClaim {
	if isCodexConnectHost(host) {
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

func isCodexConnectHost(host string) bool {
	normalizedHost := strings.Trim(strings.ToLower(host), ".")
	switch normalizedHost {
	case openAIAPIHost, openAIChatHost, chatGPTApexHost:
		return true
	}
	return strings.HasSuffix(normalizedHost, chatGPTSuffix)
}

// ClassifyPlain claims the OpenAI plain-HTTP routes the MITM proxy
// forwards. The order of the cases matches the legacy switch the
// generic proxy used to embed.
func (routeProvider) ClassifyPlain(path string) mitm.PlainRouteClaim {
	switch {
	case strings.HasPrefix(path, "/backend-api/"):
		return mitm.PlainRouteClaim{
			Claimed:     true,
			Provider:    providerID.String(),
			UpstreamURL: chatGPTUpstream,
		}
	case strings.HasPrefix(path, "/v1/"):
		return mitm.PlainRouteClaim{
			Claimed:     true,
			Provider:    providerID.String(),
			UpstreamURL: openAIUpstream,
		}
	}
	return mitm.PlainRouteClaim{
		Claimed:     false,
		Provider:    "",
		UpstreamURL: "",
	}
}

// ExtractIdentity returns the zero typed contribution. Codex
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
