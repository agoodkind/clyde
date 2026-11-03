//go:build milvusintegration

package vectorsearch

import (
	"context"
	"errors"
	"math"
	"slices"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	blankRowVectorStep   = 3
	blankRowVectorOffset = 0.1
	blankRowMessageStart = 1000
)

// WriteWhitespacePieceRowsForTest stores the first and whitespace-only pieces of doc.
// It also stores a row with the same whitespace text and a different vector. It
// returns the IDs of the whitespace-only piece and the missing third piece.
func WriteWhitespacePieceRowsForTest(ctx context.Context, client *Client, collectionID string, doc semsearch.SemDoc, otherMessageIndex int32) (string, string, error) {
	families, err := conversationFamilies(doc.ConversationID, []semsearch.SemDoc{doc})
	if err != nil {
		return "", "", err
	}
	pieces := expandOverBudget(families[0].Chunks, client.byteBudget)
	if len(pieces) != 3 || conversationTextIsStorable(pieces[1].Content) {
		return "", "", errors.New("the document does not produce three pieces with a whitespace-only second piece")
	}
	result, err := client.embedder.EmbedBatch(ctx, []string{"head vector text", "stored whitespace vector text", "other whitespace vector text"})
	if err != nil {
		return "", "", err
	}
	collectionName := CollectionName(collectionID)
	if err := client.ensureCollection(ctx, collectionName, len(result.Vectors[0])); err != nil {
		return "", "", err
	}
	other := pieces[1]
	other.RelativePath = conversationRelativePath(doc.ConversationID, otherMessageIndex, 0, false)
	other.MessageIndex = otherMessageIndex
	other.SplitPart = 0
	rows := []collection.Row{chunkRow(pieces[0], result.Vectors[0]), chunkRow(pieces[1], result.Vectors[1]), chunkRow(other, result.Vectors[2])}
	if err := client.store.Upsert(ctx, collectionName, client.declaration, rows); err != nil {
		return "", "", err
	}
	return chunkID(pieces[1]), chunkID(pieces[2]), nil
}

// WriteDistinctBlankRowsForTest stores count whitespace-only rows with vectors based on query.
// For each row, it copies the query vector, increases one component, and scales the result
// to length one.
func WriteDistinctBlankRowsForTest(ctx context.Context, client *Client, collectionID string, conversationID string, query string, count int) error {
	nearest, err := client.embedder.Embed(ctx, query)
	if err != nil {
		return err
	}
	rows := make([]collection.Row, 0, count)
	for position := range count {
		vector := slices.Clone(nearest)
		vector[(position*blankRowVectorStep+1)%len(vector)] += blankRowVectorOffset
		norm := float32(0)
		for _, value := range vector {
			norm += value * value
		}
		scale := float32(1 / math.Sqrt(float64(norm)))
		for i := range vector {
			vector[i] *= scale
		}
		messageIndex := int32(blankRowMessageStart + position)
		chunk := storedChunk{
			Content: " ", RelativePath: conversationRelativePath(conversationID, messageIndex, 0, false),
			ConversationID: conversationID, ParentConversationID: "", MessageIndex: messageIndex, Role: "assistant",
			TimestampUnix: missingTimestampUnix, WorkspaceRoot: "", Archived: false, SplitPart: 0, LoadRules: "",
		}
		rows = append(rows, chunkRow(chunk, vector))
	}
	return client.store.Upsert(ctx, CollectionName(collectionID), client.declaration, rows)
}

// ByteBudgetForTest returns the client's embedding input byte limit.
func ByteBudgetForTest(client *Client) int {
	return client.byteBudget
}

// WriteStoppedRecordForTest stores a stopped upsert record for docs without writing
// conversation rows. Tests use it to reproduce an interruption at an exact row
// boundary. TestSharedLayoutInFlightUpsertStoresWriteRecord verifies that production
// code stores the record.
func WriteStoppedRecordForTest(checkpointDir string, collectionID string, conversationID string, fingerprint string, embeddingModel string, docs []semsearch.SemDoc) error {
	families, err := conversationFamilies(conversationID, docs)
	if err != nil {
		return err
	}
	proofs := familyProofs(families, embeddingModel)
	planned := make([]familyProof, 0, len(families))
	for _, family := range families {
		planned = append(planned, familyProof{Family: family.Key, Proof: proofs[family.Key]})
	}
	_, err = newCheckpointStore(checkpointDir).beginWrite(CollectionName(collectionID), conversationID, fingerprint, planned)
	return err
}
