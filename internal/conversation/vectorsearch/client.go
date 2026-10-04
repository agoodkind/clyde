package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
	"goodkind.io/lm-semantic-search/embedding"
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
	// milvusConnectTimeout bounds opening the Milvus connection.
	milvusConnectTimeout = 2 * time.Second
	// milvusCloseTimeout bounds closing the Milvus connection.
	milvusCloseTimeout = 5 * time.Second
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
	MilvusAddress          string
	MilvusDatabase         string
	EmbeddingBaseURL       string
	EmbeddingModel         string
	EmbeddingDimension     int
	EmbeddingAPIKey        string
	EmbeddingTimeout       time.Duration
	QueryInstructionPrefix string
	// CheckpointDir is the directory of the per-conversation fingerprint
	// records the ingest persists. Empty keeps them in memory only.
	CheckpointDir string
}

// Client searches the Milvus conversation collection in process. It embeds the
// query, runs the hybrid dense and BM25 ranking through the collection library,
// and converts each ranked row to a conversation hit. It satisfies the daemon's
// conversation search client interface.
type Client struct {
	milvus      *milvusclient.Client
	store       *milvusstore.Store
	embedder    embedding.Provider
	dimension   int
	queryPrefix string
	declaration collection.Declaration

	// embeddingModel is written to each row and selects the stored vectors the
	// ingest reuses. byteBudget is the largest embedding input in bytes.
	embeddingModel string
	byteBudget     int

	// checkpoint records the manifest fingerprint of each conversation after its
	// rows are written.
	checkpoint *checkpointStore

	// ingestMu guards the collections ensured in this process and the job count.
	ingestMu sync.Mutex
	ensured  map[string]bool
	jobCount int
}

// loadCollectionIfPresent reports whether the collection exists and loads it
// into memory, because Milvus serves a stored-row query on a loaded collection
// only.
func (c *Client) loadCollectionIfPresent(ctx context.Context, collectionName string) (bool, error) {
	exists, err := c.milvus.HasCollection(ctx, milvusclient.NewHasCollectionOption(collectionName))
	if err != nil {
		return false, failRead("check Milvus collection "+collectionName, err)
	}
	if !exists {
		return false, nil
	}
	task, err := c.milvus.LoadCollection(ctx, milvusclient.NewLoadCollectionOption(collectionName))
	if err != nil {
		return false, failRead("load Milvus collection "+collectionName, err)
	}
	if err := task.Await(ctx); err != nil {
		return false, failRead("await load of Milvus collection "+collectionName, err)
	}
	return true, nil
}

// Open connects to Milvus and builds the embedding provider. It fails when the
// Milvus connection cannot open.
func Open(ctx context.Context, options Options) (*Client, error) {
	embedder, err := embedding.NewOpenAICompatible(embedding.OpenAIOptions{
		APIKey:         options.EmbeddingAPIKey,
		BaseURL:        options.EmbeddingBaseURL,
		Model:          options.EmbeddingModel,
		Dimensions:     0,
		RequestTimeout: options.EmbeddingTimeout,
	})
	if err != nil {
		return nil, operationError{operation: fmt.Sprintf("build conversation embedding provider for model %q", options.EmbeddingModel), cause: err}
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
		return nil, operationError{operation: fmt.Sprintf("connect to Milvus at %q database %q", options.MilvusAddress, options.MilvusDatabase), cause: err}
	}
	queryPrefix := options.QueryInstructionPrefix
	if queryPrefix == "" && strings.Contains(options.EmbeddingModel, nvEmbedCodeModelMarker) {
		queryPrefix = nvEmbedCodeQueryPrefix
	}
	return &Client{
		milvus:      client,
		store:       milvusstore.New(client, milvusstore.Options{Hybrid: true, EmbeddingModel: options.EmbeddingModel}),
		embedder:    embedder,
		dimension:   options.EmbeddingDimension,
		queryPrefix: queryPrefix,
		declaration: Declaration(),

		embeddingModel: options.EmbeddingModel,
		byteBudget:     embedByteBudget(options.EmbeddingModel),
		checkpoint:     newCheckpointStore(options.CheckpointDir),
		ingestMu:       sync.Mutex{},
		ensured:        make(map[string]bool),
		jobCount:       0,
	}, nil
}

// Close closes the Milvus connection.
func (c *Client) Close(ctx context.Context) error {
	if c == nil || c.milvus == nil {
		return nil
	}
	closeCtx, cancel := context.WithTimeout(ctx, milvusCloseTimeout)
	defer cancel()
	if err := c.milvus.Close(closeCtx); err != nil {
		slog.Warn("conversation.vectorsearch.milvus_close_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"err", err,
		)
		return fmt.Errorf("close Milvus connection: %w", err)
	}
	return nil
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
	vector, err := c.embedQuery(ctx, query)
	if err != nil {
		return nil, err
	}
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
	hits, err := c.search(ctx, request)
	if err != nil {
		return nil, err
	}
	return semHits(hits)
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

// search runs the ranking, resolves legacy conversation groups when the search
// caps hits per conversation, selects the final hits, and loads their rows.
func (c *Client) search(ctx context.Context, request collection.SearchRequest) ([]collection.Hit, error) {
	candidates, err := c.store.Rank(ctx, request)
	if errors.Is(err, collection.ErrCollectionNotReady) {
		// Another Milvus client can release the collection.
		slog.WarnContext(ctx, "conversation.vectorsearch.collection_reloaded",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", request.Collection,
			"err", err,
		)
		if _, loadErr := c.loadCollectionIfPresent(ctx, request.Collection); loadErr != nil {
			return nil, loadErr
		}
		candidates, err = c.store.Rank(ctx, request)
	}
	if errors.Is(err, collection.ErrCollectionMissing) {
		return []collection.Hit{}, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.rank_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", request.Collection,
			"err", err,
		)
		return nil, fmt.Errorf("rank %s: %w", request.Collection, err)
	}
	groupColumn, grouped := milvusstore.GroupColumnFor(request)
	perGroupLimit := int32(0)
	if grouped {
		perGroupLimit = request.PerGroupLimit
		if groupColumn.Name == request.Declaration.ItemIDColumn {
			candidates, err = resolveLegacyGroups(ctx, c.milvus, request.Collection, candidates)
			if err != nil {
				return nil, err
			}
		}
	}
	limit := request.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	selected := milvusstore.SelectCandidates(candidates, perGroupLimit, request.MinScore, limit)
	hits, err := c.store.Load(ctx, request.Collection, selected, request.Declaration.Scalars)
	if err != nil {
		slog.WarnContext(ctx, "conversation.vectorsearch.load_failed",
			"concern", "conversation.semantic",
			"component", "conversation",
			"collection", request.Collection,
			"err", err,
		)
		return nil, fmt.Errorf("load %s: %w", request.Collection, err)
	}
	return hits, nil
}
