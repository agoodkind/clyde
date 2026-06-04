// Package deploy owns Clyde's daemon deploy decision tree.
package deploy

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type runtimeOS string

const (
	runtimeOSDarwin runtimeOS = "darwin"
	runtimeOSLinux  runtimeOS = "linux"
)

type platform string

const (
	platformDarwin platform = "Darwin"
	platformLinux  platform = "Linux"
)

type action string

const (
	actionUnset          action = ""
	actionInstallService action = "install_service"
	actionRestartService action = "restart_service"
	actionReloadDaemon   action = "reload_daemon"
	actionRefuse         action = "refuse"
)

type reason string

const (
	reasonChangedServiceConfig reason = "changed_service_config"
	reasonDeployRequired       reason = "deploy_required"
	reasonMissingService       reason = "missing_service"
	reasonInactiveService      reason = "inactive_service"
	reasonStaleRootParent      reason = "stale_root_parent"
	reasonWorkerReload         reason = "worker_reload"
)

type cause string

const (
	causeUnset                    cause = ""
	causeConfigMismatchOrMissing  cause = "config_mismatch_or_missing"
	causeLaunchdNotLoaded         cause = "launchctl_not_loaded"
	causeSystemdInactive          cause = "systemd_inactive"
	causeRunningFingerprintFailed cause = "running_fingerprint_failed"
	causeRunningFingerprintEmpty  cause = "running_fingerprint_empty"
	causeFingerprintMismatch      cause = "fingerprint_mismatch"
)

type config struct {
	Platform        platform
	ReloadOnly      bool
	Home            string
	InstallBin      string
	LogPath         string
	LaunchdLabel    string
	LaunchdPlist    string
	LaunchdDomain   string
	SystemdUnit     string
	SystemdUserUnit string
}

type field struct {
	Key   string
	Value string
}

type logger struct {
	writer io.Writer
}

type fileSystem interface {
	readFile(path string) ([]byte, error)
	writeFile(path string, body []byte, perm os.FileMode) error
	mkdirAll(path string, perm os.FileMode) error
	pathExists(path string) (bool, error)
}

type runner interface {
	run(ctx context.Context, command command) commandResult
}

type dependencies struct {
	fileSystem          fileSystem
	runner              runner
	logWriter           io.Writer
	stdout              io.Writer
	stderr              io.Writer
	compiledFingerprint func() string
	runningFingerprint  func(ctx context.Context) (string, error)
}

// Fingerprints supplies the supervisor fingerprints the deploy compares to
// choose between a hot reload and a full restart. Compiled returns the running
// deploy process's own build-stamped fingerprint, which is the freshly
// installed binary because make deploy runs the installed binary's deploy
// command. Running RPCs the live daemon. Passing them as typed calls keeps the
// deploy from scraping a subprocess's console for the values.
type Fingerprints struct {
	Compiled func() string
	Running  func(ctx context.Context) (string, error)
}

type outcome struct {
	action action
	reason reason
	cause  cause
}

type command struct {
	name string
	args []string
}

type commandResult struct {
	output   string
	exitCode int
	err      error
}

type executor struct {
	config              config
	files               fileSystem
	runner              runner
	logger              logger
	stdout              io.Writer
	stderr              io.Writer
	outcome             outcome
	compiledFingerprint func() string
	runningFingerprint  func(ctx context.Context) (string, error)
}

const (
	defaultLaunchdLabel = "io.goodkind.clyde.daemon"
	defaultSystemdUnit  = "clyde-daemon.service"
)

//go:embed templates/macos.plist.in
var darwinTemplate string

//go:embed templates/systemd.service.in
var linuxTemplate string

