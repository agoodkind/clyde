package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

// jobIDPrefix starts the ID of an in-process upsert. An upsert finishes before
// the call returns, and every job ID reads as completed.
const jobIDPrefix = "in-process-upsert-"

// ErrUnprovenStoppedWrite reports a stopped write when the stored family rows do not
// establish which text is missing.
var ErrUnprovenStoppedWrite = errors.New("stored rows do not prove a stopped write can resume")

// ErrInputNotStored reports that the embedding endpoint rejected input that
// the client could not store. The conversation does not receive a checkpoint.
var ErrInputNotStored = errors.New("the embedding endpoint rejected text that the client could not store")

// errCheckpointWrite reports that rows exist but recording their checkpoint failed.
var errCheckpointWrite = errors.New("recording the checkpoint failed")

// ensureCollection creates the collection when it is absent and adds any
// declared scalar column it lacks. It runs once per collection and process.
func (c *Client) ensureCollection(ctx context.Context, collectionName string, dimension int) error {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	if c.ensured[collectionName] {
		return nil
	}
	err := c.store.EnsureCollection(ctx, collection.EnsureRequest{
		Collection:  collectionName,
		Declaration: c.declaration,
		Dimension:   dimension,
	})
	if err != nil {
		return failRead("ensure conversation collection "+collectionName, err)
	}
	if milvus, isMilvus := c.store.(*milvusBackend); isMilvus && milvus.shared != nil {
		// Load both collections because SharedStore reads stored vector keys before writing
		// rows, and Milvus rejects reads from a collection until it loads the collection.
		// loadStoredBatch loads existing collections, but it does not load newly created ones.
		if _, loadErr := milvus.collectionPresent(ctx, collectionName); loadErr != nil {
			return loadErr
		}
	}
	c.ensured[collectionName] = true
	return nil
}

