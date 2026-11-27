package vectorsearch

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/lm-semantic-search/collection"
)

type coverageTestBackend struct {
	backend
	exists   bool
	rows     []collection.StoredRow
	request  collection.RowsRequest
	queryErr error
}

func (store *coverageTestBackend) collectionPresent(context.Context, string) (bool, error) {
	return store.exists, nil
}

func (store *coverageTestBackend) QueryRows(
	_ context.Context,
	request collection.RowsRequest,
) ([]collection.StoredRow, error) {
	store.request = request

	return store.rows, store.queryErr
}

func TestCoveredHitsRequiresMatchingEmbeddedByteRanges(t *testing.T) {
	const path = "conv/codex:test/3"
	const model = "coverage-test-model"
	store := &coverageTestBackend{
		exists: true,
		rows: []collection.StoredRow{
			coverageTestRow(path, "abc", 1, model, []float32{1, 0}),
			coverageTestRow(path, "defg", 4, model, []float32{1, 0}),
			coverageTestRow(path, "hi", 8, model, nil),
			coverageTestRow(path, "hi", 8, "older-model", []float32{1, 0}),
			coverageTestRow(path, "hi", 8, model, []float32{0, 0}),
			coverageTestRow(path, "hi", 8, model, []float32{1}),
			coverageTestRow(path, "hi", 8, model, []float32{1, float32(math.NaN())}),
			coverageTestRow(path+"0", "hi", 8, model, []float32{1, 0}),
		},
	}
	client := &Client{store: store, dimension: 2, embeddingModel: model}
	hits := []semsearch.SemHit{
		{RelativePath: path, Content: "bcde", SplitPart: 2},
		{RelativePath: path, Content: "bcXe", SplitPart: 2},
		{RelativePath: path, Content: "efghi", SplitPart: 5},
		{RelativePath: path, Content: "defg", SplitPart: 4, LoadRules: "other"},
		{RelativePath: path, Content: "defg", SplitPart: 0},
	}

	got, err := client.CoveredHits(t.Context(), "test", hits)
	if err != nil {
		t.Fatal(err)
	}

	if want := []bool{true, false, false, false, false}; !slices.Equal(got, want) {
		t.Fatalf("coverage = %v, want %v", got, want)
	}

	if !store.request.IncludeVector || !slices.Equal(store.request.PathPrefixes, []string{path}) {
		t.Fatalf("coverage query = %+v", store.request)
	}
}

func TestCoveredHitsMissingCollectionAndReadFailure(t *testing.T) {
	hits := []semsearch.SemHit{{RelativePath: "conv/codex:test/0", Content: "pending"}}
	store := &coverageTestBackend{exists: false}
	client := &Client{store: store}
	got, err := client.CoveredHits(t.Context(), "test", hits)
	if err != nil || !slices.Equal(got, []bool{false}) {
		t.Fatalf("missing collection coverage = %v, error = %v", got, err)
	}

	store.exists = true
	store.queryErr = errors.New("Milvus connection failed")
	if _, err := client.CoveredHits(t.Context(), "test", hits); !errors.Is(err, store.queryErr) {
		t.Fatalf("coverage error = %v, want %v", err, store.queryErr)
	}
}

func coverageTestRow(
	path string,
	content string,
	splitPart int32,
	model string,
	vector []float32,
) collection.StoredRow {
	return collection.StoredRow{
		RelativePath:      path,
		Content:           content,
		SplitPart:         splitPart,
		SplitPartRecorded: true,
		EmbeddingModel:    model,
		Vector:            vector,
	}
}
