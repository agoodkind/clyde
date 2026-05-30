package mitmcontrib

import (
	"net/http"

	"goodkind.io/clyde/internal/mitm"
)

// providerID is the typed provider id this package registers with
// the MITM provider registry.
const providerID mitm.ProviderID = mitm.ProviderIDCursor

// routeProvider satisfies the [mitm.Provider] contract for Cursor
// traffic. It claims CONNECT targets that resolve to Cursor
// backend hosts and contributes a typed Cursor identity facet
// parsed from the intercepted request headers. Cursor traffic is
// not currently routed over plain-HTTP MITM; ClassifyPlain returns
// the zero claim.
type routeProvider struct{}

// ID returns the typed provider id used in the per-provider
// concern path and emitted event provider field.
func (routeProvider) ID() mitm.ProviderID { return providerID }

// ClassifyConnect claims Cursor's backend hosts (`api2.cursor.sh`,
// `api3.cursor.sh`, and anything under `*.cursor.sh` or
// `*.cursor.com`) for TLS interception.
func (routeProvider) ClassifyConnect(host string) mitm.ConnectClaim {
	if !isServiceHost(host) {
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

// ClassifyPlain returns the zero claim. Cursor traffic reaches the
// MITM proxy through CONNECT + TLS interception only; no plain-HTTP
// claim is registered.
func (routeProvider) ClassifyPlain(path string) mitm.PlainRouteClaim {
	return mitm.PlainRouteClaim{
		Claimed:     false,
		Provider:    "",
		UpstreamURL: "",
	}
}

// ExtractIdentity parses the Cursor identity headers and returns
// the typed contribution the generic MITM emit path attaches to
// emitted events.
func (routeProvider) ExtractIdentity(headers http.Header) mitm.IdentityContribution {
	return extractIdentity(headers)
}

func init() {
	mitm.RegisterProvider(routeProvider{})
}
