package daemon

import (
	"net"
	"sort"

	"goodkind.io/clyde/internal/config"
)

// MITMListenerStatus reports one configured MITM listener address and whether
// the daemon currently has that address bound.
type MITMListenerStatus struct {
	ID      string
	Address string
	Up      bool
}

// MITMStatus is the daemon's view of its MITM listeners and CA paths. The
// daemon answers from the listeners it bound rather than by dialing, so a
// half-bound address reads as down.
type MITMStatus struct {
	Listeners  []MITMListenerStatus
	CACertPath string
	CAKeyPath  string
}

// collectMITMStatus reports each configured listener address and marks it up
// when the daemon has that exact address bound in its runtime listener set.
func collectMITMStatus(mitmCfg config.MITMConfig, bound map[string][]net.Listener) MITMStatus {
	ids := make([]string, 0, len(mitmCfg.Listeners))
	for id := range mitmCfg.Listeners {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	listeners := make([]MITMListenerStatus, 0, len(ids))
	for _, id := range ids {
		boundAddrs := make(map[string]bool, len(bound[id]))
		for _, socket := range bound[id] {
			if socket != nil && socket.Addr() != nil {
				boundAddrs[socket.Addr().String()] = true
			}
		}
		listenerCfg := mitmCfg.Listeners[id]
		for _, address := range mitmBindAddrs(listenerCfg.Host, listenerCfg.Port) {
			listeners = append(listeners, MITMListenerStatus{
				ID:      id,
				Address: address,
				Up:      boundAddrs[address],
			})
		}
	}
	return MITMStatus{
		Listeners:  listeners,
		CACertPath: mitmCfg.CA.CertPath,
		CAKeyPath:  mitmCfg.CA.KeyPath,
	}
}
