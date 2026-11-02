package vectorsearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/lm-semantic-search/collection"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
	"goodkind.io/lm-semantic-search/embedding"
)

// rankingStore is the store surface the Milvus backend uses for a search: the
// collection store, the ranking search, and the load of the selected rows.
type rankingStore interface {
	collection.Store
	Rank(ctx context.Context, request collection.SearchRequest) ([]milvusstore.Candidate, error)
	Load(ctx context.Context, collectionName string, selected []milvusstore.Candidate, scalarColumns []collection.ScalarColumn) ([]collection.Hit, error)
}

// milvusBackend stores conversation rows in Milvus. shared is nil for the
// single-collection layout.
type milvusBackend struct {
	rankingStore
	client *milvusclient.Client
	shared *milvusstore.SharedStore
}

func openMilvusBackend(ctx context.Context, options Options) (openedBackend, error) {
	failed := openedBackend{store: nil, embedder: nil, embeddingModel: "", dimension: 0, queryPrefix: "", byteBudget: 0, deliveryBatchBytes: 0}
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
	// Bound connection retries during daemon startup.
	connectCtx, cancelConnect := context.WithTimeout(ctx, milvusConnectTimeout)
	defer cancelConnect()
	client, err := milvusclient.New(connectCtx, &milvusclient.ClientConfig{
		Address: options.MilvusAddress,
		DBName:  options.MilvusDatabase,
	})
	if err != nil {
		return failed, operationError{operation: fmt.Sprintf("connect to Milvus at %q database %q", options.MilvusAddress, options.MilvusDatabase), cause: err}
	}
	storeOptions := milvusstore.Options{Hybrid: true, EmbeddingModel: options.EmbeddingModel, DenseSearchParams: options.DenseSearchParams, DenseIndex: nil}
	store := &milvusBackend{rankingStore: nil, client: client, shared: nil}
	switch options.MilvusLayout {
	case "", LayoutSingle:
		store.rankingStore = milvusstore.New(client, storeOptions)
	case LayoutShared:
		shared, sharedErr := milvusstore.NewShared(client, storeOptions)
		if sharedErr != nil {
			_ = client.Close(context.WithoutCancel(ctx))
			return failed, operationError{operation: "open the shared vector conversation store", cause: sharedErr}
		}
		store.rankingStore = shared
		store.shared = shared
	default:
		_ = client.Close(context.WithoutCancel(ctx))
		return failed, operationError{operation: "open conversation vector store", cause: fmt.Errorf("milvus layout %q is not registered", options.MilvusLayout)}
	}
	return openedBackend{
		store:          store,
		embedder:       embedder,
		embeddingModel: options.EmbeddingModel,
		dimension:      options.EmbeddingDimension,
		queryPrefix:    queryPrefix,
		byteBudget:     embedByteBudget(options.EmbeddingModel),

		deliveryBatchBytes: 0,
	}, nil
}

// collectionPresent loads the collection and, in the shared layout, its vector
// collection. Milvus requires a loaded collection before stored-row queries.
// A shared occurrence collection without its vector collection is an error.
func (b *milvusBackend) collectionPresent(ctx context.Context, collectionName string) (bool, error) {
	exists, err := b.loadCollection(ctx, collectionName)
	if err != nil || !exists || b.shared == nil {
		return exists, err
	}
	vectors := milvusstore.VectorCollectionName(collectionName)
	vectorsExist, err := b.loadCollection(ctx, vectors)
	if err != nil {
		return false, err
	}
	if !vectorsExist {
		return false, failRead("load Milvus collection "+vectors, fmt.Errorf("shared collection %s has no vector collection %s", collectionName, vectors))
	}
	return true, nil
}