// RunFromEnv loads deploy config from the current runtime and environment, then
// executes the daemon deploy decision tree.
func RunFromEnv(ctx context.Context, lookup func(string) (string, bool), reloadOnly bool, stdout io.Writer, stderr io.Writer, fingerprints Fingerprints) error {
	logWriter := stderr
	if logWriter == nil {
		logWriter = io.Discard
	}
	targetPlatform, err := detectPlatform()
	if err != nil {
		slog.WarnContext(ctx, "deploy.detect_platform_failed", "err", err)
		return fmt.Errorf("detect platform: %w", err)
	}
	log := newLogger(logWriter)
	cfg, err := loadConfigFromEnv(targetPlatform, lookup)
	if err != nil {
		log.event("config.load_failed", field{Key: "err", Value: err.Error()})
		slog.WarnContext(ctx, "deploy.load_config_failed", "err", err)
		return fmt.Errorf("load deploy config: %w", err)
	}
	cfg.ReloadOnly = reloadOnly
	return run(ctx, cfg, dependencies{
		fileSystem:          osFileSystem{},
		runner:              execRunner{},
		logWriter:           logWriter,
		stdout:              stdout,
		stderr:              stderr,
		compiledFingerprint: fingerprints.Compiled,
		runningFingerprint:  fingerprints.Running,
	})
}

func detectPlatform() (platform, error) {
	switch runtimeOS(runtime.GOOS) {
	case runtimeOSDarwin:
		return platformDarwin, nil
	case runtimeOSLinux:
		return platformLinux, nil
	default:
		return "", fmt.Errorf("unsupported platform %q", runtime.GOOS)
	}
}

func newLogger(writer io.Writer) logger {
	if writer == nil {
		writer = io.Discard
	}
	return logger{writer: writer}
}

func (logger logger) event(event string, fields ...field) {
	_, _ = fmt.Fprintf(logger.writer, "deploy: event=%s", event)
	for _, current := range fields {
		_, _ = fmt.Fprintf(logger.writer, " %s=%s", current.Key, strconv.Quote(current.Value))
	}
	_, _ = io.WriteString(logger.writer, "\n")
}

func loadConfigFromEnv(targetPlatform platform, lookup func(string) (string, bool)) (config, error) {
	homeDir, err := resolveHomeDir(lookup)
	if err != nil {
		return config{}, err
	}
	installBin, err := resolveInstallBin(lookup)
	if err != nil {
		return config{}, err
	}
	logPath := lookupString(lookup, "LOG_PATH")
	if logPath == "" {
		logPath = filepath.Join(homeDir, "Library", "Logs", "clyde-daemon.log")
	}

	cfg := config{
		Platform:        targetPlatform,
		ReloadOnly:      false,
		Home:            homeDir,
		InstallBin:      installBin,
		LogPath:         logPath,
		LaunchdLabel:    "",
		LaunchdPlist:    "",
		LaunchdDomain:   "",
		SystemdUnit:     "",
		SystemdUserUnit: "",
	}

	switch targetPlatform {
	case platformDarwin:
		label := lookupString(lookup, "LAUNCHD_LABEL")
		if label == "" {
			label = defaultLaunchdLabel
		}
		domain := lookupString(lookup, "LAUNCHD_DOMAIN")
		if domain == "" {
			domain = "gui/" + strconv.Itoa(os.Getuid())
		}
		plist := lookupString(lookup, "LAUNCHD_PLIST")
		if plist == "" {
			plist = filepath.Join(homeDir, "Library", "LaunchAgents", label+".plist")
		}
		cfg.LaunchdLabel = label
		cfg.LaunchdDomain = domain
		cfg.LaunchdPlist = plist
	case platformLinux:
		unit := lookupString(lookup, "SYSTEMD_UNIT")
		if unit == "" {
			unit = defaultSystemdUnit
		}
		unitPath := lookupString(lookup, "SYSTEMD_USER_UNIT")
		if unitPath == "" {
			unitPath = filepath.Join(homeDir, ".config", "systemd", "user", unit)
		}
		cfg.SystemdUnit = unit
		cfg.SystemdUserUnit = unitPath
	default:
		return config{}, fmt.Errorf("unsupported platform %q", targetPlatform)
	}

	return cfg, nil
}

func run(ctx context.Context, cfg config, deps dependencies) error {
	files := deps.fileSystem
	if files == nil {
		files = osFileSystem{}
	}
	commandRunner := deps.runner
	if commandRunner == nil {
		commandRunner = execRunner{}
	}
	stdout := deps.stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := deps.stderr
	if stderr == nil {
		stderr = io.Discard
	}

	executor := executor{
		config:              cfg,
		files:               files,
		runner:              commandRunner,
		logger:              newLogger(deps.logWriter),
		stdout:              stdout,
		stderr:              stderr,
		outcome:             outcome{action: actionUnset, reason: "", cause: causeUnset},
		compiledFingerprint: deps.compiledFingerprint,
		runningFingerprint:  deps.runningFingerprint,
	}
	return executor.run(ctx)
}

