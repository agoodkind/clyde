package vectorsearch

import (
	"context"

	"github.com/milvus-io/milvus/client/v3/milvusclient"
)

// SearchCollectionExists checks whether a conversation collection exists
// without loading an index or embedding the query.
//
// Search uses this check when it can query another store for missing content.
//
// Results:
//   - An absent collection returns false and a nil error.
//   - A failed check returns false and an error.
func (c *Client) SearchCollectionExists(ctx context.Context, collectionID string) (bool, error) {
	name := CollectionName(collectionID)
	if milvus, ok := c.store.(*milvusBackend); ok {
		exists, err := milvus.client.HasCollection(ctx, milvusclient.NewHasCollectionOption(name))
		if err != nil {
			return false, failRead("check Milvus collection "+name, err)
		}

		return exists, nil
	}

	return c.store.searchCollectionPresent(ctx, name)
}
