package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

// errEmbeddedReconcileBackend reports a reconcile request under a
// configuration that does not use the embedded backend.
var errEmbeddedReconcileBackend = errors.New("embedded conversation reconciliation requires conversation.semantic.backend = \"embedded\"")

// ReconcileEmbeddedConversation reconciles the outbox state of one
// conversation with the embedded search library. It loads the configuration,
// refreshes the conversation index, opens the embedded store, aborts the
// unfinished tokens of the owner, rebuilds its committed fields and owner
// metadata from the published rows, and clears its blocked state. The daemon
// applies the current record metadata through ReprojectScalars on its next
// pass.
func ReconcileEmbeddedConversation(ctx context.Context, output io.Writer, conversationID string) error {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.reconcile_config_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("load config: %w", err)
	}
	semantic := cfg.Conversation.Semantic
	if semantic.Backend != config.ConversationSemanticBackendEmbedded {
		return errEmbeddedReconcileBackend
	}
	index := conversation.NewIndex(newConversationRegistry(), cfg.Conversation)
	if err := index.Refresh(ctx); err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.reconcile_refresh_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("refresh conversation index: %w", err)
	}
	log := slog.Default()
	store, err := openEmbeddedConversationStore(ctx, semantic, conversationSemanticOutboxPath(semantic.PoolID), log)
	if err != nil {
		return err
	}
	result, reconcileErr := reconcileEmbeddedConversationWithStore(ctx, store, index, conversationID)
	closeErr := store.close(context.WithoutCancel(ctx))
	if err := errors.Join(reconcileErr, closeErr); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Reconciled %s: %d published rows, projection order %d, %d aborted tokens.\n",
		strings.TrimSpace(conversationID), result.committedRows, result.projectionOrder, result.abortedTokens); err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.reconcile_write_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("write reconcile result: %w", err)
	}
	return nil
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
