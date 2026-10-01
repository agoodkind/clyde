package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"goodkind.io/lm-semantic-search/library"
	"goodkind.io/lm-semantic-search/library/observation"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
	"goodkind.io/clyde/internal/daemonsupervisor"
	"goodkind.io/clyde/internal/livetrack"
)

// embeddedTrailingSettleWindow is how long an artifact must stay unchanged
// before the worker commits a trailing message that the provider can still
// extend. A later message in the transcript releases the trailing message
// earlier. The pass deferral of an actively growing artifact is one sync
// interval, which is shorter.
const embeddedTrailingSettleWindow = 30 * time.Minute

// embeddedStoreCloseTimeout bounds the Milvus client close after the embedded
// ingestion worker stops.
const embeddedStoreCloseTimeout = 5 * time.Second

const embeddedStoreOpenTimeout = 4 * time.Second

var nextEmbeddedPassID atomic.Uint64

// embeddedConversationSync owns the store for one daemon generation.
type embeddedConversationSync struct {
	semantic   config.ConversationSemanticConfig
	outboxPath string
	store      *embeddedConversationStore
	// The cache includes every alias and the accepted catalog generation.
	// A restart discards it and repeats the source proof before selecting additions.
	processed          map[string]string
	processedSources   map[string]conversation.StampedRecord
	aliasDecision      *embeddedAliasDecision
	aliasOwnerMetadata map[string]embeddedStoredOwnerMetadata
	// changedCommitted counts conflicting committed field digests since worker
	// startup. A conflict rejects the entire candidate and retains prior rows.
	changedCommitted int
	// status receives the library and outbox state for the daemon status RPC.
	// Nil disables publishing.
	status *embeddedSemanticStatus
	// records lists every cached conversation record, including subagent
	// conversations that raw index visibility hides. The admits method applies
	// the embedded subagent setting to them.
	records embeddedRecordLister
	// libraryAbsent lists owners that the outbox has no owner metadata for and
	// that the library reported without a committed generation or published
	// rows. A later pass reads the library for these owners again only after
	// the worker restarts.
	libraryAbsent map[string]bool
	// mu serializes every use of store: the sync passes, the store close, and
	// the reconciliations that the daemon control socket requests.
	mu sync.Mutex
	// closed reports that closeStore ran. ensureStore opens no store after
	// close. closeStore sets it under mu.
	closed     bool
	closing    atomic.Bool
	closeOnce  sync.Once
	closeDone  chan struct{}
	closeErr   error
	workerDone <-chan struct{}
	// open opens the store under the outbox lock that ensureStore took. The
	// default opens the Milvus-backed store with openEmbeddedConversationStore.
	open embeddedStoreOpener
}

// embeddedStoreOpener opens the embedded store under lock, the outbox lock.
// The caller still owns lock after a failure.
type embeddedStoreOpener func(ctx context.Context, lock *os.File, log *slog.Logger) (*embeddedConversationStore, error)

// embeddedRecordLister lists every cached conversation record with its artifact
// stamp, independent of conversation.include_subagent_conversations.
type embeddedRecordLister interface {
	ListAllWithStamps(context.Context) ([]conversation.StampedRecord, error)
}

// embeddedRecordIndex is the conversation index that the embedded sync worker
// reads: the semantic sync index plus the unfiltered record listing.
type embeddedRecordIndex interface {
	conversationSemanticIndex
	embeddedRecordLister
}

func newEmbeddedConversationSync(
	semantic config.ConversationSemanticConfig,
	outboxPath string,
	status *embeddedSemanticStatus,
	records embeddedRecordLister,
) *embeddedConversationSync {
	return &embeddedConversationSync{
		semantic:           semantic,
		outboxPath:         outboxPath,
		store:              nil,
		processed:          make(map[string]string),
		processedSources:   nil,
		aliasDecision:      nil,
		aliasOwnerMetadata: nil,
		changedCommitted:   0,
		status:             status,
		records:            records,
		libraryAbsent:      make(map[string]bool),
		mu:                 sync.Mutex{},
		closed:             false,
		closing:            atomic.Bool{},
		closeOnce:          sync.Once{},
		closeDone:          make(chan struct{}),
		closeErr:           nil,
		workerDone:         nil,
		open: func(ctx context.Context, lock *os.File, log *slog.Logger) (*embeddedConversationStore, error) {
			return openEmbeddedConversationStore(ctx, semantic, outboxPath, lock, log)
		},
	}
}

