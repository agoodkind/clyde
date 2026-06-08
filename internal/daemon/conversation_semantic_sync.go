package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/transcript"
)

const (
	conversationSemanticSyncInterval         = time.Minute
	conversationSemanticMaxOperationsPerPass = 25
	conversationSemanticMaxDocsPerBatch      = 5000
	conversationSemanticJobPollInterval      = time.Second
	conversationSemanticJobTimeout           = 3 * time.Minute
	maxSemanticMessageIndex                  = int32(1<<31 - 1)
)

type conversationSemanticIndex interface {
	ListWithStamps(context.Context) ([]conversation.StampedRecord, error)
	LoadMessagesWithOptions(conversation.Record, conversation.LoadOptions) ([]transcript.Message, error)
}

type conversationSemanticClient interface {
	UpsertConversationDocuments(context.Context, string, []semsearch.SemDoc) (string, error)
	DeleteConversation(context.Context, string, string) (string, error)
	JobState(context.Context, string) (string, error)
	WaitForJob(context.Context, string, time.Duration, time.Duration) (string, error)
}

type semanticBatchMember struct {
	conversationID string
	stamp          conversation.FileStamp
}

type conversationSemanticSyncWorker struct {
	index                conversationSemanticIndex
	client               conversationSemanticClient
	collectionID         string
	log                  *slog.Logger
	interval             time.Duration
	maxOperationsPerPass int
	lastPushed           map[string]conversation.FileStamp
}

type conversationSemanticSyncStats struct {
	operations int
	upserted   int
	deleted    int
	skipped    int
	failed     int
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
		index:                index,
		client:               client,
		collectionID:         strings.TrimSpace(collectionID),
		log:                  log,
		interval:             conversationSemanticSyncInterval,
		maxOperationsPerPass: conversationSemanticMaxOperationsPerPass,
		lastPushed:           make(map[string]conversation.FileStamp),
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
	currentRecords := currentSemanticRecords(stampedRecords)
	operationLimit := w.operationLimit()
	stats := conversationSemanticSyncStats{
		operations: 0,
		upserted:   0,
		deleted:    0,
		skipped:    0,
		failed:     0,
	}

	if err := w.deleteRemovedConversations(ctx, currentRecords, operationLimit, &stats); err != nil {
		return err
	}
	if err := w.upsertChangedConversations(ctx, stampedRecords, operationLimit, &stats); err != nil {
		return err
	}
	w.logPass(ctx, len(stampedRecords), operationLimit, stats)
	return nil
}

func (w *conversationSemanticSyncWorker) operationLimit() int {
	if w.maxOperationsPerPass <= 0 {
		return conversationSemanticMaxOperationsPerPass
	}
	return w.maxOperationsPerPass
}

func (w *conversationSemanticSyncWorker) deleteRemovedConversations(
	ctx context.Context,
	currentRecords map[string]conversation.StampedRecord,
	operationLimit int,
	stats *conversationSemanticSyncStats,
) error {
	for _, conversationID := range w.removedConversationIDs(currentRecords) {
		if stats.operations >= operationLimit {
			break
		}
		if semanticSyncContextDone(ctx) {
			return nil
		}
		jobID, deleteErr := w.client.DeleteConversation(ctx, w.collectionID, conversationID)
		if deleteErr != nil {
			stats.failed++
			w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.delete_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", conversationID,
				"err", deleteErr,
			)
			continue
		}
		finalState, waitErr := w.client.WaitForJob(ctx, jobID, conversationSemanticJobPollInterval, conversationSemanticJobTimeout)
		if waitErr != nil || finalState != semsearch.JobStateCompleted {
			stats.failed++
			w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.delete_incomplete",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", conversationID,
				"job_id", jobID,
				"state", finalState,
				"err", waitErr,
			)
			continue
		}
		delete(w.lastPushed, conversationID)
		stats.deleted++
		stats.operations++
		w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.delete_completed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", conversationID,
			"job_id", jobID,
			"state", finalState,
		)
	}
	return nil
}

