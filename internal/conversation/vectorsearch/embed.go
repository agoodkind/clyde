package vectorsearch

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"unicode/utf8"

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
	// splitRetrySpecialTokenMargin reserves room for the start and end tokens a
	// tokenizer adds without consuming content bytes.
	splitRetrySpecialTokenMargin = 2
	// Rejection reasons the endpoint reports.
	contextLengthExceededReason = "context_length_exceeded"
	emptyContentReason          = "empty_content"
)

// activeTokenLimit returns the hard per-input token limit of a model.
func activeTokenLimit(embeddingModel string) int {
	if strings.ToLower(strings.TrimSpace(embeddingModel)) == nvEmbedCodeModelName {
		return nvEmbedCodeInputTokenLimit
	}
	return minimumInputTokenLimit
}

// embedByteBudget returns the byte size of the largest embedding input for a
// model. It converts the token cap to bytes at 2.5 bytes per token, a ratio
// below the densest measured content.
func embedByteBudget(embeddingModel string) int {
	tokenCap := max(int(float64(activeTokenLimit(embeddingModel))*embedTokenSafetyMargin), 1)
	return tokenCap * bytesPerTokenNumerator / bytesPerTokenDenominator
}

// expandOverBudget splits every chunk longer than byteBudget into UTF-8 aligned
// children.
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
		expanded = append(expanded, splitChunkAtBudget(chunk, byteBudget)...)
	}
	return expanded
}

// splitChunkAtBudget splits a chunk into children of at most budget bytes, each
// ending on a UTF-8 boundary. Each child records the byte offset within the
// original content plus one as its split position, so identical pieces never
// share a primary key. A child of an already split chunk adds the offset of its
// parent, so nested splits stay unique.
func splitChunkAtBudget(chunk storedChunk, budget int) []storedChunk {
	baseOffset := 0
	if chunk.SplitPart > 0 {
		baseOffset = int(chunk.SplitPart) - 1
	}
	pieces := splitTextByBytes(chunk.Content, budget)
	children := make([]storedChunk, 0, len(pieces))
	offset := 0
	for _, piece := range pieces {
		child := chunk
		child.Content = piece
		child.SplitPart = safeInt32(baseOffset + offset + 1)
		children = append(children, child)
		offset += len(piece)
	}
	return children
}

// splitChunkInHalf splits a chunk the endpoint rejected into two strictly
// smaller children.
func splitChunkInHalf(chunk storedChunk) []storedChunk {
	return splitChunkAtBudget(chunk, max(len(chunk.Content)/2, 1))
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
	split         int
	dropped       int
}

// rowWriter buffers embedded rows and writes them to the collection in batches
// of about rowFlushBytes.
type rowWriter struct {
	client         *Client
	collectionName string
	pending        []collection.Row
	pendingBytes   int
	written        int
}

func newRowWriter(client *Client, collectionName string) *rowWriter {
	return &rowWriter{client: client, collectionName: collectionName, pending: nil, pendingBytes: 0, written: 0}
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
	writer.written += len(writer.pending)
	writer.pending = nil
	writer.pendingBytes = 0
	return nil
}

// skipInfo is the endpoint's report for one skipped input of a batch.
type skipInfo struct {
	reason      string
	maxTokens   int
	maxReported bool
}

// rejection is one input the endpoint refused as individually un-embeddable.
type rejection struct {
	chunk       storedChunk
	reason      string
	maxTokens   int
	maxReported bool
}

// embedAndWrite embeds the chunks and writes their rows. A chunk with a stored
// vector reuses it. A chunk the endpoint rejects for context length splits in
// half, and each half is embedded again. A rejected input that cannot split
// further is logged and skipped, and the remaining inputs continue. Any other
// embedding failure stops the call.
func (c *Client) embedAndWrite(
	ctx context.Context,
	chunks []storedChunk,
	reuse map[string][]float32,
	writer *rowWriter,
	stats *ingestStats,
) error {
	queue := chunks
	roundReuse := reuse
	for round := 0; len(queue) > 0; round++ {
		retry := make([]storedChunk, 0)
		for _, pack := range packChunks(queue, roundReuse) {
			if err := ctx.Err(); err != nil {
				return failRead("embed conversation rows", err)
			}
			vectors, rejected, err := c.vectorsFor(ctx, pack, roundReuse, stats)
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
			for _, refusal := range rejected {
				if c.shouldSplit(refusal) {
					retry = append(retry, splitChunkInHalf(refusal.chunk)...)
					stats.split++
					continue
				}
				c.logDropped(ctx, refusal, round)
				stats.dropped++
			}
		}
		queue = retry
		roundReuse = nil
	}
	return writer.flush(ctx)
}

