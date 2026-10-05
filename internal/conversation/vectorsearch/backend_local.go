package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
	"goodkind.io/lm-semantic-search/embedding"
)

// localDeliveryBatchBytes lets one sync pass load enough conversations to keep
// the parallel loader and embedder busy. Each pass is a separate load, embed,
// and write cycle.
const localDeliveryBatchBytes = 64 << 20

type localBackend struct {
	*codestore.Store
}

// staticEmbedder embeds text with the model compiled into the binary.
type staticEmbedder struct {
	model *staticembed.Model
}

func (embedder staticEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return embedder.model.Vector(text), nil
}

// EmbedBatch embeds texts on up to GOMAXPROCS goroutines.
func (embedder staticEmbedder) EmbedBatch(ctx context.Context, texts []string) (embedding.BatchResult, error) {
	vectors := make([][]float32, len(texts))
	var next atomic.Int64
	var group sync.WaitGroup
	for range min(len(texts), runtime.GOMAXPROCS(0)) {
		group.Go(func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					slog.ErrorContext(ctx, "conversation.vectorsearch.embed_worker_panic",
						"concern", "conversation.semantic",
						"component", "conversation",
						"err", fmt.Errorf("panic: %v", recovered),
					)
				}
			}()
			for {
				position := int(next.Add(1) - 1)
				if position >= len(texts) {
					return
				}
				vectors[position] = embedder.model.Vector(texts[position])
			}
		})
	}
	group.Wait()
	for position, vector := range vectors {
		if vector == nil {
			return embedding.BatchResult{Vectors: nil, Skipped: nil}, fmt.Errorf("embed batch: input %d produced no vector", position)
		}
	}
	return embedding.BatchResult{Vectors: vectors, Skipped: nil}, nil
}

func openLocalBackend(_ context.Context, options Options) (openedBackend, error) {
	failed := openedBackend{store: nil, embedder: nil, embeddingModel: "", dimension: 0, queryPrefix: "", byteBudget: 0, deliveryBatchBytes: 0}
	model, err := staticembed.Load()
	if err != nil {
		return failed, operationError{operation: "load the static embedding model " + staticembed.ModelName, cause: err}
	}
	store, err := codestore.Open(options.LocalRoot, staticembed.ModelName)
	if err != nil {
		return failed, operationError{operation: fmt.Sprintf("open local conversation store at %q", options.LocalRoot), cause: err}
	}
	return openedBackend{
		store:          &localBackend{Store: store},
		embedder:       staticEmbedder{model: model},
		embeddingModel: staticembed.ModelName,
		dimension:      staticembed.Dimensions,
		queryPrefix:    "",
		byteBudget:     staticembed.PassageBytes,

		deliveryBatchBytes: localDeliveryBatchBytes,
	}, nil
}

func (b *localBackend) collectionPresent(_ context.Context, collectionName string) (bool, error) {
	_, err := b.Load(collectionName)
	if errors.Is(err, collection.ErrCollectionMissing) {
		return false, nil
	}
	if err != nil {
		return false, failRead("load local collection "+collectionName, err)
	}
	return true, nil
}

func (b *localBackend) search(ctx context.Context, request collection.SearchRequest) ([]collection.Hit, searchTiming, error) {
	timing := searchTiming{rank: 0, load: 0, candidates: 0}
	started := clock.Now()
	hits, err := b.Search(ctx, request)
	timing.rank = clock.Now().Sub(started)
	timing.candidates = len(hits)
	if errors.Is(err, collection.ErrCollectionMissing) {
		return []collection.Hit{}, timing, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.rank_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", request.Collection,
			"err", err,
		)
		return nil, timing, fmt.Errorf("rank %s: %w", request.Collection, err)
	}
	return hits, timing, nil
}

func (b *localBackend) close(context.Context) error {
	b.Close()
	return nil
}
