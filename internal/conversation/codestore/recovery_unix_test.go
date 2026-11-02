//go:build unix

package codestore_test

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"goodkind.io/clyde/internal/conversation/codestore"
	"goodkind.io/clyde/internal/conversation/staticembed"
	"goodkind.io/lm-semantic-search/collection"
)

func longPathRow(t *testing.T, model *staticembed.Model, id string) collection.Row {
	t.Helper()
	row := testRow(t, model, id, "claude:"+id, "x")
	row.RelativePath = "conv/" + id + "/" + strings.Repeat("p", 4000)
	return row
}

func TestStoreKeepsFilesWhenDirectoryIsUnreadableWithoutHeader(t *testing.T) {
	const (
		searchWithoutListing = 0o300
		fullAccess           = 0o700
	)
	root := t.TempDir()
	store, model := openCollection(t, root)
	upsert(t, store, testRow(t, model, "kept", "claude:first", "row before the header removal"))
	store.Close()
	directory := filepath.Join(root, testCollection)
	for _, prefix := range []string{"rows", "data"} {
		if err := os.Rename(filepath.Join(directory, prefix+".log"), filepath.Join(directory, prefix+".3.log")); err != nil {
			t.Fatalf("The test failed to rename the generation file: %v.", err)
		}
	}
	if err := os.Remove(filepath.Join(directory, "collection.json")); err != nil {
		t.Fatalf("The test failed to remove the collection header: %v.", err)
	}
	filesBefore := directoryContents(t, directory)
	if err := os.Chmod(directory, searchWithoutListing); err != nil {
		t.Fatalf("os.Chmod failed to change the directory permissions: %v.", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(directory, fullAccess); err != nil {
			t.Errorf("os.Chmod failed to restore the directory permissions: %v.", err)
		}
	})

	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("codestore.Open failed to open the store: %v.", err)
	}
	defer reopened.Close()
	ctx := context.Background()
	if _, err := reopened.Load(ctx, testCollection); !errors.Is(err, os.ErrPermission) {
		t.Errorf("Load returned %v; the test expected an error matching os.ErrPermission.", err)
	}
	ensure := collection.EnsureRequest{Collection: testCollection, Declaration: declaration(), Dimension: staticembed.Dimensions}
	if err := reopened.EnsureCollection(ctx, ensure); !errors.Is(err, os.ErrPermission) {
		t.Errorf("EnsureCollection returned %v; the test expected an error matching os.ErrPermission.", err)
	}
	if err := os.Chmod(directory, fullAccess); err != nil {
		t.Fatalf("os.Chmod failed to restore the directory permissions: %v.", err)
	}
	if !maps.EqualFunc(directoryContents(t, directory), filesBefore, bytes.Equal) {
		t.Fatalf("A request changed a file name or file contents in the collection directory.")
	}
}

func TestFailedRowLogWriteKeepsLaterRows(t *testing.T) {
	root := t.TempDir()
	store, model := openCollection(t, root)
	upsert(t, store, longPathRow(t, model, "a"))
	rowLogSize := fileSize(t, rowLogPath(root))

	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	capped := syscall.Rlimit{Cur: uint64(rowLogSize) + 100, Max: limit.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &capped); err != nil {
		t.Fatalf("Setrlimit: %v", err)
	}
	writeErr := store.Upsert(t.Context(), testCollection, declaration(), []collection.Row{longPathRow(t, model, "b")})
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatalf("restore Setrlimit: %v", err)
	}
	if writeErr == nil {
		t.Fatal("Upsert succeeded past the file size limit")
	}
	if got := fileSize(t, rowLogPath(root)); got != rowLogSize {
		t.Fatalf("row log size = %d after the failed write, want %d", got, rowLogSize)
	}

	upsert(t, store, longPathRow(t, model, "c"))
	store.Close()
	reopened, err := codestore.Open(root, staticembed.ModelName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()
	got := contents(t, reopened)
	if len(got) != 2 || got["a"] != "x" || got["c"] != "x" {
		t.Fatalf("reopened rows = %v, want a and c", got)
	}
}
