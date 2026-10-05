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

// Each vector store implements backend and adds its opener to backendOpeners.
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
// embedding input in bytes.
type openedBackend struct {
	store          backend
	embedder       textEmbedder
	embeddingModel string
	dimension      int
	queryPrefix    string
	byteBudget     int
}

type backendOpener func(ctx context.Context, options Options) (openedBackend, error)

var backendOpeners = map[Backend]backendOpener{
	BackendMilvus: openMilvusBackend,
	BackendLocal:  openLocalBackend,
}

func openBackend(ctx context.Context, options Options) (openedBackend, error) {
	name := options.Backend
	if name == "" {
		name = BackendMilvus
	}
	opener, found := backendOpeners[name]
	if !found {
		failed := openedBackend{store: nil, embedder: nil, embeddingModel: "", dimension: 0, queryPrefix: "", byteBudget: 0}
		return failed, operationError{operation: "open conversation vector store", cause: fmt.Errorf("backend %q is not registered", name)}
	}
	return opener(ctx, options)
}
