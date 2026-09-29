package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/transcript"
)

// embeddedStoreCloseTimeout bounds the Milvus client close after the embedded
// ingestion worker stops.
const embeddedStoreCloseTimeout = 5 * time.Second

// embeddedConversationSync is the embedded backend state of the semantic sync
// worker. The worker opens the store on its first pass, retries a failed open
// on the next pass, and closes the store after its last pass.
type embeddedConversationSync struct {
	semantic   config.ConversationSemanticConfig
	outboxPath string
	store      *embeddedConversationStore
	// processed maps a conversation ID to the content fingerprint at which the
	// outbox records every selected field of the conversation as committed. A
	// pass skips a conversation at that fingerprint without reading its
	// artifact. The map is in memory. After a restart the first pass reads
	// each conversation once and sends no row for a committed field.
	processed map[string]string
	// changedCommitted counts the fields, since the worker started, with a
	// digest that differs from the committed digest under the same key. The
	// committed occurrence stays and no row is sent for the key.
	changedCommitted int
}

func newEmbeddedConversationSync(semantic config.ConversationSemanticConfig, outboxPath string) *embeddedConversationSync {
	return &embeddedConversationSync{
		semantic:         semantic,
		outboxPath:       outboxPath,
		store:            nil,
		processed:        make(map[string]string),
		changedCommitted: 0,
	}
}

// ensureStore opens the store when no open store exists.
func (embedded *embeddedConversationSync) ensureStore(ctx context.Context, log *slog.Logger) (*embeddedConversationStore, error) {
	if embedded.store != nil {
		return embedded.store, nil
	}
	store, err := openEmbeddedConversationStore(ctx, embedded.semantic, embedded.outboxPath, log)
	if err != nil {
		return nil, err
	}
	embedded.store = store
	return store, nil
}

// closeStore closes the open store after the worker stops.
func (embedded *embeddedConversationSync) closeStore(ctx context.Context) {
	if embedded.store == nil {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), embeddedStoreCloseTimeout)
	defer cancel()
	if err := embedded.store.close(closeCtx); err == nil {
		embedded.store = nil
	}
}

// admits applies the conversation admission settings: indexed providers,
// archived conversations, and subagent conversations.
func (embedded *embeddedConversationSync) admits(record conversation.Record) bool {
	if len(embedded.semantic.IndexedProviders) > 0 && !slices.Contains(embedded.semantic.IndexedProviders, record.Provider.String()) {
		return false
	}
	if record.Archived && !embedded.semantic.IncludeArchived {
		return false
	}
	if record.IsSubagent() && !embedded.semantic.IncludeSubagents {
		return false
	}
	return true
}

// admittedFields keeps the fields of the indexed roles. An empty role list
// admits every role.
func (embedded *embeddedConversationSync) admittedFields(fields []searchbackend.Field) []searchbackend.Field {
	if len(embedded.semantic.IndexedRoles) == 0 {
		return fields
	}
	admitted := make([]searchbackend.Field, 0, len(fields))
	for _, field := range fields {
		if slices.Contains(embedded.semantic.IndexedRoles, field.Role) {
			admitted = append(admitted, field)
		}
	}
	return admitted
}

