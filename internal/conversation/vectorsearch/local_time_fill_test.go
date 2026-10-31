package vectorsearch_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/lm-semantic-search/collection"
)

const (
	localFillCollectionID   = "local-time-fill"
	localFillConversationID = "cursor:c6c6c6c6-0000-4000-8000-000000000001"
	localFillText           = "The retention job keeps thirty daily restic snapshots."
	localFillQuery          = "restic retention daily snapshots"
)

type localFillState struct {
	hit    semsearch.SemHit
	rows   int
	hashes []string
}

func localFillDoc(timestampUnix int64) semsearch.SemDoc {
	return semsearch.SemDoc{
		ConversationID: localFillConversationID, ParentConversationID: "", MessageIndex: 0, Role: "user",
		TimestampUnix: timestampUnix, Text: localFillText, Tools: nil, Thinking: "",
		WorkspaceRoot: "", Archived: false, LoadRules: "",
	}
}

// localFillUpsert upserts a document, searches for it, and reads stored row hashes.
func localFillUpsert(t *testing.T, root string, timestampUnix int64) localFillState {
	t.Helper()
	ctx := context.Background()
	client, err := vectorsearch.Open(ctx, vectorsearch.Options{Backend: vectorsearch.BackendLocal, LocalRoot: root})
	if err != nil {
		t.Fatalf("cannot open the local client: %v", err)
	}
	docs := []semsearch.SemDoc{localFillDoc(timestampUnix)}
	manifest := []semsearch.Fingerprint{{ConversationID: localFillConversationID, Value: time.Unix(timestampUnix, 0).UTC().String()}}
	if _, err := client.UpsertConversationDocuments(ctx, localFillCollectionID, docs, manifest); err != nil {
		t.Fatalf("cannot upsert the conversation document: %v", err)
	}
	hits, err := client.SearchConversations(ctx, localFillCollectionID, localFillQuery, 5, semsearch.SearchFilter{}, 0)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search returned an unexpected result; hit count: %d: %v", len(hits), err)
	}
	if err := client.Close(ctx); err != nil {
		t.Fatalf("cannot close the local client: %v", err)
	}
	store, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("cannot open the local store: %v", err)
	}
	defer store.Close()
	rows, err := store.QueryRows(ctx, collection.RowsRequest{
		Collection: vectorsearch.CollectionName(localFillCollectionID), Declaration: collection.Declaration{ItemIDColumn: "", Scalars: nil},
		ItemIDs: nil, PathPrefixes: []string{"conv/"}, IncludeVector: false,
	})
	if err != nil {
		t.Fatalf("cannot query local rows: %v", err)
	}
	hashes := make([]string, 0, len(rows))
	for _, row := range rows {
		hashes = append(hashes, row.ID+"="+row.ContentHash)
	}
	slices.Sort(hashes)
	return localFillState{hit: hits[0], rows: len(rows), hashes: hashes}
}

// TestLocalUpsertFillsAnUnknownTimeWithoutChangingTheRow checks timestamp fills
// and preserves row hashes, search content, and score.
func TestLocalUpsertFillsAnUnknownTimeWithoutChangingTheRow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "local")
	unknown := time.Time{}.Unix()
	recovered := time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC).Unix()
	before := localFillUpsert(t, root, unknown)
	if before.hit.TimestampUnix != unknown {
		t.Fatalf("initial timestamp differs from the unknown marker; actual: %d", before.hit.TimestampUnix)
	}
	for _, step := range []struct {
		send int64
		want int64
	}{{send: recovered, want: recovered}, {send: unknown, want: recovered}} {
		after := localFillUpsert(t, root, step.send)
		if after.hit.TimestampUnix != step.want {
			t.Fatalf("timestamp differs from expected; actual, expected: %d, %d", after.hit.TimestampUnix, step.want)
		}
		if after.rows != before.rows || !slices.Equal(after.hashes, before.hashes) {
			t.Fatalf("row count or hashes changed; row count after, row count before: %d, %d", after.rows, before.rows)
		}
		if after.hit.Content != before.hit.Content || after.hit.Score != before.hit.Score {
			t.Fatalf("search content or score changed; hit after, hit before: %+v, %+v", after.hit, before.hit)
		}
	}
}