// ensureStore opens the store when no open store exists. After closeStore it
// returns errEmbeddedReconcileUnavailable and opens nothing. The caller locks
// mu.
func (embedded *embeddedConversationSync) ensureStore(ctx context.Context, log *slog.Logger) (*embeddedConversationStore, error) {
	if embedded.closed || embedded.closing.Load() {
		return nil, errEmbeddedReconcileUnavailable
	}
	if embedded.store != nil {
		return embedded.store, nil
	}
	openCtx, cancel := context.WithTimeout(ctx, embeddedStoreOpenTimeout)
	defer cancel()
	// ensureStore takes the outbox lock first. When another worker owns the
	// lock, this worker creates no Milvus client, opens no library, and
	// registers no namespace.
	lock, err := lockConversationSemanticOutbox(openCtx, embedded.outboxPath)
	if err != nil {
		return nil, err
	}
	store, err := embedded.open(openCtx, lock, log)
	if err != nil {
		if closeErr := lock.Close(); closeErr != nil {
			log.WarnContext(
				ctx, "daemon.conversation_semantic_embedded.unlock_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"err", closeErr,
			)
		}
		return nil, err
	}
	if err := store.outbox.releaseLock(); err != nil {
		return nil, errors.Join(err, store.close(context.WithoutCancel(ctx)))
	}
	embedded.store = store
	return store, nil
}

// closeStore closes the open store after the worker stops and marks the
// state closed. No later pass or reconcile request opens the store again.
func (embedded *embeddedConversationSync) closeStore(ctx context.Context) error {
	embedded.mu.Lock()
	defer embedded.mu.Unlock()
	embedded.closed = true
	if embedded.store == nil {
		return nil
	}
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), embeddedStoreCloseTimeout)
	defer cancel()
	err := embedded.store.close(closeCtx)
	embedded.store = nil
	embedded.status.markClosed()
	slog.InfoContext(ctx, "daemon.conversation_semantic_embedded.closed", "concern", "conversation.semantic", "pid", os.Getpid(), "err", err)
	return err
}

func (embedded *embeddedConversationSync) closeGeneration(ctx context.Context) error {
	embedded.closing.Store(true)
	embedded.closeOnce.Do(func() {
		embedded.startGenerationClose(context.WithoutCancel(ctx))
	})
	select {
	case <-embedded.closeDone:
		return embedded.closeErr
	case <-ctx.Done():
		slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.close_pending", "concern", "conversation.semantic", "err", ctx.Err())
		return fmt.Errorf("wait for embedded generation storage closure: %w", ctx.Err())
	}
}

func (embedded *embeddedConversationSync) startGenerationClose(ctx context.Context) {
	go func(closeCtx context.Context) {
		defer close(embedded.closeDone)
		defer func() {
			if recovered := recover(); recovered != nil {
				embedded.closeErr = fmt.Errorf("embedded generation close panic: %v", recovered)
				slog.ErrorContext(closeCtx, "daemon.conversation_semantic_embedded.close_panic", "concern", "conversation.semantic", "err", embedded.closeErr)
			}
		}()
		embedded.finishGenerationClose(closeCtx)
	}(ctx)
}

