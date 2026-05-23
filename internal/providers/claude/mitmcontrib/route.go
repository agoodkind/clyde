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

// routeProvider satisfies the [mitm.Provider] contract for the
// Claude (Anthropic) upstream. It claims plain-HTTP paths under
// `/v1/messages` and `/v1/models`. Claude does not currently opt
// into TLS interception via this contract; the upstream is reached
// over plain HTTP MITM only.
type routeProvider struct{}

// providerID is the typed provider id this package registers with
// the MITM provider registry.
const providerID mitm.ProviderID = claudeContributorID

// ID returns the typed provider id used in the per-provider
// concern path and emitted event provider field.
func (routeProvider) ID() mitm.ProviderID { return providerID }

// ClassifyConnect returns the zero ConnectClaim. The Claude
// provider does not own any CONNECT host in the MITM registry.
func (routeProvider) ClassifyConnect(host string) mitm.ConnectClaim {
	return mitm.ConnectClaim{
		Claimed:    false,
		Host:       host,
		ProviderID: providerID,
	}
}

// ClassifyPlain claims the Anthropic plain-HTTP routes the MITM
// proxy forwards. The proxy concatenates the request URI verbatim
// onto the returned UpstreamURL.
func (routeProvider) ClassifyPlain(path string) mitm.PlainRouteClaim {
	switch {
	case strings.HasPrefix(path, "/v1/messages"), strings.HasPrefix(path, "/v1/models"):
		return mitm.PlainRouteClaim{
			Claimed:     true,
			Provider:    string(claudeContributorID),
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

// BuildCaptureExtension returns nil. Claude does not own a
// provider-specific capture extension.
func (routeProvider) BuildCaptureExtension(exchange mitm.CaptureExchange) mitm.CaptureExtension {
	return nil
}

func init() {
	mitm.RegisterProvider(routeProvider{})
}