// startEmbeddedConversationSemanticSync starts the sync worker for the
// embedded backend when ingestion is enabled. The worker goroutine closes the
// store after its last pass, before the lifecycle hook observes the worker
// stop. It reports whether a worker started.
func startEmbeddedConversationSemanticSync(
	ctx context.Context,
	log *slog.Logger,
	semantic config.ConversationSemanticConfig,
	index conversationSemanticIndex,
	freshness *conversationSemanticFreshness,
	group *livetrack.Group,
	contentKinds conversation.ContentKindSet,
) bool {
	if !semantic.FeedsEngine() {
		return false
	}
	if log == nil {
		log = slog.Default()
	}
	workerCtx, done, owned := installConversationSemanticSyncStop(ctx, group, log)
	if !owned {
		log.WarnContext(ctx, "daemon.conversation_semantic_sync.start_skipped_unowned",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", semantic.CollectionID,
		)
		return false
	}
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, log, contentKinds)
	worker.freshness = freshness
	worker.embedded = newEmbeddedConversationSync(semantic, conversationSemanticOutboxPath(semantic.PoolID))
	go func() {
		defer close(done)
		defer worker.embedded.closeStore(ctx)
		defer func() {
			if recovered := recover(); recovered != nil {
				worker.log.ErrorContext(workerCtx, "daemon.conversation_semantic_sync.panic",
					"concern", "conversation.semantic",
					"component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		worker.run(workerCtx)
	}()
	return true
}

// embeddedCandidate is one admitted conversation that the worker has not
// processed at its current fingerprint.
type embeddedCandidate struct {
	record      conversation.Record
	stamp       conversation.FileStamp
	fingerprint string
}

// embeddedSyncStats reports one embedded pass by phase: source reading,
// projection, embedding, persistence, and searchable completion.
type embeddedSyncStats struct {
	admitted         int
	needed           int
	completed        int
	completedIDs     []string
	deferred         int
	pendingBlocked   int
	sourceRead       int
	sourceBytes      int64
	sourceFailed     int
	failedSuppressed int
	projectionFailed int
	fields           int
	newFields        int
	unchangedFields  int
	changedCommitted int
	withheldFields   int
	policySkipped    int
	rows             int
	replayed         int
	deliveryFailed   int
	delivery         embeddedDeliveryCounts
}

// runEmbeddedPass replays pending outbox batches, then projects every admitted
// conversation that changed since the worker processed it, selects the fields
// that the outbox has not committed, and delivers one generation per
// conversation with new rows. A conversation that the index no longer lists
// keeps every committed occurrence.
func (w *conversationSemanticSyncWorker) runEmbeddedPass(ctx context.Context) error {
	store, err := w.embedded.ensureStore(ctx, w.log)
	if err != nil {
		return err
	}
	replay, err := store.delivery.replayPending(ctx)
	if err != nil {
		return err
	}
	var stats embeddedSyncStats
	stats.replayed = replay.replayed
	stats.delivery = replay.counts
	stampedRecords, err := w.index.ListWithStamps(ctx)
	if err != nil {
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.list_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("list conversation records with stamps: %w", err)
	}
	candidates := w.embeddedCandidates(stampedRecords, &stats)
	w.deliverEmbeddedCandidates(ctx, store, candidates, replay.blockedOwners, &stats)
	w.logEmbeddedPass(ctx, stats)
	return nil
}

// embeddedCandidates returns the admitted conversations that need a pass,
// sorted and rotated after the delivery cursor. It skips conversations
// processed at their current fingerprint and conversations suppressed after
// repeated failures at their current fingerprint.
func (w *conversationSemanticSyncWorker) embeddedCandidates(
	stampedRecords []conversation.StampedRecord,
	stats *embeddedSyncStats,
) []embeddedCandidate {
	seen := make(map[string]bool, len(stampedRecords))
	candidatesByID := make(map[string]embeddedCandidate)
	for _, stampedRecord := range stampedRecords {
		conversationID := strings.TrimSpace(stampedRecord.Record.ID)
		if conversationID == "" || seen[conversationID] {
			continue
		}
		seen[conversationID] = true
		if !w.embedded.admits(stampedRecord.Record) {
			continue
		}
		stats.admitted++
		fingerprint := conversation.ContentFingerprint(stampedRecord.Record, stampedRecord.Stamp)
		if w.embedded.processed[conversationID] == fingerprint {
			continue
		}
		if record, failed := w.failedLoad[conversationID]; failed {
			if record.fingerprint != fingerprint {
				delete(w.failedLoad, conversationID)
			} else if record.failures >= failedLoadSuppressThreshold {
				stats.failedSuppressed++
				continue
			}
		}
		candidatesByID[conversationID] = embeddedCandidate{record: stampedRecord.Record, stamp: stampedRecord.Stamp, fingerprint: fingerprint}
	}
	for conversationID := range w.embedded.processed {
		if !seen[conversationID] {
			delete(w.embedded.processed, conversationID)
		}
	}
	w.pruneFailedLoad(seen)
	ids := make([]string, 0, len(candidatesByID))
	for conversationID := range candidatesByID {
		ids = append(ids, conversationID)
	}
	sort.Strings(ids)
	ids = rotateAfter(ids, w.deliveryCursor)
	candidates := make([]embeddedCandidate, 0, len(ids))
	for _, conversationID := range ids {
		candidates = append(candidates, candidatesByID[conversationID])
	}
	stats.needed = len(candidates)
	return candidates
}

// deliverEmbeddedCandidates processes candidates in order and stops when the
// raw artifact bytes of the batch meet the batch target. A conversation that
// changed within the last interval waits for a later pass. A conversation
// with a pending batch that the replay did not acknowledge waits too.
func (w *conversationSemanticSyncWorker) deliverEmbeddedCandidates(
	ctx context.Context,
	store *embeddedConversationStore,
	candidates []embeddedCandidate,
	blockedOwners map[string]bool,
	stats *embeddedSyncStats,
) {
	var artifactBytes int64
	loaded := 0
	for _, candidate := range candidates {
		if semanticSyncContextDone(ctx) {
			return
		}
		if blockedOwners[candidate.record.ID] {
			stats.pendingBlocked++
			continue
		}
		if w.isActivelyGrowing(candidate.stamp) {
			stats.deferred++
			continue
		}
		if loaded > 0 && artifactBytes+candidate.stamp.Size > conversationSemanticBatchBytes {
			return
		}
		loaded++
		artifactBytes += candidate.stamp.Size
		w.deliverEmbeddedCandidate(ctx, store, candidate, stats)
		w.deliveryCursor = candidate.record.ID
		if artifactBytes >= conversationSemanticBatchBytes {
			return
		}
	}
}

// deliverEmbeddedCandidate builds and delivers the generation of one
// conversation. The conversation counts as processed at its fingerprint when
// the outbox records every selected field as committed and the projection
// withheld no open trailing field.
func (w *conversationSemanticSyncWorker) deliverEmbeddedCandidate(
	ctx context.Context,
	store *embeddedConversationStore,
	candidate embeddedCandidate,
	stats *embeddedSyncStats,
) {
	generation, withheld, err := w.buildEmbeddedGeneration(ctx, store, candidate, stats)
	if err != nil {
		return
	}
	if generation != nil {
		counts, err := store.delivery.deliver(ctx, *generation)
		stats.delivery.add(counts)
		if err != nil {
			stats.deliveryFailed++
			return
		}
	}
	stats.completed++
	stats.completedIDs = append(stats.completedIDs, candidate.record.ID)
	if withheld == 0 {
		w.embedded.processed[candidate.record.ID] = candidate.fingerprint
	}
}

// buildEmbeddedGeneration loads one conversation, selects its new fields, and
// prepares their occurrences as one generation. It returns a nil generation
// when no field is new, and the count of withheld open trailing fields. A
// source read failure or a projection failure counts toward failed-load
// suppression at the conversation fingerprint.
func (w *conversationSemanticSyncWorker) buildEmbeddedGeneration(
	ctx context.Context,
	store *embeddedConversationStore,
	candidate embeddedCandidate,
	stats *embeddedSyncStats,
) (*embeddedGeneration, int, error) {
	record := candidate.record
	messages, err := w.index.LoadMessagesWithOptions(record, SemanticConversationLoadOptions(w.contentKinds))
	if err != nil {
		stats.sourceFailed++
		w.recordLoadFailure(record.ID, candidate.fingerprint)
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.load_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", record.ID,
			"provider", record.Provider.String(),
			"err", err,
		)
		return nil, 0, fmt.Errorf("load conversation messages for %s: %w", record.ID, err)
	}
	stats.sourceRead++
	stats.sourceBytes += candidate.stamp.Size
	delete(w.failedLoad, record.ID)
	fields, withheld, err := w.selectEmbeddedFields(ctx, store, candidate, messages, stats)
	if err != nil || len(fields) == 0 {
		return nil, withheld, err
	}
	owner := newEmbeddedConversationOwner(record, w.contentKinds)
	rows, err := embeddedOutboxRows(ctx, store.namespace, owner, fields)
	if err != nil {
		stats.projectionFailed++
		w.recordLoadFailure(record.ID, candidate.fingerprint)
		return nil, withheld, err
	}
	stats.rows += len(rows)
	generation, err := store.delivery.prepareGeneration(ctx, embeddedOutboxBatch{
		BatchID:           "",
		Namespace:         store.namespace.ID,
		OwnerID:           record.ID,
		GenerationOrder:   0,
		SourcePath:        record.ArtifactPath,
		SourceStamp:       candidate.fingerprint,
		ProjectionProfile: owner.ProjectionProfile,
		RowCount:          0,
		ManifestHash:      "",
	}, rows)
	if err != nil {
		stats.deliveryFailed++
		return nil, withheld, err
	}
	return &generation, withheld, nil
}

// selectEmbeddedFields projects the loaded messages, keeps the fields of the
// indexed roles, and returns the fields with keys that the outbox has not
// committed. It also returns the count of withheld open trailing fields.
func (w *conversationSemanticSyncWorker) selectEmbeddedFields(
	ctx context.Context,
	store *embeddedConversationStore,
	candidate embeddedCandidate,
	messages []transcript.Message,
	stats *embeddedSyncStats,
) ([]searchbackend.Field, int, error) {
	record := candidate.record
	artifactSettled := !w.isActivelyGrowing(candidate.stamp)
	projected, built, err := projectEmbeddedConversationFields(record, messages, w.contentKinds, artifactSettled)
	if err != nil {
		stats.projectionFailed++
		w.recordLoadFailure(record.ID, candidate.fingerprint)
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.projection_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", record.ID,
			"err", err,
		)
		return nil, 0, fmt.Errorf("project fields of %s: %w", record.ID, err)
	}
	stats.policySkipped += built.PolicySkipped
	stats.withheldFields += projected.WithheldOpenFields
	fields := w.embedded.admittedFields(projected.Fields)
	stats.fields += len(fields)
	committed, err := store.outbox.committedFields(ctx, store.namespace.ID, record.ID)
	if err != nil {
		stats.deliveryFailed++
		return nil, projected.WithheldOpenFields, err
	}
	selection := searchbackend.SelectNewFields(fields, committed)
	stats.newFields += len(selection.New)
	stats.unchangedFields += selection.Unchanged
	stats.changedCommitted += selection.ChangedCommitted
	w.embedded.changedCommitted += selection.ChangedCommitted
	return selection.New, projected.WithheldOpenFields, nil
}

