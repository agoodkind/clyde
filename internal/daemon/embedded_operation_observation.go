package daemon

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/lm-semantic-search/library/observation"
)

type embeddedOperationObserver struct {
	log *slog.Logger
}

func observeEmbeddedPageContext(ctx context.Context, started time.Time, hits, groups int, stats conversation.ContextReadStats, err error) {
	scope := observation.ScopeFromContext(ctx)
	status := "success"
	if err != nil {
		status = "failure"
	}
	slog.InfoContext(ctx, "daemon.conversation_embedded_search.context_page_completed",
		"component", "daemon", "concern", "conversation.semantic",
		"run_id", scope.RunID, "generation_order", scope.Generation,
		"pid", scope.ProcessID, "purpose", string(scope.Purpose),
		"operation_id", scope.OperationID, "parent_operation_id", scope.ParentOperationID,
		"outcome", status, "duration_ns", clock.Since(started).Nanoseconds(),
		"page_hits", hits, "source_groups", groups, "source_reads", stats.SourceReads,
		"messages_visited", stats.MessagesVisited, "messages_retained", stats.MessagesRetained,
		"windows", stats.Windows,
	)
}

func (observer embeddedOperationObserver) Observe(event observation.Event) {
	if event.Phase != observation.Completed {
		return
	}
	observer.log.Info(
		"daemon.conversation_semantic_embedded.operation_completed",
		"concern", "conversation.semantic", "component", "daemon",
		"run_id", event.Scope.RunID, "generation_order", event.Scope.Generation,
		"pid", event.Scope.ProcessID, "operation_id", event.Scope.OperationID,
		"parent_operation_id", event.Scope.ParentOperationID,
		"purpose", string(event.Scope.Purpose), "operation", string(event.Operation),
		"outcome", string(event.Outcome), "duration_ns", event.Duration.Nanoseconds(),
		"started_unix_nano", event.StartedAt.UnixNano(),
		"stage_rows", event.Data.Stage.Rows, "committed_receipt", event.Data.Stage.CommittedReceipt,
		"embedding_attempt", event.Data.Embedding.Attempt,
		"embedding_requested", event.Data.Embedding.Requested,
		"embedding_returned", event.Data.Embedding.Returned,
		"embedding_validated", event.Data.Embedding.Validated,
		"validation_boundary", string(event.Data.Embedding.Validation),
		"identity_duplicate_inputs", event.Data.Identity.DuplicateInputs,
		"identity_verified_reuse", event.Data.Identity.VerifiedReuse,
		"identity_pending_reuse", event.Data.Identity.PendingReuse,
		"identity_missing", event.Data.Identity.Missing,
		"identity_second_lookup", event.Data.Identity.SecondLookup,
		"transaction_boundary", string(event.Data.Transaction.Boundary),
		"transaction_rollback_failed", event.Data.Transaction.RollbackFailed,
		"vector_requested", event.Data.Vector.Requested,
		"vector_acknowledged", event.Data.Vector.Acknowledged,
		"vector_verified", event.Data.Vector.Verified,
		"vector_client_search_duration_ns", event.Data.Vector.ClientSearchDuration.Nanoseconds(),
		"vector_local_verification_duration_ns", event.Data.Vector.LocalVerificationDuration.Nanoseconds(),
	)
}

func embeddedGenerationObservationContext(ctx context.Context, batch embeddedOutboxBatch) context.Context {
	scope := observation.ScopeFromContext(ctx)
	if scope.RunID == "" {
		scope.RunID = batch.BatchID
	}
	scope.Generation = batch.GenerationOrder
	scope.ProcessID = os.Getpid()
	scope.Purpose = observation.Ingestion
	return observation.WithScope(ctx, scope)
}

func embeddedQueryObservationContext(ctx context.Context) context.Context {
	scope := observation.Scope{
		RunID: uuid.NewString(), Generation: 0, ProcessID: os.Getpid(),
		OperationID: 0, ParentOperationID: 0, Purpose: observation.Query,
	}
	return observation.WithScope(ctx, scope)
}
