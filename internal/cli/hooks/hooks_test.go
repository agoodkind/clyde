package hooks

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/hookspec"
)

func TestRunCommandDispatchesBeforeCompactRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	input := strings.NewReader(`{
		"hook_event_name": "PreCompact",
		"transcript_path": "/tmp/session.jsonl",
		"cwd": "/tmp/project"
	}`)
	var output bytes.Buffer
	store := hookspec.NewFileSnapshotStore(t.TempDir())
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  input,
			Out: &output,
			Err: &bytes.Buffer{},
		},
	}
	calls := make([]string, 0, 1)
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
		calls = append(calls, conversationID+"|"+workspace+"|"+cursor+"|"+boolString(syntheticPreCompact))
		return conversation.ReorientPage{
			Body:      "hook body",
			Remaining: 0,
			Offset:    0,
		}, nil
	}

	cmd := newCmdWithDeps(factory, reorient, store, nil)
	cmd.SetArgs([]string{"run", "reorient", "before-compact"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	expectedCalls := []string{"/tmp/session.jsonl|/tmp/project||true"}
	if !slices.Equal(calls, expectedCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, expectedCalls)
	}
	if output.String() != "" {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestRunCommandDispatchesAfterCompactRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := hookspec.NewFileSnapshotStore(t.TempDir())

	payloadBefore := `{
		"hook_event_name": "PreCompact",
		"transcript_path": "/tmp/session.jsonl",
		"cwd": "/tmp/project",
		"session_id": "session-1"
	}`
	payloadAfter := `{
		"hook_event_name": "SessionStart",
		"source": "compact",
		"transcript_path": "/tmp/session.jsonl",
		"cwd": "/tmp/project",
		"session_id": "session-1"
	}`
	calls := make([]string, 0, 1)
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
		calls = append(calls, conversationID+"|"+workspace+"|"+cursor+"|"+boolString(syntheticPreCompact))
		return conversation.ReorientPage{
			Body:      "hook body",
			Remaining: 0,
			Offset:    0,
		}, nil
	}

	if _, err := runHookCommand(t, []string{"run", "reorient", "before-compact"}, payloadBefore, reorient, store, nil); err != nil {
		t.Fatalf("before-compact Execute: %v", err)
	}
	output, err := runHookCommand(t, []string{"run", "reorient", "after-compact"}, payloadAfter, reorient, store, nil)
	if err != nil {
		t.Fatalf("after-compact Execute: %v", err)
	}

	expectedCalls := []string{"/tmp/session.jsonl|/tmp/project||true"}
	if !slices.Equal(calls, expectedCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, expectedCalls)
	}
	if output != "" {
		t.Fatalf("after-compact output = %q, want empty; the hook must not instruct the model", output)
	}
}

func TestRunCommandDispatchesStopFollowupRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store := hookspec.NewFileSnapshotStore(t.TempDir())

	payloadStop := `{
		"hook_event_name": "stop",
		"transcript_path": "/tmp/cursor.jsonl",
		"conversation_id": "cursor-conversation",
		"session_id": "session-1",
		"workspace_roots": ["/tmp/project"]
	}`
	err := store.Save(context.Background(), hookspec.ReorientSnapshotKey{
		TranscriptPath: "/tmp/cursor.jsonl",
		ConversationID: "/tmp/cursor.jsonl",
		SessionID:      "session-1",
		CWD:            "/tmp/project",
	}, "cursor snapshot body")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	output, err := runHookCommand(
		t,
		[]string{"run", "reorient", "stop-followup"},
		payloadStop,
		nil,
		store,
		func(key string) string {
			if key == "CURSOR_VERSION" {
				return "1.2.3"
			}
			return ""
		},
	)
	if err != nil {
		t.Fatalf("stop-followup Execute: %v", err)
	}
	if output != "" {
		t.Fatalf("stop-followup output = %q, want empty; Cursor must receive no follow-up message", output)
	}
}

func TestRunCommandRejectsLegacyCommands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cases := [][]string{
		{"run", string(hookspec.HookIDReorientBeforeCompact)},
		{"run", string(hookspec.HookIDReorientAfterCompact)},
		{"run", string(hookspec.HookIDReorientStopFollowup)},
		{"run", string(hookspec.HookIDClaudeCodeReorientAfterCompact)},
		{"run", "reorient-before-compact"},
		{"run", "reorient-after-compact"},
		{"run", "reorient-stop-followup"},
	}
	for _, args := range cases {
		factory := &cli.Factory{
			IOStreams: &cli.IOStreams{
				In:  strings.NewReader(`{}`),
				Out: &bytes.Buffer{},
				Err: &bytes.Buffer{},
			},
		}
		cmd := newCmdWithDeps(factory, nil, nil, nil)
		cmd.SetArgs(args)

		err := cmd.Execute()
		if err == nil {
			t.Fatalf("Execute(%v) error = nil, want legacy syntax rejection", args)
		}
		if !strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("Execute(%v) error = %q, want unknown command", args, err)
		}
	}
}

func runHookCommand(
	t *testing.T,
	args []string,
	input string,
	reorient hookspec.ReorientFunc,
	snapshotStore hookspec.SnapshotStore,
	getenv func(string) string,
) (string, error) {
	t.Helper()

	var output bytes.Buffer
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  strings.NewReader(input),
			Out: &output,
			Err: &bytes.Buffer{},
		},
	}
	cmd := newCmdWithDeps(factory, reorient, snapshotStore, getenv)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return output.String(), err
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
