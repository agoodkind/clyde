package main

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// walkCommands visits root and every descendant command except the
// cobra-injected help and completion commands, which carry no Clyde help text.
func walkCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	if cmd.Name() == "help" || cmd.Name() == "completion" {
		return
	}
	visit(cmd)
	for _, child := range cmd.Commands() {
		walkCommands(child, visit)
	}
}

// TestEveryCommandHasFullHelp enforces the completeness invariant: every command
// in the tree declares Short, Long, and Example. This guard keeps a future
// refactor from silently dropping help on any command.
func TestEveryCommandHasFullHelp(t *testing.T) {
	factory, _, _ := testFactory()
	root := newRoot(factory)
	walkCommands(root, func(cmd *cobra.Command) {
		path := cmd.CommandPath()
		if strings.TrimSpace(cmd.Short) == "" {
			t.Errorf("%s: missing Short", path)
		}
		if strings.TrimSpace(cmd.Long) == "" {
			t.Errorf("%s: missing Long", path)
		}
		if strings.TrimSpace(cmd.Example) == "" {
			t.Errorf("%s: missing Example", path)
		}
	})
}

// TestEveryParentRejectsUnknownSubcommand enforces that every command with
// subcommands validates arguments so an unknown subcommand fails and renders
// full help rather than silently succeeding.
func TestEveryParentRejectsUnknownSubcommand(t *testing.T) {
	factory, _, _ := testFactory()
	root := newRoot(factory)
	walkCommands(root, func(cmd *cobra.Command) {
		if !cmd.HasSubCommands() {
			return
		}
		if len(strings.Fields(cmd.Use)) > 1 {
			return
		}
		if cmd.Args == nil {
			t.Errorf("%s: parent has no argument validator, so unknown subcommands are not rejected", cmd.CommandPath())
			return
		}
		if err := cmd.Args(cmd, []string{"definitely-not-a-subcommand"}); err == nil {
			t.Errorf("%s: parent accepts an unknown subcommand instead of erroring", cmd.CommandPath())
		}
	})
}

// TestMisuseRendersFullHelp drives the misuse classes end to end and asserts
// each renders the offending command's full help, not a bare error line.
func TestMisuseRendersFullHelp(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantHelp string
	}{
		{
			name:     "missing required arg on a leaf",
			args:     []string{"conversation", "export"},
			wantHelp: "Export one conversation transcript",
		},
		{
			name:     "unknown subcommand under a generated parent",
			args:     []string{"conversation", "bogus"},
			wantHelp: "Inspect indexed Claude and Codex conversations",
		},
		{
			name:     "unknown subcommand under a handwritten parent",
			args:     []string{"logs", "bogus"},
			wantHelp: "Inspect Clyde log metadata",
		},
		{
			name:     "unknown flag on a leaf",
			args:     []string{"conversation", "export", "claude:abc", "--nope"},
			wantHelp: "Export one conversation transcript",
		},
		{
			// The headline case: a valid id but no content selection is rejected
			// in Prepare (PreRunE), so full help renders rather than a bare error.
			name:     "export with no content kinds selected",
			args:     []string{"conversation", "export", "claude:abc"},
			wantHelp: "Export one conversation transcript",
		},
		{
			name:     "unsupported provider value",
			args:     []string{"conversation", "search", "--provider", "bogus"},
			wantHelp: "One operation over indexed Claude and Codex conversations",
		},
		{
			name:     "unparseable search time bound",
			args:     []string{"conversation", "search", "--query", "auth", "--after", "nope"},
			wantHelp: "One operation over indexed Claude and Codex conversations",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			factory, stdout, _ := testFactory()
			root := newRoot(factory)
			root.SetContext(context.Background())
			root.SetArgs(tc.args)
			err := root.Execute()
			if err == nil {
				t.Fatalf("expected an error for %q", strings.Join(tc.args, " "))
			}
			rendered := stdout.String()
			if !strings.Contains(rendered, tc.wantHelp) {
				t.Errorf("help body missing %q; got:\n%s", tc.wantHelp, rendered)
			}
			if !strings.Contains(rendered, "Usage:") {
				t.Errorf("usage section missing; got:\n%s", rendered)
			}
		})
	}
}
