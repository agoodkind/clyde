package mitmcontrib

import (
	"net/http"

	"goodkind.io/clyde/internal/mitm"
)

// providerID is the typed provider id this package registers with
// the MITM provider registry.
const providerID mitm.ProviderID = mitm.ProviderIDConductor

// routeProvider satisfies the [mitm.Provider] contract for Conductor
// traffic. It claims CONNECT targets that resolve to Conductor app
// hosts. Conductor traffic is not currently routed over plain-HTTP
// MITM; ClassifyPlain returns the zero claim.
type routeProvider struct{}

// ID returns the typed provider id used in the per-provider
// concern path and emitted event provider field.
func (routeProvider) ID() mitm.ProviderID { return providerID }

// ClassifyConnect claims Conductor's backend hosts for TLS
// interception.
func (routeProvider) ClassifyConnect(host string) mitm.ConnectClaim {
	if !isConductorHost(host) {
		return mitm.ConnectClaim{
			Claimed:    false,
			Host:       host,
			ProviderID: providerID,
		}
	}
	return mitm.ConnectClaim{
		Claimed:    true,
		Host:       host,
		ProviderID: providerID,
	}
}

// ClassifyPlain returns the zero claim. Conductor traffic reaches the
// MITM proxy through CONNECT + TLS interception only.
func (routeProvider) ClassifyPlain(path string) mitm.PlainRouteClaim {
	return mitm.PlainRouteClaim{
		Claimed:     false,
		Provider:    "",
		UpstreamURL: "",
	}
}

// ExtractIdentity returns the zero typed contribution. Conductor
// traffic does not currently contribute provider identity headers.
func (routeProvider) ExtractIdentity(headers http.Header) mitm.IdentityContribution {
	return mitm.IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
}

func init() {
	mitm.RegisterProvider(routeProvider{})
}
