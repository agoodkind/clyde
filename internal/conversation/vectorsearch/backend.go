package vectorsearch

import (
	"context"
	"fmt"

	"goodkind.io/lm-semantic-search/collection"
	"goodkind.io/lm-semantic-search/embedding"
)

// Backend selects the vector store that a [Client] opens.
type Backend string

const (
	// BackendMilvus stores rows in a Milvus server. An empty Backend selects it.
	BackendMilvus Backend = "milvus"
	// BackendLocal stores sign codes in files under a local directory and embeds
	// with the static model compiled into the binary.
	BackendLocal Backend = "local"
)

type backend interface {
	collection.Store
	// collectionPresent prepares an existing collection for row queries.
	collectionPresent(ctx context.Context, collectionName string) (bool, error)
	search(ctx context.Context, request collection.SearchRequest) ([]collection.Hit, searchTiming, error)
	close(ctx context.Context) error
}

// textEmbedder is the part of an embedding provider the client calls.
type textEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) (embedding.BatchResult, error)
}

// openedBackend pairs a store with the embedder that produced its vectors. A
// query vector must come from the same embedder. byteBudget is the largest
// embedding input in bytes. deliveryBatchBytes is the per-pass raw transcript
// byte budget of the sync worker. Zero selects the worker default.
type openedBackend struct {
	store              backend
	embedder           textEmbedder
	embeddingModel     string
	dimension          int
	queryPrefix        string
	byteBudget         int
	deliveryBatchBytes int64
}

func openBackend(ctx context.Context, options Options) (openedBackend, error) {
	switch options.Backend {
	case "", BackendMilvus:
		return openMilvusBackend(ctx, options)
	case BackendLocal:
		return openLocalBackend(ctx, options)
	default:
		failed := openedBackend{store: nil, embedder: nil, embeddingModel: "", dimension: 0, queryPrefix: "", byteBudget: 0, deliveryBatchBytes: 0}
		return failed, operationError{operation: "open conversation vector store", cause: fmt.Errorf("backend %q is not registered", options.Backend)}
	}
}
