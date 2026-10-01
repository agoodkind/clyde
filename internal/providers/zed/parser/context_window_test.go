package parser

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

func TestReadContextWindowReloadsZedWALAndRejectsChangesDuringVerification(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLYDE_ZED_DATA_DIRS", root)
	updated := time.Date(2026, time.June, 27, 14, 0, 0, 0, time.UTC)
	data := []byte(`{"version":"0.3.0","title":"Context","messages":[{"User":{"id":"user-1","content":[{"Text":"original"}]}}]}`)
	writeThreadsRow(t, root, "context-thread", "", updated, data)
	writeSidebarRow(t, filepath.Join(root, "db", "0-stable", "db.sqlite"), "context-thread", "", "Context", "", updated)
	parser := New()
	candidates, err := parser.Discover(t.Context(), nil)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discover actual thread: %v, %d candidates", err, len(candidates))
	}
	options := conversation.LoadOptions{IncludeSystemPrompts: false, IncludeSystemMessages: false, IncludeToolOutputs: false}
	prior, err := conversation.CollectMessages(parser.Stream(candidates[0].Path, options))
	if err != nil || len(prior) != 1 || prior[0].Text != "original" {
		t.Fatalf("initial cached thread: %+v, %v", prior, err)
	}
	path := filepath.Join(root, "threads", "threads.db")
	writer, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := writer.Close(); err != nil {
			t.Error(err)
		}
	})
	executeZedContextFixture(t, writer, "context-wal.sql")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	executeZedContextFixture(t, writer, "context-edit.sql")
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the committed edit did not remain in the WAL")
	}
	wal, err := os.Stat(path + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("actual WAL is absent or empty: %v", err)
	}
	err = parser.ReadContextWindow(t.Context(), candidates[0].Path, "", 0, 1, options, func(messages []transcript.Message) error {
		if len(messages) != 1 || messages[0].Text != "changed" {
			t.Fatalf("fresh context returned cached thread: %+v", messages)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read actual WAL context: %v", err)
	}
	err = parser.ReadContextWindow(t.Context(), candidates[0].Path, "", 0, 1, options, func([]transcript.Message) error {
		executeZedContextFixture(t, writer, "context-edit.sql")
		return nil
	})
	if err == nil {
		t.Fatal("context accepted a backing WAL edit during verification")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	err = parser.ReadContextWindow(cancelled, candidates[0].Path, "", 0, 1, options, func([]transcript.Message) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation returned %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("closed writer retained %s: %v", suffix, err)
		}
	}
	err = parser.ReadContextWindow(t.Context(), candidates[0].Path, "", 0, 1, options, func(messages []transcript.Message) error {
		if len(messages) != 1 || messages[0].Text != "changed" {
			t.Fatalf("closed-writer context returned stale source: %+v", messages)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("context rejected its own temporary sidecars: %v", err)
	}
}

func executeZedContextFixture(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), string(content)); err != nil {
		t.Fatal(err)
	}
}
