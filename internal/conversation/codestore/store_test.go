package codestore_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	testCollection = "conv_chunks_test"
	itemColumn     = "conversationId"
)

func declaration() collection.Declaration {
	return collection.Declaration{
		ItemIDColumn: itemColumn,
		Scalars:      []collection.ScalarColumn{{Name: itemColumn, Type: collection.ScalarTypeString, Nullable: true, MaxLength: 256}},
	}
}

func testRow(t *testing.T, model *staticembed.Model, id string, item string, content string) collection.Row {
	t.Helper()
	return collection.Row{
		ID:                id,
		Content:           content,
		RelativePath:      "conv/" + item + "/0",
		StartLine:         0,
		EndLine:           0,
		FileExtension:     "",
		Metadata:          `{"conversation_id":"` + item + `"}`,
		SplitPart:         0,
		SplitPartRecorded: false,
		Vector:            model.Vector(content),
		Scalars:           map[string]collection.ScalarValue{itemColumn: collection.StringScalar(item)},
	}
}

func searchIDs(t *testing.T, store *codestore.Store, model *staticembed.Model, query string) []string {
	t.Helper()
	hits, err := store.Search(context.Background(), collection.SearchRequest{
		Collection:    testCollection,
		Query:         query,
		Vector:        model.Vector(query),
		Limit:         10,
		MinScore:      -1,
		Filter:        nil,
		GroupBy:       "",
		PerGroupLimit: 0,
		Declaration:   declaration(),
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	return ids
}

func TestReopenedStoreRanksSavedRowsAndDropsAnInterruptedWrite(t *testing.T) {
	ctx := context.Background()
	model, err := staticembed.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	root := t.TempDir()
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}
	if err := store.EnsureCollection(ctx, ensure); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	rows := []collection.Row{
		testRow(t, model, "a", "claude:a", "the daemon rebinds its listener after a config change"),
		testRow(t, model, "b", "claude:b", "bake the sourdough at a high oven temperature"),
		testRow(t, model, "c", "claude:c", "the listener socket closes when the daemon reloads"),
	}
	if err := store.Upsert(ctx, testCollection, declaration(), rows); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	deleted, err := store.DeleteItems(ctx, collection.DeleteItemsRequest{
		Collection: testCollection, Declaration: declaration(), ItemIDs: []string{"claude:b"}, PathPrefixes: nil,
	})
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteItems = %d, %v; want 1 row", deleted, err)
	}
	query := "why does the daemon rebind the listener"
	want := searchIDs(t, store, model, query)
	if len(want) != 2 {
		t.Fatalf("ranked %v, want the two remaining rows", want)
	}
	store.Close()

	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := searchIDs(t, reopened, model, query); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened store ranked %v, want %v", got, want)
	}
	reopened.Close()

	// An interrupted write leaves a partial record at the end of the row log.
	rowLog, err := os.OpenFile(filepath.Join(root, testCollection, "rows.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open row log: %v", err)
	}
	if _, err := rowLog.Write([]byte{200, 0, 0, 0, 1, 2}); err != nil {
		t.Fatalf("append partial record: %v", err)
	}
	if err := rowLog.Close(); err != nil {
		t.Fatalf("close row log: %v", err)
	}
	recovered, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("open after interrupted write: %v", err)
	}
	defer recovered.Close()
	if got := searchIDs(t, recovered, model, query); !reflect.DeepEqual(got, want) {
		t.Fatalf("store after interrupted write ranked %v, want %v", got, want)
	}
	if err := recovered.Upsert(ctx, testCollection, declaration(), rows[1:2]); err != nil {
		t.Fatalf("Upsert after interrupted write: %v", err)
	}
	if count, err := recovered.Load(testCollection); err != nil || count != 3 {
		t.Fatalf("Load = %d, %v; want 3 rows", count, err)
	}
}
