package vectorsearch

import (
	"context"
	"fmt"

	"goodkind.io/lm-semantic-search/collection"
)

// Backend selects the vector store that a [Client] opens.
type Backend string

const (
	// BackendMilvus stores rows in a Milvus server. An empty Backend selects it.
	BackendMilvus Backend = "milvus"
	// BackendLocal stores rows in files under a local directory.
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

type backendOpener func(ctx context.Context, options Options) (backend, error)

var backendOpeners = map[Backend]backendOpener{
	BackendMilvus: openMilvusBackend,
}

func openBackend(ctx context.Context, options Options) (backend, error) {
	name := options.Backend
	if name == "" {
		name = BackendMilvus
	}
	opener, found := backendOpeners[name]
	if !found {
		return nil, operationError{operation: "open conversation vector store", cause: fmt.Errorf("backend %q is not registered", name)}
	}
	return opener(ctx, options)
}
