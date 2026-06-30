package hookspec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

type memorySnapshotStore struct {
	snapshots map[ReorientSnapshotKey]string
}

func newMemorySnapshotStore() *memorySnapshotStore {
	return &memorySnapshotStore{snapshots: map[ReorientSnapshotKey]string{}}
}

func (store *memorySnapshotStore) Save(_ context.Context, key ReorientSnapshotKey, body string) error {
	store.snapshots[normalizeSnapshotKey(key)] = body
	return nil
}

func (store *memorySnapshotStore) Consume(_ context.Context, key ReorientSnapshotKey) (string, bool, error) {
	normalized := normalizeSnapshotKey(key)
	body, ok := store.snapshots[normalized]
	if ok {
		delete(store.snapshots, normalized)
	}
	return body, ok, nil
}

func TestRunnerPreCompactStoresSnapshot(t *testing.T) {
	t.Parallel()

	store := newMemorySnapshotStore()
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "PreCompact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project",
			"session_id": "session-1"
		}`),
		Output:        &strings.Builder{},
		SnapshotStore: store,
		Reorient: func(
			context.Context,
			string,
			string,
			string,
			string,
			int,
			int,
			int,
			bool,
		) (conversation.ReorientPage, error) {
			return conversation.ReorientPage{
				Items:      []conversation.ReorientItem{{Title: "Hook", Body: "body"}},
				Remaining:  0,
				TotalItems: 1,
			}, nil
		},
	}

	if err := runner.Run(context.Background(), HookIDReorientBeforeCompact); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.snapshots) != 1 {
		t.Fatalf("snapshots len = %d, want 1", len(store.snapshots))
	}
}

func TestRunnerAfterCompactConsumesSnapshot(t *testing.T) {
	t.Parallel()

	store := newMemorySnapshotStore()
	key := normalizeSnapshotKey(ReorientSnapshotKey{
		TranscriptPath: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	})
	store.snapshots[key] = "snapshot body"

	var output strings.Builder
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project",
			"session_id": "session-1"
		}`),
		Output:        &output,
		SnapshotStore: store,
	}

	if err := runner.Run(context.Background(), HookIDClaudeCodeReorientAfterCompact); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.String() != "snapshot body" {
		t.Fatalf("output = %q, want snapshot body", output.String())
	}
	if len(store.snapshots) != 0 {
		t.Fatalf("snapshots len = %d, want 0", len(store.snapshots))
	}
}

func TestRunnerReturnsDaemonErrorForPreCompactCapture(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("daemon unavailable")
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "PreCompact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output:        &strings.Builder{},
		SnapshotStore: newMemorySnapshotStore(),
		Reorient: func(
			context.Context,
			string,
			string,
			string,
			string,
			int,
			int,
			int,
			bool,
		) (conversation.ReorientPage, error) {
			return conversation.ReorientPage{}, expectedErr
		},
	}

	err := runner.Run(context.Background(), HookIDReorientBeforeCompact)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("Run error = %v, want %v", err, expectedErr)
	}
}
