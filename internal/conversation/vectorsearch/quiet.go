package vectorsearch

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/lm-semantic-search/embedding"
)

// embeddingBatchGate grants one ingestion batch permission to use the embedder.
type embeddingBatchGate interface {
	Acquire(ctx context.Context) (func(), error)
	WaitingReason() string
	Close()
}

func (c *Client) embedIngestionBatch(
	ctx context.Context,
	texts []string,
) (embedding.BatchResult, error) {
	if c.ingestGate != nil {
		release, err := c.ingestGate.Acquire(ctx)
		if err != nil {
			slog.WarnContext(ctx, "conversation.vectorsearch.embedding_wait_ended", "err", err)

			return embedding.BatchResult{}, fmt.Errorf("wait for idle embedding: %w", err)
		}

		defer release()
	}

	result, err := c.embedder.EmbedBatch(ctx, texts)
	if err != nil {
		slog.WarnContext(
			ctx,
			"conversation.vectorsearch.embed_batch_failed",
			"concern",
			"conversation.semantic",
			"component",
			"conversation",
			"inputs",
			len(texts),
			"err",
			err,
		)

		return result, fmt.Errorf("request embeddings: %w", err)
	}

	return result, nil
}
