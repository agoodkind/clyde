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
	return c.checkpoint.needed(CollectionName(trimmedCollectionID), ids, values), nil
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
// conversations once. For each conversation it writes every row group that has
// no stored row and skips every group that has one. The call changes and deletes
// no stored row. A stored vector with the same content and embedding model is
// reused, and only content without a stored vector is embedded. After a
// conversation's rows are written, its manifest fingerprint is recorded on disk.
// The call returns after the work finishes.
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
	fingerprints := make(map[string]string, len(manifest))
	for _, fingerprint := range manifest {
		fingerprints[fingerprint.ConversationID] = fingerprint.Value
	}
	batch, err := c.loadStoredBatch(ctx, collectionName, order)
	if err != nil {
		return "", err
	}
	stats := ingestStats{conversations: len(order), rowsWritten: 0, reused: 0, embedded: 0, split: 0, dropped: 0}
	// writtenConversations lists each conversation that received rows, as
	// "<conversation id>=<row count>", for the completion log record.
	writtenConversations := make([]string, 0)
	for _, conversationID := range order {
		chunks, planErr := c.planConversation(conversationID, byConversation[conversationID], batch.state(conversationID))
		if planErr != nil {
			return "", planErr
		}
		writer := newRowWriter(c, collectionName)
		if err := c.embedAndWrite(ctx, chunks, batch.reuse, writer, &stats); err != nil {
			return "", err
		}
		stats.rowsWritten += writer.written
		if writer.written > 0 {
			writtenConversations = append(writtenConversations, fmt.Sprintf("%s=%d", conversationID, writer.written))
		}
		fingerprint, found := fingerprints[conversationID]
		if !found {
			continue
		}
		if err := c.checkpoint.record(ctx, collectionName, conversationID, fingerprint); err != nil {
			slog.WarnContext(ctx, "conversation.vectorsearch.checkpoint_unrecorded",
				"concern", "conversation.semantic",
				"component", "conversation",
				"conversation_id", conversationID,
				"err", err,
			)
		}
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
	)
	return jobID, nil
}

func (c *Client) nextJobID() string {
	c.ingestMu.Lock()
	defer c.ingestMu.Unlock()
	c.jobCount++
	return fmt.Sprintf("%s%d", jobIDPrefix, c.jobCount)
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

// planConversation returns the chunks to write for one conversation: every row
// of each family that has no stored row. A family with a stored row is skipped
// whole and never replaced.
func (c *Client) planConversation(conversationID string, docs []semsearch.SemDoc, state *conversationState) ([]storedChunk, error) {
	generated := make([]storedChunk, 0)
	for _, doc := range docs {
		documentRows, err := documentChunks(doc, conversationChunkMaxBytes)
		if err != nil {
			return nil, err
		}
		generated = append(generated, documentRows...)
	}
	chunks := make([]storedChunk, 0, len(generated))
	seen := make(map[string]struct{})
	for _, family := range groupFamilies(conversationID, generated) {
		if state.present(family.Key) {
			continue
		}
		for _, chunk := range expandOverBudget(family.Chunks, c.byteBudget) {
			id := chunkID(chunk)
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			chunks = append(chunks, chunk)
		}
	}
	return chunks, nil
}
