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

func TestRunCommandDispatchesHookRuntime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	input := strings.NewReader(`{
		"hook_event_name": "PreCompact",
		"transcript_path": "/tmp/session.jsonl",
		"cwd": "/tmp/project"
	}`)
	var output bytes.Buffer
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

	cmd := NewCmdWithReorient(factory, reorient)
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

func TestRunCommandRejectsLegacyHookIDSyntax(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  strings.NewReader(`{}`),
			Out: &bytes.Buffer{},
			Err: &bytes.Buffer{},
		},
	}
	cmd := NewCmdWithReorient(factory, nil)
	cmd.SetArgs([]string{"run", string(hookspec.HookIDReorientBeforeCompact)})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want legacy syntax rejection")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("Execute() error = %q, want unknown command", err)
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
