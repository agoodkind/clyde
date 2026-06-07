package main

import (
	"strings"
	"testing"
)

// TestRootListsAllTopLevelCommands asserts `clyde --help` lists every top-level
// command and group, with nothing hidden.
func TestRootListsAllTopLevelCommands(t *testing.T) {
	factory, stdout, _ := testFactory()
	root := newRoot(factory)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("root help: %v", err)
	}
	out := stdout.String()
	for _, name := range []string{"conversation", "daemon", "logs", "mitm", "mcp"} {
		if !strings.Contains(out, name) {
			t.Errorf("root help missing %q:\n%s", name, out)
		}
	}
}

// TestGroupNameListsSubcommands asserts a group name alone lists the commands
// beneath it.
func TestGroupNameListsSubcommands(t *testing.T) {
	factory, stdout, _ := testFactory()
	root := newRoot(factory)
	root.SetArgs([]string{"conversation", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("conversation help: %v", err)
	}
	out := stdout.String()
	for _, name := range []string{"list", "show", "context", "search", "export"} {
		if !strings.Contains(out, name) {
			t.Errorf("conversation help missing subcommand %q:\n%s", name, out)
		}
	}
}

func TestSearchGroupNameListsSubcommands(t *testing.T) {
	factory, stdout, _ := testFactory()
	root := newRoot(factory)
	root.SetArgs([]string{"conversation", "search", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("conversation search help: %v", err)
	}
	out := stdout.String()
	for _, name := range []string{"across", "within", "status", "cancel", "analyze"} {
		if !strings.Contains(out, name) {
			t.Errorf("conversation search help missing subcommand %q:\n%s", name, out)
		}
	}
}

// TestLeafHelpShowsArgumentsExamplesAndEnum asserts a leaf command's help shows
// an arguments section, an example, and a constrained flag's accepted values.
func TestLeafHelpShowsArgumentsExamplesAndEnum(t *testing.T) {
	factory, stdout, _ := testFactory()
	root := newRoot(factory)
	root.SetArgs([]string{"conversation", "export", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("export help: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"Arguments:",
		"CONVERSATION_ID",
		"Examples:",
		"clyde conversation export",
		"markdown|html|json|plain_text",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("export help missing %q:\n%s", want, out)
		}
	}
}

// TestOperationalLeafHelpShowsExample asserts a migrated operational leaf
// command prints an example.
func TestOperationalLeafHelpShowsExample(t *testing.T) {
	factory, stdout, _ := testFactory()
	root := newRoot(factory)
	root.SetArgs([]string{"mitm", "status", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("mitm status help: %v", err)
	}
	if out := stdout.String(); !strings.Contains(out, "clyde mitm status") {
		t.Errorf("mitm status help missing example:\n%s", out)
	}
}
