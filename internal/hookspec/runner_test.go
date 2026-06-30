package hookspec

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

type testReorientCall struct {
	ConversationID      string
	Workspace           string
	Cursor              string
	SyntheticPreCompact bool
}

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

func TestRunnerPreCompactStoresSyntheticBoundarySnapshot(t *testing.T) {
	t.Parallel()

	store := newMemorySnapshotStore()
	calls := make([]testReorientCall, 0, 2)
	pages := []conversation.ReorientPage{
		{
			Items:      []conversation.ReorientItem{{Title: "First", Body: "one"}},
			NextCursor: "cursor-two",
			Remaining:  1,
			Offset:     0,
			TotalItems: 2,
		},
		{
			Items:      []conversation.ReorientItem{{Title: "Second", Body: "two"}},
			NextCursor: "",
			Remaining:  0,
			Offset:     1,
			TotalItems: 2,
		},
	}
	reorient := func(
		_ context.Context,
		conversationID string,
		workspace string,
		_ string,
		cursor string,
		_ int,
		_ int,
		_ int,
		syntheticPreCompact bool,
	) (conversation.ReorientPage, error) {
		calls = append(calls, testReorientCall{
			ConversationID:      conversationID,
			Workspace:           workspace,
			Cursor:              cursor,
			SyntheticPreCompact: syntheticPreCompact,
		})
		return pages[len(calls)-1], nil
	}

	var output strings.Builder
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "PreCompact",
			"transcript_path": "/Users/me/.claude/projects/proj/session.jsonl",
			"cwd": "/Users/me/project",
			"session_id": "session-1"
		}`),
		Output:        &output,
		Reorient:      reorient,
		SnapshotStore: store,
	}
	err := runner.Run(context.Background(), HookIDReorientBeforeCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	expectedCalls := []testReorientCall{
		{
			ConversationID:      "/Users/me/.claude/projects/proj/session.jsonl",
			Workspace:           "/Users/me/project",
			Cursor:              "",
			SyntheticPreCompact: true,
		},
		{
			ConversationID:      "/Users/me/.claude/projects/proj/session.jsonl",
			Workspace:           "/Users/me/project",
			Cursor:              "cursor-two",
			SyntheticPreCompact: true,
		},
	}
	if !slices.Equal(calls, expectedCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, expectedCalls)
	}
	if output.String() != "" {
		t.Fatalf("pre-compact output = %q, want empty", output.String())
	}
	body := storedSnapshot(t, store, ReorientSnapshotKey{
		TranscriptPath: "/Users/me/.claude/projects/proj/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/Users/me/project",
	})
	if !strings.Contains(body, "## First") || !strings.Contains(body, "## Second") {
		t.Fatalf("snapshot missing pages:\n%s", body)
	}
}

func TestRunnerAfterCompactConsumesClaudeSnapshot(t *testing.T) {
	t.Parallel()

	store := newMemorySnapshotStore()
	key := ReorientSnapshotKey{
		TranscriptPath: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	}
	if err := store.Save(context.Background(), key, "snapshot body"); err != nil {
		t.Fatalf("Save: %v", err)
	}

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
	err := runner.Run(context.Background(), HookIDClaudeCodeReorientAfterCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.String() != "snapshot body" {
		t.Fatalf("output = %q, want snapshot body", output.String())
	}
	if _, ok := store.snapshots[normalizeSnapshotKey(key)]; ok {
		t.Fatal("snapshot was not consumed")
	}
}

func TestRunnerAfterCompactWritesCodexAdditionalContext(t *testing.T) {
	t.Parallel()

	store := newMemorySnapshotStore()
	key := ReorientSnapshotKey{
		TranscriptPath: "/tmp/session.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	}
	if err := store.Save(context.Background(), key, "snapshot body"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var output strings.Builder
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project",
			"session_id": "session-1",
			"model": "gpt-5.4",
			"permission_mode": "default"
		}`),
		Output:        &output,
		SnapshotStore: store,
	}
	err := runner.Run(context.Background(), HookIDReorientAfterCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var decoded hookSpecificOutputEnvelope
	if err := json.Unmarshal([]byte(output.String()), &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, output.String())
	}
	if decoded.HookSpecificOutput.HookEventName != EventSessionStart {
		t.Fatalf("hook event = %q", decoded.HookSpecificOutput.HookEventName)
	}
	if decoded.HookSpecificOutput.AdditionalContext != "snapshot body" {
		t.Fatalf("additional context = %q", decoded.HookSpecificOutput.AdditionalContext)
	}
}

func TestRunnerCursorPreCompactStoresAndStopReturnsFollowup(t *testing.T) {
	t.Parallel()

	store := newMemorySnapshotStore()
	reorient := func(
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
			Items:      []conversation.ReorientItem{{Title: "Cursor", Body: "snapshot"}},
			Remaining:  0,
			TotalItems: 1,
		}, nil
	}
	pre := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "preCompact",
			"conversation_id": "cursor-conv",
			"session_id": "session-1",
			"transcript_path": "/tmp/cursor.jsonl",
			"workspace_roots": ["/tmp/project"]
		}`),
		Reorient:      reorient,
		SnapshotStore: store,
	}
	if err := pre.Run(context.Background(), HookIDReorientBeforeCompact); err != nil {
		t.Fatalf("pre Run: %v", err)
	}

	var output strings.Builder
	stop := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "stop",
			"conversation_id": "cursor-conv",
			"session_id": "session-1",
			"transcript_path": "/tmp/cursor.jsonl",
			"workspace_roots": ["/tmp/project"]
		}`),
		Output:        &output,
		SnapshotStore: store,
	}
	if err := stop.Run(context.Background(), HookIDReorientStopFollowup); err != nil {
		t.Fatalf("stop Run: %v", err)
	}
	var decoded cursorFollowupOutput
	if err := json.Unmarshal([]byte(output.String()), &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, output.String())
	}
	if !strings.Contains(decoded.FollowupMessage, "## Cursor") {
		t.Fatalf("followup message missing snapshot:\n%s", decoded.FollowupMessage)
	}
}

func storedSnapshot(t *testing.T, store *memorySnapshotStore, key ReorientSnapshotKey) string {
	t.Helper()

	body, ok := store.snapshots[normalizeSnapshotKey(key)]
	if !ok {
		t.Fatalf("missing snapshot for %#v", key)
	}
	return body
}
