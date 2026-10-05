package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	// defaultSearchLimit is the hit count a search returns when the request sets
	// no positive limit.
	defaultSearchLimit = 10
	// nvEmbedCodeModelMarker identifies the embedding models that need the query
	// instruction prefix.
	nvEmbedCodeModelMarker = "NV-EmbedCode"
	// nvEmbedCodeQueryPrefix is the instruction the NV-EmbedCode models expect in
	// front of a query. Stored rows embed without it.
	nvEmbedCodeQueryPrefix = "Instruct: Retrieve code or text relevant to the query.\nQuery: "
	milvusConnectTimeout   = 2 * time.Second
	milvusCloseTimeout     = 5 * time.Second
	queryVectorCacheSize   = 64
)

// operationError pairs a failed operation with its cause. The constructors
// return it without logging, and the daemon runtime logs each failed attempt.
type operationError struct {
	operation string
	cause     error
}

func (e operationError) Error() string {
	return e.operation + ": " + e.cause.Error()
}

func (e operationError) Unwrap() error {
	return e.cause
}

// Options configures a [Client].
type Options struct {
	Backend                Backend
	MilvusAddress          string
	MilvusDatabase         string
	EmbeddingBaseURL       string
	EmbeddingModel         string
	EmbeddingDimension     int
	EmbeddingAPIKey        string
	EmbeddingTimeout       time.Duration
	QueryInstructionPrefix string
	DenseSearchParams      map[string]string
	// LocalRoot is the store directory of BackendLocal. BackendLocal ignores the
	// Milvus and embedding endpoint options.
	LocalRoot string
	// CheckpointDir is the directory of the per-conversation fingerprint
	// records the ingest persists. Empty keeps them in memory only.
	CheckpointDir string
}

// Client searches and ingests the Milvus conversation collection in process
// through the collection library.
type Client struct {
	store       backend
	embedder    textEmbedder
	dimension   int
	queryPrefix string
	declaration collection.Declaration

	denseSearchParams map[string]string

	// embeddingModel is written to each row and selects the stored vectors the
	// ingest reuses. byteBudget is the largest embedding input in bytes.
	embeddingModel string
	byteBudget     int

	// deliveryBatchBytes is the raw transcript byte budget for each sync worker
	// pass. Zero keeps the worker default.
	deliveryBatchBytes int64

	checkpoint *checkpointStore

	// ingestMu guards the collections ensured in this process and the job count.
	ingestMu sync.Mutex
	ensured  map[string]bool
	jobCount int

	// vectorMu guards the query vector cache. vectorOrder lists the cached
	// queries, oldest first.
	vectorMu    sync.Mutex
	vectors     map[string][]float32
	vectorOrder []string
}

func (c *Client) loadCollectionIfPresent(ctx context.Context, collectionName string) (bool, error) {
	return c.store.collectionPresent(ctx, collectionName)
}

// Open builds the embedding provider and opens the vector store that
// options.Backend selects.
func Open(ctx context.Context, options Options) (*Client, error) {
	opened, err := openBackend(ctx, options)
	if err != nil {
		return nil, err
	}
	return &Client{
		store:       opened.store,
		embedder:    opened.embedder,
		dimension:   opened.dimension,
		queryPrefix: opened.queryPrefix,
		declaration: Declaration(),

		denseSearchParams: options.DenseSearchParams,

		embeddingModel: opened.embeddingModel,
		byteBudget:     opened.byteBudget,

		deliveryBatchBytes: opened.deliveryBatchBytes,

		checkpoint:  newCheckpointStore(options.CheckpointDir),
		ingestMu:    sync.Mutex{},
		ensured:     make(map[string]bool),
		jobCount:    0,
		vectorMu:    sync.Mutex{},
		vectors:     make(map[string][]float32, queryVectorCacheSize),
		vectorOrder: nil,
	}, nil
}

// DeliveryBatchBytes returns the per-pass raw transcript byte budget for this
// backend. Zero keeps the worker default.
func (c *Client) DeliveryBatchBytes() int64 {
	if c == nil {
		return 0
	}
	return c.deliveryBatchBytes
}

// Close closes the vector store.
func (c *Client) Close(ctx context.Context) error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.close(ctx)
}