// loadCollection loads one Milvus collection. It reports false when the
// collection does not exist.
func (b *milvusBackend) loadCollection(ctx context.Context, collectionName string) (bool, error) {
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

// optionalModelAt reads the embedding model of one row. It returns an empty string
// when the column is missing or the value is null, as QueryRows does.
func optionalModelAt(modelColumn column.Column, rowIndex int) (string, error) {
	if modelColumn == nil {
		return "", nil
	}
	isNull, err := modelColumn.IsNull(rowIndex)
	if err != nil {
		return "", failRead("read embedding model null state", err)
	}
	if isNull {
		return "", nil
	}
	model, err := modelColumn.GetAsString(rowIndex)
	if err != nil {
		return "", failRead("read embedding model", err)
	}
	return model, nil
}

// storedRowColumns stores the columns from a query for stored rows.
type storedRowColumns struct {
	id      column.Column
	content column.Column
	model   column.Column
	value   column.Column
}

// readRowVector reads one stored row and returns the row and its ID. With the shared
// layout, it returns the vector key separately. With the other layout, it includes the
// vector in the row.
func (b *milvusBackend) readRowVector(collectionName string, columns storedRowColumns, rowIndex int) (string, storedRowVector, string, error) {
	empty := storedRowVector{content: "", embeddingModel: "", vector: nil}
	id, err := columns.id.GetAsString(rowIndex)
	if err != nil {
		return "", empty, "", failRead(fmt.Sprintf("read row ID %d from %s", rowIndex, collectionName), err)
	}
	content, err := columns.content.GetAsString(rowIndex)
	if err != nil {
		return "", empty, "", failRead(fmt.Sprintf("read content of row %s from %s", id, collectionName), err)
	}
	model, err := optionalModelAt(columns.model, rowIndex)
	if err != nil {
		return "", empty, "", failRead(fmt.Sprintf("read embedding model of row %s from %s", id, collectionName), err)
	}
	row := storedRowVector{content: content, embeddingModel: model, vector: nil}
	if b.shared != nil {
		key, keyErr := columns.value.GetAsString(rowIndex)
		if keyErr != nil {
			return "", empty, "", failRead(fmt.Sprintf("read vector key of row %s from %s", id, collectionName), keyErr)
		}
		return id, row, key, nil
	}
	vector, err := milvusstore.VectorAt(columns.value, rowIndex)
	if err != nil {
		return "", empty, "", failRead(fmt.Sprintf("read vector of row %s from %s", id, collectionName), err)
	}
	row.vector = vector
	return id, row, "", nil
}

// storedRowVector stores the content, embedding model, and vector of a reusable row.
// The caller compares its content and model with the planned row.
type storedRowVector struct {
	content        string
	embeddingModel string
	vector         []float32
}

// readRowVectors returns the stored content, embedding model, and dense vector for
// each requested row ID. It omits missing rows. For the shared layout, it reads each
// row's vector key before reading the vector.
func (b *milvusBackend) readRowVectors(ctx context.Context, collectionName string, ids []string) (map[string]storedRowVector, error) {
	rows := make(map[string]storedRowVector, len(ids))
	field := milvusstore.DenseVectorField
	if b.shared != nil {
		field = milvusstore.VectorKeyField
	}
	values := make(map[string]string, len(ids))
	for start := 0; start < len(ids); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(ids))
		resultSet, err := b.client.Query(ctx, milvusclient.NewQueryOption(collectionName).
			WithIDs(column.NewColumnVarChar(milvusstore.IDField, slices.Clone(ids[start:end]))).
			WithOutputFields(milvusstore.IDField, milvusstore.ContentField, milvusstore.EmbeddingModelField, field).
			WithConsistencyLevel(entity.ClStrong))
		if err != nil {
			return nil, failRead("read stored row vectors from "+collectionName, err)
		}
		if resultSet.ResultCount == 0 {
			continue
		}
		columns := storedRowColumns{
			id:      resultSet.GetColumn(milvusstore.IDField),
			content: resultSet.GetColumn(milvusstore.ContentField),
			model:   resultSet.GetColumn(milvusstore.EmbeddingModelField),
			value:   resultSet.GetColumn(field),
		}
		if columns.id == nil || columns.content == nil || columns.value == nil {
			return nil, collection.ErrSearchResultIncomplete
		}
		for rowIndex := range resultSet.ResultCount {
			id, row, key, rowErr := b.readRowVector(collectionName, columns, rowIndex)
			if rowErr != nil {
				return nil, rowErr
			}
			rows[id] = row
			if b.shared != nil {
				values[id] = key
			}
		}
	}
	if b.shared == nil {
		return rows, nil
	}
	vectors, err := b.readSharedVectors(ctx, collectionName, values)
	if err != nil {
		return nil, err
	}
	for id, row := range rows {
		vector, found := vectors[id]
		if !found {
			return nil, failRead("read stored row vectors from "+collectionName, fmt.Errorf("row %s references missing vector %s", id, values[id]))
		}
		row.vector = vector
		rows[id] = row
	}
	return rows, nil
}

// readSharedVectors uses each row's vector key to read its stored vector.
func (b *milvusBackend) readSharedVectors(ctx context.Context, collectionName string, keyByID map[string]string) (map[string][]float32, error) {
	vectorCollection := milvusstore.VectorCollectionName(collectionName)
	keys := make([]string, 0, len(keyByID))
	for _, key := range keyByID {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	vectorByKey := make(map[string][]float32, len(keys))
	for start := 0; start < len(keys); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(keys))
		resultSet, err := b.client.Query(ctx, milvusclient.NewQueryOption(vectorCollection).
			WithIDs(column.NewColumnVarChar(milvusstore.VectorKeyField, slices.Clone(keys[start:end]))).
			WithOutputFields(milvusstore.VectorKeyField, milvusstore.DenseVectorField).
			WithConsistencyLevel(entity.ClStrong))
		if err != nil {
			return nil, failRead("read stored vectors from "+vectorCollection, err)
		}
		if resultSet.ResultCount == 0 {
			continue
		}
		keyColumn := resultSet.GetColumn(milvusstore.VectorKeyField)
		vectorColumn := resultSet.GetColumn(milvusstore.DenseVectorField)
		if keyColumn == nil || vectorColumn == nil {
			return nil, collection.ErrSearchResultIncomplete
		}
		for rowIndex := range resultSet.ResultCount {
			key, keyErr := keyColumn.GetAsString(rowIndex)
			if keyErr != nil {
				return nil, failRead(fmt.Sprintf("read vector key %d from %s", rowIndex, vectorCollection), keyErr)
			}
			vector, vectorErr := milvusstore.VectorAt(vectorColumn, rowIndex)
			if vectorErr != nil {
				return nil, failRead(fmt.Sprintf("read vector %s from %s", key, vectorCollection), vectorErr)
			}
			vectorByKey[key] = vector
		}
	}
	vectors := make(map[string][]float32, len(keyByID))
	for id, key := range keyByID {
		if vector, found := vectorByKey[key]; found {
			vectors[id] = vector
		}
	}
	return vectors, nil
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
