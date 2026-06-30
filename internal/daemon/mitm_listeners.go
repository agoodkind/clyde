package daemon

import (
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"

	"goodkind.io/clyde/internal/config"
)

// mitmListenerKey returns the reload/listener-map key for a MITM listener id.
func mitmListenerKey(id string) string {
	return listenerNameMITMPrefix + id
}

// mitmSocketKey returns the reload spec name for one physical MITM socket. A
// dual-stack "localhost" listener binds two sockets under the same id, so the
// bound address disambiguates them in the inherited-file-descriptor map.
func mitmSocketKey(id, addr string) string {
	return mitmListenerKey(id) + "#" + addr
}

// mitmBindAddrs expands a configured MITM listener host into the concrete
// loopback addresses the daemon binds. "localhost" binds both loopback stacks
// ([::1] and 127.0.0.1) so a client reaches the proxy over IPv6 or IPv4; an
// explicit IP host binds exactly that one address. IPv6 is listed first for a
// deterministic reload spec order.
func mitmBindAddrs(host string, port int) []string {
	portStr := strconv.Itoa(port)
	normalized := normalizeListenHost(host)
	if normalized == "localhost" {
		return []string{
			net.JoinHostPort("::1", portStr),
			net.JoinHostPort("127.0.0.1", portStr),
		}
	}
	return []string{net.JoinHostPort(normalized, portStr)}
}

// validateMITMListenerSet rejects a zero-bind-gap reload whenever the configured
// MITM listener set differs from the running set by id or bound address. A
// reload inherits one file descriptor per running socket but cannot add, remove,
// or re-address a listener, so any of those changes requires a full restart. A
// "localhost" listener owns two sockets, so each listener is compared by its
// whole bound-address set rather than a single address.
func validateMITMListenerSet(runtime *runtimeServices, cfg *config.Config) error {
	if len(runtime.mitmListeners) == 0 {
		return nil
	}
	configured := make(map[string][]string, len(cfg.MITM.Listeners))
	for id, listenerCfg := range cfg.MITM.Listeners {
		configured[id] = mitmBindAddrs(listenerCfg.Host, listenerCfg.Port)
	}
	if len(configured) != len(runtime.mitmListeners) {
		slog.Warn("daemon.reload.mitm_listener_set_changed", "concern", "daemon.workers.reload", "component", "daemon",
			"configured", len(configured), "running", len(runtime.mitmListeners))
		return fmt.Errorf("mitm listener set changed; full daemon restart required")
	}
	for id, sockets := range runtime.mitmListeners {
		wantAddrs, ok := configured[id]
		if !ok {
			slog.Warn("daemon.reload.mitm_listener_removed", "concern", "daemon.workers.reload", "component", "daemon", "listener_id", id)
			return fmt.Errorf("mitm listener %q removed; full daemon restart required", id)
		}
		gotAddrs := make([]string, 0, len(sockets))
		for _, socket := range sockets {
			gotAddrs = append(gotAddrs, socket.Addr().String())
		}
		if !equalStringSet(gotAddrs, wantAddrs) {
			slog.Warn("daemon.reload.mitm_listener_address_changed", "concern", "daemon.workers.reload", "component", "daemon",
				"listener_id", id, "running", gotAddrs, "configured", wantAddrs)
			return fmt.Errorf("mitm listener %q address changed from %v to %v; full daemon restart required", id, gotAddrs, wantAddrs)
		}
	}
	return nil
}

// equalStringSet reports whether two string slices contain the same elements
// regardless of order, used to compare a MITM listener's bound-address set
// against its configured set during reload validation.
func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := slices.Clone(a)
	bc := slices.Clone(b)
	slices.Sort(ac)
	slices.Sort(bc)
	return slices.Equal(ac, bc)
}
