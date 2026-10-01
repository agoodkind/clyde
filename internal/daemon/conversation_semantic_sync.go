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
	// Admit artifacts before loading them, so preparing the next batch never
	// retains the previous batch. A larger artifact is streamed alone.
	conversationSemanticBatchBytes = 8 << 20
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

const (
	conversationSemanticSyncHookName = "conversation.semantic.sync_stop"
	maxLoggedConversationIDs         = 10
)

type conversationSemanticSyncWorker struct {
	index          conversationSemanticIndex
	collectionID   string
	log            *slog.Logger
	interval       time.Duration
	deliveryCursor string
	failedLoad     map[string]failedLoadRecord
	now            func() time.Time
	freshness      *conversationSemanticFreshness
	contentKinds   conversation.ContentKindSet
	embedded       *embeddedConversationSync
}

func newConversationSemanticSyncWorker(index conversationSemanticIndex, collectionID string, log *slog.Logger, contentKinds conversation.ContentKindSet) *conversationSemanticSyncWorker {
	if log == nil {
		log = slog.Default()
	}
	return &conversationSemanticSyncWorker{
		index:          index,
		collectionID:   strings.TrimSpace(collectionID),
		log:            log,
		interval:       conversationSemanticSyncInterval,
		deliveryCursor: "",
		failedLoad:     make(map[string]failedLoadRecord),
		now:            clock.Now,
		freshness:      nil,
		contentKinds:   contentKinds,
		embedded:       nil,
	}
}

func (w *conversationSemanticSyncWorker) runPass(ctx context.Context) error {
	if semanticSyncContextDone(ctx) {
		return nil
	}
	if w == nil || w.index == nil || w.embedded == nil {
		return fmt.Errorf("conversation ingestion worker is not configured")
	}
	return w.runEmbeddedPass(ctx)
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
	// sentConversationIDs names the conversations this pass delivered, so a
	// pass in the log can be attributed to specific conversations. The log line
	// bounds the list; the full slice stays here for the freshness snapshot.
	sentConversationIDs []string
	// unchangedPinned counts conversations this pass recognized as touched
	// without a content change: their stamp moved, their projected bytes did
	// not, so nothing was delivered and the manifest pins the checkpointed
	// fingerprint from the next pass on.
	unchangedPinned int
}

func boundedConversationIDs(ids []string) []string {
	if len(ids) <= maxLoggedConversationIDs {
		return ids
	}
	return ids[:maxLoggedConversationIDs]
}

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

func (w *conversationSemanticSyncWorker) pruneFailedLoad(seen map[string]bool) {
	for conversationID := range w.failedLoad {
		if !seen[conversationID] {
			delete(w.failedLoad, conversationID)
		}
	}
}

func (w *conversationSemanticSyncWorker) recordLoadFailure(conversationID, fingerprint string) {
	failureRecord := w.failedLoad[conversationID]
	if failureRecord.fingerprint == fingerprint {
		failureRecord.failures++
	} else {
		failureRecord = failedLoadRecord{fingerprint: fingerprint, failures: 1}
	}
	w.failedLoad[conversationID] = failureRecord
}

func (w *conversationSemanticSyncWorker) isActivelyGrowing(stamp conversation.FileStamp) bool {
	return w.now().Sub(stamp.Mtime) < w.interval
}

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

func (w *conversationSemanticSyncWorker) loadDocs(ctx context.Context, record conversation.Record) (SemanticConversationDocuments, error) {
	empty := SemanticConversationDocuments{Docs: nil, PolicySkipped: 0, InjectedStripped: 0, SystemStripped: 0}
	// The tally counts what the parsers removed or withheld during this load,
	// including records dropped entirely, which per-message counting loses.
	var tally transcript.HarnessStrips
	options := SemanticConversationLoadOptions(w.contentKinds)
	options.HarnessTally = &tally
	messages, err := w.index.LoadMessagesWithOptions(record, options)
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
	built.InjectedStripped = tally.Injected
	built.SystemStripped = tally.System
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
