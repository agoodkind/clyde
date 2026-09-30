package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// errEmbeddedReconcileUnavailable reports a reconcile request to a daemon
// without a running embedded ingestion worker.
var errEmbeddedReconcileUnavailable = errors.New("the daemon runs no embedded conversation ingestion worker; conversation.semantic.backend must be \"embedded\" with ingestion enabled")

// embeddedReconcileGate gives the daemon control socket access to the running
// embedded ingestion worker. The worker attaches its state when it starts and
// detaches it after its last pass.
type embeddedReconcileGate struct {
	mu       sync.Mutex
	embedded *embeddedConversationSync
	log      *slog.Logger
}

func newEmbeddedReconcileGate(log *slog.Logger) *embeddedReconcileGate {
	if log == nil {
		log = slog.Default()
	}
	return &embeddedReconcileGate{mu: sync.Mutex{}, embedded: nil, log: log}
}

func (gate *embeddedReconcileGate) attach(embedded *embeddedConversationSync) {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.embedded = embedded
}

func (gate *embeddedReconcileGate) detach(embedded *embeddedConversationSync) {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.embedded == embedded {
		gate.embedded = nil
	}
}

// reconcile reconciles one conversation in the running worker. It locks the
// worker store after the current sync pass ends and unlocks it after the
// reconciliation.
func (gate *embeddedReconcileGate) reconcile(ctx context.Context, conversationID string) (result embeddedReconcileResult, resultErr error) {
	var embedded *embeddedConversationSync
	log := slog.Default()
	if gate != nil {
		gate.mu.Lock()
		embedded = gate.embedded
		log = gate.log
		gate.mu.Unlock()
	}
	if embedded == nil || !embedded.semantic.FeedsEngine() {
		return embeddedReconcileResult{}, errEmbeddedReconcileUnavailable
	}
	embedded.mu.Lock()
	defer embedded.mu.Unlock()
	store, err := embedded.ensureStore(ctx, log)
	if err != nil {
		return embeddedReconcileResult{}, err
	}
	lock, err := lockConversationSemanticOutbox(ctx, embedded.outboxPath)
	if err != nil {
		return embeddedReconcileResult{}, err
	}
	store.outbox.lock = lock
	defer func() {
		resultErr = errors.Join(resultErr, store.outbox.releaseLock())
	}()
	return reconcileEmbeddedConversationWithStore(ctx, store, embedded.records, conversationID)
}

// ReconcileEmbeddedConversation asks the running daemon to reconcile the
// embedded ingestion outbox state of one conversation with the embedded search
// library. The daemon runs the reconciliation between two sync passes with
// the store that its worker owns. The daemon applies the current record
// metadata through ReprojectScalars on its next pass.
func ReconcileEmbeddedConversation(ctx context.Context, output io.Writer, conversationID string) error {
	client, err := connectDaemon(ctx)
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.reconcile_connect_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("reconcile runs inside the running daemon; connect to the daemon: %w", err)
	}
	defer func() { _ = client.conn.Close() }()
	response, err := client.rpc.ReconcileEmbeddedConversation(ctx, &clydev1.ReconcileEmbeddedConversationRequest{
		ConversationId: strings.TrimSpace(conversationID),
	})
	if err != nil {
		return daemonRPCError(ctx, "reconcile embedded conversation", err)
	}
	if _, err := fmt.Fprintf(output, "Reconciled %s: %d published rows, projection order %d, %d aborted tokens.\n",
		strings.TrimSpace(conversationID), response.GetPublishedRows(), response.GetProjectionOrder(), response.GetAbortedTokens()); err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.reconcile_write_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("write reconcile result: %w", err)
	}
	return nil
}

// ReconcileEmbeddedConversation reconciles one conversation in the embedded
// ingestion worker of this daemon.
func (s *controlServer) ReconcileEmbeddedConversation(
	ctx context.Context,
	request *clydev1.ReconcileEmbeddedConversationRequest,
) (*clydev1.ReconcileEmbeddedConversationResponse, error) {
	client, _ := peer.FromContext(ctx)
	conversationID := strings.TrimSpace(request.GetConversationId())
	if conversationID == "" {
		return nil, status.Error(codes.InvalidArgument, "conversation_id is required")
	}
	result, err := s.embeddedReconcile.reconcile(ctx, conversationID)
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.reconcile_request_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"peer", peerString(client),
			"conversation_id", conversationID,
			"err", err,
		)
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &clydev1.ReconcileEmbeddedConversationResponse{
		PublishedRows:   int64(result.committedRows),
		ProjectionOrder: result.projectionOrder,
		AbortedTokens:   int64(result.abortedTokens),
	}, nil
}

// reconcileEmbeddedConversationWithStore finds one conversation record in the
// unfiltered index listing and reconciles its owner state in store.
func reconcileEmbeddedConversationWithStore(
	ctx context.Context,
	store *embeddedConversationStore,
	records embeddedRecordLister,
	conversationID string,
) (embeddedReconcileResult, error) {
	conversationID = strings.TrimSpace(conversationID)
	stampedRecords, err := records.ListAllWithStamps(ctx)
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.reconcile_list_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return embeddedReconcileResult{}, fmt.Errorf("list conversation records: %w", err)
	}
	for _, stampedRecord := range stampedRecords {
		if stampedRecord.Record.ID != conversationID {
			continue
		}
		listed, err := store.library.ListOwnerOccurrences(ctx, store.namespace.ID, conversationID)
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.owner_occurrences_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", conversationID,
				"err", err,
			)
			return embeddedReconcileResult{}, fmt.Errorf("list published rows of %s: %w", conversationID, err)
		}
		return store.delivery.reconcileOwner(ctx, store.namespace.ID, stampedRecord.Record, listed)
	}
	return embeddedReconcileResult{}, fmt.Errorf("conversation %q is not in the conversation index", conversationID)
}