// embeddedOutboxRows prepares the occurrences of each field and validates
// every occurrence against the namespace declaration before the outbox
// records it.
func embeddedOutboxRows(
	ctx context.Context,
	namespace library.NamespaceSpec,
	owner embeddedConversationOwner,
	fields []searchbackend.Field,
) ([]embeddedOutboxRow, error) {
	rows := make([]embeddedOutboxRow, 0, len(fields))
	for _, field := range fields {
		occurrences, err := embeddedFieldOccurrences(ctx, owner, field)
		if err != nil {
			return nil, err
		}
		for _, occurrence := range occurrences {
			if err := namespace.ValidateOccurrence(occurrence); err != nil {
				slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.occurrence_invalid",
					"concern", "conversation.semantic",
					"component", "daemon",
					"conversation_id", owner.ConversationID,
					"row_key", occurrence.RowKey,
					"err", err,
				)
				return nil, fmt.Errorf("validate occurrence %s of %s: %w", occurrence.RowKey, owner.ConversationID, err)
			}
			rows = append(rows, embeddedOutboxRow{
				FieldKey:          field.Key,
				FieldDigest:       field.Digest,
				ProviderMessageID: field.ProviderMessageID,
				Occurrence:        occurrence,
			})
		}
	}
	return rows, nil
}

