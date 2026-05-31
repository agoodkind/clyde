package mitm

import (
	"net/http"
	"strings"
	"testing"
)

// testRouteID identifies a built-in test-only [Provider] registered for
// capture tests via [registerTestRoute].
type testRouteID string

const (
	testRouteOpenAIV1      testRouteID = "test_route_openai_v1"
	testRouteOpenAIBackend testRouteID = "test_route_openai_backend"
)

const (
	testRouteProviderOpenAIV1 ProviderID = 100 + iota
	testRouteProviderOpenAIBackend
)

// testRouteProvider is a typed test-only [Provider] that claims a single
// plain-HTTP path prefix and routes it to a fixed upstream URL.
type testRouteProvider struct {
	id       ProviderID
	provider string
	upstream string
	prefix   string
}

func (p testRouteProvider) ID() ProviderID { return p.id }

func (p testRouteProvider) ClassifyConnect(host string) ConnectClaim {
	return ConnectClaim{Claimed: false, Host: host, ProviderID: p.id}
}

func (p testRouteProvider) ClassifyPlain(path string) PlainRouteClaim {
	if p.prefix != "" && strings.HasPrefix(path, p.prefix) {
		return PlainRouteClaim{Claimed: true, Provider: p.provider, UpstreamURL: p.upstream}
	}
	return PlainRouteClaim{Claimed: false, Provider: "", UpstreamURL: ""}
}

func (p testRouteProvider) ExtractIdentity(h http.Header) IdentityContribution {
	return IdentityContribution{}
}

// registerTestRoute registers a typed test-only provider that claims a plain
// HTTP route to the supplied upstream for the duration of a test, returning a
// cleanup func that unregisters it.
func registerTestRoute(t *testing.T, id testRouteID, upstream string) func() {
	t.Helper()
	provider := testRouteProvider{
		id:       providerIDForTestRoute(id),
		provider: providerNameForTestRoute(id),
		upstream: upstream,
		prefix:   prefixForTestRoute(id),
	}
	RegisterProviderFirst(provider)
	return func() { UnregisterProvider(provider.id) }
}

func providerIDForTestRoute(id testRouteID) ProviderID {
	switch id {
	case testRouteOpenAIV1:
		return testRouteProviderOpenAIV1
	case testRouteOpenAIBackend:
		return testRouteProviderOpenAIBackend
	}
	return ProviderIDUnknown
}

func providerNameForTestRoute(id testRouteID) string {
	switch id {
	case testRouteOpenAIV1:
		return "openai"
	case testRouteOpenAIBackend:
		return "openai"
	}
	return ""
}

func prefixForTestRoute(id testRouteID) string {
	switch id {
	case testRouteOpenAIV1:
		return "/v1"
	case testRouteOpenAIBackend:
		return "/backend-api"
	}
	return ""
}
