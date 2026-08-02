package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/livetrack"
	"goodkind.io/clyde/internal/transcript"
)

// conversationSemanticFreshness is the mutex-guarded latest sync snapshot the
// sync worker publishes after each pass and the control server reads at query
// time. The zero value reports an empty snapshot until the first pass lands.
type conversationSemanticFreshness struct {
	mu        sync.Mutex
	freshness conversation.SearchFreshness
}

// newConversationSemanticFreshness constructs an empty freshness holder.
func newConversationSemanticFreshness() *conversationSemanticFreshness {
	return &conversationSemanticFreshness{
		mu: sync.Mutex{},
		freshness: conversation.SearchFreshness{
			Manifest:     0,
			Needed:       0,
			Embedded:     0,
			Pending:      0,
			LastSyncUnix: 0,
		},
	}
}

// snapshot returns the latest published freshness. Safe for concurrent reads
// from the control server while the worker publishes.
func (f *conversationSemanticFreshness) snapshot() conversation.SearchFreshness {
	if f == nil {
		return conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.freshness
}

// publish records one pass's stats as the latest freshness. embedded is the
// cumulative conversation coverage (advertised manifest minus the conversations
// the engine still needs), pending is the conversations whose content the store
// does not hold yet, and last_sync is the pass completion time on the repo
// clock. embedded is conversations, not the per-pass document count, so
// embedded + needed + failedSuppressed == manifest and the number tracks real
// coverage.
//
// A conversation suppressed after repeated load failures is counted back into
// manifest and pending. It left the advertised manifest, but its content is
// still missing from the store, and freshness exists so a thin search result
// reads as a cold index rather than a true miss. Without this a suppressed
// conversation would read as fully indexed.
func (f *conversationSemanticFreshness) publish(stats conversationSemanticSyncStats) {
	if f == nil {
		return
	}
	pending := max(stats.needed-stats.sentConversations, 0) + stats.failedSuppressed
	embedded := max(stats.manifest-stats.needed, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freshness = conversation.SearchFreshness{
		Manifest:     stats.manifest + stats.failedSuppressed,
		Needed:       stats.needed,
		Embedded:     embedded,
		Pending:      pending,
		LastSyncUnix: clock.Now().Unix(),
	}
}

const (
	conversationSemanticSyncInterval = time.Minute
	maxSemanticMessageIndex          = int32(1<<31 - 1)
	// failedLoadSuppressThreshold is how many consecutive load failures at one
	// fingerprint a conversation gets before the manifest stops advertising it.
	// Passes run a minute apart, so a transient failure such as a busy Cursor
	// SQLite store has minutes to clear before its conversation is suppressed.
	failedLoadSuppressThreshold = 3
)

// failedLoadRecord is one conversation's consecutive load-failure history at a
// single fingerprint. A failure at a different fingerprint restarts the count,
// because the new bytes have not been tried yet.
type failedLoadRecord struct {
	fingerprint string
	failures    int
}

type conversationSemanticIndex interface {
	ListWithStamps(context.Context) ([]conversation.StampedRecord, error)
	LoadMessagesWithOptions(conversation.Record, conversation.LoadOptions) ([]transcript.Message, error)
}

// conversationSemanticClient is the feeder surface: state the full manifest,
// learn which conversations the engine needs, send only those, and check
// whether the engine job from the last accepted upsert is still running so a
// pass can skip its disk reads instead of being rejected after them.
type conversationSemanticClient interface {
	SyncConversationManifest(context.Context, string, []semsearch.Fingerprint) ([]string, error)
	UpsertConversationDocuments(context.Context, string, []semsearch.SemDoc, []semsearch.Fingerprint) (string, error)
	JobState(ctx context.Context, jobID string) (string, error)
}

// conversationSemanticClientResolver resolves the current engine feeder surface.
// It returns nil while the engine is unreachable, which the worker treats as a
// skipped pass rather than a permanent stop. The worker calls it once per pass,
// so a background registration success starts feeding the engine with no daemon
// reload, the same way the search path picks up the recovered client.
type conversationSemanticClientResolver func() conversationSemanticClient

// conversationSemanticSyncHookName names the lifecycle hook that stops the sync
// feeder. It runs in PhaseWorkers, before that phase's members drain, so the
// feeder is gone before the semantic connection registry closes the engine
// connection the feeder writes to.
const conversationSemanticSyncHookName = "conversation.semantic.sync_stop"

// conversationSemanticSyncWorker feeds the engine a content fingerprint per
// conversation each pass and sends documents only for the conversations the
// engine reports it needs. The engine owns drift, so the worker keeps no
// change-tracking state beyond two scheduling hints: the engine job id of the
// last accepted upsert, which lets a pass skip all work while that job still
// runs, and the delivery cursor, which rotates batch order so late-sorting
// conversations are not starved.
type conversationSemanticSyncWorker struct {
	index conversationSemanticIndex
	// resolveClient returns the engine feeder surface for the pass about to run.
	// It is resolved per pass, never cached, so a pass that starts after the
	// engine comes back feeds it without waiting for a daemon reload.
	resolveClient conversationSemanticClientResolver
	collectionID  string
	log           *slog.Logger
	interval      time.Duration
	// activeJobID is the engine job started by the last accepted upsert. The
	// worker runs in a single goroutine, so the field needs no lock.
	activeJobID string
	// deliveryCursor is the last conversation id delivered; each batch resumes
	// after it instead of restarting at the lexicographically smallest id.
	deliveryCursor string
	// emptyDelivered maps a conversation id to the fingerprint at which it
	// rendered zero deliverable documents. The engine only marks a conversation
	// satisfied once it receives a document, so a zero-document conversation
	// would stay in the needed set forever. The next manifest omits any
	// conversation whose current fingerprint still matches its entry here, and
	// re-includes it once its content, and therefore its fingerprint, changes.
	// The worker runs in a single goroutine, so the map needs no lock.
	emptyDelivered map[string]string
	// failedLoad maps a conversation id to its consecutive load failures at one
	// fingerprint. A conversation that yields no document is never marked
	// satisfied by the engine, so without this a permanently failing
	// conversation is read and re-read on every pass forever: the daemon never
	// goes idle, the log grows without bound, and the pass never reports itself
	// caught up.
	//
	// Suppression waits for failedLoadSuppressThreshold consecutive failures at
	// the same fingerprint, because a load failure can be transient. Cursor's
	// live SQLite store can be busy while Cursor checkpoints, and a single busy
	// read must not take a finished conversation out of the index for the life
	// of the daemon. A transient failure therefore retries on the next passes
	// and a successful load clears its entry; only a failure that repeats at
	// unchanged bytes is suppressed.
	//
	// It is deliberately separate from emptyDelivered. That map means the
	// conversation has nothing to index, so dropping it from the manifest is
	// the truth. This one means the conversation has content the reader could
	// not produce, so dropping it from the manifest hides a real loss. Sharing
	// one map would let the second meaning inherit the first's silence, which
	// is what the failed counter's contract forbids. Suppression here is
	// therefore always reported: see failedSuppressed.
	//
	// A failure is keyed by fingerprint like an empty delivery, so a
	// transcript that grows lifts its own suppression and the conversation is
	// retried against the new bytes. The worker runs in a single goroutine, so
	// the map needs no lock.
	failedLoad map[string]failedLoadRecord
	// now returns the wall clock; tests override it to pin deferral decisions.
	now func() time.Time
	// freshness receives the latest pass stats so the control server can report
	// index sync state at query time. Nil disables publishing.
	freshness *conversationSemanticFreshness
	// contentKinds names the content this feeder offers. It is resolved once at
	// daemon startup from the export surface's selector vocabulary, so a change
	// takes effect on the next daemon generation.
	contentKinds conversation.ContentKindSet
}

type conversationSemanticSyncStats struct {
	manifest          int
	needed            int
	sentConversations int
	documents         int
	deferred          int
	// failed counts conversations whose transcript could not be loaded or
	// projected. It is content lost, so nothing else may be folded into it.
	failed int
	// failedSuppressed counts conversations this pass left out of the manifest
	// because an earlier pass failed to load them and their transcript has not
	// changed since. They are content the store does not hold, so the number is
	// reported rather than folded into any other count: a manifest that shrinks
	// silently would read as a corpus that is fully indexed.
	failedSuppressed int
	// policySkipped counts messages the content policy withheld because every
	// indexed class was empty for them. It is deliberate, so it is reported
	// beside failed rather than inside it.
	policySkipped int
	// injectedStripped and systemStripped total what the provider parsers
	// removed from offered message text this pass: injected spans are
	// hook-pushed context, system spans are harness-native tags. Both are the
	// policy working, reported so a live pass proves stripping ran.
	injectedStripped int
	systemStripped   int
}

// installConversationSemanticSyncStop creates the feeder's worker context and
// registers its stop as a PhaseWorkers before-hook on the lifecycle group. The
// worker context and its completion channel do not exist until the hook is
// installed, so a feeder goroutine cannot be launched before the group owns its
// stop, and any drain that begins afterwards cancels and joins the feeder inside
// the workers phase instead of finishing while it still runs. A nil group has no
// owner, so nothing is created and the caller must start nothing.
func installConversationSemanticSyncStop(
	ctx context.Context,
	group *livetrack.Group,
	log *slog.Logger,
) (context.Context, chan struct{}, bool) {
	if group == nil {
		return nil, nil, false
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	group.AddHookBefore(livetrack.PhaseWorkers, conversationSemanticSyncHookName, func(stopCtx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			log.WarnContext(stopCtx, "daemon.conversation_semantic_sync.stop_timeout",
				"concern", "conversation.semantic",
				"component", "daemon",
				"err", stopCtx.Err(),
			)
			return fmt.Errorf("wait for conversation semantic sync worker: %w", stopCtx.Err())
		}
	})
	return workerCtx, done, true
}

// startConversationSemanticSync starts the feeder whenever semantic search is
// configured, even while the engine is unreachable. The worker resolves its
// client per pass, so an engine that comes back after boot is fed with no daemon
// reload. It reports whether a feeder started. Two cases start nothing: a nil
// resolver, which means semantic search is not configured at all, and a nil
// lifecycle group, which would leave the goroutine unowned across a reload
// drain. The daemon must call this before the control server accepts reload or
// rebind requests, so no drain can begin between the hook install and the
// goroutine launch.
func startConversationSemanticSync(
	ctx context.Context,
	log *slog.Logger,
	index conversationSemanticIndex,
	resolveClient conversationSemanticClientResolver,
	collectionID string,
	freshness *conversationSemanticFreshness,
	group *livetrack.Group,
	contentKinds conversation.ContentKindSet,
) bool {
	if resolveClient == nil {
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
			"collection_id", collectionID,
		)
		return false
	}
	worker := newConversationSemanticSyncWorker(index, resolveClient, collectionID, log, contentKinds)
	worker.freshness = freshness
	go func() {
		defer close(done)
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

func newConversationSemanticSyncWorker(
	index conversationSemanticIndex,
	resolveClient conversationSemanticClientResolver,
	collectionID string,
	log *slog.Logger,
	contentKinds conversation.ContentKindSet,
) *conversationSemanticSyncWorker {
	if log == nil {
		log = slog.Default()
	}
	return &conversationSemanticSyncWorker{
		index:          index,
		resolveClient:  resolveClient,
		collectionID:   strings.TrimSpace(collectionID),
		log:            log,
		interval:       conversationSemanticSyncInterval,
		activeJobID:    "",
		deliveryCursor: "",
		emptyDelivered: make(map[string]string),
		failedLoad:     make(map[string]failedLoadRecord),
		now:            time.Now,
		freshness:      nil,
		contentKinds:   contentKinds,
	}
}

func (w *conversationSemanticSyncWorker) run(ctx context.Context) {
	w.runPassAndLog(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.runPassAndLog(ctx)
		}
	}
}

// runPassAndLog runs one pass and contains a panic to that pass, so the ticker
// in run keeps its cadence and the next pass still happens. Without this a
// panic anywhere in a pass unwinds past the ticker, and the feeder stops for
// the life of the daemon generation, returning only on a reload.
//
// This is the outer bound rather than the working one. A defect in a provider's
// per-record mapping is contained by transcript.ContainMappingPanic and costs
// one message, which is the case that fired in production. A defect elsewhere
// in a provider is contained by conversation.CollectMessages and fails that one
// conversation: the load returns an error, this pass counts it in failed and
// moves to the next conversation. Later passes retry the failed conversation
// and, after repeated failures at unchanged bytes, suppress it from the
// manifest until its transcript changes; see failedLoad. Reaching here means
// something panicked outside a reader, such as the index scan, the manifest
// build, the document projection, or the upsert.
func (w *conversationSemanticSyncWorker) runPassAndLog(ctx context.Context) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		w.log.ErrorContext(ctx, "daemon.conversation_semantic_sync.pass_panic",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", fmt.Sprintf("panic: %v", recovered),
			"stack", string(debug.Stack()),
		)
	}()
	if err := w.runPass(ctx); err != nil && ctx.Err() == nil {
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.pass_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
	}
}

