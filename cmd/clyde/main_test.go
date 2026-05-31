package main

import (
	"bytes"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
)

func TestRootNoArgsShowsHelp(t *testing.T) {
	factory, stdout, _ := testFactory()
	root := newRoot(factory)
	root.SetArgs(nil)

	if err := root.Execute(); err != nil {
		t.Fatalf("execute root help: %v", err)
	}
	output := stdout.String()
	if !strings.Contains(output, "Usage:") {
		t.Fatalf("help output missing Usage: %q", output)
	}
	if !strings.Contains(output, "conversation") {
		t.Fatalf("help output missing conversation parent: %q", output)
	}
}

func TestRootUnknownCommandErrors(t *testing.T) {
	factory, _, _ := testFactory()
	root := newRoot(factory)
	root.SetArgs([]string{"definitely-not-a-clyde-command"})

	err := root.Execute()
	if err == nil {
		t.Fatalf("expected unknown command error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %q, want unknown command", err.Error())
	}
}

func TestRootRegistersConversationCommands(t *testing.T) {
	factory, _, _ := testFactory()
	root := newRoot(factory)
	expected := [][]string{
		{"conversation", "list"},
		{"conversation", "get"},
		{"conversation", "context"},
		{"conversation", "search"},
		{"conversation", "analyze"},
		{"conversation", "export"},
	}
	for _, path := range expected {
		if _, _, err := root.Find(path); err != nil {
			t.Fatalf("conversation command %v not registered: %v", path, err)
		}
	}
}

func testFactory() (*cli.Factory, *bytes.Buffer, *bytes.Buffer) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	return &cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  strings.NewReader(""),
			Out: stdout,
			Err: stderr,
		},
		Logger: nil,
		Build: cli.BuildInfo{
			Version: "test",
			Commit:  "",
			Date:    "",
		},
		Verbose: func() bool { return false },
		Config: func() (*config.Config, error) {
			return &config.Config{}, nil
		},
	}, stdout, stderr
}
