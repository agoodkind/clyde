//go:build unix

package codestore_test

import (
	"os/signal"
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
