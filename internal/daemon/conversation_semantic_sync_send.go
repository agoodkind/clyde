package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

// sendDocuments fires one upsert for the collected documents and the full
// manifest. A conflicting-active-job rejection means a prior embed is still
// running, which is expected and benign: the worker logs it and the next pass
// retries once the engine is free. Any other failure is a real error.
func (w *conversationSemanticSyncWorker) sendDocuments(
	ctx context.Context,
	client conversationSemanticClient,
	docs []semsearch.SemDoc,
	manifest []semsearch.Fingerprint,
	sent []deliveredConversation,
	stats *conversationSemanticSyncStats,
) {
	// The upsert can save checkpoints for earlier conversations before a later
	// conversation fails. sendDocuments removes delivery memos before the upsert
	// changes the checkpoint fingerprints.
	for _, delivery := range sent {
		delete(w.deliveredContent, delivery.id)
	}
	upsertStarted := clock.Now()
	upsert := client.UpsertConversationDocuments
	if slicing, sliced := client.(conversationSliceClient); sliced {
		upsert = slicing.UpsertConversationSlice
	}
	jobID, upsertErr := upsert(ctx, w.collectionID, docs, manifest)
	var partial *vectorsearch.PartialUpsertError
	if errors.As(upsertErr, &partial) {
		w.recordPartialDelivery(ctx, partial, sent, stats)
		return
	}
	if upsertErr != nil {
		if isConflictingActiveJob(upsertErr) {
			w.passBlocked = true
			w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.engine_busy",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversations", len(sent),
				"documents", len(docs),
			)
		}
		failure := errors.Join(failuresBesidesActiveJob(upsertErr)...)
		if failure == nil {
			return
		}
		stats.failed += len(sent)
		w.freshness.recordSyncError(failure)
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.upsert_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversations", len(sent),
			"documents", len(docs),
			"err", failure,
		)
		return
	}
	sentIDs := make([]string, 0, len(sent))
	for _, delivery := range sent {
		sentIDs = append(sentIDs, delivery.id)
		if delivery.fingerprint == "" {
			continue
		}
		w.lastCompleted++
		w.deliveredContent[delivery.id] = deliveredContentRecord{
			fingerprint:    delivery.fingerprint,
			projectionHash: delivery.projectionHash,
		}
	}
	stats.sentConversations = len(sent)
	stats.sentConversationIDs = sentIDs
	stats.documents = len(docs)
	w.activeJobID = jobID
	w.freshness.recordUpsert(client, w.collectionID, jobID, upsertStarted, clock.Now())
	w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.upsert_started",
		"concern", "conversation.semantic",
		"component", "daemon",
		"conversations", len(sent),
		"documents", len(docs),
		"job_id", jobID,
	)
}

// recordPartialDelivery records completed checkpoints and progress for the backlog
// loop. It does not memoize partial conversations; they still need ingestion.
// It counts other batch failures and marks the pass as blocked when another client
// is writing a conversation.
func (w *conversationSemanticSyncWorker) recordPartialDelivery(
	ctx context.Context,
	partial *vectorsearch.PartialUpsertError,
	sent []deliveredConversation,
	stats *conversationSemanticSyncStats,
) {
	sentIDs := make([]string, 0, len(sent))
	for _, delivery := range sent {
		if slices.Contains(partial.Failed, delivery.id) || slices.Contains(partial.Unstarted, delivery.id) {
			continue
		}
		sentIDs = append(sentIDs, delivery.id)
		if delivery.fingerprint == "" || !slices.Contains(partial.Completed, delivery.id) {
			continue
		}
		w.lastCompleted++
		w.deliveredContent[delivery.id] = deliveredContentRecord{
			fingerprint:    delivery.fingerprint,
			projectionHash: delivery.projectionHash,
		}
	}
	w.lastSliced = len(sentIDs) > 0
	w.resumeConversation = partial.Incomplete[0]
	stats.sentConversations = len(sentIDs)
	stats.sentConversationIDs = sentIDs
	stats.partialConversations = len(partial.Incomplete)
	failures := make([]error, 0, len(partial.Failures))
	for _, failure := range partial.Failures {
		if isConflictingActiveJob(failure) {
			w.passBlocked = true
			continue
		}
		failures = append(failures, failure)
	}
	stats.failed += len(failures)
	joined := errors.Join(failures...)
	if joined != nil {
		w.freshness.recordSyncError(joined)
	}
	w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.upsert_partial",
		"concern", "conversation.semantic",
		"component", "daemon",
		"completed", boundedConversationIDs(partial.Completed),
		"incomplete", boundedConversationIDs(partial.Incomplete),
		"failed", boundedConversationIDs(partial.Failed),
		"err", joined,
	)
}

// isConflictingActiveJob reports whether an upsert error is the engine's
// rejection of a second job while one is already running for the collection.
func isConflictingActiveJob(err error) bool {
	return err != nil && strings.Contains(err.Error(), "conflicting active job")
}

// failuresBesidesActiveJob lists the errors in err that do not report a conflicting
// active job. The list is empty when every joined error reports that condition.
func failuresBesidesActiveJob(err error) []error {
	if !isConflictingActiveJob(err) {
		return []error{err}
	}
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return nil
	}
	failures := make([]error, 0)
	for _, part := range joined.Unwrap() {
		failures = append(failures, failuresBesidesActiveJob(part)...)
	}
	return failures
}
