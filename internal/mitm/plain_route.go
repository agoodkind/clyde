package mitm

import "net/url"

// classifyRoute calls ClassifyPlain on each registered provider with path. It
// returns the provider name and upstream URL from the first result with Claimed
// set to true.
func classifyRoute(path string) (provider string, upstream string) {
	if _, claim, ok := providerForPlain(path); ok {
		return claim.Provider, claim.UpstreamURL
	}
	return "", ""
}

// classifyPlainRequest calls ClassifyConnect on each registered provider with
// the URL host. The first provider with Claimed set to true is the result, and
// the URL scheme and host form the upstream. Otherwise classifyRoute selects the
// provider from the URL path.
//
// Example: Claude Code sends POST https://api.anthropic.com/v1/environments/bridge
// through HTTPS_PROXY. The Claude provider ClassifyConnect sets Claimed to true
// for api.anthropic.com. classifyPlainRequest returns the Claude provider and
// https://api.anthropic.com. The Codex provider ClassifyPlain matches the /v1/
// prefix and returns https://api.openai.com.
func classifyPlainRequest(target *url.URL) (provider string, upstream string) {
	if target.IsAbs() && target.Host != "" {
		if hostProvider, _, ok := providerForConnect(target.Host); ok {
			return hostProvider.ID().String(), target.Scheme + "://" + target.Host
		}
	}
	return classifyRoute(target.Path)
}
