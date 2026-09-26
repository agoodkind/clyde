package mitm

import "net/url"

// classifyRoute returns the provider and upstream from the registered
// provider that claims the supplied path. The generic MITM proxy contains no
// provider identifiers. Provider packages register their upstream claims with
// [RegisterProvider] at init time.
func classifyRoute(path string) (provider string, upstream string) {
	if _, claim, ok := providerForPlain(path); ok {
		return claim.Provider, claim.UpstreamURL
	}
	return "", ""
}

// classifyPlainRequest picks the provider and upstream for a plain-HTTP
// request. Providers share path prefixes such as /v1/. A client that uses this
// listener as HTTP_PROXY sends the full target URL. For that request, a
// registered host claim picks the provider, and the proxy forwards the request
// to the host the client requested. A client that uses this listener as its
// base URL sends only a path. For that request, the path claim picks the
// provider and upstream.
func classifyPlainRequest(target *url.URL) (provider string, upstream string) {
	if target.IsAbs() && target.Host != "" {
		if hostProvider, _, ok := providerForConnect(target.Host); ok {
			return hostProvider.ID().String(), target.Scheme + "://" + target.Host
		}
	}
	return classifyRoute(target.Path)
}