// SearchConversations ranks the conversation collection for query and returns
// at most limit hits, at most perConversationLimit per conversation, none
// scoring below the filter's MinScore. A missing collection returns no hits.
func (c *Client) SearchConversations(
	ctx context.Context,
	collectionID string,
	query string,
	limit int32,
	filter semsearch.SearchFilter,
	perConversationLimit int32,
) ([]semsearch.SemHit, error) {
	if c == nil {
		return nil, errors.New("search semantic conversations: client is nil")
	}
	trimmedCollectionID := strings.TrimSpace(collectionID)
	if trimmedCollectionID == "" {
		return nil, errors.New("search semantic conversations: collection id is empty")
	}
	started := clock.Now()
	vector, reused, err := c.queryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	embedDuration := clock.Now().Sub(started)
	request := collection.SearchRequest{
		Collection:    CollectionName(trimmedCollectionID),
		Query:         query,
		Vector:        vector,
		Limit:         limit,
		MinScore:      filter.MinScore,
		Filter:        searchFilter(filter),
		GroupBy:       "",
		PerGroupLimit: 0,
		Declaration:   c.declaration,
	}
	if perConversationLimit > 0 {
		request.GroupBy = c.declaration.ItemIDColumn
		request.PerGroupLimit = perConversationLimit
	}
	hits, timing, err := c.search(ctx, request)
	if err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.search_completed",
		"concern", "conversation.semantic",
		"component", "conversation",
		"collection", request.Collection,
		"embed_ms", embedDuration.Milliseconds(),
		"vector_reused", reused,
		"rank_ms", timing.rank.Milliseconds(),
		"load_ms", timing.load.Milliseconds(),
		"total_ms", clock.Now().Sub(started).Milliseconds(),
		"candidates", timing.candidates,
		"hits", len(hits),
	)
	return semHits(hits)
}

// searchTiming is the duration of the rank and load stages of one search and
// the number of ranked candidates.
type searchTiming struct {
	rank       time.Duration
	load       time.Duration
	candidates int
}

// queryVector returns the vector of a query and whether it came from the
// cache. A paged search sends the same query once per page.
func (c *Client) queryVector(ctx context.Context, query string) ([]float32, bool, error) {
	c.vectorMu.Lock()
	cached, found := c.vectors[query]
	c.vectorMu.Unlock()
	if found {
		return cached, true, nil
	}
	vector, err := c.embedQuery(ctx, query)
	if err != nil {
		return nil, false, err
	}
	c.vectorMu.Lock()
	defer c.vectorMu.Unlock()
	if _, present := c.vectors[query]; !present {
		if len(c.vectorOrder) >= queryVectorCacheSize {
			delete(c.vectors, c.vectorOrder[0])
			c.vectorOrder = c.vectorOrder[1:]
		}
		c.vectors[query] = vector
		c.vectorOrder = append(c.vectorOrder, query)
	}
	return vector, false, nil
}

// SearchWithinConversation ranks one conversation's rows for query. The
// in-process search stores no checkpoint of delivered content and always
// returns an empty fingerprint.
func (c *Client) SearchWithinConversation(
	ctx context.Context,
	collectionID string,
	conversationID string,
	query string,
	limit int32,
	filter semsearch.SearchFilter,
) ([]semsearch.SemHit, string, error) {
	trimmedConversationID := strings.TrimSpace(conversationID)
	if trimmedConversationID == "" {
		return nil, "", errors.New("search within semantic conversation: conversation id is empty")
	}
	scoped := filter
	scoped.ConversationIDs = []string{trimmedConversationID}
	hits, err := c.SearchConversations(ctx, collectionID, query, limit, scoped, 0)
	if err != nil {
		return nil, "", err
	}
	return hits, "", nil
}

// embedQuery embeds the query with the instruction prefix and checks the vector
// width against the configured dimension.
func (c *Client) embedQuery(ctx context.Context, query string) ([]float32, error) {
	vector, err := c.embedder.Embed(ctx, c.queryPrefix+query)
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.embed_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", err,
		)
		return nil, fmt.Errorf("embed conversation query: %w", err)
	}
	if c.dimension > 0 && len(vector) != c.dimension {
		return nil, fmt.Errorf("embed conversation query: endpoint returned %d dimensions, configured embedding_dimension is %d", len(vector), c.dimension)
	}
	return vector, nil
}

func (c *Client) search(ctx context.Context, request collection.SearchRequest) ([]collection.Hit, searchTiming, error) {
	return c.store.search(ctx, request)
}