// SyncConversationManifest returns the sorted IDs of the conversations with no
// recorded fingerprint or a recorded fingerprint that differs from the manifest.
// The records persist on disk. After a daemon restart the call reports only the
// changed conversations, and no stored row is read for the unchanged ones. A
// conversation absent from the manifest changes nothing.
func (c *Client) SyncConversationManifest(_ context.Context, collectionID string, manifest []semsearch.Fingerprint) ([]string, error) {
	if c == nil {
		return nil, errors.New("sync semantic conversation manifest: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return nil, errors.New("sync semantic conversation manifest: collection id is empty")
	}
	ids := make([]string, 0, len(manifest))
	values := make([]string, 0, len(manifest))
	for _, fingerprint := range manifest {
		ids = append(ids, fingerprint.ConversationID)
		values = append(values, fingerprint.Value)
	}
	needed, err := c.checkpoint.needed(CollectionName(trimmedCollectionID), ids, values)
	if err != nil {
		return nil, failRead("sync semantic conversation manifest", err)
	}
	return needed, nil
}

// JobState reports a job started by UpsertConversationDocuments. An upsert
// finishes before it returns, and the state is always completed.
func (c *Client) JobState(_ context.Context, jobID string) (string, error) {
	if c == nil {
		return "", errors.New("get semantic job state: client is nil")
	}
	if strings.TrimSpace(jobID) == "" {
		return "", errors.New("get semantic job state: job id is empty")
	}
	return semsearch.JobStateCompleted, nil
}

// UpsertConversationDocuments writes the rows that the collection lacks for the
// delivered conversations. It loads the stored rows of the delivered
// conversations once. The call includes edited text only when the stored family
// has split metadata and content-derived row IDs. The call changes and deletes
// no stored row. A stored vector with the same content and embedding model is
// reused, and only content without a stored vector is embedded. After a
// conversation's rows are written, its manifest fingerprint is recorded on disk.
// If a checkpoint write fails, the call keeps written rows and continues with the
// other conversations. It returns the joined checkpoint errors when the batch ends.
// If a later conversation fails during planning, row writing, embedding, or timestamp
// repair, the call stops and returns that failure joined with earlier checkpoint errors.
func (c *Client) UpsertConversationDocuments(
	ctx context.Context,
	collectionID string,
	docs []semsearch.SemDoc,
	manifest []semsearch.Fingerprint,
) (string, error) {
	return c.upsertDocuments(ctx, collectionID, docs, manifest, 0)
}

// UpsertConversationSlice writes delivered conversations up to the slice row limit.
// It returns a [PartialUpsertError] when rows remain unwritten. It does not record a
// new checkpoint for a partial conversation. Later calls continue the write and
// check the current source and stored rows before recording the checkpoint.
func (c *Client) UpsertConversationSlice(
	ctx context.Context,
	collectionID string,
	docs []semsearch.SemDoc,
	manifest []semsearch.Fingerprint,
) (string, error) {
	if c == nil {
		return "", errors.New("upsert semantic conversation slice: client is nil")
	}
	return c.upsertDocuments(ctx, collectionID, docs, manifest, c.ingestSliceRows)
}

func (c *Client) upsertDocuments(
	ctx context.Context,
	collectionID string,
	docs []semsearch.SemDoc,
	manifest []semsearch.Fingerprint,
	rowLimit int,
) (string, error) {
	if c == nil {
		return "", errors.New("upsert semantic conversation documents: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return "", errors.New("upsert semantic conversation documents: collection id is empty")
	}
	collectionName := CollectionName(trimmedCollectionID)
	byConversation, order, err := groupDocuments(docs)
	if err != nil {
		return "", err
	}
	jobID := c.nextJobID()
	if len(order) == 0 {
		return jobID, nil
	}
	if rowLimit > 0 {
		order = deliveredOrder(docs)
	}
	fingerprints := make(map[string]string, len(manifest))
	for _, fingerprint := range manifest {
		fingerprints[fingerprint.ConversationID] = fingerprint.Value
	}
	// partial stores conversation errors in Failures and the corresponding IDs in Failed. The
	// client continues processing other conversations.
	partial := PartialUpsertError{Completed: nil, Incomplete: nil, Unstarted: nil, Failed: nil, Failures: nil}
	// The client claims each conversation before reading its stored rows. Other clients
	// cannot write those conversations until this call releases the claims.
	claimed, release, err := c.checkpoint.claimConversations(collectionName, order, &partial)
	defer release()
	if err != nil {
		return "", stopUpsert(ctx, collectionName, partial.Failures, err)
	}
	resuming, loading := c.splitResumingConversations(collectionName, claimed, fingerprints, rowLimit)
	batch, err := c.loadStoredBatch(ctx, collectionName, loading)
	if err != nil {
		return "", stopUpsert(ctx, collectionName, partial.Failures, err)
	}
	stats := ingestStats{conversations: len(claimed), rowsRead: batch.rowsRead, rowsWritten: 0, reused: 0, embedded: 0, split: 0, dropped: 0, timesFilled: 0}
	// writtenConversations lists each conversation that received rows, as
	// "<conversation id>=<row count>", for the completion log record.
	writtenConversations := make([]string, 0)
	remaining := rowLimit
	for _, conversationID := range claimed {
		if rowLimit > 0 && remaining <= 0 {
			partial.Incomplete = append(partial.Incomplete, conversationID)
			partial.Unstarted = append(partial.Unstarted, conversationID)
			continue
		}
		var result conversationUpsert
		var conversationErr error
		if progress, resumed := resuming[conversationID]; resumed {
			result, conversationErr = c.resumeSlice(ctx, collectionName, conversationID, byConversation[conversationID], progress, &stats, remaining)
		} else {
			result, conversationErr = c.upsertConversation(ctx, collectionName, conversationID, byConversation[conversationID], fingerprints, batch, &stats, remaining)
		}
		stats.rowsWritten += result.written
		remaining -= result.processed
		if result.written > 0 {
			writtenConversations = append(writtenConversations, fmt.Sprintf("%s=%d", conversationID, result.written))
		}
		if err := partial.record(conversationID, result, conversationErr); err != nil {
			return "", stopUpsert(ctx, collectionName, partial.Failures, err)
		}
	}
	c.recordUpsert(UpsertStats{JobID: jobID, RowsRead: stats.rowsRead, RowsWritten: stats.rowsWritten, VectorsReused: stats.reused, VectorsEmbedded: stats.embedded})
	if len(partial.Incomplete) > 0 {
		return "", &partial
	}
	if err := errors.Join(partial.Failures...); err != nil {
		return "", err
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.upsert_completed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", collectionName,
		"conversations", stats.conversations,
		"rows_written", stats.rowsWritten,
		"written_conversations", writtenConversations,
		"vectors_reused", stats.reused,
		"vectors_embedded", stats.embedded,
		"inputs_split", stats.split,
		"inputs_skipped", stats.dropped,
		"times_filled", stats.timesFilled,
	)
	return jobID, nil
}

func stopUpsert(ctx context.Context, collectionName string, failures []error, err error) error {
	joined := errors.Join(append(failures, err)...)
	slog.WarnContext(ctx, "conversation.vectorsearch.upsert_stopped",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", collectionName,
		"err", joined,
	)
	return joined
}

// claimConversations returns a callable release function on every return, including errors.
func (store *checkpointStore) claimConversations(collectionName string, order []string, partial *PartialUpsertError) ([]string, func(), error) {
	claimed := make([]string, 0, len(order))
	claims := make([]*conversationClaim, 0, len(order))
	release := func() {
		for _, claim := range claims {
			claim.release()
		}
	}
	for _, conversationID := range order {
		claim, err := store.claimConversation(collectionName, conversationID)
		if errors.Is(err, ErrConversationWriteBusy) {
			partial.Failed = append(partial.Failed, conversationID)
			partial.Failures = append(partial.Failures, fmt.Errorf("conversation %s: %w", conversationID, err))
			continue
		}
		if err != nil {
			return claimed, release, failWrite("claim conversation "+conversationID, err)
		}
		claims = append(claims, claim)
		claimed = append(claimed, conversationID)
	}
	return claimed, release, nil
}

// splitResumingConversations returns current plans and the IDs of conversations that must
// read their stored rows. It returns plans only when rowLimit is positive. Each plan
// selects the next rows without loading all stored rows.
func (c *Client) splitResumingConversations(collectionName string, claimed []string, fingerprints map[string]string, rowLimit int) (map[string]*sliceProgress, []string) {
	resuming := make(map[string]*sliceProgress)
	loading := make([]string, 0, len(claimed))
	for _, conversationID := range claimed {
		if rowLimit > 0 {
			if progress := c.currentSliceProgress(collectionName, conversationID, fingerprints[conversationID]); progress != nil {
				resuming[conversationID] = progress
				continue
			}
		}
		loading = append(loading, conversationID)
	}
	return resuming, loading
}

// conversationUpsert reports the write result for one conversation. A complete
// result does not have unwritten rows. An incomplete result does not have a new
// checkpoint.
type conversationUpsert struct {
	written   int
	processed int
	complete  bool
}

// PartialUpsertError reports an upsert that used its row limit. Completed lists
// conversations with a checkpoint or without a manifest entry. Incomplete lists
// conversations with rows for a later upsert. Unstarted lists the Incomplete
// conversations that this upsert did not process. Failed lists conversations that
// failed for another reason. Failures lists their errors in the same order.
type PartialUpsertError struct {
	Completed  []string
	Incomplete []string
	Unstarted  []string
	Failed     []string
	Failures   []error
}

// record adds a conversation result to the matching list. It returns errors that stop the
// upsert.
func (e *PartialUpsertError) record(conversationID string, result conversationUpsert, err error) error {
	if errors.Is(err, ErrUnprovenStoppedWrite) || errors.Is(err, errCheckpointWrite) || errors.Is(err, ErrInputNotStored) {
		e.Failed = append(e.Failed, conversationID)
		e.Failures = append(e.Failures, err)
		return nil
	}
	if err != nil {
		return err
	}
	if result.complete {
		e.Completed = append(e.Completed, conversationID)
		return nil
	}
	e.Incomplete = append(e.Incomplete, conversationID)
	return nil
}

// Error reports the conversation counts and each failure.
func (e *PartialUpsertError) Error() string {
	summary := fmt.Sprintf("upsert used its row limit with %d incomplete and %d failed conversations", len(e.Incomplete), len(e.Failed))
	if len(e.Failures) == 0 {
		return summary
	}
	return summary + ": " + errors.Join(e.Failures...).Error()
}

// Unwrap returns the errors of the failed conversations.
func (e *PartialUpsertError) Unwrap() []error {
	return e.Failures
}

// upsertConversation writes rows for one claimed conversation. It creates a write
// record before writing the first row. The checkpoint completes the write. If the
// manifest omits the conversation, the record remains. A positive rowLimit can stop
// the write before the checkpoint.
func (c *Client) upsertConversation(
	ctx context.Context,
	collectionName string,
	conversationID string,
	docs []semsearch.SemDoc,
	fingerprints map[string]string,
	batch *storedBatch,
	stats *ingestStats,
	rowLimit int,
) (conversationUpsert, error) {
	result := conversationUpsert{written: 0, processed: 0, complete: false}
	families, err := conversationFamilies(conversationID, docs)
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.plan_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"conversation_id", conversationID,
			"err", err,
		)
		return result, err
	}
	proofs := familyProofs(families, c.embeddingModel)
	stopped, err := c.checkpoint.stoppedWrites(collectionName, conversationID)
	if err != nil {
		return result, fmt.Errorf("conversation %s: %w: %w", conversationID, ErrUnprovenStoppedWrite, err)
	}
	stoppedKeys := stoppedFamilies(stopped, proofs)
	state := batch.state(conversationID)
	if unproven := state.unprovenFamilies(families, stoppedKeys); len(unproven) > 0 {
		return result, fmt.Errorf("conversation %s families %v: %w", conversationID, unproven, ErrUnprovenStoppedWrite)
	}
	chunks, fills := c.planConversation(families, state, markedFamilies(stopped))
	fingerprint, inManifest := fingerprints[conversationID]
	var active *storedWrite
	if len(chunks) > 0 {
		started, beginErr := c.checkpoint.beginWrite(collectionName, conversationID, fingerprint, plannedFamilyProofs(conversationID, chunks, proofs))
		if beginErr != nil {
			return result, fmt.Errorf("record the write of %s: %w", conversationID, beginErr)
		}
		active = &started
	}
	writer := newRowWriter(c, collectionName, rowLimit)
	storedChild := func(chunk storedChunk) bool {
		return state.hasRow(chunkFamilyKey(conversationID, chunk.RelativePath), chunkID(chunk))
	}
	processed, unwritten, err := c.embedAndWrite(ctx, chunks, batch.reuse, writer, stats, rowLimit, storedChild)
	result.written = writer.written
	result.processed = processed
	if err != nil {
		return result, err
	}
	filled, err := c.fillUnknownTimes(ctx, collectionName, fills)
	if err != nil {
		return result, err
	}
	stats.timesFilled += filled
	if err := unstoredTextError(ctx, conversationID, writer.unstoredText); err != nil {
		return result, err
	}
	if len(unwritten) > 0 {
		c.checkpoint.supersedeStoppedWrites(collectionName, conversationID, active, stopped, proofs)
		c.saveSliceProgress(collectionName, conversationID, docs, fingerprint, writer.storedChunks(chunks), unwritten, batch)
		return result, nil
	}
	result.complete = true
	if !inManifest {
		return result, nil
	}
	if err := c.checkpoint.commitWrite(collectionName, conversationID, fingerprint, active, stopped, proofs); err != nil {
		return result, fmt.Errorf("conversation %s: %w: %w", conversationID, errCheckpointWrite, err)
	}
	return result, nil
}