func (executor *executor) run(ctx context.Context) (err error) {
	executor.logger.event("main.start", field{Key: "platform", Value: string(executor.config.Platform)})
	if executor.config.ReloadOnly {
		executor.logger.event("main.mode", field{Key: "reload_only", Value: "true"})
	}
	defer func() {
		fields := []field{
			{Key: "platform", Value: string(executor.config.Platform)},
			{Key: "action", Value: string(executor.outcome.action)},
			{Key: "reason", Value: string(executor.outcome.reason)},
			{Key: "cause", Value: string(executor.outcome.cause)},
		}
		if err != nil {
			fields = append(fields, field{Key: "err", Value: err.Error()})
			executor.logger.event("main.failed", fields...)
			return
		}
		executor.logger.event("main.done", fields...)
	}()

	switch executor.config.Platform {
	case platformDarwin:
		return executor.deployDarwin(ctx)
	case platformLinux:
		return executor.deployLinux(ctx)
	default:
		return fmt.Errorf("unsupported platform %q", executor.config.Platform)
	}
}

func (executor *executor) deployDarwin(ctx context.Context) error {
	executor.logger.event("deploy.darwin.start", field{Key: "label", Value: executor.config.LaunchdLabel})
	matches, err := executor.darwinServiceMatches()
	if err != nil {
		return err
	}
	if !matches {
		executor.logger.event("branch.darwin.changed_service_config", field{Key: "plist", Value: executor.config.LaunchdPlist})
		if executor.config.ReloadOnly {
			return executor.refuseDeployRequired(causeConfigMismatchOrMissing)
		}
		return executor.installDarwinService(ctx, reasonChangedServiceConfig, causeConfigMismatchOrMissing)
	}
	executor.logger.event("branch.darwin.service_config_current", field{Key: "plist", Value: executor.config.LaunchdPlist})

	loaded, err := executor.darwinServiceLoaded(ctx)
	if err != nil {
		return err
	}
	if !loaded {
		executor.logger.event("branch.darwin.missing_service", field{Key: "target", Value: executor.darwinTarget()})
		if executor.config.ReloadOnly {
			return executor.refuseDeployRequired(causeLaunchdNotLoaded)
		}
		return executor.installDarwinService(ctx, reasonMissingService, causeLaunchdNotLoaded)
	}
	executor.logger.event("branch.darwin.service_loaded", field{Key: "target", Value: executor.darwinTarget()})
	return executor.reloadOrRestart(ctx)
}

func (executor *executor) deployLinux(ctx context.Context) error {
	executor.logger.event("deploy.linux.start", field{Key: "unit", Value: executor.config.SystemdUnit})
	matches, err := executor.linuxServiceMatches()
	if err != nil {
		return err
	}
	if !matches {
		executor.logger.event("branch.linux.changed_service_config", field{Key: "unit", Value: executor.config.SystemdUserUnit})
		if executor.config.ReloadOnly {
			return executor.refuseDeployRequired(causeConfigMismatchOrMissing)
		}
		return executor.installLinuxService(ctx, reasonChangedServiceConfig, causeConfigMismatchOrMissing)
	}
	executor.logger.event("branch.linux.service_config_current", field{Key: "unit", Value: executor.config.SystemdUserUnit})

	active, err := executor.linuxServiceActive(ctx)
	if err != nil {
		return err
	}
	if !active {
		executor.logger.event("branch.linux.inactive_service", field{Key: "unit", Value: executor.config.SystemdUnit})
		if executor.config.ReloadOnly {
			return executor.refuseDeployRequired(causeSystemdInactive)
		}
		return executor.installLinuxService(ctx, reasonInactiveService, causeSystemdInactive)
	}
	executor.logger.event("branch.linux.service_active", field{Key: "unit", Value: executor.config.SystemdUnit})
	return executor.reloadOrRestart(ctx)
}

