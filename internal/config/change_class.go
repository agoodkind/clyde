package config

import "reflect"

// Route is the lifecycle decision for one config change. It is consumed by the
// watcher and the in-process apply path so the
// hot-apply-vs-reload-vs-rebind-vs-restart decision lives in one place.
type Route uint8

const (
	// RouteHotApply means every changed field is safe to apply in process: the
	// daemon swaps the affected config-derived state without re-exec.
	RouteHotApply Route = iota
	// RouteReload means a zero-bind-gap re-exec is required (state the new
	// generation must reopen, e.g. capture db path, or any change not in the
	// hot set), but listeners are unchanged.
	RouteReload
	// RouteRebind means a listener address moved; ports must be rebound.
	RouteRebind
	// RouteRestartRequired means a change the running generation cannot honor
	// without a full restart (enabling or disabling a whole surface).
	RouteRestartRequired
)

// String returns a stable label for logs.
func (r Route) String() string {
	switch r {
	case RouteHotApply:
		return "hot_apply"
	case RouteReload:
		return "reload"
	case RouteRebind:
		return "rebind"
	case RouteRestartRequired:
		return "restart_required"
	default:
		return "unknown"
	}
}

// ClassifyConfigChange compares old and new config and returns the most
// disruptive route any changed field requires. The classifier is conservative:
// a changed field that is not explicitly in the hot set routes to reload, never
// silently no-op. The escalation order is RestartRequired > Rebind > Reload >
// HotApply.
//
// The v1 hot set, the only fields the in-process apply path knows how to swap
// without re-exec, is:
//   - adapter: Models, Families, Pricing, DefaultModel, PassthroughOverrides,
//     OpenAICompatPassthrough (the model-registry inputs; the daemon rebuilds
//     and atomically swaps the adapter model registry)
//   - mitm: Providers, Drift (the proxy swaps its config-derived routing)
//
// Every other field change routes to reload (or rebind/restart for listener and
// surface-toggle changes).
func ClassifyConfigChange(oldCfg, newCfg Config) Route {
	if oldCfg.Daemon.GRPCAddress != newCfg.Daemon.GRPCAddress {
		return RouteRestartRequired
	}
	if oldCfg.Adapter.Enabled != newCfg.Adapter.Enabled {
		return RouteRestartRequired
	}
	if oldCfg.MITM.EnabledDefault != newCfg.MITM.EnabledDefault {
		return RouteRestartRequired
	}
	if listenerTopologyMoved(oldCfg, newCfg) {
		return RouteRebind
	}
	if reloadOnlyStateChanged(oldCfg, newCfg) {
		return RouteReload
	}
	if onlyHotFieldsChanged(oldCfg, newCfg) {
		return RouteHotApply
	}
	return RouteReload
}

// listenerTopologyMoved reports whether any bound-listener address changed, in
// which case the new worker must rebind fresh.
func listenerTopologyMoved(oldCfg, newCfg Config) bool {
	if oldCfg.Adapter.Host != newCfg.Adapter.Host ||
		oldCfg.Adapter.Port != newCfg.Adapter.Port ||
		oldCfg.Adapter.CursorIngressPort != newCfg.Adapter.CursorIngressPort {
		return true
	}
	if oldCfg.Debug.PProfAddr != newCfg.Debug.PProfAddr {
		return true
	}
	return mitmListenersMoved(oldCfg.MITM, newCfg.MITM)
}

// mitmListenersMoved reports whether the MITM listener set or any listener's
// host/port changed, which requires a fresh bind.
func mitmListenersMoved(oldMITM, newMITM MITMConfig) bool {
	if len(oldMITM.Listeners) != len(newMITM.Listeners) {
		return true
	}
	for id, oldListener := range oldMITM.Listeners {
		newListener, ok := newMITM.Listeners[id]
		if !ok {
			return true
		}
		if oldListener.Host != newListener.Host || oldListener.Port != newListener.Port {
			return true
		}
	}
	return false
}

// reloadOnlyStateChanged reports whether a field changed that the running
// generation cannot swap in process but does not move a listener, so a
// zero-bind-gap reload (re-exec inheriting the open sockets) is required. v1
// covers the capture-store db path and the MITM CA cert/key paths, which the
// new generation must reopen.
func reloadOnlyStateChanged(oldCfg, newCfg Config) bool {
	if oldCfg.MITM.CaptureStore.DBPath != newCfg.MITM.CaptureStore.DBPath {
		return true
	}
	if oldCfg.MITM.CA.CertPath != newCfg.MITM.CA.CertPath ||
		oldCfg.MITM.CA.KeyPath != newCfg.MITM.CA.KeyPath {
		return true
	}
	return false
}

// onlyHotFieldsChanged reports whether the sole differences between oldCfg and
// newCfg lie in the hot set. It blanks the hot-set fields on copies of both
// configs and reports whether the remainder is identical: if so, every changed
// field is hot-appliable.
func onlyHotFieldsChanged(oldCfg, newCfg Config) bool {
	oldBlank := blankHotFields(oldCfg)
	newBlank := blankHotFields(newCfg)
	if reflect.DeepEqual(oldBlank, newBlank) {
		// The non-hot remainder is identical. Require that at least one hot
		// field actually changed so an identical config is not classified as a
		// (no-op) hot apply by a caller that already filtered identical hashes.
		return !reflect.DeepEqual(oldCfg, newCfg)
	}
	return false
}

// blankHotFields returns a copy of cfg with every hot-set field zeroed, so two
// configs that differ only in the hot set compare equal. The set is kept narrow
// and exactly matches what the in-process apply swaps: the adapter
// model-registry inputs and the MITM proxy provider routing. MITM drift is
// deliberately excluded so a drift change takes a reload, where the periodic
// drift sweep restarts with the new config rather than running stale.
func blankHotFields(cfg Config) Config {
	cfg.Adapter.Models = nil
	cfg.Adapter.Families = nil
	cfg.Adapter.Pricing = nil
	cfg.Adapter.DefaultModel = ""
	cfg.Adapter.PassthroughOverrides = nil
	cfg.Adapter.OpenAICompatPassthrough = AdapterOpenAICompatPassthrough{BaseURL: "", APIKey: "", APIKeyEnv: "", Model: ""}
	cfg.MITM.Providers = nil
	return cfg
}
