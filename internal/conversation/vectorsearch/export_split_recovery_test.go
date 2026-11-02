//go:build milvusintegration

package vectorsearch

import (
	"context"
	"errors"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
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