func (w *conversationSemanticSyncWorker) runPass(ctx context.Context) error {
	if semanticSyncContextDone(ctx) {
		return nil
	}
	if w == nil || w.index == nil || w.resolveClient == nil {
		return fmt.Errorf("semantic conversation sync worker is not configured")
	}
	client := w.resolveClient()
	if client == nil {
		// The engine is configured but not yet reachable: registration has not
		// succeeded. Skip the pass without reading any transcript from disk and
		// let the next pass pick the client up once the background retry lands.
		w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.pass_skipped_engine_unavailable",
			"concern", "conversation.semantic",
			"component", "daemon",
			"collection_id", w.collectionID,
		)
		return nil
	}
	if w.engineBusyWithLastJob(ctx, client) {
		w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.pass_skipped_engine_busy",
			"concern", "conversation.semantic",
			"component", "daemon",
			"job_id", w.activeJobID,
		)
		return nil
	}
	stampedRecords, err := w.index.ListWithStamps(ctx)
	if err != nil {
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.list_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("list conversation records with stamps: %w", err)
	}

	manifest, recordsByID, stampsByID, failedSuppressed := w.buildManifest(stampedRecords)
	stats := conversationSemanticSyncStats{
		manifest:          len(manifest),
		needed:            0,
		sentConversations: 0,
		documents:         0,
		deferred:          0,
		failed:            0,
		failedSuppressed:  failedSuppressed,
		policySkipped:     0,
		injectedStripped:  0,
		systemStripped:    0,
	}
	// An empty manifest still syncs when this pass omitted anything, because
	// the engine's copy of the manifest is whatever the last sync stated.
	// Skipping the sync would leave an omitted conversation in the engine's
	// needed set forever. Both omission paths count: failed-load suppression,
	// and zero-document suppression, whose map buildManifest has just pruned
	// to exactly the conversations it omitted this pass. Absence is
	// retain-only on the wire, so an empty-manifest sync removes nothing from
	// the store. Only a corpus with nothing to state at all skips the round
	// trip.
	if len(manifest) == 0 && failedSuppressed == 0 && len(w.emptyDelivered) == 0 {
		w.logPass(ctx, stats)
		return nil
	}

	needed, err := client.SyncConversationManifest(ctx, w.collectionID, manifest)
	if err != nil {
		return fmt.Errorf("sync conversation manifest: %w", err)
	}
	stats.needed = len(needed)
	if len(needed) == 0 {
		w.logPass(ctx, stats)
		return nil
	}

	docs, sentConversations := w.collectNeededDocuments(ctx, needed, recordsByID, stampsByID, &stats)
	if len(docs) == 0 {
		w.logPass(ctx, stats)
		return nil
	}
	if semanticSyncContextDone(ctx) {
		return nil
	}

	w.sendDocuments(ctx, client, docs, manifest, sentConversations, &stats)
	w.logPass(ctx, stats)
	return nil
}

