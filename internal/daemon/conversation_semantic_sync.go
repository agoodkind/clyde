package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/semsearch"
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
// cumulative conversation coverage (manifest minus the conversations the engine
// still needs), pending is the conversations the engine still needs that this
// pass did not cover, and last_sync is the pass completion time on the repo
// clock. embedded is conversations, not the per-pass document count, so
// embedded + needed == manifest and the number tracks real coverage.
func (f *conversationSemanticFreshness) publish(stats conversationSemanticSyncStats) {
	if f == nil {
		return
	}
	pending := max(stats.needed-stats.sentConversations, 0)
	embedded := max(stats.manifest-stats.needed, 0)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.freshness = conversation.SearchFreshness{
		Manifest:     stats.manifest,
		Needed:       stats.needed,
		Embedded:     embedded,
		Pending:      pending,
		LastSyncUnix: clock.Now().Unix(),
	}
}

const (
	conversationSemanticSyncInterval = time.Minute
	maxSemanticMessageIndex          = int32(1<<31 - 1)
)

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

// conversationSemanticSyncWorker feeds the engine a content fingerprint per
// conversation each pass and sends documents only for the conversations the
// engine reports it needs. The engine owns drift, so the worker keeps no
// change-tracking state beyond two scheduling hints: the engine job id of the
// last accepted upsert, which lets a pass skip all work while that job still
// runs, and the delivery cursor, which rotates batch order so late-sorting
// conversations are not starved.
type conversationSemanticSyncWorker struct {
	index        conversationSemanticIndex
	client       conversationSemanticClient
	collectionID string
	log          *slog.Logger
	interval     time.Duration
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
	// now returns the wall clock; tests override it to pin deferral decisions.
	now func() time.Time
	// freshness receives the latest pass stats so the control server can report
	// index sync state at query time. Nil disables publishing.
	freshness *conversationSemanticFreshness
}

type conversationSemanticSyncStats struct {
	manifest          int
	needed            int
	sentConversations int
	documents         int
	deferred          int
	failed            int
}

