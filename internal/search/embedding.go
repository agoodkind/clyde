package search

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/transcript"
)

const (
	defaultEmbeddingModel      = "text-embedding-nomic-embed-text-v1.5"
	defaultSimilarityThreshold = 0.5
)

// embeddingFilter uses a local embedding model to pre-filter chunks,
// skipping those with low cosine similarity to the query.
type embeddingFilter struct {
	client    *openai.Client
	model     string
	threshold float64
}

func newEmbeddingFilter(cfg config.SearchLocal) *embeddingFilter {
	base := cfg.ResolvedEmbeddingURL()
	if base == "" {
		base = "http://localhost:1234"
	}

	opts := []option.RequestOption{
		option.WithBaseURL(base + "/v1"),
	}
	if cfg.ResolvedEmbeddingToken() != "" {
		opts = append(opts, option.WithAPIKey(cfg.ResolvedEmbeddingToken()))
	} else {
		opts = append(opts, option.WithAPIKey("not-needed"))
	}

	threshold := cfg.EmbeddingThreshold
	if threshold <= 0 {
		threshold = defaultSimilarityThreshold
	}

	model := cfg.EmbeddingModel
	if model == "" {
		model = defaultEmbeddingModel
	}

	c := openai.NewClient(opts...)
	return &embeddingFilter{client: &c, model: model, threshold: threshold}
}

// filterChunks embeds the query and each chunk, returning only chunks whose
// cosine similarity to the query exceeds the threshold.
func (e *embeddingFilter) filterChunks(ctx context.Context, query string, chunks [][]transcript.Message) [][]transcript.Message {
	log := slog.Default()
	start := clock.Now()

	// Build text for each chunk
	chunkTexts := make([]string, len(chunks))
	for i, chunk := range chunks {
		var text string
		for _, m := range chunk {
			text += m.Text + "\n"
			if len(text) > 2000 {
				text = text[:2000]
				break
			}
		}
		chunkTexts[i] = text
	}

	// Embed query
	queryEmbStart := clock.Now()
	queryEmb, err := e.embed(ctx, []string{query})
	if err != nil {
		log.WarnContext(ctx, "embedding query failed, skipping pre-filter", "concern", "search", "err", err)
		return chunks // fall back to no filtering
	}
	log.DebugContext(ctx, "embedding: query embedded", "concern", "search", "duration", clock.Since(queryEmbStart).Round(time.Millisecond))

	// Embed all chunks in one batch
	chunksEmbStart := clock.Now()
	chunkEmbs, err := e.embed(ctx, chunkTexts)
	if err != nil {
		log.WarnContext(ctx, "embedding chunks failed, skipping pre-filter", "concern", "search", "err", err)
		return chunks
	}
	log.DebugContext(ctx, "embedding: chunks embedded", "concern", "search", "chunks", len(chunkTexts), "duration", clock.Since(chunksEmbStart).Round(time.Millisecond))

	if len(queryEmb) == 0 || len(chunkEmbs) != len(chunks) {
		return chunks
	}

	// Filter by cosine similarity
	queryVec := queryEmb[0]
	var filtered [][]transcript.Message
	for i, chunkVec := range chunkEmbs {
		sim := cosineSimilarity(queryVec, chunkVec)
		if sim >= e.threshold {
			filtered = append(filtered, chunks[i])
		}
	}

	log.InfoContext(ctx, "embedding pre-filter complete", "concern", "search", "model", e.model,
		"total_chunks", len(chunks),
		"passed", len(filtered),
		"filtered_out", len(chunks)-len(filtered),
		"threshold", e.threshold,
		"query_embed_duration", clock.Since(queryEmbStart).Round(time.Millisecond),
		"chunks_embed_duration", clock.Since(chunksEmbStart).Round(time.Millisecond),
		"total_duration", clock.Since(start).Round(time.Millisecond),
	)

	if len(filtered) == 0 {
		// If nothing passed, return all chunks (threshold might be too high)
		log.WarnContext(ctx, "embedding filter removed all chunks, falling back to unfiltered", "concern", "search")
		return chunks
	}

	return filtered
}

// embed returns embeddings for the given texts.
func (e *embeddingFilter) embed(ctx context.Context, texts []string) ([][]float64, error) {
	resp, err := e.client.Embeddings.New(ctx, openai.EmbeddingNewParams{
		Model: e.model,
		Input: openai.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: texts,
		},
	})
	if err != nil {
		slog.WarnContext(ctx, "search.embedding.create_failed", "concern", "search", "model", e.model, "err", err)
		return nil, fmt.Errorf("create embeddings with model %s: %w", e.model, err)
	}

	result := make([][]float64, len(resp.Data))
	for _, d := range resp.Data {
		result[d.Index] = d.Embedding
	}
	return result, nil
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
