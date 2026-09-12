package deploy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
)

type serviceControl string

const (
	serviceControlStop    serviceControl = "stop"
	serviceControlDisable serviceControl = "disable"
)

// StopFromEnv stops Clyde's native user service without removing its registration.
func StopFromEnv(ctx context.Context, lookup func(string) (string, bool), output io.Writer) error {
	return controlFromEnv(ctx, lookup, output, serviceControlStop)
}

// DisableFromEnv stops and disables Clyde's native user service without removing its registration.
func DisableFromEnv(ctx context.Context, lookup func(string) (string, bool), output io.Writer) error {
	return controlFromEnv(ctx, lookup, output, serviceControlDisable)
}

func controlFromEnv(ctx context.Context, lookup func(string) (string, bool), output io.Writer, control serviceControl) error {
	targetPlatform, err := detectPlatform()
	if err != nil {
		slog.WarnContext(ctx, "deploy.control_detect_platform_failed", "err", err)
		return fmt.Errorf("detect platform: %w", err)
	}
	cfg, err := loadConfigFromEnv(targetPlatform, lookup)
	if err != nil {
		slog.WarnContext(ctx, "deploy.control_load_config_failed", "err", err)
		return fmt.Errorf("load deploy config: %w", err)
	}
	executor := executor{
		config:              cfg,
		files:               osFileSystem{},
		runner:              execRunner{},
		logger:              newLogger(output),
		stdout:              output,
		stderr:              output,
		outcome:             outcome{action: actionUnset, reason: "", cause: causeUnset},
		compiledFingerprint: nil,
		runningFingerprint:  nil,
	}
	return executor.controlService(ctx, control)
}

func (executor *executor) controlService(ctx context.Context, control serviceControl) error {
	commands, err := controlCommands(executor.config, control)
	if err != nil {
		return err
	}
	for i, current := range commands {
		step := fmt.Sprintf("control.%s.%d", control, i+1)
		if err := executor.runActionCommand(ctx, step, current); err != nil {
			return err
		}
	}
	result := "stopped"
	if control == serviceControlDisable {
		result = "disabled"
	}
	_, _ = fmt.Fprintf(executor.stdout, "Clyde daemon %s.\n", result)
	return nil
}

func controlCommands(cfg config, control serviceControl) ([]command, error) {
	switch cfg.Platform {
	case platformDarwin:
		target := cfg.LaunchdDomain + "/" + cfg.LaunchdLabel
		switch control {
		case serviceControlStop:
			return []command{{name: "launchctl", args: []string{"bootout", target}}}, nil
		case serviceControlDisable:
			return []command{
				{name: "launchctl", args: []string{"disable", target}},
				{name: "launchctl", args: []string{"bootout", target}},
			}, nil
		}
	case platformLinux:
		switch control {
		case serviceControlStop:
			return []command{{name: "systemctl", args: []string{"--user", "stop", cfg.SystemdUnit}}}, nil
		case serviceControlDisable:
			return []command{{name: "systemctl", args: []string{"--user", "disable", "--now", cfg.SystemdUnit}}}, nil
		}
	default:
		return nil, fmt.Errorf("unsupported platform %q", cfg.Platform)
	}
	return nil, fmt.Errorf("unsupported daemon control %q", control)
}
