package daemon

// MITM OAuth rotator wiring.
//
// The MITM proxy boots before the adapter (and therefore before the OAuth
// rotator is constructed), so [mitm.NewProxy] cannot take the rotator as a
// constructor argument without inverting the daemon's startup order. The
// wiring here registers a one-shot receiver against the shared rotator
// holder; the receiver fires the moment startAdapter publishes the rotator
// and calls [mitm.Proxy.SetRotatorSink] so subsequent Anthropic requests
// flowing through the proxy carry rotator-owned bearers.

import (
	"fmt"
	"log/slog"
	"net"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/oauthrotation"
)

// newMITMProxyWithRotator constructs the daemon-owned MITM proxy and
// registers the OAuth-rotator receiver in one call so startMITM in run.go
// stays a single-line wiring site (CLYDE-OAUTH). The proxy is identical
// to one built by [mitm.NewProxy]; the only side effect on top is that
// the daemon's shared OAuth rotator (when ready) is pushed into
// [mitm.Proxy.SetRotatorSink] so the proxy can swap bearer tokens on the
// Anthropic forwarding paths.
func newMITMProxyWithRotator(cfg config.MITMConfig, logging config.LoggingRequest, log *slog.Logger, listener net.Listener) (*mitm.Proxy, error) {
	proxy, err := mitm.NewProxy(cfg, logging, log, listener)
	if err != nil {
		log.Error("mitm.new_proxy_failed",
			"component", "daemon",
			"err", err,
		)
		return nil, fmt.Errorf("mitm new proxy: %w", err)
	}
	registerOAuthRotatorReceiver(func(rotator *oauthrotation.Rotator) {
		if rotator == nil {
			proxy.SetRotatorSink(nil)
			return
		}
		proxy.SetRotatorSink(mitm.RotatorSink(rotator))
	})
	return proxy, nil
}
