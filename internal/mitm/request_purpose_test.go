package mitm

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// purposeTestHeader is the header the test provider below converts into a
// purpose. The generic layer never reads it; only the test provider does.
const purposeTestHeader = "x-purpose-test-class"

// purposeTestProvider is a registered provider that declares a purpose from its
// own header, standing in for a real provider package.
type purposeTestProvider struct{}

func (purposeTestProvider) ID() ProviderID { return ProviderIDCursor }

func (purposeTestProvider) ClassifyConnect(host string) ConnectClaim {
	return ConnectClaim{Claimed: false, Host: host, ProviderID: ProviderIDCursor}
}

func (purposeTestProvider) ClassifyPlain(string) PlainRouteClaim {
	return PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}
}

func (purposeTestProvider) ExtractIdentity(http.Header) IdentityContribution {
	return IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
}

func (purposeTestProvider) ClassifyRequestPurpose(headers http.Header) RequestPurpose {
	if headers.Get(purposeTestHeader) == "compaction" {
		return RequestPurposeCompaction
	}
	return RequestPurposeUnspecified
}

// unclassifiedTestProvider registers without the optional classifier, so a
// request it claims reaches a hook with no declared purpose.
type unclassifiedTestProvider struct{}

func (unclassifiedTestProvider) ID() ProviderID { return ProviderIDConductor }

func (unclassifiedTestProvider) ClassifyConnect(host string) ConnectClaim {
	return ConnectClaim{Claimed: false, Host: host, ProviderID: ProviderIDConductor}
}

func (unclassifiedTestProvider) ClassifyPlain(string) PlainRouteClaim {
	return PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}
}

func (unclassifiedTestProvider) ExtractIdentity(http.Header) IdentityContribution {
	return IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		ConversationID:             "",
		ConversationSource:         "",
		Facet:                      nil,
	}
}

// TestHookRequestCarriesDeclaredPurpose asserts the seam asks the claiming
// provider for a purpose and stores the answer where a hook reads it.
func TestHookRequestCarriesDeclaredPurpose(t *testing.T) {
	RegisterProvider(purposeTestProvider{})
	t.Cleanup(func() { UnregisterProvider(ProviderIDCursor) })

	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	req.Header.Set(purposeTestHeader, "compaction")

	hookRequest := newRequestResponseHookRequest(
		ProviderIDCursor.String(),
		"example.test",
		req,
		newStaticRequestResponseHookBody(nil),
	)
	if hookRequest.Purpose != RequestPurposeCompaction {
		t.Fatalf("Purpose = %q, want %q", hookRequest.Purpose, RequestPurposeCompaction)
	}
}

// TestHookRequestPurposeUnspecifiedWithoutHeader asserts a request from the
// same provider without the header reaches a hook unspecified.
func TestHookRequestPurposeUnspecifiedWithoutHeader(t *testing.T) {
	RegisterProvider(purposeTestProvider{})
	t.Cleanup(func() { UnregisterProvider(ProviderIDCursor) })

	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)

	hookRequest := newRequestResponseHookRequest(
		ProviderIDCursor.String(),
		"example.test",
		req,
		newStaticRequestResponseHookBody(nil),
	)
	if hookRequest.Purpose != RequestPurposeUnspecified {
		t.Fatalf("Purpose = %q, want unspecified", hookRequest.Purpose)
	}
}

// TestHookRequestPurposeUnspecifiedForProviderWithoutClassifier asserts a
// provider that implements no classifier leaves the purpose unspecified rather
// than failing the request.
func TestHookRequestPurposeUnspecifiedForProviderWithoutClassifier(t *testing.T) {
	RegisterProvider(unclassifiedTestProvider{})
	t.Cleanup(func() { UnregisterProvider(ProviderIDConductor) })

	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	req.Header.Set(purposeTestHeader, "compaction")

	hookRequest := newRequestResponseHookRequest(
		ProviderIDConductor.String(),
		"example.test",
		req,
		newStaticRequestResponseHookBody(nil),
	)
	if hookRequest.Purpose != RequestPurposeUnspecified {
		t.Fatalf("Purpose = %q, want unspecified", hookRequest.Purpose)
	}
}

// TestHookRequestPurposeIgnoresOtherProviders asserts the seam asks only the
// claiming provider, so one provider's header cannot declare a purpose on
// another provider's request.
func TestHookRequestPurposeIgnoresOtherProviders(t *testing.T) {
	RegisterProvider(purposeTestProvider{})
	t.Cleanup(func() { UnregisterProvider(ProviderIDCursor) })

	req := httptest.NewRequest(http.MethodPost, "https://example.test/v1/messages", nil)
	req.Header.Set(purposeTestHeader, "compaction")

	hookRequest := newRequestResponseHookRequest(
		ProviderIDClaude.String(),
		"example.test",
		req,
		newStaticRequestResponseHookBody(nil),
	)
	if hookRequest.Purpose != RequestPurposeUnspecified {
		t.Fatalf("Purpose = %q, want unspecified for a different provider", hookRequest.Purpose)
	}
}
