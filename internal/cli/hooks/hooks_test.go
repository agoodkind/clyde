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
		_ string,
		cursor string,
		_ int,
		_ int,
		_ int,
		syntheticPreCompact bool,
	) (conversation.ReorientPage, error) {
		calls = append(calls, conversationID+"|"+workspace+"|"+cursor+"|"+boolString(syntheticPreCompact))
		return conversation.ReorientPage{
			Items:      []conversation.ReorientItem{{Title: "Hook", Body: "body"}},
			Remaining:  0,
			Offset:     0,
			TotalItems: 1,
		}, nil
	}

	cmd := NewCmdWithReorient(factory, reorient)
	cmd.SetArgs([]string{"run", string(hookspec.HookIDReorientBeforeCompact)})
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

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
