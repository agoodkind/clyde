package deploy

import (
	"bytes"
	"context"
	"reflect"
	"testing"
)

type recordingDeployRunner struct {
	*fakeRunner
	commands []command
}

func (runner *recordingDeployRunner) run(ctx context.Context, current command) commandResult {
	runner.commands = append(runner.commands, current)
	return runner.fakeRunner.run(ctx, current)
}

func TestStopServiceRetainsRegistration(t *testing.T) {
	t.Parallel()
	testControlService(t, serviceControlStop)
}

func TestDisableServiceRetainsRegistration(t *testing.T) {
	t.Parallel()
	testControlService(t, serviceControlDisable)
}

func testControlService(t *testing.T, control serviceControl) {
	t.Helper()
	for _, targetPlatform := range []platform{platformDarwin, platformLinux} {
		t.Run(string(targetPlatform), func(t *testing.T) {
			fixture := newDeployFixture(t, targetPlatform)
			registrationPath := fixture.config.LaunchdPlist
			if targetPlatform == platformLinux {
				registrationPath = fixture.config.SystemdUserUnit
			}
			registration := []byte("existing registration")
			fixture.fs.exists[registrationPath] = true
			fixture.fs.files[registrationPath] = registration

			commands, err := controlCommands(fixture.config, control)
			if err != nil {
				t.Fatal(err)
			}
			runner := &recordingDeployRunner{fakeRunner: newFakeRunner()}
			for _, current := range commands {
				runner.setSuccess(current, "")
			}
			executor := executor{
				config: fixture.config,
				files:  fixture.fs,
				runner: runner,
				logger: newLogger(&fixture.logs),
				stdout: &fixture.stdout,
				stderr: &fixture.stderr,
			}

			if err := executor.controlService(context.Background(), control); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(runner.commands, commands) {
				t.Fatalf("commands = %v, want %v", runner.commands, commands)
			}
			if !fixture.fs.exists[registrationPath] || !bytes.Equal(fixture.fs.files[registrationPath], registration) {
				t.Fatalf("registration %q changed", registrationPath)
			}
		})
	}
}

func TestDeployReenablesDisabledService(t *testing.T) {
	t.Parallel()
	for _, targetPlatform := range []platform{platformDarwin, platformLinux} {
		t.Run(string(targetPlatform), func(t *testing.T) {
			fixture := newDeployFixture(t, targetPlatform)
			runner := &recordingDeployRunner{fakeRunner: newFakeRunner()}
			disableCommands, err := controlCommands(fixture.config, serviceControlDisable)
			if err != nil {
				t.Fatal(err)
			}
			for _, current := range disableCommands {
				runner.setSuccess(current, "")
			}
			executor := executor{
				config: fixture.config,
				files:  fixture.fs,
				runner: runner,
				logger: newLogger(&fixture.logs),
				stdout: &fixture.stdout,
				stderr: &fixture.stderr,
			}
			if err := executor.controlService(context.Background(), serviceControlDisable); err != nil {
				t.Fatal(err)
			}

			var deployCommands []command
			if targetPlatform == platformDarwin {
				deployCommands = []command{
					{name: "launchctl", args: []string{"enable", executor.darwinTarget()}},
					{name: "launchctl", args: []string{"bootout", fixture.config.LaunchdDomain, fixture.config.LaunchdPlist}},
					{name: "launchctl", args: []string{"bootstrap", fixture.config.LaunchdDomain, fixture.config.LaunchdPlist}},
					{name: "launchctl", args: []string{"print", executor.darwinTarget()}},
				}
			} else {
				deployCommands = []command{
					{name: "systemctl", args: []string{"--user", "daemon-reload"}},
					{name: "systemctl", args: []string{"--user", "enable", fixture.config.SystemdUnit}},
					{name: "systemctl", args: []string{"--user", "restart", fixture.config.SystemdUnit}},
					{name: "systemctl", args: []string{"--user", "status", fixture.config.SystemdUnit, "--no-pager"}},
				}
			}
			for _, current := range deployCommands {
				runner.setSuccess(current, "")
			}
			if targetPlatform == platformDarwin {
				if err := executor.installDarwinService(context.Background(), reasonInactiveService, causeLaunchdNotLoaded); err != nil {
					t.Fatal(err)
				}
			} else if err := executor.installLinuxService(context.Background(), reasonInactiveService, causeSystemdInactive); err != nil {
				t.Fatal(err)
			}

			want := append(disableCommands, deployCommands...)
			if !reflect.DeepEqual(runner.commands, want) {
				t.Fatalf("commands = %v, want %v", runner.commands, want)
			}
		})
	}
}