// engineBusyWithLastJob reports whether the engine job from the last accepted
// upsert is still active. While it is, a pass would only re-read transcripts
// from disk and then be rejected with a conflicting-active-job error, so the
// worker skips the whole pass: no manifest sync and no transcript loads. A
// terminal state or a failed lookup clears the id and lets the pass proceed;
// the upsert path still handles a busy engine on its own.
func (w *conversationSemanticSyncWorker) engineBusyWithLastJob(ctx context.Context, client conversationSemanticClient) bool {
	if w.activeJobID == "" {
		return false
	}
	state, err := client.JobState(ctx, w.activeJobID)
	if err != nil {
		w.activeJobID = ""
		return false
	}
	if isEngineJobActive(state) {
		return true
	}
	w.activeJobID = ""
	return false
}

// engineJobState models the engine job lifecycle states the GetJob RPC
// reports, typed at this wire boundary. Only the non-terminal states matter
// here; any other value is treated as terminal.
type engineJobState string

const (
	engineJobStateQueued     engineJobState = "queued"
	engineJobStateRunning    engineJobState = "running"
	engineJobStateCancelling engineJobState = "cancelling"
)

// isEngineJobActive reports whether an engine job state names a job that is
// still queued or running, so a new upsert would be rejected.
func isEngineJobActive(state string) bool {
	switch engineJobState(strings.ToLower(strings.TrimSpace(state))) {
	case engineJobStateQueued, engineJobStateRunning, engineJobStateCancelling:
		return true
	default:
		return false
	}
}