// unstoredTextError returns ErrInputNotStored when count is positive.
func unstoredTextError(ctx context.Context, conversationID string, count int) error {
	if count == 0 {
		return nil
	}
	slog.WarnContext(ctx, "conversation.vectorsearch.input_not_stored",
		"concern", "conversation.semantic",
		"component", "conversation",
		"conversation_id", conversationID,
		"rejected_inputs", count,
	)
	return fmt.Errorf("conversation %s has %d rejected inputs: %w", conversationID, count, ErrInputNotStored)
}

func (c *Client) nextJobID() string {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	c.jobCount++
	return fmt.Sprintf("%s%d", jobIDPrefix, c.jobCount)
}

func groupDocuments(docs []semsearch.SemDoc) (map[string][]semsearch.SemDoc, []string, error) {
	byConversation := make(map[string][]semsearch.SemDoc)
	for _, doc := range docs {
		conversationID := strings.TrimSpace(doc.ConversationID)
		if conversationID == "" {
			return nil, nil, errors.New("conversation id is required")
		}
		byConversation[conversationID] = append(byConversation[conversationID], doc)
	}
	order := make([]string, 0, len(byConversation))
	for conversationID := range byConversation {
		order = append(order, conversationID)
	}
	slices.Sort(order)
	return byConversation, order, nil
}