// logEmbeddedPass publishes freshness and logs one pass with a separate group
// of counts for source reading, projection, embedding, persistence, and
// searchable completion.
func (w *conversationSemanticSyncWorker) logEmbeddedPass(ctx context.Context, stats embeddedSyncStats) {
	w.freshness.publish(conversationSemanticSyncStats{
		manifest:            stats.admitted,
		needed:              stats.needed + stats.failedSuppressed,
		sentConversations:   stats.completed,
		documents:           stats.rows,
		deferred:            stats.deferred,
		failed:              stats.sourceFailed + stats.projectionFailed + stats.deliveryFailed,
		failedSuppressed:    stats.failedSuppressed,
		policySkipped:       stats.policySkipped,
		injectedStripped:    0,
		systemStripped:      0,
		sentConversationIDs: stats.completedIDs,
		unchangedPinned:     0,
	})
	attributes := []slog.Attr{
		slog.String("concern", "conversation.semantic"),
		slog.String("component", "daemon"),
		slog.String("backend", string(config.ConversationSemanticBackendEmbedded)),
		slog.Int("admitted", stats.admitted),
		slog.Int("needed", stats.needed),
		slog.Int("deferred", stats.deferred),
		slog.Int("pending_blocked", stats.pendingBlocked),
		slog.Int("source_read", stats.sourceRead),
		slog.Int64("source_bytes", stats.sourceBytes),
		slog.Int("source_failed", stats.sourceFailed),
		slog.Int("source_failed_suppressed", stats.failedSuppressed),
		slog.Int("projection_fields", stats.fields),
		slog.Int("projection_new_fields", stats.newFields),
		slog.Int("projection_unchanged_fields", stats.unchangedFields),
		slog.Int("projection_changed_committed", stats.changedCommitted),
		slog.Int("projection_changed_committed_total", w.embedded.changedCommitted),
		slog.Int("projection_withheld_fields", stats.withheldFields),
		slog.Int("projection_policy_skipped", stats.policySkipped),
		slog.Int("projection_failed", stats.projectionFailed),
		slog.Int("projection_rows", stats.rows),
		slog.Int("embedding_staged_rows", stats.delivery.stagedRows),
		slog.Int64("embedding_stage_ms", stats.delivery.stageMilliseconds),
		slog.Int("persistence_recorded_batches", stats.delivery.recordedBatches),
		slog.Int("persistence_replayed_batches", stats.replayed),
		slog.Int("searchable_generations", stats.delivery.committedGenerations),
		slog.Int("searchable_rows", stats.delivery.committedRows),
		slog.Int("searchable_conversations", stats.completed),
		slog.String("searchable_conversation_ids", strings.Join(boundedConversationIDs(stats.completedIDs), ",")),
		slog.Int("delivery_failed", stats.deliveryFailed),
	}
	level := slog.LevelDebug
	if stats.delivery.recordedBatches > 0 || stats.replayed > 0 || stats.sourceFailed > 0 || stats.projectionFailed > 0 ||
		stats.deliveryFailed > 0 || stats.failedSuppressed > 0 || stats.changedCommitted > 0 || stats.pendingBlocked > 0 {
		level = slog.LevelInfo
	}
	w.log.LogAttrs(ctx, level, "daemon.conversation_semantic_sync.pass_completed", attributes...)
}
