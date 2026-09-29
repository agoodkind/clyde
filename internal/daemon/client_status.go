package daemon

import (
	"context"
	"strings"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"google.golang.org/protobuf/types/known/emptypb"
)

// RuntimeStatus is the passive semantic and listener snapshot returned by the daemon.
type RuntimeStatus struct {
	Semantic                 SemanticStatus        `json:"semantic"`
	Listeners                []BoundListenerStatus `json:"listeners"`
	Profiling                *BoundListenerStatus  `json:"profiling"`
	CursorRawIndexingEnabled *bool                 `json:"cursor_raw_indexing_enabled,omitempty"`
}

// SemanticStatus reports effective flags and the existing engine connection.
type SemanticStatus struct {
	IngestionEnabled bool                    `json:"ingestion_enabled"`
	SearchEnabled    bool                    `json:"search_enabled"`
	Connection       SemanticConnectionState `json:"connection"`
	NextRetryUnix    int64                   `json:"next_retry_unix"`
	Attempts         uint64                  `json:"attempts"`
	// Embedded is absent unless the daemon uses the embedded backend.
	Embedded *EmbeddedSemanticStatus `json:"embedded,omitempty"`
}

// EmbeddedSemanticStatus reports the embedded ingestion store after the last
// sync pass: whether the library is open, the pending outbox items, and the
// blocked owners with up to 10 of their conversation IDs.
type EmbeddedSemanticStatus struct {
	LibraryOpen            bool     `json:"library_open"`
	PendingBatches         uint64   `json:"pending_batches"`
	PendingProjections     uint64   `json:"pending_projections"`
	BlockedOwners          uint64   `json:"blocked_owners"`
	BlockedConversationIDs []string `json:"blocked_conversation_ids"`
}

// SemanticConnectionState is the wire enum's lowercase connection-state name.
type SemanticConnectionState string

// BoundListenerStatus identifies an address the daemon already bound.
type BoundListenerStatus struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	Address string `json:"address"`
}

func currentRuntimeStatus(ctx context.Context) (*RuntimeStatus, error) {
	client, err := connectDaemon(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, daemonProbeTimeout)
	defer cancel()
	response, err := client.rpc.GetDaemonStatus(rpcCtx, &emptypb.Empty{})
	if err != nil {
		return nil, daemonRPCError(rpcCtx, "get daemon status", err)
	}
	semantic := response.GetSemantic()
	result := &RuntimeStatus{
		Semantic: SemanticStatus{
			IngestionEnabled: semantic.GetIngestionEnabled(), SearchEnabled: semantic.GetSearchEnabled(),
			Connection:    SemanticConnectionState(strings.ToLower(strings.TrimPrefix(semantic.GetConnection().String(), "SEMANTIC_CONNECTION_STATE_"))),
			NextRetryUnix: semantic.GetNextRetryUnix(), Attempts: semantic.GetAttempts(),
			Embedded: embeddedStatusFromProto(semantic.GetEmbedded()),
		},
		Listeners:                make([]BoundListenerStatus, 0, len(response.GetListeners())),
		Profiling:                nil,
		CursorRawIndexingEnabled: response.CursorRawIndexingEnabled,
	}
	for _, listener := range response.GetListeners() {
		result.Listeners = append(result.Listeners, listenerStatusFromProto(listener))
	}
	if response.GetProfiling() != nil {
		profiling := listenerStatusFromProto(response.GetProfiling())
		result.Profiling = &profiling
	}
	return result, nil
}

func embeddedStatusFromProto(embedded *clydev1.EmbeddedSemanticStatus) *EmbeddedSemanticStatus {
	if embedded == nil {
		return nil
	}
	return &EmbeddedSemanticStatus{
		LibraryOpen:            embedded.GetLibraryOpen(),
		PendingBatches:         embedded.GetPendingBatches(),
		PendingProjections:     embedded.GetPendingProjections(),
		BlockedOwners:          embedded.GetBlockedOwners(),
		BlockedConversationIDs: append([]string{}, embedded.GetBlockedConversationIds()...),
	}
}

func listenerStatusFromProto(listener *clydev1.BoundListenerStatus) BoundListenerStatus {
	return BoundListenerStatus{Name: listener.GetName(), Network: listener.GetNetwork(), Address: listener.GetAddress()}
}