// buildManifest renders the current conversation set as a fingerprint list, a
// lookup from conversation id to its record, and a lookup to its file stamp,
// skipping records with no id.
func (w *conversationSemanticSyncWorker) buildManifest(
	stampedRecords []conversation.StampedRecord,
) ([]semsearch.Fingerprint, map[string]conversation.Record, map[string]conversation.FileStamp, int) {
	manifest := make([]semsearch.Fingerprint, 0, len(stampedRecords))
	recordsByID := make(map[string]conversation.Record, len(stampedRecords))
	stampsByID := make(map[string]conversation.FileStamp, len(stampedRecords))
	seen := make(map[string]bool, len(stampedRecords))
	suppressed := 0
	for _, stampedRecord := range stampedRecords {
		conversationID := strings.TrimSpace(stampedRecord.Record.ID)
		if conversationID == "" {
			continue
		}
		if seen[conversationID] {
			continue
		}
		seen[conversationID] = true
		// ContentFingerprint preserves any compatibility bytes already advertised
		// for this artifact kind while changing only with its file stamp.
		fingerprint := conversation.ContentFingerprint(stampedRecord.Record, stampedRecord.Stamp)
		if priorFingerprint, delivered := w.emptyDelivered[conversationID]; delivered {
			if priorFingerprint == fingerprint {
				continue
			}
			delete(w.emptyDelivered, conversationID)
		}
		// A conversation whose transcript repeatedly failed to load stays out
		// of the manifest until its bytes change, so the engine stops asking
		// for it and the pass stops reading it. Below the threshold it stays
		// advertised, because a single failure can be a transient one such as
		// a busy Cursor store, and the next pass retries it. The suppression
		// is counted rather than silent, because unlike a zero-document
		// conversation this one holds content the store does not have.
		if record, failed := w.failedLoad[conversationID]; failed {
			if record.fingerprint == fingerprint {
				if record.failures >= failedLoadSuppressThreshold {
					suppressed++
					continue
				}
			} else {
				delete(w.failedLoad, conversationID)
			}
		}
		recordsByID[conversationID] = stampedRecord.Record
		stampsByID[conversationID] = stampedRecord.Stamp
		manifest = append(manifest, semsearch.Fingerprint{
			ConversationID: conversationID,
			Value:          fingerprint,
		})
	}
	w.pruneEmptyDelivered(seen)
	w.pruneFailedLoad(seen)
	return manifest, recordsByID, stampsByID, suppressed
}

