package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/lm-semantic-search/collection"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
	"goodkind.io/lm-semantic-search/embedding"
)

type milvusBackend struct {
	*milvusstore.Store
	client *milvusclient.Client
}

func openMilvusBackend(ctx context.Context, options Options) (openedBackend, error) {
	failed := openedBackend{store: nil, embedder: nil, embeddingModel: "", dimension: 0, queryPrefix: ""}
	embedder, err := embedding.NewOpenAICompatible(embedding.OpenAIOptions{
		APIKey:         options.EmbeddingAPIKey,
		BaseURL:        options.EmbeddingBaseURL,
		Model:          options.EmbeddingModel,
		Dimensions:     0,
		RequestTimeout: options.EmbeddingTimeout,
	})
	if err != nil {
		return failed, operationError{operation: fmt.Sprintf("build conversation embedding provider for model %q", options.EmbeddingModel), cause: err}
	}
	queryPrefix := options.QueryInstructionPrefix
	if queryPrefix == "" && strings.Contains(options.EmbeddingModel, nvEmbedCodeModelMarker) {
		queryPrefix = nvEmbedCodeQueryPrefix
	}
	// The Milvus client retries an unreachable address until its context ends.
	// The daemon opens this connection during startup, and a local Milvus
	// answers in milliseconds.
	connectCtx, cancelConnect := context.WithTimeout(ctx, milvusConnectTimeout)
	defer cancelConnect()
	client, err := milvusclient.New(connectCtx, &milvusclient.ClientConfig{
		Address: options.MilvusAddress,
		DBName:  options.MilvusDatabase,
	})
	if err != nil {
		return failed, operationError{operation: fmt.Sprintf("connect to Milvus at %q database %q", options.MilvusAddress, options.MilvusDatabase), cause: err}
	}
	store := &milvusBackend{
		Store:  milvusstore.New(client, milvusstore.Options{Hybrid: true, EmbeddingModel: options.EmbeddingModel, DenseSearchParams: options.DenseSearchParams}),
		client: client,
	}
	return openedBackend{store: store, embedder: embedder, embeddingModel: options.EmbeddingModel, dimension: options.EmbeddingDimension, queryPrefix: queryPrefix}, nil
}

// collectionPresent loads an existing collection into memory, because Milvus
// serves a stored-row query on a loaded collection only.
func (b *milvusBackend) collectionPresent(ctx context.Context, collectionName string) (bool, error) {
	exists, err := b.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(collectionName))
	if err != nil {
		return false, failRead("check Milvus collection "+collectionName, err)
	}
	if !exists {
		return false, nil
	}
	task, err := b.client.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(collectionName))
	if err != nil {
		return false, failRead("load Milvus collection "+collectionName, err)
	}
	if err := task.Await(ctx); err != nil {
		return false, failRead("await load of Milvus collection "+collectionName, err)
	}
	return true, nil
}

func (b *milvusBackend) close(ctx context.Context) error {
	closeCtx, cancel := context.WithTimeout(ctx, milvusCloseTimeout)
	defer cancel()
	if err := b.client.Close(closeCtx); err != nil {
		slog.Warn("conversation.vectorsearch.milvus_close_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", err,
		)
		return fmt.Errorf("close Milvus connection: %w", err)
	}
	return nil
}

// search resolves legacy conversation groups when the search limits hits per
// conversation.
func (b *milvusBackend) search(ctx context.Context, request collection.SearchRequest) ([]collection.Hit, searchTiming, error) {
	timing := searchTiming{rank: 0, load: 0, candidates: 0}
	rankStarted := clock.Now()
	candidates, err := b.Rank(ctx, request)
	if errors.Is(err, collection.ErrCollectionNotReady) {
		// Another Milvus client can release the collection.
		slog.WarnContext(ctx, "conversation.vectorsearch.collection_reloaded",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", request.Collection,
			"err", err,
		)
		if _, loadErr := b.collectionPresent(ctx, request.Collection); loadErr != nil {
			return nil, timing, loadErr
		}
		candidates, err = b.Rank(ctx, request)
	}
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
	groupColumn, grouped := milvusstore.GroupColumnFor(request)
	perGroupLimit := int32(0)
	if grouped {
		perGroupLimit = request.PerGroupLimit
		if groupColumn.Name == request.Declaration.ItemIDColumn {
			candidates, err = resolveLegacyGroups(ctx, b.client, request.Collection, candidates)
			if err != nil {
				return nil, timing, err
			}
		}
	}
	limit := request.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	selected := milvusstore.SelectCandidates(candidates, perGroupLimit, request.MinScore, limit)
	loadStarted := clock.Now()
	timing.rank = loadStarted.Sub(rankStarted)
	timing.candidates = len(candidates)
	hits, err := b.Load(ctx, request.Collection, selected, request.Declaration.Scalars)
	timing.load = clock.Now().Sub(loadStarted)
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.load_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", request.Collection,
			"err", err,
		)
		return nil, timing, fmt.Errorf("load %s: %w", request.Collection, err)
	}
	return hits, timing, nil
}
