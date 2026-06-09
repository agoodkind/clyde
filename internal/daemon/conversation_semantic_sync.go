package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
)

const (
	conversationSemanticSyncInterval    = time.Minute
	conversationSemanticMaxDocsPerBatch = 800
	maxSemanticMessageIndex             = int32(1<<31 - 1)
)

type conversationSemanticIndex interface {
	ListWithStamps(context.Context) ([]conversation.StampedRecord, error)
	LoadMessagesWithOptions(conversation.Record, conversation.LoadOptions) ([]transcript.Message, error)
}

// conversationSemanticClient is the two-call feeder surface: state the full
// manifest, learn which conversations the engine needs, then send only those.
type conversationSemanticClient interface {
	SyncConversationManifest(context.Context, string, []semsearch.Fingerprint) ([]string, error)
	UpsertConversationDocuments(context.Context, string, []semsearch.SemDoc, []semsearch.Fingerprint) (string, error)
}

// conversationSemanticSyncWorker feeds the engine a content fingerprint per
// conversation each pass and sends documents only for the conversations the
// engine reports it needs. The engine owns drift, so the worker keeps no
// change-tracking state and never waits on or retries an engine job: a slow
// first embed runs once because the engine checkpoints each conversation, and a
// busy engine simply rejects the next upsert, which the worker logs and retries
// on the following pass.
type conversationSemanticSyncWorker struct {
	index           conversationSemanticIndex
	client          conversationSemanticClient
	collectionID    string
	log             *slog.Logger
	interval        time.Duration
	maxDocsPerBatch int
}

type conversationSemanticSyncStats struct {
	manifest          int
	needed            int
	sentConversations int
	documents         int
	failed            int
}

func startConversationSemanticSync(
	ctx context.Context,
	log *slog.Logger,
	index *conversation.Index,
	client conversationSemanticClient,
	collectionID string,
) func() {
	if client == nil {
		return func() {}
	}
	worker := newConversationSemanticSyncWorker(index, client, collectionID, log)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
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
	return func() {
		cancel()
		<-done
	}
}

