package daemon

import (
	"net/url"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic/oauthprovider"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/mitm"
)

// anthropicHostSuffix is the DNS suffix the tracker matches when deciding
// whether a MITM session targets the Anthropic upstream. Both the trailing
// dot ("anthropic.com" as a complete host) and any subdomain ("api.anthropic.com")
// satisfy the filter via hostMatchesSuffix.
const anthropicHostSuffix = "anthropic.com"

// anthropicMITMTracker satisfies oauthprovider.ClaudeMITMTracker by walking
// the MITM proxy's tunnel registry and counting sessions whose destination
// is under *.anthropic.com. The lookup is a single CountByPredicate call
// against an in-process map, so it runs in O(n) over the small number of
// live MITM sessions with no I/O.
//
// Layer separation: this type lives in internal/daemon so the
// oauthprovider package does not import internal/mitm. The provider package
// declares the interface; the daemon wires the implementation when it has
// both the rotator and the live MITM proxy in hand.
type anthropicMITMTracker struct {
	proxy func() *mitm.Proxy
}

// newAnthropicMITMTracker constructs a tracker bound to the supplied proxy
// accessor. The accessor returning nil (proxy not yet started) is treated
// as "no active sessions"; callers can wire this safely before the MITM
// subsystem is up.
func newAnthropicMITMTracker(proxy func() *mitm.Proxy) oauthprovider.ClaudeMITMTracker {
	if proxy == nil {
		return nil
	}
	return &anthropicMITMTracker{proxy: proxy}
}

// ClaudeAnthropicSessionCount reports how many tunnel sessions currently
// target an *.anthropic.com host. The predicate inspects both ConnectHost
// (set by raw CONNECT tunnels and TLS-intercepted CONNECT sessions) and
// UpstreamAddr (set by the plain-HTTP MITM path, which carries the full
// upstream URL such as "https://api.anthropic.com"), so all three MITM
// session shapes register through the same filter.
func (t *anthropicMITMTracker) ClaudeAnthropicSessionCount() int {
	if t == nil || t.proxy == nil {
		return 0
	}
	proxy := t.proxy()
	if proxy == nil || proxy.Tunnels == nil {
		return 0
	}
	return proxy.Tunnels.CountByPredicate(func(s livetrack.Session[mitm.TunnelMeta]) bool {
		if hostMatchesSuffix(s.Meta.ConnectHost, anthropicHostSuffix) {
			return true
		}
		return upstreamMatchesSuffix(s.Meta.UpstreamAddr, anthropicHostSuffix)
	})
}

// hostMatchesSuffix returns true when host is the suffix itself or a
// subdomain of it. The function strips an optional port so callers can pass
// values from either ConnectHost (bare host) or [net.SplitHostPort] outputs.
func hostMatchesSuffix(host, suffix string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	if host == suffix {
		return true
	}
	return strings.HasSuffix(host, "."+suffix)
}

// upstreamMatchesSuffix parses upstream as a URL (the plain-HTTP path
// stores a full upstream like "https://api.anthropic.com") and applies the
// host-suffix check to its host component. A bare host without a scheme
// falls back to hostMatchesSuffix so a future change to the registered
// UpstreamAddr shape does not silently lose the signal.
func upstreamMatchesSuffix(upstream, suffix string) bool {
	upstream = strings.TrimSpace(upstream)
	if upstream == "" {
		return false
	}
	if strings.Contains(upstream, "://") {
		parsed, err := url.Parse(upstream)
		if err != nil || parsed.Host == "" {
			return false
		}
		return hostMatchesSuffix(parsed.Host, suffix)
	}
	return hostMatchesSuffix(upstream, suffix)
}
