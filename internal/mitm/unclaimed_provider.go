package mitm

import "net/http"

// unclaimedProvider is the metadata-only Provider the MITM uses for a host that
// no registered provider claims. Each listener's port already scopes a captured
// exchange to one application through the capture client tag, so capture is
// unconditional and the provider is only a metadata label. This fallback claims
// nothing, contributes no identity, and tags the exchange ProviderIDUnknown
// ("unspecified"). It never gates whether a host is captured, and it is never
// registered; the transparent front-door and the CONNECT path use it directly
// as the fallback when providerForConnect finds no claim.
type unclaimedProvider struct{}

// ID returns ProviderIDUnknown so the capture row and emitted events tag the
// exchange "unspecified".
func (unclaimedProvider) ID() ProviderID { return ProviderIDUnknown }

// ClassifyConnect always returns an unclaimed result; the fallback owns no
// hosts and only carries metadata.
func (unclaimedProvider) ClassifyConnect(host string) ConnectClaim {
	return ConnectClaim{Claimed: false, Host: host, ProviderID: ProviderIDUnknown}
}

// ClassifyPlain always returns the zero, unclaimed plain-route claim.
func (unclaimedProvider) ClassifyPlain(string) PlainRouteClaim {
	return PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}
}

// ExtractIdentity returns the zero contribution; an unclaimed host has no
// provider-specific identity to add.
func (unclaimedProvider) ExtractIdentity(http.Header) IdentityContribution {
	return IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
}