func newConversationSemanticSyncWorker(
	index conversationSemanticIndex,
	client conversationSemanticClient,
	collectionID string,
	log *slog.Logger,
) *conversationSemanticSyncWorker {
	if log == nil {
		log = slog.Default()
	}
	return &conversationSemanticSyncWorker{
		index:           index,
		client:          client,
		collectionID:    strings.TrimSpace(collectionID),
		log:             log,
		interval:        conversationSemanticSyncInterval,
		maxDocsPerBatch: conversationSemanticMaxDocsPerBatch,
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

func (w *conversationSemanticSyncWorker) runPassAndLog(ctx context.Context) {
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
	if w == nil || w.index == nil || w.client == nil {
		return fmt.Errorf("semantic conversation sync worker is not configured")
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

	manifest, recordsByID := w.buildManifest(stampedRecords)
	stats := conversationSemanticSyncStats{manifest: len(manifest), needed: 0, sentConversations: 0, documents: 0, failed: 0}
	if len(manifest) == 0 {
		w.logPass(ctx, stats)
		return nil
	}

	needed, err := w.client.SyncConversationManifest(ctx, w.collectionID, manifest)
	if err != nil {
		return fmt.Errorf("sync conversation manifest: %w", err)
	}
	stats.needed = len(needed)
	if len(needed) == 0 {
		w.logPass(ctx, stats)
		return nil
	}

	docs, sentConversations := w.collectNeededDocuments(ctx, needed, recordsByID, &stats)
	if len(docs) == 0 {
		w.logPass(ctx, stats)
		return nil
	}
	if semanticSyncContextDone(ctx) {
		return nil
	}

	w.sendDocuments(ctx, docs, manifest, sentConversations, &stats)
	w.logPass(ctx, stats)
	return nil
}

// buildManifest renders the current conversation set as a fingerprint list and a
// lookup from conversation id to its record, skipping records with no id.
func (w *conversationSemanticSyncWorker) buildManifest(stampedRecords []conversation.StampedRecord) ([]semsearch.Fingerprint, map[string]conversation.Record) {
	manifest := make([]semsearch.Fingerprint, 0, len(stampedRecords))
	recordsByID := make(map[string]conversation.Record, len(stampedRecords))
	for _, stampedRecord := range stampedRecords {
		conversationID := strings.TrimSpace(stampedRecord.Record.ID)
		if conversationID == "" {
			continue
		}
		if _, seen := recordsByID[conversationID]; seen {
			continue
		}
		recordsByID[conversationID] = stampedRecord.Record
		manifest = append(manifest, semsearch.Fingerprint{
			ConversationID: conversationID,
			Value:          stampedRecord.Stamp.Fingerprint(),
		})
	}
	return manifest, recordsByID
}

// collectNeededDocuments loads the documents for the conversations the engine
// asked for, bounded by the per-batch document cap. The remaining needed
// conversations are picked up on the next pass, when the engine reports them
// still needed. It returns the documents and the count of conversations covered.
func (w *conversationSemanticSyncWorker) collectNeededDocuments(
	ctx context.Context,
	needed []string,
	recordsByID map[string]conversation.Record,
	stats *conversationSemanticSyncStats,
) ([]semsearch.SemDoc, int) {
	ordered := make([]string, len(needed))
	copy(ordered, needed)
	sort.Strings(ordered)

	docs := make([]semsearch.SemDoc, 0)
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
		conversationDocs, loadErr := w.loadDocs(ctx, record)
		if loadErr != nil {
			stats.failed++
			continue
		}
		if len(docs) > 0 && len(docs)+len(conversationDocs) > w.maxDocsPerBatch {
			break
		}
		docs = append(docs, conversationDocs...)
		sentConversations++
	}
	return docs, sentConversations
}

// sendDocuments fires one upsert for the collected documents and the full
// manifest. A conflicting-active-job rejection means a prior embed is still
// running, which is expected and benign: the worker logs it and the next pass
// retries once the engine is free. Any other failure is a real error.
func (w *conversationSemanticSyncWorker) sendDocuments(
	ctx context.Context,
	docs []semsearch.SemDoc,
	manifest []semsearch.Fingerprint,
	sentConversations int,
	stats *conversationSemanticSyncStats,
) {
	jobID, upsertErr := w.client.UpsertConversationDocuments(ctx, w.collectionID, docs, manifest)
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

func (w *conversationSemanticSyncWorker) logPass(ctx context.Context, stats conversationSemanticSyncStats) {
	if stats.sentConversations > 0 || stats.failed > 0 {
		w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"manifest", stats.manifest,
			"needed", stats.needed,
			"sent_conversations", stats.sentConversations,
			"documents", stats.documents,
			"failed", stats.failed,
		)
		return
	}
	w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
		"concern", "conversation.semantic",
		"component", "daemon",
		"manifest", stats.manifest,
		"needed", stats.needed,
	)
}

func (w *conversationSemanticSyncWorker) loadDocs(ctx context.Context, record conversation.Record) ([]semsearch.SemDoc, error) {
	messages, err := w.index.LoadMessagesWithOptions(record, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
	})
	if err != nil {
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.load_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", record.ID,
			"provider", record.Provider.String(),
			"err", err,
		)
		return nil, fmt.Errorf("load conversation-only messages for %s: %w", record.ID, err)
	}
	// All messages of one conversation share the same lineage parent, so derive
	// it once. ParentConversationID returns ok=false for records with no
	// resolvable parent, in which case the parent id stays "".
	parentConversationID := ""
	if derivedParentID, ok := conversation.ParentConversationID(record); ok {
		parentConversationID = derivedParentID
	}
	docs := make([]semsearch.SemDoc, 0, len(messages))
	for i, message := range messages {
		if i > int(maxSemanticMessageIndex) {
			return nil, fmt.Errorf("message index %d exceeds semantic search int32 limit", i)
		}
		docs = append(docs, semsearch.SemDoc{
			ConversationID:       record.ID,
			ParentConversationID: parentConversationID,
			MessageIndex:         int32(i),
			Role:                 message.Role,
			TimestampUnix:        message.Timestamp.Unix(),
			Text:                 message.Text,
		})
	}
	return docs, nil
}

func semanticSyncContextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
