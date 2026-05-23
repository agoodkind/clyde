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

// providerID is the typed provider id this package registers with
// the MITM provider registry.
const providerID mitm.ProviderID = codexContributorID

// routeProvider satisfies the [mitm.Provider] contract for the
// Codex (OpenAI) upstream. It claims plain-HTTP paths under
// `/backend-api/` and `/v1/`.
type routeProvider struct{}

// ID returns the typed provider id used in the per-provider
// concern path and emitted event provider field.
func (routeProvider) ID() mitm.ProviderID { return providerID }

// ClassifyConnect returns the zero ConnectClaim. Codex does not
// own any CONNECT host in the MITM registry.
func (routeProvider) ClassifyConnect(host string) mitm.ConnectClaim {
	return mitm.ConnectClaim{
		Claimed:    false,
		Host:       host,
		ProviderID: providerID,
	}
}

// ClassifyPlain claims the OpenAI plain-HTTP routes the MITM proxy
// forwards. The order of the cases matches the legacy switch the
// generic proxy used to embed.
func (routeProvider) ClassifyPlain(path string) mitm.PlainRouteClaim {
	switch {
	case strings.HasPrefix(path, "/backend-api/"):
		return mitm.PlainRouteClaim{
			Claimed:     true,
			Provider:    string(providerID),
			UpstreamURL: chatGPTUpstream,
		}
	case strings.HasPrefix(path, "/v1/"):
		return mitm.PlainRouteClaim{
			Claimed:     true,
			Provider:    string(providerID),
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

// BuildCaptureExtension returns nil. Codex does not own a
// provider-specific capture extension.
func (routeProvider) BuildCaptureExtension(exchange mitm.CaptureExchange) mitm.CaptureExtension {
	return nil
}

func init() {
	mitm.RegisterProvider(routeProvider{})
}
