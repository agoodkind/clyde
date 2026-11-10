package cursorstore_test

import (
	"context"
	"os"
	"reflect"
	"testing"

	cursorstore "goodkind.io/clyde/internal/providers/cursor/store"
)

const (
	addedComposerHeader    = `{"name":"Added","createdAt":1710000000400,"lastUpdatedAt":1710000000500,"status":"none","unifiedMode":"chat","fullConversationHeadersOnly":[{"bubbleId":"bubble-n","type":1}]}`
	replacedComposerHeader = `{"name":"Replacement","createdAt":1710000000600,"lastUpdatedAt":1710000000700,"fullConversationHeadersOnly":[{"bubbleId":"bubble-9","type":2}]}`
)

func TestReadGlobalDiscoveryTracksNewAndDeletedHeaders(t *testing.T) {
	root, _, _ := discoveryFixture(t)
	initial := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if initial.Err != nil || len(initial.Headers) != 2 {
		t.Fatalf("The first read returned headers %+v and error %v.", initial.Headers, initial.Err)
	}
	execDiscoveryStatements(t, root.GlobalDBPath, `INSERT INTO cursorDiskKV VALUES ('composerData:composer-new', '{}')`)
	writeComposerData(t, root.GlobalDBPath, "composer-new", addedComposerHeader)
	added := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	want := map[string]cursorstore.ComposerHeader{
		"composer-a":   initial.Headers["composer-a"],
		"composer-b":   initial.Headers["composer-b"],
		"composer-new": decodedComposerHeader(t, "composer-new", addedComposerHeader),
	}
	if added.Err != nil || !reflect.DeepEqual(added.Headers, want) {
		t.Fatalf("The read returned headers %+v and error %v after the row insert.", added.Headers, added.Err)
	}
	execDiscoveryStatements(t, root.GlobalDBPath,
		`DELETE FROM cursorDiskKV WHERE key IN ('composerData:composer-new', 'composerData:composer-b')`)
	removed := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	delete(want, "composer-new")
	delete(want, "composer-b")
	if removed.Err != nil || !reflect.DeepEqual(removed.Headers, want) {
		t.Fatalf("The read returned headers %+v and error %v after the two row deletions.", removed.Headers, removed.Err)
	}
}

func TestReadGlobalDiscoveryRecoversFromMalformedHeader(t *testing.T) {
	root, _, _ := discoveryFixture(t)
	execDiscoveryStatements(t, root.GlobalDBPath, `INSERT INTO cursorDiskKV VALUES ('composerData:composer-bad', '{')`)
	initial := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if _, found := initial.Headers["composer-bad"]; found || initial.Err != nil {
		t.Fatalf("The read returned header %+v and error %v for a malformed row without an earlier header.", initial.Headers["composer-bad"], initial.Err)
	}
	for _, malformed := range []string{"{", "[", `{"name":`} {
		writeComposerData(t, root.GlobalDBPath, "composer-a", malformed)
		kept := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
		if kept.Err != nil || !reflect.DeepEqual(kept.Headers["composer-a"], initial.Headers["composer-a"]) {
			t.Fatalf("The malformed value %q produced header %+v and error %v.", malformed, kept.Headers["composer-a"], kept.Err)
		}
	}
	writeComposerData(t, root.GlobalDBPath, "composer-a", replacedComposerHeader)
	writeComposerData(t, root.GlobalDBPath, "composer-bad", addedComposerHeader)
	recovered := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if recovered.Err != nil ||
		!reflect.DeepEqual(recovered.Headers["composer-a"], decodedComposerHeader(t, "composer-a", replacedComposerHeader)) ||
		!reflect.DeepEqual(recovered.Headers["composer-bad"], decodedComposerHeader(t, "composer-bad", addedComposerHeader)) {
		t.Fatalf("headers after repair = %+v, %v", recovered.Headers, recovered.Err)
	}
}

func TestReadGlobalDiscoveryToleratesMissingOptionalTables(t *testing.T) {
	root, _ := cursorstore.CreateDiscoveryFixture(t)
	withoutMetadata := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if withoutMetadata.Err != nil || withoutMetadata.Metadata.Err != nil || len(withoutMetadata.Headers) != 2 || len(withoutMetadata.Metadata.ByComposerID) != 0 {
		t.Fatalf("The read without composerHeaders returned %+v.", withoutMetadata)
	}
	execDiscoveryStatements(t, root.GlobalDBPath, `DROP TABLE ItemTable`)
	withoutItems := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if withoutItems.Err != nil || !reflect.DeepEqual(withoutItems.Headers, withoutMetadata.Headers) {
		t.Fatalf("The read without ItemTable returned %+v.", withoutItems)
	}
	execDiscoveryStatements(t, root.GlobalDBPath, `DROP TABLE cursorDiskKV`)
	withoutRows := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if withoutRows.Err != nil || len(withoutRows.Headers) != 0 || len(withoutRows.Stocks) != 0 {
		t.Fatalf("The read without cursorDiskKV returned %+v.", withoutRows)
	}
}

func TestReadGlobalDiscoveryCancellationKeepsPriorHeaders(t *testing.T) {
	root, _, _ := discoveryFixture(t)
	initial := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if initial.Err != nil {
		t.Fatal(initial.Err)
	}
	writeComposerData(t, root.GlobalDBPath, "composer-a", replacedComposerHeader)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cancelled := cursorstore.ReadGlobalDiscovery(ctx, root.GlobalDBPath)
	if cancelled.Err == nil || !reflect.DeepEqual(cancelled.Headers, initial.Headers) {
		t.Fatalf("The read with a canceled context returned headers %+v and error %v.", cancelled.Headers, cancelled.Err)
	}
	recovered := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if recovered.Err != nil || !reflect.DeepEqual(recovered.Headers["composer-a"], decodedComposerHeader(t, "composer-a", replacedComposerHeader)) {
		t.Fatalf("The read after cancellation returned header %+v and error %v.", recovered.Headers["composer-a"], recovered.Err)
	}
}

func TestReadGlobalDiscoveryFollowsDatabaseReplacement(t *testing.T) {
	root, _, _ := discoveryFixture(t)
	initial := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	if initial.Err != nil || len(initial.Headers) != 2 {
		t.Fatalf("The first read returned headers %+v and error %v.", initial.Headers, initial.Err)
	}
	replacement, _ := cursorstore.CreateDiscoveryFixture(t)
	execDiscoveryStatements(t, replacement.GlobalDBPath,
		`DELETE FROM cursorDiskKV WHERE key = 'composerData:composer-b'`,
		`INSERT INTO cursorDiskKV VALUES ('composerData:composer-c', '{}')`)
	writeComposerData(t, replacement.GlobalDBPath, "composer-a", replacedComposerHeader)
	writeComposerData(t, replacement.GlobalDBPath, "composer-c", addedComposerHeader)
	if err := os.Rename(replacement.GlobalDBPath, root.GlobalDBPath); err != nil {
		t.Fatal(err)
	}
	replaced := cursorstore.ReadGlobalDiscovery(t.Context(), root.GlobalDBPath)
	want := map[string]cursorstore.ComposerHeader{
		"composer-a": decodedComposerHeader(t, "composer-a", replacedComposerHeader),
		"composer-c": decodedComposerHeader(t, "composer-c", addedComposerHeader),
	}
	if replaced.Err != nil || !reflect.DeepEqual(replaced.Headers, want) || len(replaced.Metadata.ByComposerID) != 0 {
		t.Fatalf("The read after the database file replacement returned headers %+v and error %v.", replaced.Headers, replaced.Err)
	}
}