// vectorsFor returns one vector per chunk of the pack, nil for a chunk the
// endpoint rejected, and the rejections. It sends only the chunks without a
// stored vector.
func (c *Client) vectorsFor(ctx context.Context, pack []storedChunk, reuse map[string][]float32, stats *ingestStats) ([][]float32, []rejection, error) {
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
		return vectors, nil, nil
	}
	result, err := c.embedder.EmbedBatch(ctx, missTexts)
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.embed_batch_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"inputs", len(missTexts),
			"err", err,
		)
		return nil, nil, fmt.Errorf("embed %d conversation rows: %w", len(missTexts), err)
	}
	if len(result.Vectors) != len(missTexts) {
		return nil, nil, failRead("embed conversation rows", fmt.Errorf("endpoint returned %d vectors for %d inputs", len(result.Vectors), len(missTexts)))
	}
	skipped := make(map[int]skipInfo, len(result.Skipped))
	for _, skip := range result.Skipped {
		skipped[skip.Index] = skipInfo{
			reason:      string(skip.Reason),
			maxTokens:   skip.MaxTokens.Value,
			maxReported: skip.MaxTokens.Reported,
		}
	}
	rejected := make([]rejection, 0)
	for position, chunkPosition := range missPositions {
		vector := result.Vectors[position]
		if vector == nil {
			info, wasSkipped := skipped[position]
			if !wasSkipped {
				return nil, nil, failRead("embed conversation rows", fmt.Errorf("endpoint returned no vector and no skip for %s", pack[chunkPosition].RelativePath))
			}
			rejected = append(rejected, rejection{
				chunk:       pack[chunkPosition],
				reason:      info.reason,
				maxTokens:   info.maxTokens,
				maxReported: info.maxReported,
			})
			continue
		}
		if c.dimension > 0 && len(vector) != c.dimension {
			return nil, nil, failRead("embed conversation rows", fmt.Errorf("endpoint returned %d dimensions, configured embedding_dimension is %d", len(vector), c.dimension))
		}
		vectors[chunkPosition] = vector
		stats.embedded++
	}
	return vectors, rejected, nil
}

// splitByteFloor converts the token limit of a rejection to the smallest content
// size in bytes where another split can help. The limit comes from the endpoint
// when it reported one, otherwise from the active model. A limit with no content
// capacity after the special tokens reports false.
func (c *Client) splitByteFloor(refusal rejection) (int, bool) {
	modelMaxTokens := minimumInputTokenLimit
	if refusal.maxReported && refusal.maxTokens >= 0 {
		modelMaxTokens = refusal.maxTokens
	} else if limit := activeTokenLimit(c.embeddingModel); limit > 0 {
		modelMaxTokens = limit
	}
	if modelMaxTokens <= splitRetrySpecialTokenMargin {
		return 0, false
	}
	return modelMaxTokens - splitRetrySpecialTokenMargin, true
}

// shouldSplit reports whether a rejected chunk splits in half and retries. Only a
// context length rejection splits, and only while the content is longer than one
// codepoint and longer than the byte floor.
func (c *Client) shouldSplit(refusal rejection) bool {
	if refusal.reason != contextLengthExceededReason {
		return false
	}
	if utf8.RuneCountInString(refusal.chunk.Content) <= 1 {
		return false
	}
	byteFloor, hasCapacity := c.splitByteFloor(refusal)
	if !hasCapacity {
		return false
	}
	return len(refusal.chunk.Content) > byteFloor
}

func (c *Client) dropKind(refusal rejection) string {
	if refusal.reason == emptyContentReason {
		return "empty_content"
	}
	if refusal.reason != contextLengthExceededReason {
		return "unexpected_reason"
	}
	if utf8.RuneCountInString(refusal.chunk.Content) <= 1 {
		return "indivisible"
	}
	byteFloor, hasCapacity := c.splitByteFloor(refusal)
	if !hasCapacity {
		return "no_content_capacity"
	}
	if len(refusal.chunk.Content) <= byteFloor {
		return "below_token_floor"
	}
	return "unknown"
}

// logDropped logs an input that is skipped because it still fails at the
// smallest size.
func (c *Client) logDropped(ctx context.Context, refusal rejection, round int) {
	byteFloor, _ := c.splitByteFloor(refusal)
	slog.WarnContext(ctx, "conversation.vectorsearch.embed_input_dropped",
		"concern", "conversation.semantic",
		"component", "conversation",
		"drop_kind", c.dropKind(refusal),
		"reason", refusal.reason,
		"conversation_id", refusal.chunk.ConversationID,
		"relative_path", refusal.chunk.RelativePath,
		"content_bytes", len(refusal.chunk.Content),
		"model_max_tokens", refusal.maxTokens,
		"content_byte_floor", byteFloor,
		"retry_round", round,
	)
}