func (executor *executor) reloadOrRestart(ctx context.Context) error {
	built := executor.compiledFingerprint()
	if built == "" {
		executor.logger.event("branch.built_fingerprint_empty")
		return errors.New("installed binary returned an empty supervisor fingerprint")
	}
	executor.logger.event("branch.built_fingerprint_ready", field{Key: "value", Value: built})

	running, err := executor.runningFingerprint(ctx)
	if err != nil {
		executor.logger.event("branch.stale_root_parent", field{Key: "cause", Value: string(causeRunningFingerprintFailed)})
		return executor.restartByPlatform(ctx, causeRunningFingerprintFailed)
	}
	if running == "" {
		executor.logger.event("branch.stale_root_parent", field{Key: "cause", Value: string(causeRunningFingerprintEmpty)})
		return executor.restartByPlatform(ctx, causeRunningFingerprintEmpty)
	}
	executor.logger.event("branch.running_fingerprint_ready", field{Key: "value", Value: running})
	if running != built {
		executor.logger.event("branch.stale_root_parent",
			field{Key: "cause", Value: string(causeFingerprintMismatch)},
			field{Key: "built_supervisor", Value: built},
			field{Key: "running_supervisor", Value: running},
		)
		return executor.restartByPlatform(ctx, causeFingerprintMismatch)
	}

	executor.logger.event("branch.worker_reload",
		field{Key: "built_supervisor", Value: built},
		field{Key: "running_supervisor", Value: running},
	)
	return executor.reloadDaemon(ctx, built, running)
}

func (executor *executor) restartByPlatform(ctx context.Context, currentCause cause) error {
	switch executor.config.Platform {
	case platformDarwin:
		return executor.restartDarwinService(ctx, reasonStaleRootParent, currentCause)
	case platformLinux:
		return executor.restartLinuxService(ctx, reasonStaleRootParent, currentCause)
	default:
		return fmt.Errorf("unsupported platform %q", executor.config.Platform)
	}
}

func (executor *executor) installDarwinService(ctx context.Context, currentReason reason, currentCause cause) error {
	executor.outcome = outcome{action: actionInstallService, reason: currentReason, cause: currentCause}
	executor.logger.event("action.install_service.selected", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})

	if err := executor.files.mkdirAll(filepath.Dir(executor.config.LaunchdPlist), 0o755); err != nil {
		executor.logger.event("action.install_service.failed", field{Key: "step", Value: "mkdir_launchd_dir"}, field{Key: "err", Value: err.Error()})
		slog.WarnContext(ctx, "deploy.install_darwin.mkdir_launchd_dir_failed", "path", filepath.Dir(executor.config.LaunchdPlist), "err", err)
		return fmt.Errorf("mkdir launchd dir %s: %w", filepath.Dir(executor.config.LaunchdPlist), err)
	}
	if err := executor.files.mkdirAll(filepath.Dir(executor.config.LogPath), 0o755); err != nil {
		executor.logger.event("action.install_service.failed", field{Key: "step", Value: "mkdir_log_dir"}, field{Key: "err", Value: err.Error()})
		slog.WarnContext(ctx, "deploy.install_darwin.mkdir_log_dir_failed", "path", filepath.Dir(executor.config.LogPath), "err", err)
		return fmt.Errorf("mkdir log dir %s: %w", filepath.Dir(executor.config.LogPath), err)
	}
	rendered := executor.renderTemplate(darwinTemplate, executor.config.LaunchdLabel)
	if err := executor.files.writeFile(executor.config.LaunchdPlist, rendered, 0o644); err != nil {
		executor.logger.event("action.install_service.failed", field{Key: "step", Value: "write_launchd_plist"}, field{Key: "err", Value: err.Error()})
		slog.WarnContext(ctx, "deploy.install_darwin.write_plist_failed", "path", executor.config.LaunchdPlist, "err", err)
		return fmt.Errorf("write launchd plist %s: %w", executor.config.LaunchdPlist, err)
	}
	_ = executor.runActionCommand(ctx, "action.install_service.darwin.bootout", command{name: "launchctl", args: []string{"bootout", executor.config.LaunchdDomain, executor.config.LaunchdPlist}})
	if err := executor.runActionCommand(ctx, "action.install_service.darwin.bootstrap", command{name: "launchctl", args: []string{"bootstrap", executor.config.LaunchdDomain, executor.config.LaunchdPlist}}); err != nil {
		return err
	}
	if err := executor.runActionCommand(ctx, "action.install_service.darwin.status", command{name: "launchctl", args: []string{"print", executor.darwinTarget()}}); err != nil {
		return err
	}
	executor.logger.event("action.install_service.done", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})
	return nil
}

