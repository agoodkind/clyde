package vectorsearch

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"goodkind.io/lm-semantic-search/collection"
)

// Embedding limits. A model that Clyde does not recognize gets the smallest
// token limit of any known model.
const (
	nvEmbedCodeModelName        = "nvidia/nv-embedcode-7b-v1"
	nvEmbedCodeInputTokenLimit  = 4096
	minimumInputTokenLimit      = 512
	embedTokenSafetyMargin      = 0.9
	bytesPerTokenNumerator      = 5
	bytesPerTokenDenominator    = 2
	embedBatchRows              = 32
	embedBatchTokenBudget       = 6000
	estimatedBytesPerPackToken  = 4
	rowFlushBytes               = 32 << 20
	estimatedRowOverheadBytes   = 512
	estimatedBytesPerVectorItem = 4
)

// embedByteBudget returns the byte size of the largest embedding input for a
// model. It converts the token cap to bytes at 2.5 bytes per token, a ratio
// below the densest measured content.
func embedByteBudget(embeddingModel string) int {
	limit := minimumInputTokenLimit
	if strings.ToLower(strings.TrimSpace(embeddingModel)) == nvEmbedCodeModelName {
		limit = nvEmbedCodeInputTokenLimit
	}
	tokenCap := max(int(float64(limit)*embedTokenSafetyMargin), 1)
	return tokenCap * bytesPerTokenNumerator / bytesPerTokenDenominator
}

// expandOverBudget splits every chunk longer than byteBudget into UTF-8 aligned
// children. Each child records its byte offset within the parent plus one as
// the split position, so identical pieces never share a primary key.
func expandOverBudget(chunks []storedChunk, byteBudget int) []storedChunk {
	if byteBudget <= 0 {
		return chunks
	}
	expanded := make([]storedChunk, 0, len(chunks))
	for _, chunk := range chunks {
		if len(chunk.Content) <= byteBudget {
			expanded = append(expanded, chunk)
			continue
		}
		pieces, offsets := splitWithOffsets(chunk.Content, byteBudget)
		for i, piece := range pieces {
			child := chunk
			child.Content = piece
			child.SplitPart = safeInt32(offsets[i] + 1)
			expanded = append(expanded, child)
		}
	}
	return expanded
}

// splitWithOffsets splits value like splitTextByBytes and reports the start byte
// offset of each piece.
func splitWithOffsets(value string, maxBytes int) ([]string, []int) {
	pieces := splitTextByBytes(value, maxBytes)
	offsets := make([]int, 0, len(pieces))
	offset := 0
	for _, piece := range pieces {
		offsets = append(offsets, offset)
		offset += len(piece)
	}
	return pieces, offsets
}

func safeInt32(value int) int32 {
	if value > math.MaxInt32 {
		return math.MaxInt32
	}
	if value < math.MinInt32 {
		return math.MinInt32
	}
	return int32(value)
}

func estimatedTokens(content string) int {
	return max((len(content)+estimatedBytesPerPackToken-1)/estimatedBytesPerPackToken, 1)
}

// packChunks groups consecutive chunks into embedding requests. A group closes
// at embedBatchRows chunks or when the next chunk would pass the estimated token
// budget. A chunk with a stored vector costs no tokens, because the request
// leaves it out.
func packChunks(chunks []storedChunk, reuse map[string][]float32) [][]storedChunk {
	groups := make([][]storedChunk, 0)
	current := make([]storedChunk, 0, embedBatchRows)
	currentTokens := 0
	for _, chunk := range chunks {
		tokens := estimatedTokens(chunk.Content)
		if _, reused := reuse[contentKey(chunk.Content)]; reused {
			tokens = 0
		}
		overBudget := currentTokens+tokens > embedBatchTokenBudget
		overRows := len(current) >= embedBatchRows
		if len(current) > 0 && (overBudget || overRows) {
			groups = append(groups, current)
			current = make([]storedChunk, 0, embedBatchRows)
			currentTokens = 0
		}
		current = append(current, chunk)
		currentTokens += tokens
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// ingestStats counts the outcome of one upsert.
type ingestStats struct {
	conversations int
	rowsWritten   int
	reused        int
	embedded      int
	dropped       int
	replaced      int
}

// rowWriter buffers embedded rows and writes them to the collection in batches
// of about rowFlushBytes. written records the primary key of every row the
// store acknowledged.
type rowWriter struct {
	client         *Client
	collectionName string
	pending        []collection.Row
	pendingBytes   int
	written        map[string]struct{}
}

func newRowWriter(client *Client, collectionName string) *rowWriter {
	return &rowWriter{
		client:         client,
		collectionName: collectionName,
		pending:        nil,
		pendingBytes:   0,
		written:        make(map[string]struct{}),
	}
}

func (writer *rowWriter) add(ctx context.Context, row collection.Row) error {
	writer.pending = append(writer.pending, row)
	writer.pendingBytes += len(row.Content) + len(row.RelativePath) + len(row.Metadata) +
		len(row.Vector)*estimatedBytesPerVectorItem + estimatedRowOverheadBytes
	if writer.pendingBytes < rowFlushBytes {
		return nil
	}
	return writer.flush(ctx)
}

func (writer *rowWriter) flush(ctx context.Context) error {
	if len(writer.pending) == 0 {
		return nil
	}
	dimension := len(writer.pending[0].Vector)
	if err := writer.client.ensureCollection(ctx, writer.collectionName, dimension); err != nil {
		return err
	}
	if err := writer.client.store.Upsert(ctx, writer.collectionName, writer.client.declaration, writer.pending); err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.upsert_rows_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", writer.collectionName,
			"rows", len(writer.pending),
			"err", err,
		)
		return fmt.Errorf("write %d conversation rows to %s: %w", len(writer.pending), writer.collectionName, err)
	}
	for _, row := range writer.pending {
		writer.written[row.ID] = struct{}{}
	}
	writer.pending = nil
	writer.pendingBytes = 0
	return nil
}

