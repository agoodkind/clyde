package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type recordedDaemonCommand struct {
	Name string
	Args []string
}

type fakeDaemonCommandRunner struct {
	commands        []recordedDaemonCommand
	runErrors       map[string]error
	outputErrors    map[string]error
	combinedErrors  map[string]error
	outputs         map[string][]byte
	combinedOutputs map[string][]byte
}

func (runner *fakeDaemonCommandRunner) Run(name string, args ...string) error {
	runner.record(name, args)
	key := commandKey(name, args)
	if err, ok := runner.runErrors[key]; ok {
		delete(runner.runErrors, key)
		return err
	}
	return nil
}

func (runner *fakeDaemonCommandRunner) Output(name string, args ...string) ([]byte, error) {
	runner.record(name, args)
	key := commandKey(name, args)
	if err, ok := runner.outputErrors[key]; ok {
		delete(runner.outputErrors, key)
		return runner.outputs[key], err
	}
	return runner.outputs[key], nil
}

func (runner *fakeDaemonCommandRunner) CombinedOutput(name string, args ...string) ([]byte, error) {
	runner.record(name, args)
	key := commandKey(name, args)
	if err, ok := runner.combinedErrors[key]; ok {
		delete(runner.combinedErrors, key)
		return runner.combinedOutputs[key], err
	}
	return runner.combinedOutputs[key], nil
}

func (runner *fakeDaemonCommandRunner) record(name string, args []string) {
	copiedArgs := append([]string(nil), args...)
	runner.commands = append(runner.commands, recordedDaemonCommand{
		Name: name,
		Args: copiedArgs,
	})
}

func commandKey(name string, args []string) string {
	parts := append([]string{name}, args...)
	return strings.Join(parts, "\x00")
}

func withFakeDaemonRunner(t *testing.T, runner *fakeDaemonCommandRunner) {
	t.Helper()
	previousRunner := clientCommandRunner
	clientCommandRunner = runner
	t.Cleanup(func() {
		clientCommandRunner = previousRunner
	})
}

func TestStartDarwinLaunchdDaemonBootstrapsAndKickstarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := "gui/" + userIDForTest() + "/" + launchAgentLabel
	plistPath := launchAgentPathForTest()
	runner := &fakeDaemonCommandRunner{
		runErrors: map[string]error{
			commandKey("launchctl", []string{"kickstart", "-k", target}): errors.New("not loaded"),
		},
	}
	withFakeDaemonRunner(t, runner)

	err := startDarwinLaunchdDaemon(context.Background(), daemonClientLog(context.Background()))
	if err != nil {
		t.Fatalf("startDarwinLaunchdDaemon returned error: %v", err)
	}

	if len(runner.commands) != 3 {
		t.Fatalf("command count = %d, want 3: %#v", len(runner.commands), runner.commands)
	}
	assertCommand(t, runner.commands[0], "launchctl", "kickstart", "-k", target)
	assertCommand(t, runner.commands[1], "launchctl", "bootstrap", "gui/"+userIDForTest(), plistPath)
	assertCommand(t, runner.commands[2], "launchctl", "kickstart", "-k", target)
}

func TestStartDarwinLaunchdDaemonDoesNotDirectSpawnWhenLaunchctlFails(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("startDaemon only takes the launchd path on Darwin")
	}

	t.Setenv("HOME", t.TempDir())
	target := "gui/" + userIDForTest() + "/" + launchAgentLabel
	plistPath := launchAgentPathForTest()
	runner := &fakeDaemonCommandRunner{
		runErrors: map[string]error{
			commandKey("launchctl", []string{"kickstart", "-k", target}): errors.New("not loaded"),
		},
		combinedErrors: map[string]error{
			commandKey("launchctl", []string{"bootstrap", "gui/" + userIDForTest(), plistPath}): errors.New("bootstrap failed"),
		},
		combinedOutputs: map[string][]byte{
			commandKey("launchctl", []string{"bootstrap", "gui/" + userIDForTest(), plistPath}): []byte("bootstrap denied"),
		},
	}
	withFakeDaemonRunner(t, runner)

	spawnCalled := false
	previousSpawn := spawnDaemonDirectForPlatform
	spawnDaemonDirectForPlatform = func() error {
		spawnCalled = true
		return nil
	}
	t.Cleanup(func() {
		spawnDaemonDirectForPlatform = previousSpawn
	})

	err := startDaemon(context.Background())
	if err == nil {
		t.Fatal("startDaemon returned nil, want launchctl error")
	}
	if spawnCalled {
		t.Fatal("startDaemon called direct spawn after launchctl failure")
	}
	if !strings.Contains(err.Error(), "bootstrap launch agent") {
		t.Fatalf("error = %q, want bootstrap launch agent context", err.Error())
	}
}

func TestStartDaemonKeepsNonDarwinDirectSpawn(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("non-Darwin direct spawn behavior is covered on non-Darwin builders")
	}

	spawnCalled := false
	previousSpawn := spawnDaemonDirectForPlatform
	spawnDaemonDirectForPlatform = func() error {
		spawnCalled = true
		return nil
	}
	t.Cleanup(func() {
		spawnDaemonDirectForPlatform = previousSpawn
	})

	if err := startDaemon(context.Background()); err != nil {
		t.Fatalf("startDaemon returned error: %v", err)
	}
	if !spawnCalled {
		t.Fatal("startDaemon did not call direct spawn on non-Darwin")
	}
}

func TestEnsureConnectedDaemonOwnedByLaunchdReportsUnownedDaemon(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd ownership check only runs on Darwin")
	}

	target := "gui/" + userIDForTest() + "/" + launchAgentLabel
	runner := &fakeDaemonCommandRunner{
		runErrors: map[string]error{
			commandKey("launchctl", []string{"print", target}): errors.New("not loaded"),
		},
	}
	withFakeDaemonRunner(t, runner)

	err := ensureConnectedDaemonOwnedByLaunchd(context.Background())
	if err == nil {
		t.Fatal("ensureConnectedDaemonOwnedByLaunchd returned nil, want unowned error")
	}
	if !strings.Contains(err.Error(), "daemon is reachable but launchd does not own") {
		t.Fatalf("error = %q, want unowned daemon diagnostic", err.Error())
	}
}

func assertCommand(t *testing.T, got recordedDaemonCommand, name string, args ...string) {
	t.Helper()
	if got.Name != name {
		t.Fatalf("command name = %q, want %q", got.Name, name)
	}
	if len(got.Args) != len(args) {
		t.Fatalf("command args = %#v, want %#v", got.Args, args)
	}
	for i, want := range args {
		if got.Args[i] != want {
			t.Fatalf("command args = %#v, want %#v", got.Args, args)
		}
	}
}

func userIDForTest() string {
	return strconv.Itoa(os.Getuid())
}

func launchAgentPathForTest() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}
