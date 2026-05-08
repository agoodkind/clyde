package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/cli"
	daemonsvc "goodkind.io/clyde/internal/daemon"
)

func TestDaemonStatusDoesNotRunSupervisor(t *testing.T) {
	command, output := newTestDaemonCommand(t)
	runCalled := false
	inspectCalled := false
	runCommand = func(_ *slog.Logger, _ ...daemonsvc.ExtraLoop) error {
		runCalled = true
		return errors.New("run command should not be called")
	}
	inspectDaemonStatus = func(_ context.Context) daemonsvc.StatusReport {
		inspectCalled = true
		return daemonsvc.StatusReport{
			LaunchdTarget:          "gui/501/io.goodkind.clyde.daemon",
			SupervisorPID:          56664,
			DaemonSocketPath:       "/tmp/clyde-daemon.sock",
			DaemonResponding:       true,
			SupervisorSocketPath:   "/tmp/clyde-supervisor.sock",
			SupervisorSocketExists: true,
			WorkerPIDs:             []int{56680},
		}
	}

	err := executeDaemonCommand(command, "status")
	if err != nil {
		t.Fatalf("execute daemon status: %v", err)
	}
	if runCalled {
		t.Fatal("daemon status called RunCommand")
	}
	if !inspectCalled {
		t.Fatal("daemon status did not inspect status")
	}
	if !strings.Contains(output.String(), "launchd: running target=gui/501/io.goodkind.clyde.daemon supervisor_pid=56664") {
		t.Fatalf("status output missing launchd state: %q", output.String())
	}
	if !strings.Contains(output.String(), "worker: pids=56680") {
		t.Fatalf("status output missing worker pid: %q", output.String())
	}
}

func TestUnknownDaemonArgumentDoesNotRunSupervisor(t *testing.T) {
	command, _ := newTestDaemonCommand(t)
	runCalled := false
	runCommand = func(_ *slog.Logger, _ ...daemonsvc.ExtraLoop) error {
		runCalled = true
		return nil
	}

	err := executeDaemonCommand(command, "unknown")
	if err == nil {
		t.Fatal("daemon unknown returned nil, want argument error")
	}
	if runCalled {
		t.Fatal("daemon unknown called RunCommand")
	}
}

func TestDaemonWorkerExtraArgumentDoesNotRunWorker(t *testing.T) {
	command, _ := newTestDaemonCommand(t)
	workerCalled := false
	runWorker = func(_ *slog.Logger, _ ...daemonsvc.ExtraLoop) error {
		workerCalled = true
		return nil
	}

	err := executeDaemonCommand(command, "worker", "extra")
	if err == nil {
		t.Fatal("daemon worker extra returned nil, want argument error")
	}
	if workerCalled {
		t.Fatal("daemon worker extra called Run")
	}
}

func TestDaemonReloadExtraArgumentDoesNotReload(t *testing.T) {
	command, _ := newTestDaemonCommand(t)
	reloadCalled := false
	reloadDaemon = func(_ context.Context) (*clydev1.ReloadDaemonResponse, error) {
		reloadCalled = true
		return &clydev1.ReloadDaemonResponse{}, nil
	}

	err := executeDaemonCommand(command, "reload", "extra")
	if err == nil {
		t.Fatal("daemon reload extra returned nil, want argument error")
	}
	if reloadCalled {
		t.Fatal("daemon reload extra called ReloadDaemon")
	}
}

func TestDaemonReloadRoutesNormally(t *testing.T) {
	command, output := newTestDaemonCommand(t)
	reloadCalled := false
	reloadDaemon = func(_ context.Context) (*clydev1.ReloadDaemonResponse, error) {
		reloadCalled = true
		return &clydev1.ReloadDaemonResponse{
			ActiveSessions: 2,
			BinaryReloaded: true,
			NewPid:         1234,
		}, nil
	}

	err := executeDaemonCommand(command, "reload")
	if err != nil {
		t.Fatalf("execute daemon reload: %v", err)
	}
	if !reloadCalled {
		t.Fatal("daemon reload did not call ReloadDaemon")
	}
	if !strings.Contains(output.String(), "daemon binary reloaded: active_sessions=2 new_pid=1234") {
		t.Fatalf("reload output = %q", output.String())
	}
}

func newTestDaemonCommand(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	restoreDaemonCommandFunctions(t)
	output := &bytes.Buffer{}
	return NewCmd(&cli.Factory{
		IOStreams: &cli.IOStreams{
			In:  &bytes.Buffer{},
			Out: output,
			Err: &bytes.Buffer{},
		},
		Build: cli.BuildInfo{Version: "test"},
	}), output
}

func restoreDaemonCommandFunctions(t *testing.T) {
	t.Helper()
	previousRunCommand := runCommand
	previousRunWorker := runWorker
	previousReloadDaemon := reloadDaemon
	previousInspectDaemonStatus := inspectDaemonStatus
	t.Cleanup(func() {
		runCommand = previousRunCommand
		runWorker = previousRunWorker
		reloadDaemon = previousReloadDaemon
		inspectDaemonStatus = previousInspectDaemonStatus
	})
}

func executeDaemonCommand(command *cobra.Command, args ...string) error {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	command.SetOut(out)
	command.SetErr(errOut)
	command.SetArgs(args)
	return command.Execute()
}
