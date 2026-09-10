package daemon

import (
	"net"
	"sort"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"google.golang.org/grpc/connectivity"
)

func (r *runtimeServices) statusSnapshot() *clydev1.GetDaemonStatusResponse {
	cfg := r.currentConfig.Load()
	semantic := &clydev1.SemanticStatus{
		IngestionEnabled: cfg.Conversation.Semantic.FeedsEngine(),
		SearchEnabled:    cfg.Conversation.Semantic.AnswersSearch(),
		Connection:       clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_DISABLED,
	}
	if cfg.Conversation.Semantic.UsesEngine() {
		semantic.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_UNAVAILABLE
		if r.semantic != nil {
			r.semantic.readStatus(semantic)
		}
	}
	listeners := make([]*clydev1.BoundListenerStatus, 0)
	for name, listener := range map[string]net.Listener{
		listenerNameDaemon:        r.listener,
		listenerNameAdapter:       r.adapterListener,
		listenerNameAdapterCursor: r.adapterCursorListener,
	} {
		if listener != nil {
			listeners = append(listeners, boundListenerStatus(name, listener.Addr()))
		}
	}
	for id, sockets := range r.mitmListeners {
		for _, socket := range sockets {
			listeners = append(listeners, boundListenerStatus("mitm."+id, socket.Addr()))
		}
	}
	for id, sockets := range r.mitmPacketConns {
		for _, socket := range sockets {
			listeners = append(listeners, boundListenerStatus("mitm."+id, socket.LocalAddr()))
		}
	}
	sort.Slice(listeners, func(i, j int) bool {
		if listeners[i].GetName() != listeners[j].GetName() {
			return listeners[i].GetName() < listeners[j].GetName()
		}
		if listeners[i].GetNetwork() != listeners[j].GetNetwork() {
			return listeners[i].GetNetwork() < listeners[j].GetNetwork()
		}
		return listeners[i].GetAddress() < listeners[j].GetAddress()
	})
	var profiling *clydev1.BoundListenerStatus
	if r.pprofListener != nil {
		profiling = boundListenerStatus(listenerNamePProf, r.pprofListener.Addr())
	}
	return &clydev1.GetDaemonStatusResponse{Semantic: semantic, Listeners: listeners, Profiling: profiling}
}

func boundListenerStatus(name string, addr net.Addr) *clydev1.BoundListenerStatus {
	return &clydev1.BoundListenerStatus{Name: name, Network: addr.Network(), Address: addr.String()}
}

func (r *conversationSemanticRuntime) readStatus(snapshot *clydev1.SemanticStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	snapshot.Attempts = r.attempts
	if !r.nextRetry.IsZero() {
		snapshot.NextRetryUnix = r.nextRetry.Unix()
	}
	if r.connecting {
		snapshot.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_CONNECTING
	} else if r.registered {
		snapshot.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_READY
		if r.connectionState != nil {
			switch r.connectionState.GetState() {
			case connectivity.Idle:
				snapshot.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_IDLE
			case connectivity.Connecting:
				snapshot.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_CONNECTING
			case connectivity.Ready:
				snapshot.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_READY
			case connectivity.TransientFailure:
				snapshot.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_UNAVAILABLE
			case connectivity.Shutdown:
				snapshot.Connection = clydev1.SemanticConnectionState_SEMANTIC_CONNECTION_STATE_SHUTDOWN
			}
		}
	}
}
