package clispec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

func TestUninstallRequiresApplyAndDefaultsToRegistrations(t *testing.T) {
	t.Parallel()
	calls := []string{}
	dependency := func(name string) func(context.Context, ResultSink) error {
		return func(context.Context, ResultSink) error {
			calls = append(calls, name)
			return nil
		}
	}
	op := uninstallOpWithDependencies(uninstallDependencies{
		uninstallDaemon: dependency("daemon"),
		uninstallHooks:  dependency("hooks"),
		uninstallMCP:    dependency("mcp"),
		uninstallBinary: dependency("binary"),
	})
	if _, err := op.Prepare(uninstallInput{}); err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("Prepare without apply = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("no-apply calls = %v", calls)
	}
	payload, err := op.Prepare(uninstallInput{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Run(context.Background(), payload, SurfaceCLI, NewCLISink(context.Background(), &bytes.Buffer{}, &bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	if want := []string{"daemon", "hooks", "mcp"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("default calls = %v, want %v", calls, want)
	}
}

func TestUninstallSelectorRestrictsRemoval(t *testing.T) {
	t.Parallel()
	calls := []string{}
	dependency := func(name string) func(context.Context, ResultSink) error {
		return func(context.Context, ResultSink) error {
			calls = append(calls, name)
			return nil
		}
	}
	op := uninstallOpWithDependencies(uninstallDependencies{
		uninstallDaemon: dependency("daemon"),
		uninstallHooks:  dependency("hooks"),
		uninstallMCP:    dependency("mcp"),
		uninstallBinary: dependency("binary"),
	})
	payload, err := op.Prepare(uninstallInput{Apply: true, Binary: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Run(context.Background(), payload, SurfaceCLI, NewCLISink(context.Background(), &bytes.Buffer{}, &bytes.Buffer{})); err != nil {
		t.Fatal(err)
	}
	if want := []string{"binary"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("binary calls = %v, want %v", calls, want)
	}
}

func TestUninstallCommandHelpDocumentsPreservedData(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	command := uninstallOp().cobraCommand(testFactory(&output))
	for _, want := range []string{"configuration", "cache", "state", "logs", "exports", "credentials", "provider data", "repositories", "LMS", "binary", "--apply", "--daemon", "--hooks", "--mcp", "--binary"} {
		if !strings.Contains(command.Long+command.Flags().FlagUsages(), want) {
			t.Errorf("uninstall help missing %q", want)
		}
	}
}

func TestUninstallWithoutApplyPrintsHelpAndDoesNotMutate(t *testing.T) {
	t.Parallel()
	called := false
	dependency := func(context.Context, ResultSink) error {
		called = true
		return nil
	}
	var output bytes.Buffer
	factory := testFactory(&output)
	root := &cobra.Command{Use: "clyde"}
	root.SetOut(&output)
	root.SetErr(&output)
	root.AddCommand(uninstallOpWithDependencies(uninstallDependencies{
		uninstallDaemon: dependency,
		uninstallHooks:  dependency,
		uninstallMCP:    dependency,
		uninstallBinary: dependency,
	}).cobraCommand(factory))
	cli.InstallHelpRendering(root)
	root.SetArgs([]string{"uninstall"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--apply") {
		t.Fatalf("Execute without apply = %v", err)
	}
	if called {
		t.Fatal("uninstall without apply mutated a component")
	}
	if rendered := output.String(); !strings.Contains(rendered, "Remove Clyde's daemon service") || !strings.Contains(rendered, "Usage:") {
		t.Fatalf("uninstall without apply did not print help:\n%s", rendered)
	}
}

func TestUninstallBinaryRemovesSelectedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "clyde")
	if err := os.WriteFile(path, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := uninstallBinaryAt(context.Background(), NewCLISink(context.Background(), &output, &bytes.Buffer{}), path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("binary remains: %v", err)
	}
	if !strings.Contains(output.String(), path) {
		t.Fatalf("binary removal output missing path: %q", output.String())
	}
}