func startConversationSemanticSync(
	ctx context.Context,
	log *slog.Logger,
	index *conversation.Index,
	client conversationSemanticClient,
	collectionID string,
	freshness *conversationSemanticFreshness,
) func() {
	if client == nil {
		return func() {}
	}
	worker := newConversationSemanticSyncWorker(index, client, collectionID, log)
	worker.freshness = freshness
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
		index:          index,
		client:         client,
		collectionID:   strings.TrimSpace(collectionID),
		log:            log,
		interval:       conversationSemanticSyncInterval,
		activeJobID:    "",
		deliveryCursor: "",
		emptyDelivered: make(map[string]string),
		now:            time.Now,
		freshness:      nil,
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
	if w.engineBusyWithLastJob(ctx) {
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

	manifest, recordsByID, stampsByID := w.buildManifest(stampedRecords)
	stats := conversationSemanticSyncStats{manifest: len(manifest), needed: 0, sentConversations: 0, documents: 0, deferred: 0, failed: 0}
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

	docs, sentConversations := w.collectNeededDocuments(ctx, needed, recordsByID, stampsByID, &stats)
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

// engineBusyWithLastJob reports whether the engine job from the last accepted
// upsert is still active. While it is, a pass would only re-read transcripts
// from disk and then be rejected with a conflicting-active-job error, so the
// worker skips the whole pass: no manifest sync and no transcript loads. A
// terminal state or a failed lookup clears the id and lets the pass proceed;
// the upsert path still handles a busy engine on its own.
func (w *conversationSemanticSyncWorker) engineBusyWithLastJob(ctx context.Context) bool {
	if w.activeJobID == "" {
		return false
	}
	state, err := w.client.JobState(ctx, w.activeJobID)
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
func (w *conversationSemanticSyncWorker) buildManifest(stampedRecords []conversation.StampedRecord) ([]semsearch.Fingerprint, map[string]conversation.Record, map[string]conversation.FileStamp) {
	manifest := make([]semsearch.Fingerprint, 0, len(stampedRecords))
	recordsByID := make(map[string]conversation.Record, len(stampedRecords))
	stampsByID := make(map[string]conversation.FileStamp, len(stampedRecords))
	seen := make(map[string]bool, len(stampedRecords))
	for _, stampedRecord := range stampedRecords {
		conversationID := strings.TrimSpace(stampedRecord.Record.ID)
		if conversationID == "" {
			continue
		}
		if seen[conversationID] {
			continue
		}
		seen[conversationID] = true
		fingerprint := stampedRecord.Stamp.Fingerprint()
		if priorFingerprint, delivered := w.emptyDelivered[conversationID]; delivered {
			if priorFingerprint == fingerprint {
				continue
			}
			delete(w.emptyDelivered, conversationID)
		}
		recordsByID[conversationID] = stampedRecord.Record
		stampsByID[conversationID] = stampedRecord.Stamp
		manifest = append(manifest, semsearch.Fingerprint{
			ConversationID: conversationID,
			Value:          fingerprint,
		})
	}
	w.pruneEmptyDelivered(seen)
	return manifest, recordsByID, stampsByID
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
		conversationDocs, loadErr := w.loadDocs(ctx, record)
		if loadErr != nil {
			stats.failed++
			continue
		}
		if len(conversationDocs) == 0 {
			// The conversation rendered no deliverable documents, so the engine
			// can never mark it satisfied and would keep listing it as needed.
			// Record its fingerprint so the next manifest omits it until its
			// content changes, and do not count it as a delivered conversation.
			if stamp, stamped := stampsByID[conversationID]; stamped {
				w.emptyDelivered[conversationID] = stamp.Fingerprint()
			}
			continue
		}
		docs = append(docs, conversationDocs...)
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

func (w *conversationSemanticSyncWorker) logPass(ctx context.Context, stats conversationSemanticSyncStats) {
	w.freshness.publish(stats)
	if stats.sentConversations > 0 || stats.failed > 0 {
		w.log.InfoContext(ctx, "daemon.conversation_semantic_sync.pass_completed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"manifest", stats.manifest,
			"needed", stats.needed,
			"sent_conversations", stats.sentConversations,
			"documents", stats.documents,
			"deferred", stats.deferred,
			"failed", stats.failed,
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

// SemanticConversationLoadOptions returns the transcript load options for
// semantic document production.
func SemanticConversationLoadOptions() conversation.LoadOptions {
	return conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    true,
	}
}

// BuildSemanticConversationDocuments projects loaded transcript messages into
// the document shape sent to lm-semantic-search.
func BuildSemanticConversationDocuments(record conversation.Record, messages []transcript.Message) ([]semsearch.SemDoc, error) {
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
			// Replace invalid UTF-8 so the protobuf upsert never fails to marshal
			// on a transcript byte sequence the encoder rejects (one codex doc
			// with invalid UTF-8 used to break the whole batch).
			Text:          strings.ToValidUTF8(message.Text, ""),
			Tools:         semanticToolCalls(message.Tools),
			Thinking:      strings.ToValidUTF8(message.Thinking, ""),
			WorkspaceRoot: record.WorkspaceRoot,
			Archived:      record.Archived,
		})
	}
	return docs, nil
}

func semanticToolCalls(tools []transcript.ToolCall) []semsearch.SemToolCall {
	out := make([]semsearch.SemToolCall, 0, len(tools))
	for _, tool := range tools {
		command, langHint := deriveToolCommandAndLang(tool.Name, tool.Input)
		out = append(out, semsearch.SemToolCall{
			Name:      tool.Name,
			InputJSON: string(tool.Input.Raw),
			Command:   command,
			LangHint:  langHint,
			Output:    tool.Output,
			IsError:   tool.IsError,
		})
	}
	return out
}

type semanticToolCommandInput struct {
	Command string `json:"command"`
	Cmd     string `json:"cmd"`
}

func deriveToolCommandAndLang(name string, input transcript.ToolInputJSON) (string, string) {
	var parsed semanticToolCommandInput
	if len(input.Raw) > 0 && json.Unmarshal(input.Raw, &parsed) == nil {
		if strings.TrimSpace(parsed.Command) != "" {
			return parsed.Command, "bash"
		}
		if strings.TrimSpace(parsed.Cmd) != "" {
			return parsed.Cmd, "bash"
		}
	}
	if name == "ExitPlanMode" {
		return "", "markdown"
	}
	return "", "json"
}

func (w *conversationSemanticSyncWorker) loadDocs(ctx context.Context, record conversation.Record) ([]semsearch.SemDoc, error) {
	messages, err := w.index.LoadMessagesWithOptions(record, SemanticConversationLoadOptions())
	if err != nil {
		w.log.WarnContext(ctx, "daemon.conversation_semantic_sync.load_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", record.ID,
			"provider", record.Provider.String(),
			"err", err,
		)
		return nil, fmt.Errorf("load conversation messages for %s: %w", record.ID, err)
	}
	docs, err := BuildSemanticConversationDocuments(record, messages)
	if err != nil {
		return nil, err
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
