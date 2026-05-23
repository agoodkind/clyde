package mitm

import (
	"net/http"
	"strings"
	"testing"
)

// testRouteID identifies a built-in test-only [Provider]
// registration that claims a path prefix and routes it to an
// arbitrary upstream URL. Tests use this to redirect production
// route shapes onto an httptest server without importing a
// provider package from the generic MITM layer.
type testRouteID string

const (
	testRouteOpenAIV1      testRouteID = "test_route_openai_v1"
	testRouteOpenAIBackend testRouteID = "test_route_openai_backend"
)

type testRouteProvider struct {
	id         ProviderID
	pathPrefix string
	upstream   string
}

func (p testRouteProvider) ID() ProviderID { return p.id }

func (p testRouteProvider) ClassifyConnect(host string) ConnectClaim {
	return ConnectClaim{Claimed: false, Host: host, ProviderID: p.id}
}

func (p testRouteProvider) ClassifyPlain(path string) PlainRouteClaim {
	if !strings.HasPrefix(path, p.pathPrefix) {
		return PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}
	}
	return PlainRouteClaim{
		Claimed:     true,
		Provider:    string(p.id),
		UpstreamURL: p.upstream,
	}
}

func (testRouteProvider) ExtractIdentity(http.Header) IdentityContribution {
	return IdentityContribution{
		PreferredRequestID:         "",
		PreferredUpstreamRequestID: "",
		SessionID:                  "",
		Facet:                      nil,
	}
}

func (testRouteProvider) BuildCaptureExtension(CaptureExchange) CaptureExtension {
	return nil
}

// registerTestRoute registers a typed test-only provider that
// claims the supplied path prefix and routes it to upstream.
// Returns a cleanup function that unregisters the provider.
func registerTestRoute(t *testing.T, id testRouteID, upstream string) func() {
	t.Helper()
	provider := testRouteProvider{
		id:         ProviderID(id),
		pathPrefix: prefixForTestRoute(id),
		upstream:   upstream,
	}
	RegisterProviderFirst(provider)
	return func() {
		UnregisterProvider(provider.ID())
	}
}

func prefixForTestRoute(id testRouteID) string {
	switch id {
	case testRouteOpenAIV1:
		return "/v1/"
	case testRouteOpenAIBackend:
		return "/backend-api/"
	}
	return "/"
}
