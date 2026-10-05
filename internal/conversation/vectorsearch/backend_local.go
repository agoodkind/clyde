package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/lm-semantic-search/collection"
	localstore "goodkind.io/lm-semantic-search/collection/local"
	localembedding "goodkind.io/lm-semantic-search/embedding/local"
)

type localBackend struct {
	*localstore.Store
}

func openLocalBackend(ctx context.Context, options Options) (openedBackend, error) {
	failed := openedBackend{store: nil, embedder: nil, embeddingModel: "", dimension: 0, queryPrefix: ""}
	model, err := localembedding.Describe(options.EmbeddingModel)
	if err != nil {
		return failed, operationError{operation: fmt.Sprintf("describe local embedding model %q", options.EmbeddingModel), cause: err}
	}
	embedder, err := localembedding.New(ctx, localembedding.Options{Model: model.Name, CacheRoot: options.ModelCacheRoot})
	if err != nil {
		return failed, operationError{operation: fmt.Sprintf("load local embedding model %q", model.Name), cause: err}
	}
	store, err := localstore.Open(localstore.Options{Root: options.LocalRoot, EmbeddingModel: model.Name})
	if err != nil {
		return failed, operationError{operation: fmt.Sprintf("open local conversation store at %q", options.LocalRoot), cause: err}
	}
	return openedBackend{
		store:          &localBackend{Store: store},
		embedder:       embedder,
		embeddingModel: model.Name,
		dimension:      model.Dimension,
		queryPrefix:    model.QueryPrefix,
	}, nil
}

func (b *localBackend) collectionPresent(ctx context.Context, collectionName string) (bool, error) {
	_, err := b.Query(ctx, collection.QueryRequest{
		Collection:  collectionName,
		Filter:      nil,
		Limit:       1,
		Declaration: storedRowsDeclaration(),
	})
	if errors.Is(err, collection.ErrCollectionMissing) {
		return false, nil
	}
	if err != nil {
		return false, failRead("check local collection "+collectionName, err)
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