// deliveredOrder lists conversation IDs in the order of their first documents.
// A row limit can stop a batch early. The worker orders delivery to select the next
// conversation first.
func deliveredOrder(docs []semsearch.SemDoc) []string {
	order := make([]string, 0)
	for _, doc := range docs {
		conversationID := strings.TrimSpace(doc.ConversationID)
		if !slices.Contains(order, conversationID) {
			order = append(order, conversationID)
		}
	}
	return order
}

func conversationFamilies(conversationID string, docs []semsearch.SemDoc) ([]chunkFamily, error) {
	generated := make([]storedChunk, 0)
	for _, doc := range docs {
		documentRows, err := documentChunks(doc)
		if err != nil {
			return nil, err
		}
		generated = append(generated, documentRows...)
	}
	return groupFamilies(conversationID, generated), nil
}

// planConversation includes edited text when stored rows do not prove that ingestion
// has stored the current message. The interrupted set lists families with unfinished
// writes. Without that evidence, a stored prefix can represent a complete older
// version, and the plan preserves that family. planConversation includes only byte ranges
// without a matching stored piece when stored rows do not cover the generated chunk. It
// skips a chunk when stored pieces for the same path match its full content, even if their
// split boundaries differ.
func (c *Client) planConversation(families []chunkFamily, state *conversationState, interrupted map[string]struct{}) ([]storedChunk, map[string]matchedRow) {
	chunks := make([]storedChunk, 0)
	fills := make(map[string]matchedRow)
	seen := make(map[string]struct{})
	for _, family := range families {
		unchanged := state.present(family.Key) && !state.differs(family.Key, family.Chunks)
		if _, stopped := interrupted[family.Key]; stopped && !state.hasOlderRow(family.Key) {
			unchanged = false
		}
		for _, generated := range family.Chunks {
			skipRows := unchanged || state.pathComplete(family.Key, generated)
			for _, chunk := range expandOverBudget([]storedChunk{generated}, c.byteBudget) {
				id := chunkID(chunk)
				if chunk.TimestampUnix != missingTimestampUnix && state.hasUnknownTimeRow(family.Key, id) {
					fills[id] = matchedRow{relativePath: chunk.RelativePath, timestampUnix: chunk.TimestampUnix}
				}
			}
			if skipRows {
				continue
			}
			for _, chunk := range expandOverBudget(state.missingPieces(family.Key, generated), c.byteBudget) {
				id := chunkID(chunk)
				if _, duplicate := seen[id]; duplicate {
					continue
				}
				if state.hasRow(family.Key, id) {
					continue
				}
				seen[id] = struct{}{}
				chunks = append(chunks, chunk)
			}
		}
	}
	return chunks, fills
}