func (executor *executor) installLinuxService(ctx context.Context, currentReason reason, currentCause cause) error {
	executor.outcome = outcome{action: actionInstallService, reason: currentReason, cause: currentCause}
	executor.logger.event("action.install_service.selected", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})

	if err := executor.files.mkdirAll(filepath.Dir(executor.config.SystemdUserUnit), 0o755); err != nil {
		executor.logger.event("action.install_service.failed", field{Key: "step", Value: "mkdir_systemd_dir"}, field{Key: "err", Value: err.Error()})
		slog.WarnContext(ctx, "deploy.install_linux.mkdir_systemd_dir_failed", "path", filepath.Dir(executor.config.SystemdUserUnit), "err", err)
		return fmt.Errorf("mkdir systemd dir %s: %w", filepath.Dir(executor.config.SystemdUserUnit), err)
	}
	label := strings.TrimSuffix(executor.config.SystemdUnit, filepath.Ext(executor.config.SystemdUnit))
	rendered := executor.renderTemplate(linuxTemplate, label)
	if err := executor.files.writeFile(executor.config.SystemdUserUnit, rendered, 0o644); err != nil {
		executor.logger.event("action.install_service.failed", field{Key: "step", Value: "write_systemd_unit"}, field{Key: "err", Value: err.Error()})
		slog.WarnContext(ctx, "deploy.install_linux.write_unit_failed", "path", executor.config.SystemdUserUnit, "err", err)
		return fmt.Errorf("write systemd unit %s: %w", executor.config.SystemdUserUnit, err)
	}
	if err := executor.runActionCommand(ctx, "action.install_service.linux.daemon_reload", command{name: "systemctl", args: []string{"--user", "daemon-reload"}}); err != nil {
		return err
	}
	if err := executor.runActionCommand(ctx, "action.install_service.linux.enable", command{name: "systemctl", args: []string{"--user", "enable", executor.config.SystemdUnit}}); err != nil {
		return err
	}
	if err := executor.runActionCommand(ctx, "action.install_service.linux.restart", command{name: "systemctl", args: []string{"--user", "restart", executor.config.SystemdUnit}}); err != nil {
		return err
	}
	if err := executor.runActionCommand(ctx, "action.install_service.linux.status", command{name: "systemctl", args: []string{"--user", "status", executor.config.SystemdUnit, "--no-pager"}}); err != nil {
		return err
	}
	executor.logger.event("action.install_service.done", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})
	return nil
}

func (executor *executor) restartDarwinService(ctx context.Context, currentReason reason, currentCause cause) error {
	executor.outcome = outcome{action: actionRestartService, reason: currentReason, cause: currentCause}
	executor.logger.event("action.restart_service.selected", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})
	if err := executor.runActionCommand(ctx, "action.restart_service.darwin.kickstart", command{name: "launchctl", args: []string{"kickstart", "-k", executor.darwinTarget()}}); err != nil {
		return err
	}
	if err := executor.runActionCommand(ctx, "action.restart_service.darwin.status", command{name: "launchctl", args: []string{"print", executor.darwinTarget()}}); err != nil {
		return err
	}
	executor.logger.event("action.restart_service.done", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})
	return nil
}

func (executor *executor) restartLinuxService(ctx context.Context, currentReason reason, currentCause cause) error {
	executor.outcome = outcome{action: actionRestartService, reason: currentReason, cause: currentCause}
	executor.logger.event("action.restart_service.selected", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})
	if err := executor.runActionCommand(ctx, "action.restart_service.linux.restart", command{name: "systemctl", args: []string{"--user", "restart", executor.config.SystemdUnit}}); err != nil {
		return err
	}
	if err := executor.runActionCommand(ctx, "action.restart_service.linux.status", command{name: "systemctl", args: []string{"--user", "status", executor.config.SystemdUnit, "--no-pager"}}); err != nil {
		return err
	}
	executor.logger.event("action.restart_service.done", field{Key: "reason", Value: string(currentReason)}, field{Key: "cause", Value: string(currentCause)})
	return nil
}

