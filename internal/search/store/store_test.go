package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	st, err := Open(context.Background(), Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close(context.Background(), "test")
	})
	return st, dbPath
}

func TestInsertStartProgressCompleteRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	job := Job{
		ResultID:       "r1",
		ConversationID: "codex:abc",
		Query:          "auth timeout",
		Depth:          "normal",
		Status:         StatusPending,
	}
	if err := st.Insert(ctx, job); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, ok, err := st.Get(ctx, "r1")
	if err != nil || !ok {
		t.Fatalf("get after insert: ok=%v err=%v", ok, err)
	}
	if got.Status != StatusPending || got.ConversationID != "codex:abc" || got.Query != "auth timeout" || got.Depth != "normal" {
		t.Fatalf("unexpected job after insert: %+v", got)
	}

	if err := st.Start(ctx, "r1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := st.UpdateProgress(ctx, "r1", Progress{ChunksDone: 7, ChunksTotal: 19, LayerIndex: 1, LayerTotal: 3, LayerName: "sweep"}); err != nil {
		t.Fatalf("progress: %v", err)
	}
	got, _, _ = st.Get(ctx, "r1")
	if got.Status != StatusRunning {
		t.Fatalf("status after start: %q", got.Status)
	}
	if got.Progress.ChunksDone != 7 || got.Progress.ChunksTotal != 19 || got.Progress.LayerIndex != 1 || got.Progress.LayerTotal != 3 || got.Progress.LayerName != "sweep" {
		t.Fatalf("unexpected progress: %+v", got.Progress)
	}

	set := json.RawMessage(`{"messages":[{"uuid":"m1"}]}`)
	if err := st.Complete(ctx, "r1", "rendered excerpts", set); err != nil {
		t.Fatalf("complete: %v", err)
	}
	got, _, _ = st.Get(ctx, "r1")
	if got.Status != StatusComplete {
		t.Fatalf("status after complete: %q", got.Status)
	}
	if got.ResultText != "rendered excerpts" {
		t.Fatalf("result text: %q", got.ResultText)
	}
	if string(got.ResultSetJSON) != string(set) {
		t.Fatalf("result set json: %q", string(got.ResultSetJSON))
	}
}

func TestFailAndCancel(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)

	if err := st.Insert(ctx, Job{ResultID: "f1", Status: StatusRunning}); err != nil {
		t.Fatalf("insert f1: %v", err)
	}
	if err := st.Fail(ctx, "f1", "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	got, _, _ := st.Get(ctx, "f1")
	if got.Status != StatusFailed || got.Error != "boom" {
		t.Fatalf("after fail: status=%q err=%q", got.Status, got.Error)
	}

	if err := st.Insert(ctx, Job{ResultID: "c1", Status: StatusRunning}); err != nil {
		t.Fatalf("insert c1: %v", err)
	}
	if err := st.Cancel(ctx, "c1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got, _, _ = st.Get(ctx, "c1")
	if got.Status != StatusCanceled {
		t.Fatalf("after cancel: status=%q", got.Status)
	}
}

func TestGetMissing(t *testing.T) {
	ctx := context.Background()
	st, _ := newTestStore(t)
	_, ok, err := st.Get(ctx, "nope")
	if err != nil {
		t.Fatalf("get missing err: %v", err)
	}
	if ok {
		t.Fatalf("expected missing row to report ok=false")
	}
}

func TestReopenAfterCloseKeepsRows(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "jobs.db")

	st, err := Open(ctx, Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if err := st.Insert(ctx, Job{ResultID: "p1", ConversationID: "claude:x", Status: StatusRunning}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.Complete(ctx, "p1", "done", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := st.Close(ctx, "test-reopen"); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	reopened, err := Open(ctx, Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer func() { _ = reopened.Close(ctx, "test") }()
	got, ok, err := reopened.Get(ctx, "p1")
	if err != nil || !ok {
		t.Fatalf("get after reopen: ok=%v err=%v", ok, err)
	}
	if got.Status != StatusComplete || got.ResultText != "done" || string(got.ResultSetJSON) != `{"ok":true}` {
		t.Fatalf("row not durable after reopen: %+v", got)
	}
}