// pruneFailedLoad drops suppression entries for conversations that no longer
// appear in the scanned set, so the map stays bounded by the current
// conversation count rather than growing across the daemon's lifetime.
func (w *conversationSemanticSyncWorker) pruneFailedLoad(seen map[string]bool) {
	for conversationID := range w.failedLoad {
		if !seen[conversationID] {
			delete(w.failedLoad, conversationID)
		}
	}
}

// pruneEmptyDelivered drops zero-document entries for conversations that no
// longer appear in the scanned set, so the map stays bounded by the current
// conversation count rather than growing across the daemon's lifetime.
func (w *conversationSemanticSyncWorker) pruneEmptyDelivered(seen map[string]bool) {
	for conversationID := range w.emptyDelivered {
		if !seen[conversationID] {
			delete(w.emptyDelivered, conversationID)
		}
	}
}

// collectNeededDocuments loads the documents for the conversations the engine
// asked for, bounded by the per-batch byte budget. Delivery order rotates from
// the previous batch's last conversation so every needed conversation is
// reached across passes, and a conversation whose transcript changed within
// the last interval is deferred: it is still being appended to, and delivering
// it now would re-send a growing transcript on every pass. The remaining
// needed conversations are picked up on later passes, when the engine reports
// them still needed. It returns the documents and the count of conversations
// covered.
func (w *conversationSemanticSyncWorker) collectNeededDocuments(
	ctx context.Context,
	needed []string,
	recordsByID map[string]conversation.Record,
	stampsByID map[string]conversation.FileStamp,
	stats *conversationSemanticSyncStats,
) ([]semsearch.SemDoc, int) {
	ordered := make([]string, len(needed))
	copy(ordered, needed)
	sort.Strings(ordered)
	ordered = rotateAfter(ordered, w.deliveryCursor)

	docs := make([]semsearch.SemDoc, 0)
	// Streaming carries the whole needed set in one client-streamed upsert, so a
	// pass collects every needed conversation rather than splitting by a byte
	// budget. The stream client frames the documents and the manifest into
	// bounded chunks on the wire.
	sentConversations := 0
	for _, conversationID := range ordered {
		if semanticSyncContextDone(ctx) {
			break
		}
		record, found := recordsByID[conversationID]
		if !found {
			// The engine needs a conversation the manifest no longer lists. It will
			// drop on a later pass once the manifest reflects the removal; nothing to
			// send now.
			continue
		}
		if stamp, stamped := stampsByID[conversationID]; stamped && w.isActivelyGrowing(stamp) {
			stats.deferred++
			continue
		}
		built, loadErr := w.loadDocs(ctx, record)
		if loadErr != nil {
			stats.failed++
			// The conversation delivers no document, so the engine will keep
			// listing it as needed and every later pass would read and fail it
			// again. Count the failure against the fingerprint that failed; the
			// manifest suppresses the conversation once the count reaches the
			// threshold, and a transcript that grows lifts that on its own. A
			// failure at a new fingerprint restarts the count, because the new
			// bytes have not been tried yet.
			if stamp, stamped := stampsByID[conversationID]; stamped {
				fingerprint := conversation.ContentFingerprint(record, stamp)
				failureRecord := w.failedLoad[conversationID]
				if failureRecord.fingerprint == fingerprint {
					failureRecord.failures++
				} else {
					failureRecord = failedLoadRecord{fingerprint: fingerprint, failures: 1}
				}
				w.failedLoad[conversationID] = failureRecord
			}
			continue
		}
		// A load that succeeds clears the conversation's failure history, so a
		// transient failure such as a busy Cursor store costs the passes it
		// spanned and nothing after.
		delete(w.failedLoad, conversationID)
		// A message the content policy withheld is counted apart from failed and
		// deferred, because it is the policy working rather than content lost.
		stats.policySkipped += built.PolicySkipped
		stats.injectedStripped += built.InjectedStripped
		stats.systemStripped += built.SystemStripped
		if len(built.Docs) == 0 {
			// The conversation rendered no deliverable documents, so the engine
			// can never mark it satisfied and would keep listing it as needed.
			// Record its fingerprint so the next manifest omits it until its
			// content changes, and do not count it as a delivered conversation.
			// A conversation every one of whose messages the policy withheld
			// lands here too, which is why the policy feeds this path rather
			// than bypassing it.
			// The same fingerprint the manifest advertises, so the suppression
			// lifts on exactly the changes that re-advertise the conversation.
			if stamp, stamped := stampsByID[conversationID]; stamped {
				w.emptyDelivered[conversationID] = conversation.ContentFingerprint(record, stamp)
			}
			continue
		}
		docs = append(docs, built.Docs...)
		sentConversations++
		w.deliveryCursor = conversationID
	}
	return docs, sentConversations
}