func (w *conversationSemanticSyncWorker) upsertChangedConversations(
	ctx context.Context,
	stampedRecords []conversation.StampedRecord,
	operationLimit int,
	stats *conversationSemanticSyncStats,
) error {
	docs, members := w.collectChangedBatch(ctx, stampedRecords, operationLimit, stats)
	if len(members) == 0 {
		return nil
	}
	if semanticSyncContextDone(ctx) {
		return nil
	}
	jobID, upsertErr := w.client.UpsertConversationDocuments(ctx, w.collectionID, docs)
	if upsertErr != nil {
		stats.failed += len(members)
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.upsert_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversations", len(members),
			"documents", len(docs),
			"err", upsertErr,
		)
		return nil
	}
	finalState, waitErr := w.client.WaitForJob(ctx, jobID, conversationSemanticJobPollInterval, conversationSemanticJobTimeout)
	if waitErr != nil || finalState != semsearch.JobStateCompleted {
		stats.failed += len(members)
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.upsert_incomplete",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversations", len(members),
			"documents", len(docs),
			"job_id", jobID,
			"state", finalState,
			"err", waitErr,
		)
		return nil
	}
	for _, member := range members {
		w.lastPushed[member.conversationID] = member.stamp
		stats.upserted++
		stats.operations++
	}
	w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.upsert_completed",
		"concern", "conversation.semantic",
		"component", "daemon",
		"conversations", len(members),
		"documents", len(docs),
		"job_id", jobID,
		"state", finalState,
	)
	return nil
}

// collectChangedBatch gathers the changed conversations for one pass into a
// single combined document slice, bounded by the remaining per-pass operation
// budget and the per-batch document cap, returning the docs and the parallel
// list of conversations whose stamps should be marked once the job completes.
func (w *conversationSemanticSyncWorker) collectChangedBatch(
	ctx context.Context,
	stampedRecords []conversation.StampedRecord,
	operationLimit int,
	stats *conversationSemanticSyncStats,
) ([]semsearch.SemDoc, []semanticBatchMember) {
	remaining := operationLimit - stats.operations
	if remaining <= 0 {
		return nil, nil
	}
	docs := make([]semsearch.SemDoc, 0)
	members := make([]semanticBatchMember, 0)
	for _, stampedRecord := range stampedRecords {
		if len(members) >= remaining {
			break
		}
		if semanticSyncContextDone(ctx) {
			break
		}
		record := stampedRecord.Record
		if strings.TrimSpace(record.ID) == "" {
			stats.skipped++
			continue
		}
		if lastStamp, ok := w.lastPushed[record.ID]; ok && lastStamp.Equal(stampedRecord.Stamp) {
			stats.skipped++
			continue
		}
		conversationDocs, loadErr := w.loadDocs(ctx, record)
		if loadErr != nil {
			stats.failed++
			continue
		}
		if len(docs) > 0 && len(docs)+len(conversationDocs) > conversationSemanticMaxDocsPerBatch {
			break
		}
		docs = append(docs, conversationDocs...)
		members = append(members, semanticBatchMember{
			conversationID: record.ID,
			stamp:          stampedRecord.Stamp,
		})
	}
	return docs, members
}

func (w *conversationSemanticSyncWorker) logPass(
	ctx context.Context,
	recordCount int,
	operationLimit int,
	stats conversationSemanticSyncStats,
) {
	if stats.operations > 0 || stats.failed > 0 {
		w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"records", recordCount,
			"operations", stats.operations,
			"upserted", stats.upserted,
			"deleted", stats.deleted,
			"skipped", stats.skipped,
			"failed", stats.failed,
			"operation_limit", operationLimit,
		)
	} else {
		w.log.DebugContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"records", recordCount,
			"operations", stats.operations,
			"skipped", stats.skipped,
			"operation_limit", operationLimit,
		)
	}
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
	docs := make([]semsearch.SemDoc, 0, len(messages))
	for i, message := range messages {
		if i > int(maxSemanticMessageIndex) {
			return nil, fmt.Errorf("message index %d exceeds semantic search int32 limit", i)
		}
		docs = append(docs, semsearch.SemDoc{
			ConversationID: record.ID,
			MessageIndex:   int32(i),
			Role:           message.Role,
			TimestampUnix:  message.Timestamp.Unix(),
			Text:           message.Text,
		})
	}
	return docs, nil
}

func currentSemanticRecords(stampedRecords []conversation.StampedRecord) map[string]conversation.StampedRecord {
	currentRecords := make(map[string]conversation.StampedRecord, len(stampedRecords))
	for _, stampedRecord := range stampedRecords {
		conversationID := strings.TrimSpace(stampedRecord.Record.ID)
		if conversationID == "" {
			continue
		}
		currentRecords[conversationID] = stampedRecord
	}
	return currentRecords
}

func (w *conversationSemanticSyncWorker) removedConversationIDs(currentRecords map[string]conversation.StampedRecord) []string {
	removed := make([]string, 0)
	for conversationID := range w.lastPushed {
		if _, ok := currentRecords[conversationID]; ok {
			continue
		}
		removed = append(removed, conversationID)
	}
	slices.Sort(removed)
	return removed
}

func semanticSyncContextDone(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