func (embedded *embeddedConversationSync) finishGenerationClose(ctx context.Context) {
	if embedded.workerDone != nil {
		<-embedded.workerDone
	}
	embedded.closeErr = embedded.closeStore(ctx)
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

// startEmbeddedConversationSemanticSync registers storage closure after the
// worker phase. Search-only generations retry store admission without ingestion.
func startEmbeddedConversationSemanticSync(
	ctx context.Context,
	log *slog.Logger,
	semantic config.ConversationSemanticConfig,
	index embeddedRecordIndex,
	freshness *conversationSemanticFreshness,
	status *embeddedSemanticStatus,
	reconcileGate *embeddedReconcileGate,
	group *livetrack.Group,
	contentKinds conversation.ContentKindSet,
) error {
	if !semantic.UsesEngine() || group == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	embedded := newEmbeddedConversationSync(semantic, conversationSemanticOutboxPath(semantic.PoolID), status, index)
	reconcileGate.attach(embedded)
	group.AddHook(livetrack.PhaseStorage, "conversation.semantic.embedded_storage_close", func(closeCtx context.Context) error {
		reconcileGate.detach(embedded)
		return embedded.closeGeneration(closeCtx)
	})
	if err := embedded.openForStartup(ctx, log, os.Getenv(daemonsupervisor.EnvReloadChild) == "1"); err != nil {
		return err
	}
	workerCtx, done, owned := installConversationSemanticSyncStop(ctx, group, log)
	if !owned {
		log.WarnContext(
			ctx, "daemon.conversation_semantic_sync.start_skipped_unowned",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", semantic.CollectionID,
		)
		return nil
	}
	worker := newConversationSemanticSyncWorker(index, semantic.CollectionID, log, contentKinds)
	worker.freshness = freshness
	worker.embedded = embedded
	embedded.workerDone = done
	go func() {
		defer close(done)
		defer worker.log.InfoContext(workerCtx, "daemon.conversation_semantic_embedded.worker_exited", "concern", "conversation.semantic", "pid", os.Getpid())
		defer func() {
			if recovered := recover(); recovered != nil {
				worker.log.ErrorContext(
					workerCtx, "daemon.conversation_semantic_sync.panic",
					"concern", "conversation.semantic",
					"component", "daemon",
					"err", fmt.Sprintf("panic: %v", recovered),
				)
			}
		}()
		if semantic.FeedsEngine() {
			worker.run(workerCtx)
			return
		}
		embedded.retryStoreOpen(workerCtx, log)
	}()
	return nil
}

func (embedded *embeddedConversationSync) openForStartup(ctx context.Context, log *slog.Logger, replacement bool) error {
	startupCtx, cancel := context.WithTimeout(ctx, embeddedStoreOpenTimeout)
	defer cancel()
	for {
		attemptCtx, attemptCancel := context.WithTimeout(startupCtx, embeddedStoreOpenTimeout)
		embedded.mu.Lock()
		_, err := embedded.ensureStore(attemptCtx, log)
		embedded.mu.Unlock()
		attemptCancel()
		if err == nil {
			return nil
		}
		if errors.Is(err, library.ErrStoreMismatch) || errors.Is(err, library.ErrInvalidRequest) {
			return err
		}
		if !replacement {
			return nil
		}
		select {
		case <-startupCtx.Done():
			log.WarnContext(ctx, "daemon.conversation_semantic_embedded.replacement_unavailable", "concern", "conversation.semantic", "err", err)
			return err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (embedded *embeddedConversationSync) retryStoreOpen(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, embeddedStoreOpenTimeout)
		embedded.mu.Lock()
		_, err := embedded.ensureStore(attemptCtx, log)
		embedded.mu.Unlock()
		cancel()
		if err == nil || errors.Is(err, library.ErrStoreMismatch) || errors.Is(err, library.ErrInvalidRequest) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (embedded *embeddedConversationSync) lockStore(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.admission_canceled", "concern", "conversation.semantic", "err", err)
			return fmt.Errorf("wait for embedded store access: %w", err)
		}
		if embedded.mu.TryLock() {
			return nil
		}
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "daemon.conversation_semantic_embedded.admission_canceled", "concern", "conversation.semantic", "err", ctx.Err())
			return fmt.Errorf("wait for embedded store access: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// embeddedCandidate is one admitted conversation that the worker has not
// processed at its current fingerprint.
type embeddedCandidate struct {
	record           conversation.Record
	stamp            conversation.FileStamp
	fingerprint      string
	groupFingerprint string
	aliases          []conversation.StampedRecord
	sourceBytes      int64
}

// embeddedSyncStats reports one embedded pass by phase: source reading,
// projection, embedding, persistence, and searchable completion.
type embeddedSyncStats struct {
	admitted                      int
	needed                        int
	completed                     int
	completedIDs                  []string
	deferred                      int
	pendingBlocked                int
	sourceRead                    int
	sourceBytes                   int64
	sourceFailed                  int
	sourceReadDuration            time.Duration
	projectionDuration            time.Duration
	policySelectionDuration       time.Duration
	committedReadDuration         time.Duration
	fieldSelectionDuration        time.Duration
	occurrencePreparationDuration time.Duration
	outboxPreparationDuration     time.Duration
	failedSuppressed              int
	projectionFailed              int
	fields                        int
	newFields                     int
	unchangedFields               int
	changedCommitted              int
	withheldFields                int
	policySkipped                 int
	rows                          int
	replayed                      int
	replayDeferred                int
	deliveryFailed                int
	delivery                      embeddedDeliveryCounts
	// replayedProjections, reprojectedOwners, and reprojectionFailed count
	// scalar reprojections of already indexed owners.
	replayedProjections int
	reprojectedOwners   int
	reprojectionFailed  int
	// blockedOwners counts owners with a blocked batch or projection after
	// the pass.
	blockedOwners int
	// reconciledOwners, abortedTokens, and reconcileFailed count owner
	// reconciliations from the library.
	reconciledOwners int
	abortedTokens    int
	reconcileFailed  int
}

// runEmbeddedPass resolves each owner after replay and uses one accepted
// decision for reconciliation, metadata publication and missing-field delivery.
func (w *conversationSemanticSyncWorker) runEmbeddedPass(ctx context.Context) (resultErr error) {
	w.embedded.mu.Lock()
	defer w.embedded.mu.Unlock()
	scope := observation.ScopeFromContext(ctx)
	scope.RunID = fmt.Sprintf("ingestion-%d-%d", os.Getpid(), nextEmbeddedPassID.Add(1))
	scope.Purpose = observation.Ingestion
	ctx = observation.WithScope(ctx, scope)
	w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.pass_started", "concern", "conversation.semantic", "run_id", scope.RunID, "pid", os.Getpid())
	store, err := w.embedded.ensureStore(ctx, w.log)
	if err != nil {
		return err
	}
	lock, err := lockConversationSemanticOutbox(ctx, w.embedded.outboxPath)
	if err != nil {
		return err
	}
	store.outbox.lock = lock
	defer func() {
		resultErr = errors.Join(resultErr, store.outbox.releaseLock())
	}()
	replay, err := store.delivery.replayPending(ctx)
	if err != nil {
		return err
	}
	replayedProjections, projectionBlocked, err := store.delivery.replayPendingProjections(ctx)
	if err != nil {
		return err
	}
	blockedInOutbox, err := store.outbox.blockedOwners(ctx, store.namespace.ID)
	if err != nil {
		return err
	}
	var stats embeddedSyncStats
	stats.replayed = replay.replayed
	stats.replayDeferred = replay.deferred
	stats.delivery = replay.counts
	stats.replayedProjections = replayedProjections
	stampedRecords, err := w.embedded.records.ListAllWithStamps(ctx)
	if err != nil {
		w.log.WarnContext(
			ctx, "daemon.conversation_semantic_sync.list_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("list conversation records with stamps: %w", err)
	}
	blockedSet := make(map[string]bool, len(blockedInOutbox))
	for _, ownerID := range blockedInOutbox {
		blockedSet[ownerID] = true
	}
	w.processEmbeddedAliasGroups(ctx, store, stampedRecords, replay.blockedOwners, projectionBlocked, blockedSet, &stats)
	blockedAfterPass, err := store.outbox.blockedOwners(ctx, store.namespace.ID)
	if err == nil {
		stats.blockedOwners = len(blockedAfterPass)
	}
	w.embedded.status.publishStore(ctx, store)
	w.logEmbeddedPass(ctx, stats)
	return nil
}

// embeddedExcludedOwners returns the owners that a pass must not deliver to
// or reproject: owners with a pending batch or projection that the replay did
// not acknowledge, and owners with a blocked outbox item. Reconciliation
// aborts the blocked items and pending projections of an owner. A reconciled
// owner stays excluded only while a pending batch above the committed order
// waits for replay.
func embeddedExcludedOwners(
	replayBlocked map[string]bool,
	projectionBlocked map[string]bool,
	blockedInOutbox map[string]bool,
	reconciled map[string]bool,
) map[string]bool {
	excluded := make(map[string]bool, len(replayBlocked)+len(projectionBlocked)+len(blockedInOutbox))
	for _, source := range []map[string]bool{replayBlocked, projectionBlocked, blockedInOutbox} {
		for ownerID := range source {
			excluded[ownerID] = true
		}
	}
	for ownerID := range reconciled {
		if !replayBlocked[ownerID] {
			delete(excluded, ownerID)
		}
	}
	return excluded
}

// embeddedCandidates preserves the selected source after alias preflight.
// Recovery callers resolve their original alias group during generation preparation.
func (w *conversationSemanticSyncWorker) embeddedCandidates(
	stampedRecords []conversation.StampedRecord,
	stats *embeddedSyncStats,
) []embeddedCandidate {
	seen := make(map[string]bool, len(stampedRecords))
	candidatesByID := make(map[string]embeddedCandidate)
	for _, group := range groupEmbeddedAliases(stampedRecords) {
		seen[group.ownerID] = true
		for _, alias := range group.aliases {
			if !w.embedded.admits(alias.Record) {
				continue
			}
			if w.embedded.aliasDecision == nil {
				stats.admitted++
			}
			candidate := embeddedCandidate{record: alias.Record, stamp: alias.Stamp, fingerprint: conversation.ContentFingerprint(alias.Record, alias.Stamp), groupFingerprint: "", aliases: group.aliases, sourceBytes: group.bytes}
			if decision := w.embedded.aliasDecision; decision != nil && decision.source.Record.ID == group.ownerID {
				if decision.cached || !decision.admitted {
					break
				}
				candidate.record, candidate.stamp = decision.source.Record, decision.source.Stamp
				candidate.fingerprint = conversation.ContentFingerprint(candidate.record, candidate.stamp)
				candidate.groupFingerprint = decision.fingerprint
			}
			candidatesByID[group.ownerID] = candidate
			break
		}
	}
	if w.embedded.aliasDecision == nil {
		w.pruneEmbeddedAliasCaches(seen)
	}

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
	if w.embedded.aliasDecision == nil {
		stats.needed += len(candidates)
	}
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
		candidateBytes := candidate.sourceBytes
		if candidateBytes == 0 {
			candidateBytes = max(candidate.stamp.Size, 0)
		}
		if loaded > 0 && candidateBytes > conversationSemanticBatchBytes-artifactBytes {
			return
		}
		loaded++
		artifactBytes += candidateBytes
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
	generation, err := w.buildEmbeddedGeneration(ctx, store, candidate, stats)
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
	{
		w.embedded.processed[candidate.record.ID] = candidate.groupFingerprint
		if w.embedded.processedSources == nil {
			w.embedded.processedSources = make(map[string]conversation.StampedRecord)
		}
		w.embedded.processedSources[candidate.record.ID] = conversation.StampedRecord{Record: candidate.record, Stamp: candidate.stamp}
	}
}

// buildEmbeddedGeneration prepares only missing fields from a proven alias group.
func (w *conversationSemanticSyncWorker) buildEmbeddedGeneration(
	ctx context.Context,
	store *embeddedConversationStore,
	candidate embeddedCandidate,
	stats *embeddedSyncStats,
) (*embeddedGeneration, error) {
	decision := w.embedded.aliasDecision
	if decision == nil || decision.source.Record.ID != candidate.record.ID {
		aliases := candidate.aliases
		if len(aliases) == 0 {
			aliases = []conversation.StampedRecord{{Record: candidate.record, Stamp: candidate.stamp}}
		}
		group := groupEmbeddedAliases(aliases)[0]
		var err error
		decision, err = w.resolveEmbeddedAliasGroup(ctx, store, group, stats)
		if err != nil {
			w.aliasFailure(ctx, group, err, stats)
			return nil, err
		}
	}
	if decision.cached || !decision.admitted {
		return nil, nil
	}
	record := decision.source.Record
	candidate.record, candidate.stamp = record, decision.source.Stamp
	candidate.fingerprint = conversation.ContentFingerprint(record, candidate.stamp)
	candidate.groupFingerprint = decision.fingerprint
	delete(w.failedLoad, record.ID)
	fields, err := w.selectEmbeddedFields(ctx, store, candidate, decision.fields, stats)
	if err != nil || len(fields) == 0 {
		return nil, err
	}
	owner := embeddedAliasOwner(decision, w.contentKinds)
	started := w.now()
	rows, err := embeddedOutboxRows(ctx, store.namespace, owner, fields)
	stats.occurrencePreparationDuration += w.now().Sub(started)
	if err != nil {
		stats.projectionFailed++
		w.recordLoadFailure(record.ID, candidate.fingerprint)
		return nil, err
	}
	stats.rows += len(rows)
	started = w.now()
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
		Metadata:          decision.metadata,
	}, rows)
	stats.outboxPreparationDuration += w.now().Sub(started)
	if err != nil {
		stats.deliveryFailed++
		return nil, err
	}
	return &generation, nil
}

// selectEmbeddedFields applies role policy after the complete alias proof.
func (w *conversationSemanticSyncWorker) selectEmbeddedFields(
	ctx context.Context,
	store *embeddedConversationStore,
	candidate embeddedCandidate,
	projected []searchbackend.Field,
	stats *embeddedSyncStats,
) ([]searchbackend.Field, error) {
	record := candidate.record
	started := w.now()
	fields := w.embedded.admittedFields(projected)
	stats.policySelectionDuration += w.now().Sub(started)
	stats.fields += len(fields)
	started = w.now()
	committed, err := store.outbox.committedFields(ctx, store.namespace.ID, record.ID)
	stats.committedReadDuration += w.now().Sub(started)
	if err != nil {
		stats.deliveryFailed++
		return nil, err
	}
	started = w.now()
	selection := searchbackend.SelectNewFields(fields, committed)
	stats.fieldSelectionDuration += w.now().Sub(started)
	stats.newFields += len(selection.New)
	stats.unchangedFields += selection.Unchanged
	stats.changedCommitted += selection.ChangedCommitted
	w.embedded.changedCommitted += selection.ChangedCommitted
	if selection.ChangedCommitted > 0 {
		stats.projectionFailed++
		err := fmt.Errorf("select fields of %s: %w", record.ID, library.ErrAppendConflict)
		w.log.WarnContext(ctx, "daemon.conversation_semantic_embedded.source_conflict",
			"concern", "conversation.semantic", "component", "daemon",
			"conversation_id", record.ID, "source_fingerprint", candidate.fingerprint,
			"conflicting_fields", selection.ChangedCommitted, "err", err)
		return nil, err
	}
	return selection.New, nil
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
				slog.WarnContext(
					ctx, "daemon.conversation_semantic_embedded.occurrence_invalid",
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
		slog.String("run_id", observation.ScopeFromContext(ctx).RunID),
		slog.String("backend", "embedded"),
		slog.Int("admitted", stats.admitted),
		slog.Int("needed", stats.needed),
		slog.Int("deferred", stats.deferred),
		slog.Int("pending_blocked", stats.pendingBlocked),
		slog.Int("source_read", stats.sourceRead),
		slog.Int64("source_read_us", stats.sourceReadDuration.Microseconds()),
		slog.Int64("projection_us", stats.projectionDuration.Microseconds()),
		slog.Int64("policy_selection_us", stats.policySelectionDuration.Microseconds()),
		slog.Int64("committed_field_read_us", stats.committedReadDuration.Microseconds()),
		slog.Int64("field_selection_us", stats.fieldSelectionDuration.Microseconds()),
		slog.Int64("occurrence_preparation_us", stats.occurrencePreparationDuration.Microseconds()),
		slog.Int64("outbox_preparation_us", stats.outboxPreparationDuration.Microseconds()),
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
		slog.Int("persistence_replay_deferred_batches", stats.replayDeferred),
		slog.Int("searchable_generations", stats.delivery.committedGenerations),
		slog.Int("searchable_rows", stats.delivery.committedRows),
		slog.Int("searchable_conversations", stats.completed),
		slog.String("searchable_conversation_ids", strings.Join(boundedConversationIDs(stats.completedIDs), ",")),
		slog.Int("delivery_failed", stats.deliveryFailed),
		slog.Int("metadata_replayed_projections", stats.replayedProjections),
		slog.Int("metadata_reprojected_owners", stats.reprojectedOwners),
		slog.Int("metadata_reprojection_failed", stats.reprojectionFailed),
		slog.Int("blocked_owners", stats.blockedOwners),
		slog.Int("reconciled_owners", stats.reconciledOwners),
		slog.Int("reconcile_aborted_tokens", stats.abortedTokens),
		slog.Int("reconcile_failed", stats.reconcileFailed),
	}
	level := slog.LevelDebug
	if stats.delivery.recordedBatches > 0 || stats.replayed > 0 || stats.sourceFailed > 0 || stats.projectionFailed > 0 ||
		stats.deliveryFailed > 0 || stats.failedSuppressed > 0 || stats.changedCommitted > 0 || stats.pendingBlocked > 0 ||
		stats.replayedProjections > 0 || stats.reprojectedOwners > 0 || stats.reprojectionFailed > 0 || stats.blockedOwners > 0 ||
		stats.reconciledOwners > 0 || stats.reconcileFailed > 0 {
		level = slog.LevelInfo
	}
	w.log.LogAttrs(ctx, level, "daemon.conversation_semantic_sync.pass_completed", attributes...)
}