// isActivelyGrowing reports whether a conversation's transcript changed within
// the last sync interval, which marks the session the user is typing into
// right now. It settles after one quiet interval and is delivered then.
func (w *conversationSemanticSyncWorker) isActivelyGrowing(stamp conversation.FileStamp) bool {
	return w.now().Sub(stamp.Mtime) < w.interval
}

// rotateAfter returns ids rotated so iteration starts at the first id greater
// than cursor, wrapping around to the start. An empty cursor, or one at or
// past every id, keeps the original order. Each batch then resumes after the
// previous batch's last delivery instead of restarting at the smallest id, so
// late-sorting conversations are not starved by early-sorting ones.
func rotateAfter(ids []string, cursor string) []string {
	if cursor == "" || len(ids) == 0 {
		return ids
	}
	start := sort.SearchStrings(ids, cursor)
	if start < len(ids) && ids[start] == cursor {
		start++
	}
	if start <= 0 || start >= len(ids) {
		return ids
	}
	rotated := make([]string, 0, len(ids))
	rotated = append(rotated, ids[start:]...)
	rotated = append(rotated, ids[:start]...)
	return rotated
}

// sendDocuments fires one upsert for the collected documents and the full
// manifest. A conflicting-active-job rejection means a prior embed is still
// running, which is expected and benign: the worker logs it and the next pass
// retries once the engine is free. Any other failure is a real error.
func (w *conversationSemanticSyncWorker) sendDocuments(
	ctx context.Context,
	client conversationSemanticClient,
	docs []semsearch.SemDoc,
	manifest []semsearch.Fingerprint,
	sentConversations int,
	stats *conversationSemanticSyncStats,
) {
	jobID, upsertErr := client.UpsertConversationDocuments(ctx, w.collectionID, docs, manifest)
	if upsertErr != nil {
		if isConflictingActiveJob(upsertErr) {
			w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.engine_busy",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversations", sentConversations,
				"documents", len(docs),
			)
			return
		}
		stats.failed += sentConversations
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.upsert_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversations", sentConversations,
			"documents", len(docs),
			"err", upsertErr,
		)
		return
	}
	stats.sentConversations = sentConversations
	stats.documents = len(docs)
	w.activeJobID = jobID
	w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.upsert_started",
		"concern", "conversation.semantic",
		"component", "daemon",
		"conversations", sentConversations,
		"documents", len(docs),
		"job_id", jobID,
	)
}