// fillUnknownTimes writes known timestamps to matching stored rows after checking metadata.
func (c *Client) fillUnknownTimes(ctx context.Context, collectionName string, fills map[string]matchedRow) (int, error) {
	if len(fills) == 0 {
		return 0, nil
	}
	c.fillMu.Lock()
	defer c.fillMu.Unlock()
	switch store := c.store.(type) {
	case *localBackend:
		return store.fillUnknownTimes(ctx, collectionName, fills)
	case *milvusBackend:
		return c.fillUnknownMilvusTimes(ctx, store, collectionName, fills)
	default:
		return 0, ErrTimestampRepairUnsupported
	}
}

// fillUnknownMilvusTimes reads stored timestamps and fills eligible Milvus rows.
func (c *Client) fillUnknownMilvusTimes(ctx context.Context, milvus *milvusBackend, collectionName string, fills map[string]matchedRow) (int, error) {
	ids := sortedFillIDs(fills)
	stored, err := milvus.readRowTimestamps(ctx, collectionName, ids)
	if err != nil {
		return 0, err
	}
	updates := make([]timestampUpdate, 0, len(ids))
	for _, id := range ids {
		row, found := stored[id]
		if !found {
			continue
		}
		fill := fills[id]
		rewritten, apply := unknownTimeFill(id, row.metadata, row.timestampKnown, row.timestampUnix, fill.timestampUnix)
		if !apply {
			continue
		}
		updates = append(updates, timestampUpdate{id: id, relativePath: fill.relativePath, metadata: rewritten, timestampUnix: fill.timestampUnix})
	}
	report := TimestampRepairReport{
		Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0,
		RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0,
	}
	if err := writeSourceTimestamps(ctx, c, milvus, collectionName, updates, &report); err != nil {
		return report.RowsWritten, err
	}
	return report.RowsWritten, nil
}

