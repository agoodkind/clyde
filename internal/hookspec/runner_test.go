package hookspec

import (
	"context"
	"encoding/json"
	"errors"
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

func TestHookInputDetectRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       hookInput
		environment map[string]string
		want        Client
	}{
		{
			name: "codex thread env wins over cursor payload",
			input: hookInput{
				ComposerID: "composer-1",
			},
			environment: map[string]string{
				"CODEX_THREAD_ID": "thread-1",
			},
			want: ClientCodex,
		},
		{
			name: "cursor version env wins over payload fallback",
			input: hookInput{
				PermissionMode: "default",
			},
			environment: map[string]string{
				"CURSOR_VERSION": "1.2.3",
			},
			want: ClientCursor,
		},
		{
			name: "claude entrypoint env gives claude",
			environment: map[string]string{
				"CLAUDE_CODE_ENTRYPOINT": "cli",
			},
			want: ClientClaudeCode,
		},
		{
			name: "claude ai agent prefix gives claude",
			environment: map[string]string{
				"AI_AGENT": "claude-code/2.1/agent",
			},
			want: ClientClaudeCode,
		},
		{
			name: "empty env falls back to cursor payload",
			input: hookInput{
				ComposerID: "composer-1",
			},
			want: ClientCursor,
		},
		{
			name: "empty env falls back to codex payload",
			input: hookInput{
				PermissionMode: "default",
			},
			want: ClientCodex,
		},
		{
			name: "empty env falls back to claude payload",
			input: hookInput{
				HookEventName: EventSessionStart,
			},
			want: ClientClaudeCode,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			getenv := func(key string) string {
				return testCase.environment[key]
			}
			got := testCase.input.detectRuntime(getenv)
			if got != testCase.want {
				t.Fatalf("detectRuntime() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func clearRuntimeDetectionEnvironment(t *testing.T) {
	t.Helper()

	for _, key := range []string{
		"CODEX_THREAD_ID",
		"CODEX_CI",
		"CURSOR_VERSION",
		"CURSOR_WORKSPACE_NAME",
		"CURSOR_MODE",
		"CLAUDE_CODE_ENTRYPOINT",
		"AI_AGENT",
	} {
		t.Setenv(key, "")
	}
}

func TestRunnerPreCompactStoresSyntheticBoundarySnapshot(t *testing.T) {
	t.Parallel()

	store := newMemorySnapshotStore()
	calls := make([]testReorientCall, 0, 2)
	pages := []conversation.ReorientPage{
		{
			Body:       "first page one",
			NextCursor: "cursor-two",
			Remaining:  15,
			Offset:     0,
			TotalBytes: 29,
			TotalLines: 1,
		},
		{
			Body:       "second page two",
			NextCursor: "",
			Remaining:  0,
			Offset:     14,
			TotalBytes: 29,
			TotalLines: 1,
		},
	}
	reorient := func(
		_ context.Context,
		conversationID string,
		workspace string,
		cursor string,
		_ int,
		_ int,
		_ bool,
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
	if !strings.Contains(body, "first page one") || !strings.Contains(body, "second page two") {
		t.Fatalf("snapshot missing pages:\n%s", body)
	}
}

func TestRunnerAfterCompactConsumesClaudeSnapshot(t *testing.T) {
	clearRuntimeDetectionEnvironment(t)

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
	clearRuntimeDetectionEnvironment(t)

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
		int,
		int,
		bool,
		bool,
	) (conversation.ReorientPage, error) {
		return conversation.ReorientPage{
			Body:       "cursor snapshot body",
			Remaining:  0,
			TotalBytes: 20,
			TotalLines: 1,
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
	if !strings.Contains(decoded.FollowupMessage, "cursor snapshot body") {
		t.Fatalf("followup message missing snapshot:\n%s", decoded.FollowupMessage)
	}
}

func TestRunnerIgnoresNonCompactSessionStart(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	store := newMemorySnapshotStore()
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "startup",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output:        &output,
		SnapshotStore: store,
	}

	err := runner.Run(context.Background(), HookIDReorientAfterCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.String() != "" {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestRunnerCursorStopWithNoSnapshotReturnsNoOutput(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "stop",
			"conversation_id": "cursor-conv",
			"session_id": "session-1"
		}`),
		Output:        &output,
		SnapshotStore: newMemorySnapshotStore(),
	}
	err := runner.Run(context.Background(), HookIDReorientStopFollowup)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if output.String() != "" {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestRunnerAfterCompactMissingSnapshotErrors(t *testing.T) {
	t.Parallel()

	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output:        &strings.Builder{},
		SnapshotStore: newMemorySnapshotStore(),
	}
	err := runner.Run(context.Background(), HookIDReorientAfterCompact)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !strings.Contains(err.Error(), "snapshot not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerPreCompactErrorsWhenReorientCursorStalls(t *testing.T) {
	t.Parallel()

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
			int,
			int,
			bool,
			bool,
		) (conversation.ReorientPage, error) {
			return conversation.ReorientPage{Remaining: 1, NextCursor: ""}, nil
		},
	}

	err := runner.Run(context.Background(), HookIDReorientBeforeCompact)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !strings.Contains(err.Error(), "remaining 1 but next cursor is empty") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerPreCompactErrorsWhenReorientCursorRepeats(t *testing.T) {
	t.Parallel()

	callCount := 0
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
			int,
			int,
			bool,
			bool,
		) (conversation.ReorientPage, error) {
			callCount++
			if callCount == 1 {
				return conversation.ReorientPage{Remaining: 1, NextCursor: "cursor-1"}, nil
			}
			return conversation.ReorientPage{Remaining: 1, NextCursor: "cursor-1"}, nil
		},
	}

	err := runner.Run(context.Background(), HookIDReorientBeforeCompact)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !strings.Contains(err.Error(), "repeated next cursor") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerPreCompactReturnsDaemonErrors(t *testing.T) {
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
			int,
			int,
			bool,
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

func TestRunnerPreCompactFallsBackToWorkspaceWhenTranscriptNotFound(t *testing.T) {
	t.Parallel()

	calls := make([]testReorientCall, 0, 2)
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "PreCompact",
			"transcript_path": "/tmp/missing-session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output:        &strings.Builder{},
		SnapshotStore: newMemorySnapshotStore(),
		Reorient: func(
			_ context.Context,
			conversationID string,
			workspace string,
			cursor string,
			_ int,
			_ int,
			_ bool,
			syntheticPreCompact bool,
		) (conversation.ReorientPage, error) {
			calls = append(calls, testReorientCall{
				ConversationID:      conversationID,
				Workspace:           workspace,
				Cursor:              cursor,
				SyntheticPreCompact: syntheticPreCompact,
			})
			if len(calls) == 1 {
				return conversation.ReorientPage{}, errors.New("conversation not found")
			}
			return conversation.ReorientPage{Remaining: 0}, nil
		},
	}

	err := runner.Run(context.Background(), HookIDReorientBeforeCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	expectedCalls := []testReorientCall{
		{
			ConversationID:      "/tmp/missing-session.jsonl",
			Workspace:           "/tmp/project",
			Cursor:              "",
			SyntheticPreCompact: true,
		},
		{
			ConversationID:      "",
			Workspace:           "/tmp/project",
			Cursor:              "",
			SyntheticPreCompact: true,
		},
	}
	if !slices.Equal(calls, expectedCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, expectedCalls)
	}
}

func TestRunnerPreCompactStopsWhenContextIsCanceledBetweenPages(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	callCount := 0
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
			int,
			int,
			bool,
			bool,
		) (conversation.ReorientPage, error) {
			callCount++
			cancel()
			return conversation.ReorientPage{Remaining: 1, NextCursor: "next"}, nil
		},
	}

	err := runner.Run(ctx, HookIDReorientBeforeCompact)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if callCount != 1 {
		t.Fatalf("reorient calls = %d, want 1", callCount)
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
