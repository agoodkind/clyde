package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RemoveFromEnv unregisters only Clyde's native user service. It never removes
// daemon data, credentials, or the executable.
func RemoveFromEnv(ctx context.Context, lookup func(string) (string, bool), output io.Writer) error {
	platform, err := detectPlatform()
	if err != nil {
		return err
	}
	cfg, err := loadConfigFromEnv(platform, lookup)
	if err != nil {
		return err
	}
	executor := executor{config: cfg, files: osFileSystem{}, runner: execRunner{}, logger: newLogger(output), stdout: output, stderr: output, outcome: outcome{action: actionUnset, reason: "", cause: causeUnset}, compiledFingerprint: nil, runningFingerprint: nil}
	return executor.removeService(ctx)
}

func (executor *executor) removeService(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "deploy.remove_service_failed", "err", err)
		}
	}()
	path, err := executor.removableServicePath()
	if err != nil {
		return err
	}
	if info, statErr := os.Lstat(path); statErr == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refuse non-regular service registration %s", path)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect service registration %s: %w", path, statErr)
	}
	if executor.config.Platform == platformDarwin {
		err = executor.removeDarwinRegistration(ctx)
	} else {
		err = executor.removeLinuxRegistration(ctx)
	}
	if err != nil {
		return err
	}
	if err := executor.files.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove service registration %s: %w", path, err)
	}
	if executor.config.Platform == platformLinux {
		if err := executor.runActionCommand(ctx, "remove.daemon_reload", command{name: "systemctl", args: []string{"--user", "daemon-reload"}}); err != nil {
			return err
		}
	}
	writeCommandOutput(executor.stdout, "Removed Clyde service registration: "+path)
	return nil
}

func (executor *executor) removeDarwinRegistration(ctx context.Context) error {
	probeCommand := command{name: "launchctl", args: []string{"print", executor.darwinTarget()}}
	probe := executor.runner.run(ctx, probeCommand)
	if probe.success() {
		return executor.runActionCommand(ctx, "remove.bootout", command{name: "launchctl", args: []string{"bootout", executor.darwinTarget()}})
	}
	if (probe.exitCode == 113 || probe.exitCode == 3) && strings.Contains(probe.output, "Could not find service") {
		return nil
	}
	return newCommandRunError("inspect launchd registration", probeCommand, probe)
}

func (executor *executor) removeLinuxRegistration(ctx context.Context) error {
	probeCommand := command{name: "systemctl", args: []string{"--user", "show", executor.config.SystemdUnit, "--property=LoadState", "--value"}}
	probe := executor.runner.run(ctx, probeCommand)
	if !probe.success() && !strings.Contains(strings.ToLower(probe.output), "not-found") {
		return newCommandRunError("inspect systemd registration", probeCommand, probe)
	}
	if !strings.Contains(strings.ToLower(probe.output), "not-found") {
		if err := executor.runActionCommand(ctx, "remove.stop", command{name: "systemctl", args: []string{"--user", "stop", executor.config.SystemdUnit}}); err != nil {
			return err
		}
	}
	if err := executor.runActionCommand(ctx, "remove.disable", command{name: "systemctl", args: []string{"--user", "disable", executor.config.SystemdUnit}}); err != nil {
		return err
	}
	return nil
}

func (executor *executor) removableServicePath() (string, error) {
	cfg := executor.config
	switch cfg.Platform {
	case platformDarwin:
		expected := filepath.Join(cfg.Home, "Library", "LaunchAgents", defaultLaunchdLabel+".plist")
		if cfg.LaunchdLabel == defaultLaunchdLabel && cfg.LaunchdDomain == "gui/"+strconv.Itoa(os.Getuid()) && filepath.Clean(cfg.LaunchdPlist) == expected {
			return expected, nil
		}
	case platformLinux:
		expected := systemdUnitPath(cfg, defaultSystemdUnit)
		if cfg.SystemdUnit == defaultSystemdUnit && filepath.Clean(cfg.SystemdUserUnit) == expected {
			return expected, nil
		}
	}
	return "", fmt.Errorf("refuse removal outside Clyde's native user service registration")
}