func (executor *executor) reloadDaemon(ctx context.Context, built string, running string) error {
	executor.outcome = outcome{action: actionReloadDaemon, reason: reasonWorkerReload, cause: causeUnset}
	executor.logger.event("action.reload_daemon.selected",
		field{Key: "reason", Value: string(reasonWorkerReload)},
		field{Key: "built_supervisor", Value: built},
		field{Key: "running_supervisor", Value: running},
	)
	if err := executor.runActionCommand(ctx, "action.reload_daemon.command", command{name: executor.config.InstallBin, args: []string{"daemon", "reload"}}); err != nil {
		return err
	}
	switch executor.config.Platform {
	case platformDarwin:
		if err := executor.runActionCommand(ctx, "action.reload_daemon.darwin.status", command{name: "launchctl", args: []string{"print", executor.darwinTarget()}}); err != nil {
			return err
		}
	case platformLinux:
		if err := executor.runActionCommand(ctx, "action.reload_daemon.linux.status", command{name: "systemctl", args: []string{"--user", "status", executor.config.SystemdUnit, "--no-pager"}}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported platform %q", executor.config.Platform)
	}
	executor.logger.event("action.reload_daemon.done",
		field{Key: "reason", Value: string(reasonWorkerReload)},
		field{Key: "built_supervisor", Value: built},
		field{Key: "running_supervisor", Value: running},
	)
	return nil
}

func (executor *executor) refuseDeployRequired(currentCause cause) error {
	executor.outcome = outcome{action: actionRefuse, reason: reasonDeployRequired, cause: currentCause}
	executor.logger.event("action.refuse.selected",
		field{Key: "reason", Value: string(reasonDeployRequired)},
		field{Key: "cause", Value: string(currentCause)},
	)
	return fmt.Errorf("daemon deploy reload-only requires `make deploy` first (cause=%s)", currentCause)
}

func (executor *executor) darwinServiceMatches() (bool, error) {
	executor.logger.event("darwin.service_config.check", field{Key: "plist", Value: executor.config.LaunchdPlist})
	exists, err := executor.files.pathExists(executor.config.LaunchdPlist)
	if err != nil {
		executor.logger.event("darwin.service_config.failed", field{Key: "err", Value: err.Error()})
		slog.Warn("deploy.darwin.service_config_check_failed", "path", executor.config.LaunchdPlist, "err", err)
		return false, fmt.Errorf("check launchd plist %s: %w", executor.config.LaunchdPlist, err)
	}
	if !exists {
		executor.logger.event("darwin.service_config.missing", field{Key: "plist", Value: executor.config.LaunchdPlist})
		return false, nil
	}
	rendered := executor.renderTemplate(darwinTemplate, executor.config.LaunchdLabel)
	current, err := executor.files.readFile(executor.config.LaunchdPlist)
	if err != nil {
		executor.logger.event("darwin.service_config.failed", field{Key: "err", Value: err.Error()})
		slog.Warn("deploy.darwin.service_config_read_failed", "path", executor.config.LaunchdPlist, "err", err)
		return false, fmt.Errorf("read launchd plist %s: %w", executor.config.LaunchdPlist, err)
	}
	if bytes.Equal(rendered, current) {
		executor.logger.event("darwin.service_config.match", field{Key: "plist", Value: executor.config.LaunchdPlist})
		return true, nil
	}
	executor.logger.event("darwin.service_config.mismatch", field{Key: "plist", Value: executor.config.LaunchdPlist})
	return false, nil
}

func (executor *executor) darwinServiceLoaded(ctx context.Context) (bool, error) {
	executor.logger.event("darwin.service_loaded.check", field{Key: "target", Value: executor.darwinTarget()})
	err := executor.runQuietCommand(ctx, "darwin.service_loaded.command", command{name: "launchctl", args: []string{"print", executor.darwinTarget()}})
	if err != nil {
		var runErr *commandRunError
		if errors.As(err, &runErr) {
			executor.logger.event("darwin.service_loaded.false", field{Key: "target", Value: executor.darwinTarget()}, field{Key: "status", Value: strconv.Itoa(runErr.result.normalizedExitCode())})
			return false, nil
		}
		return false, err
	}
	executor.logger.event("darwin.service_loaded.true", field{Key: "target", Value: executor.darwinTarget()})
	return true, nil
}

func (executor *executor) linuxServiceMatches() (bool, error) {
	label := strings.TrimSuffix(executor.config.SystemdUnit, filepath.Ext(executor.config.SystemdUnit))
	executor.logger.event("linux.service_config.check", field{Key: "unit", Value: executor.config.SystemdUserUnit}, field{Key: "label", Value: label})
	exists, err := executor.files.pathExists(executor.config.SystemdUserUnit)
	if err != nil {
		executor.logger.event("linux.service_config.failed", field{Key: "err", Value: err.Error()})
		slog.Warn("deploy.linux.service_config_check_failed", "path", executor.config.SystemdUserUnit, "err", err)
		return false, fmt.Errorf("check systemd unit %s: %w", executor.config.SystemdUserUnit, err)
	}
	if !exists {
		executor.logger.event("linux.service_config.missing", field{Key: "unit", Value: executor.config.SystemdUserUnit})
		return false, nil
	}
	rendered := executor.renderTemplate(linuxTemplate, label)
	current, err := executor.files.readFile(executor.config.SystemdUserUnit)
	if err != nil {
		executor.logger.event("linux.service_config.failed", field{Key: "err", Value: err.Error()})
		slog.Warn("deploy.linux.service_config_read_failed", "path", executor.config.SystemdUserUnit, "err", err)
		return false, fmt.Errorf("read systemd unit %s: %w", executor.config.SystemdUserUnit, err)
	}
	if bytes.Equal(rendered, current) {
		executor.logger.event("linux.service_config.match", field{Key: "unit", Value: executor.config.SystemdUserUnit})
		return true, nil
	}
	executor.logger.event("linux.service_config.mismatch", field{Key: "unit", Value: executor.config.SystemdUserUnit})
	return false, nil
}

func (executor *executor) linuxServiceActive(ctx context.Context) (bool, error) {
	executor.logger.event("linux.service_active.check", field{Key: "unit", Value: executor.config.SystemdUnit})
	err := executor.runQuietCommand(ctx, "linux.service_active.command", command{name: "systemctl", args: []string{"--user", "is-active", "--quiet", executor.config.SystemdUnit}})
	if err != nil {
		var runErr *commandRunError
		if errors.As(err, &runErr) {
			executor.logger.event("linux.service_active.false", field{Key: "unit", Value: executor.config.SystemdUnit}, field{Key: "status", Value: strconv.Itoa(runErr.result.normalizedExitCode())})
			return false, nil
		}
		return false, err
	}
	executor.logger.event("linux.service_active.true", field{Key: "unit", Value: executor.config.SystemdUnit})
	return true, nil
}

func (executor *executor) runQuietCommand(ctx context.Context, step string, current command) error {
	executor.logger.event(step+".start", field{Key: "command", Value: current.display()})
	result := executor.runner.run(ctx, current)
	if result.success() {
		executor.logger.event(step+".success", field{Key: "command", Value: current.display()}, field{Key: "output", Value: strings.TrimSpace(result.output)})
		return nil
	}
	err := newCommandRunError(step, current, result)
	executor.logger.event(step+".failed",
		field{Key: "command", Value: current.display()},
		field{Key: "status", Value: strconv.Itoa(result.normalizedExitCode())},
		field{Key: "output", Value: strings.TrimSpace(result.output)},
		field{Key: "err", Value: err.Error()},
	)
	return err
}

func (executor *executor) runActionCommand(ctx context.Context, step string, current command) error {
	executor.logger.event(step+".start", field{Key: "command", Value: current.display()})
	result := executor.runner.run(ctx, current)
	if result.success() {
		writeCommandOutput(executor.stdout, result.output)
		executor.logger.event(step+".success", field{Key: "command", Value: current.display()}, field{Key: "output", Value: strings.TrimSpace(result.output)})
		return nil
	}
	writeCommandOutput(executor.stderr, result.output)
	err := newCommandRunError(step, current, result)
	executor.logger.event(step+".failed",
		field{Key: "command", Value: current.display()},
		field{Key: "status", Value: strconv.Itoa(result.normalizedExitCode())},
		field{Key: "output", Value: strings.TrimSpace(result.output)},
		field{Key: "err", Value: err.Error()},
	)
	return err
}

func (executor *executor) renderTemplate(template string, label string) []byte {
	executor.logger.event("template.render", field{Key: "label", Value: label})
	replacer := strings.NewReplacer(
		"@@BIN_PATH@@", executor.config.InstallBin,
		"@@HOME@@", executor.config.Home,
		"@@LABEL@@", label,
		"@@LOG_PATH@@", executor.config.LogPath,
	)
	return []byte(replacer.Replace(template))
}

func (executor *executor) darwinTarget() string {
	return executor.config.LaunchdDomain + "/" + executor.config.LaunchdLabel
}

func resolveHomeDir(lookup func(string) (string, bool)) (string, error) {
	if home := lookupString(lookup, "HOME"); home != "" {
		return home, nil
	}
	currentUser, err := user.Current()
	if err != nil {
		slog.Warn("deploy.resolve_home_failed", "err", err)
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	if strings.TrimSpace(currentUser.HomeDir) == "" {
		return "", errors.New("empty home dir")
	}
	return currentUser.HomeDir, nil
}

func resolveInstallBin(lookup func(string) (string, bool)) (string, error) {
	if installBin := lookupString(lookup, "INSTALL_BIN"); installBin != "" {
		return installBin, nil
	}
	path, err := os.Executable()
	if err != nil {
		slog.Warn("deploy.resolve_install_bin_failed", "err", err)
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return path, nil
}

func lookupString(lookup func(string) (string, bool), key string) string {
	if lookup == nil {
		return ""
	}
	value, ok := lookup(key)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

func writeCommandOutput(writer io.Writer, output string) {
	if writer == nil || output == "" {
		return
	}
	_, _ = io.WriteString(writer, output)
	if !strings.HasSuffix(output, "\n") {
		_, _ = io.WriteString(writer, "\n")
	}
}

type osFileSystem struct{}

func (osFileSystem) readFile(path string) ([]byte, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("deploy.fs.read_file_failed", "path", path, "err", err)
		return nil, fmt.Errorf("read file %s: %w", path, err)
	}
	return body, nil
}

func (osFileSystem) writeFile(path string, body []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, body, perm); err != nil {
		slog.Warn("deploy.fs.write_file_failed", "path", path, "err", err)
		return fmt.Errorf("write file %s: %w", path, err)
	}
	return nil
}

func (osFileSystem) mkdirAll(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		slog.Warn("deploy.fs.mkdir_all_failed", "path", path, "err", err)
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	return nil
}

func (osFileSystem) pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	slog.Warn("deploy.fs.path_exists_failed", "path", path, "err", err)
	return false, fmt.Errorf("stat %s: %w", path, err)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, current command) commandResult {
	slog.DebugContext(ctx, "deploy.exec_runner.run", "command", current.display())
	// #nosec G204
	cmd := exec.CommandContext(ctx, current.name, current.args...)
	output, err := cmd.CombinedOutput()
	result := commandResult{
		output:   string(output),
		exitCode: 0,
		err:      err,
	}
	if err == nil {
		return result
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
		return result
	}
	result.exitCode = -1
	return result
}

func (result commandResult) success() bool {
	return result.err == nil && result.exitCode == 0
}

func (result commandResult) normalizedExitCode() int {
	if result.exitCode != 0 {
		return result.exitCode
	}
	if result.err != nil {
		return -1
	}
	return 0
}

type commandRunError struct {
	step    string
	command command
	result  commandResult
}

func newCommandRunError(step string, current command, result commandResult) *commandRunError {
	return &commandRunError{
		step:    step,
		command: current,
		result:  result,
	}
}

func (err *commandRunError) Error() string {
	if err.result.err != nil {
		return fmt.Sprintf("%s failed for %s: %v", err.step, err.command.display(), err.result.err)
	}
	return fmt.Sprintf("%s failed for %s with exit code %d", err.step, err.command.display(), err.result.normalizedExitCode())
}

func (current command) display() string {
	if len(current.args) == 0 {
		return strconv.Quote(current.name)
	}
	parts := make([]string, 0, len(current.args)+1)
	parts = append(parts, shellQuote(current.name))
	for _, arg := range current.args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(raw string) string {
	if raw == "" {
		return `""`
	}
	if strings.IndexFunc(raw, func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= 'A' && r <= 'Z':
			return false
		case r >= '0' && r <= '9':
			return false
		case strings.ContainsRune("._/:=-", r):
			return false
		default:
			return true
		}
	}) == -1 {
		return raw
	}
	return strconv.Quote(raw)
}