// isConflictingActiveJob reports whether an upsert error is the engine's
// rejection of a second job while one is already running for the collection.
func isConflictingActiveJob(err error) bool {
	return err != nil && strings.Contains(err.Error(), "conflicting active job")
}

// logPass records one pass. policy_skipped and failed_suppressed are reported
// beside failed rather than folded into it, and a pass that withheld anything is
// logged at Info, so a feeder skipping most of a corpus is visible rather than
// buried at Debug.
//
// failed_suppressed keeps a shrinking manifest honest. A conversation left out
// because it failed earlier is content the store does not hold, so a pass that
// suppresses any is reported at Info even when it did nothing else.
func (w *conversationSemanticSyncWorker) logPass(ctx context.Context, stats conversationSemanticSyncStats) {
	w.freshness.publish(stats)
	if stats.sentConversations > 0 || stats.failed > 0 || stats.policySkipped > 0 || stats.failedSuppressed > 0 {
		w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"manifest", stats.manifest,
			"needed", stats.needed,
			"sent_conversations", stats.sentConversations,
			"documents", stats.documents,
			"deferred", stats.deferred,
			"failed", stats.failed,
			"failed_suppressed", stats.failedSuppressed,
			"policy_skipped", stats.policySkipped,
			"injected_stripped", stats.injectedStripped,
			"system_stripped", stats.systemStripped,
		)
		return
	}
	w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
		"concern", "conversation.semantic",
		"component", "daemon",
		"manifest", stats.manifest,
		"needed", stats.needed,
		"deferred", stats.deferred,
	)
}

func (w *conversationSemanticSyncWorker) loadDocs(ctx context.Context, record conversation.Record) (SemanticConversationDocuments, error) {
	empty := SemanticConversationDocuments{Docs: nil, PolicySkipped: 0, InjectedStripped: 0, SystemStripped: 0}
	messages, err := w.index.LoadMessagesWithOptions(record, SemanticConversationLoadOptions(w.contentKinds))
	if err != nil {
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.load_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", record.ID,
			"provider", record.Provider.String(),
			"err", err,
		)
		return empty, fmt.Errorf("load conversation messages for %s: %w", record.ID, err)
	}
	built, err := BuildSemanticConversationDocuments(record, messages, w.contentKinds)
	if err != nil {
		return empty, err
	}
	return built, nil
}

func semanticSyncContextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