// fillUnknownTimes fills eligible local rows with known timestamps.
func (b *localBackend) fillUnknownTimes(ctx context.Context, collectionName string, fills map[string]matchedRow) (int, error) {
	columns := []collection.ScalarColumn{{Name: timestampUnixColumn, Type: collection.ScalarTypeInt64, Nullable: true, MaxLength: 0}}
	written, err := b.UpdateRows(ctx, collectionName, sortedFillIDs(fills), columns,
		func(id string, metadata string, cells map[string]collection.ScalarCell) (string, map[string]collection.ScalarValue, bool, error) {
			cell := cells[timestampUnixColumn]
			want := fills[id].timestampUnix
			rewritten, apply := unknownTimeFill(id, metadata, cell.State == collection.ScalarCellValue, cell.Value.Int64, want)
			if !apply {
				return "", nil, false, nil
			}
			return rewritten, map[string]collection.ScalarValue{timestampUnixColumn: timestampScalar(want)}, true, nil
		})
	if err != nil {
		return written, failRead("fill unknown row times in "+collectionName, err)
	}
	return written, nil
}

// unknownTimeFill replaces an unknown metadata timestamp when the target is known
// and the stored scalar is null or contains the unknown marker.
func unknownTimeFill(id string, metadata string, scalarKnown bool, scalarValue int64, want int64) (string, bool) {
	if want == missingTimestampUnix || (scalarKnown && scalarValue != missingTimestampUnix) {
		return "", false
	}
	rewritten, metadataTimestamp, err := replaceMetadataTimestamp(id, metadata, want)
	if err != nil || metadataTimestamp != missingTimestampUnix {
		return "", false
	}
	return rewritten, true
}

func sortedFillIDs(fills map[string]matchedRow) []string {
	ids := make([]string, 0, len(fills))
	for id := range fills {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
