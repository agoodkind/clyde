package main

import (
	"bytes"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/gklog/version"
)

func TestRootNoArgsShowsHelp(t *testing.T) {
	factory, stdout, _ := testFactory()
	root := newRoot(factory)
	// An explicit empty slice runs the root with no args; SetArgs(nil) would make
	// cobra fall back to os.Args, which carries the test runner's -test.* flags.
	root.SetArgs([]string{})

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
		{"conversation", "search"},
		{"conversation", "export"},
	}
	for _, path := range expected {
		if _, _, err := root.Find(path); err != nil {
			t.Fatalf("conversation command %v not registered: %v", path, err)
		}
	}
}

func TestRootRegistersOperationalPackages(t *testing.T) {
	factory, _, _ := testFactory()
	root := newRoot(factory)
	expected := [][]string{
		{"daemon", "run"},
		{"daemon", "fingerprint"},
		{"mcp", "serve"},
		{"mitm", "baseline", "seed"},
	}
	for _, path := range expected {
		if _, _, err := root.Find(path); err != nil {
			t.Fatalf("operational command %v not registered: %v", path, err)
		}
	}
}

func TestRootUsesStampedGklogVersion(t *testing.T) {
	factory, _, _ := testFactory()
	root := newRoot(factory)

	if root.Version != version.Version {
		t.Fatalf("root version = %q, want stamped gklog version %q", root.Version, version.Version)
	}
}

func TestRootHelpIncludesRegisteredConversationProviders(t *testing.T) {
	factory, _, _ := testFactory()
	root := newRoot(factory)
	help := root.Short + "\n" + root.Long
	for _, provider := range daemon.ConversationProviders() {
		label := provider.String()
		displayLabel := strings.ToUpper(label[:1]) + label[1:]
		if !strings.Contains(help, displayLabel) {
			t.Errorf("root help missing %s: %q", displayLabel, help)
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