// embedAndWrite embeds the chunks and writes their rows. A chunk with a stored
// vector reuses it. An input the endpoint rejects is logged and skipped, and the
// remaining inputs continue. Any other embedding failure stops the call.
func (c *Client) embedAndWrite(
	ctx context.Context,
	chunks []storedChunk,
	reuse map[string][]float32,
	writer *rowWriter,
	stats *ingestStats,
) error {
	for _, pack := range packChunks(chunks, reuse) {
		if err := ctx.Err(); err != nil {
			return failRead("embed conversation rows", err)
		}
		vectors, err := c.vectorsFor(ctx, pack, reuse, stats)
		if err != nil {
			return err
		}
		for i, chunk := range pack {
			if vectors[i] == nil {
				continue
			}
			if err := writer.add(ctx, chunkRow(chunk, vectors[i])); err != nil {
				return err
			}
		}
	}
	return writer.flush(ctx)
}

// vectorsFor returns one vector per chunk of the pack, nil for a chunk the
// endpoint rejected. It sends only the chunks without a stored vector.
func (c *Client) vectorsFor(ctx context.Context, pack []storedChunk, reuse map[string][]float32, stats *ingestStats) ([][]float32, error) {
	vectors := make([][]float32, len(pack))
	missTexts := make([]string, 0, len(pack))
	missPositions := make([]int, 0, len(pack))
	for i, chunk := range pack {
		if vector, found := reuse[contentKey(chunk.Content)]; found {
			vectors[i] = vector
			stats.reused++
			continue
		}
		missTexts = append(missTexts, chunk.Content)
		missPositions = append(missPositions, i)
	}
	if len(missTexts) == 0 {
		return vectors, nil
	}
	result, err := c.embedder.EmbedBatch(ctx, missTexts)
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.embed_batch_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"inputs", len(missTexts),
			"err", err,
		)
		return nil, fmt.Errorf("embed %d conversation rows: %w", len(missTexts), err)
	}
	if len(result.Vectors) != len(missTexts) {
		return nil, fmt.Errorf("embedding endpoint returned %d vectors for %d inputs", len(result.Vectors), len(missTexts))
	}
	skipped := make(map[int]string, len(result.Skipped))
	for _, skip := range result.Skipped {
		skipped[skip.Index] = string(skip.Reason)
	}
	for position, chunkPosition := range missPositions {
		vector := result.Vectors[position]
		if vector == nil {
			chunk := pack[chunkPosition]
			reason, wasSkipped := skipped[position]
			if !wasSkipped {
				return nil, fmt.Errorf("embedding endpoint returned no vector and no skip for %s", chunk.RelativePath)
			}
			slog.WarnContext(ctx, "conversation.vectorsearch.embed_input_skipped",
				"concern", "conversation.semantic",
				"component", "conversation",
				"conversation_id", chunk.ConversationID,
				"relative_path", chunk.RelativePath,
				"content_bytes", len(chunk.Content),
				"reason", reason,
			)
			stats.dropped++
			continue
		}
		if c.dimension > 0 && len(vector) != c.dimension {
			return nil, fmt.Errorf("embedding endpoint returned %d dimensions, configured embedding_dimension is %d", len(vector), c.dimension)
		}
		vectors[chunkPosition] = vector
		stats.embedded++
	}
	return vectors, nil
}
