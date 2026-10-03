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
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

// jobIDPrefix starts the ID of an in-process upsert. An upsert finishes before
// the call returns, and every job ID reads as completed.
const jobIDPrefix = "in-process-upsert-"

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
	c.ensured[collectionName] = true
	return nil
}

// SyncConversationManifest returns the sorted IDs of the conversations with a
// fingerprint that differs from the fingerprint of their last completed upsert.
// A conversation absent from the manifest changes nothing: no row is deleted
// because a conversation is missing.
func (c *Client) SyncConversationManifest(_ context.Context, collectionID string, manifest []semsearch.Fingerprint) ([]string, error) {
	if c == nil {
		return nil, errors.New("sync semantic conversation manifest: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return nil, errors.New("sync semantic conversation manifest: collection id is empty")
	}
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	checkpoint := c.checkpoints[trimmedCollectionID]
	needed := make([]string, 0)
	for _, fingerprint := range manifest {
		if fingerprint.ConversationID == "" {
			continue
		}
		if stored, found := checkpoint[fingerprint.ConversationID]; found && stored == fingerprint.Value {
			continue
		}
		needed = append(needed, fingerprint.ConversationID)
	}
	slices.Sort(needed)
	return slices.Compact(needed), nil
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
// delivered conversations. It loads the stored rows of each delivered
// conversation, writes a row group that has no stored row, and replaces a
// message text when the stored text differs from the delivered text.
// Replacement rows are written first, and the stored rows they replace are
// deleted afterwards. A stored vector with the same content and embedding model
// is reused, and only content without a stored vector is embedded. The call
// returns after the work finishes.
func (c *Client) UpsertConversationDocuments(
	ctx context.Context,
	collectionID string,
	docs []semsearch.SemDoc,
	manifest []semsearch.Fingerprint,
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
	batch, err := c.loadStoredBatch(ctx, collectionName, order)
	if err != nil {
		return "", err
	}
	stats := ingestStats{conversations: len(order), rowsWritten: 0, reused: 0, embedded: 0, dropped: 0, replaced: 0}
	chunks, replacements, err := c.planUpsert(byConversation, order, batch)
	if err != nil {
		return "", err
	}
	writer := newRowWriter(c, collectionName)
	if err := c.embedAndWrite(ctx, chunks, batch.reuse, writer, &stats); err != nil {
		return "", err
	}
	stats.rowsWritten = len(writer.written)
	if err := c.deleteReplaced(ctx, collectionName, replacements, writer.written, &stats); err != nil {
		return "", err
	}
	c.recordCheckpoints(trimmedCollectionID, order, manifest)
	slog.InfoContext(ctx, "conversation.vectorsearch.upsert_completed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", collectionName,
		"conversations", stats.conversations,
		"rows_written", stats.rowsWritten,
		"vectors_reused", stats.reused,
		"vectors_embedded", stats.embedded,
		"inputs_skipped", stats.dropped,
		"families_replaced", stats.replaced,
	)
	return jobID, nil
}

func (c *Client) nextJobID() string {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	c.jobCount++
	return fmt.Sprintf("%s%d", jobIDPrefix, c.jobCount)
}

// recordCheckpoints stores the manifest fingerprint of every delivered
// conversation. A conversation without a manifest entry stays needed.
func (c *Client) recordCheckpoints(collectionID string, conversationIDs []string, manifest []semsearch.Fingerprint) {
	fingerprints := make(map[string]string, len(manifest))
	for _, fingerprint := range manifest {
		fingerprints[fingerprint.ConversationID] = fingerprint.Value
	}
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	checkpoint := c.checkpoints[collectionID]
	if checkpoint == nil {
		checkpoint = make(map[string]string, len(conversationIDs))
		c.checkpoints[collectionID] = checkpoint
	}
	for _, conversationID := range conversationIDs {
		value, found := fingerprints[conversationID]
		if !found {
			continue
		}
		checkpoint[conversationID] = value
	}
}

// groupDocuments groups documents by trimmed conversation ID and returns the
// conversation IDs in sorted order.
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

// familyReplacement is a message text family with a stored text that differs
// from the delivered text. newIDs are the primary keys of the delivered rows.
// storedIDs are the stored rows already in that set. oldIDs are the stored rows
// to delete after every delivered row exists.
type familyReplacement struct {
	conversationID string
	familyKey      string
	newIDs         []string
	storedIDs      map[string]struct{}
	oldIDs         []string
}

// planUpsert returns the chunks to write for every delivered conversation and
// the families to replace after those chunks are written.
func (c *Client) planUpsert(
	byConversation map[string][]semsearch.SemDoc,
	order []string,
	batch *storedBatch,
) ([]storedChunk, []familyReplacement, error) {
	chunks := make([]storedChunk, 0)
	replacements := make([]familyReplacement, 0)
	seen := make(map[string]struct{})
	for _, conversationID := range order {
		generated := make([]storedChunk, 0)
		for _, doc := range byConversation[conversationID] {
			documentRows, err := documentChunks(doc, conversationChunkMaxBytes)
			if err != nil {
				return nil, nil, err
			}
			generated = append(generated, documentRows...)
		}
		state := batch.state(conversationID)
		for _, family := range groupFamilies(conversationID, generated) {
			expanded := expandOverBudget(family.Chunks, c.byteBudget)
			toWrite, replacement := planFamily(conversationID, family.Key, expanded, state)
			for _, chunk := range toWrite {
				id := chunkID(chunk)
				if _, duplicate := seen[id]; duplicate {
					continue
				}
				seen[id] = struct{}{}
				chunks = append(chunks, chunk)
			}
			if replacement != nil {
				replacements = append(replacements, *replacement)
			}
		}
	}
	return chunks, replacements, nil
}

// planFamily decides what to write for one family. A family with no stored row
// writes every chunk. A message text family with stored rows compares the
// stored text with the delivered text. Equal text writes nothing. Different text
// writes the chunks without a stored row and returns a replacement that deletes
// the stored rows outside the new set. A tool call or thinking family with a
// stored row writes nothing.
func planFamily(conversationID string, familyKey string, chunks []storedChunk, state *conversationState) ([]storedChunk, *familyReplacement) {
	if !state.present(familyKey) {
		return chunks, nil
	}
	if !strings.HasPrefix(familyKey, conversationRelativePathPrefix(conversationID)) {
		return nil, nil
	}
	storedRows := state.rows(familyKey)
	storedText, storedRole := storedTextOf(conversationID, storedRows)
	var deliveredText strings.Builder
	for _, chunk := range chunks {
		deliveredText.WriteString(chunk.Content)
	}
	sanitized, _ := milvusstore.SanitizeUTF8(deliveredText.String())
	roleChanged := storedRole != "" && !strings.EqualFold(storedRole, chunks[0].Role)
	if storedText == sanitized && !roleChanged {
		return nil, nil
	}
	storedIDs := make(map[string]struct{}, len(storedRows))
	for _, row := range storedRows {
		storedIDs[row.ID] = struct{}{}
	}
	newIDs := make([]string, 0, len(chunks))
	newIDSet := make(map[string]struct{}, len(chunks))
	toWrite := make([]storedChunk, 0, len(chunks))
	for _, chunk := range chunks {
		id := chunkID(chunk)
		newIDs = append(newIDs, id)
		newIDSet[id] = struct{}{}
		if _, stored := storedIDs[id]; !stored {
			toWrite = append(toWrite, chunk)
		}
	}
	oldIDs := make([]string, 0, len(storedRows))
	for _, row := range storedRows {
		if _, kept := newIDSet[row.ID]; !kept {
			oldIDs = append(oldIDs, row.ID)
		}
	}
	replacement := familyReplacement{
		conversationID: conversationID,
		familyKey:      familyKey,
		newIDs:         newIDs,
		storedIDs:      storedIDs,
		oldIDs:         oldIDs,
	}
	return toWrite, &replacement
}

// complete reports whether every delivered row of the family exists: written in
// this call or stored already.
func (replacement familyReplacement) complete(written map[string]struct{}) bool {
	for _, id := range replacement.newIDs {
		if _, wasWritten := written[id]; wasWritten {
			continue
		}
		if _, stored := replacement.storedIDs[id]; stored {
			continue
		}
		return false
	}
	return true
}

// deleteReplaced deletes the stored rows of each replaced family after the
// replacement rows are written. A family keeps its stored rows when the
// endpoint rejected a replacement input.
func (c *Client) deleteReplaced(
	ctx context.Context,
	collectionName string,
	replacements []familyReplacement,
	written map[string]struct{},
	stats *ingestStats,
) error {
	deleteIDs := make([]string, 0)
	for _, replacement := range replacements {
		if !replacement.complete(written) {
			slog.WarnContext(ctx, "conversation.vectorsearch.replacement_incomplete",
				"concern", "conversation.semantic",
				"component", "conversation",
				"conversation_id", replacement.conversationID,
				"family", replacement.familyKey,
			)
			continue
		}
		deleteIDs = append(deleteIDs, replacement.oldIDs...)
		stats.replaced++
	}
	if len(deleteIDs) == 0 {
		return nil
	}
	return c.deleteRowsByID(ctx, collectionName, deleteIDs)
}
