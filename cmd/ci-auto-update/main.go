// Command ci-auto-update verifies Clyde's published automatic update path.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"goodkind.io/go-makefile/selfupdate"
)

const (
	githubAPIBaseURL    = "https://api.github.com"
	maxReleaseAttempts  = 10
	maxUpdateAttempts   = 120
	pollInterval        = 5 * time.Second
	maxGitHubBodyBytes  = 4 << 20
	diagnosticLineCount = 200
	testRootParent      = "/tmp"
	testRootPattern     = "clyde-auto-update-"
	processStopTimeout  = 15 * time.Second
)

const daemonConfig = `[logging]
level = "debug"

[conversation.semantic]
enabled = false
search_enabled = false

[adapter]
enabled = false

[mitm]
enabled_default = false
`

type environment struct {
	repository string
	commit     string
	refType    string
	refName    string
	token      string
	manual     bool
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
}

type releaseSelection struct {
	target   string
	previous string
}

type childProcess struct {
	process  *os.Process
	done     chan error
	finished bool
	result   error
}

type updateCheck struct {
	environment environment
	repository  string
	testRoot    string
	testHome    string
	fakeBinDir  string
	installBin  string
	daemonLog   string
	stateRoot   string
	configRoot  string
	cacheRoot   string
	runtimeRoot string
	stdout      io.Writer
	stderr      io.Writer
	child       *childProcess
}

func main() {
	if !isServiceManagerInvocation(os.Args[0]) {
		slog.Info("ci.auto_update.invoked", "component", "ci.auto_update")
	}
	os.Exit(realMain())
}

func realMain() int {
	if isServiceManagerInvocation(os.Args[0]) {
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "auto-update check failed: %v\n", err)
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	return 0
}

func run(
	ctx context.Context,
	getenv func(string) string,
	stdout io.Writer,
	stderr io.Writer,
) (runErr error) {
	env, err := loadEnvironment(getenv)
	if err != nil {
		return err
	}
	repository, err := commandOutput(ctx, "", nil, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	testRoot, err := os.MkdirTemp(testRootParent, testRootPattern)
	if err != nil {
		slog.ErrorContext(ctx, "ci.auto_update.root_create_failed", "component", "ci.auto_update", "err", err)
		return fmt.Errorf("create test root: %w", err)
	}
	check := &updateCheck{
		environment: env,
		repository:  repository,
		testRoot:    testRoot,
		testHome:    filepath.Join(testRoot, "home"),
		fakeBinDir:  filepath.Join(testRoot, "fake-bin"),
		installBin:  filepath.Join(testRoot, "bin", "clyde"),
		daemonLog:   filepath.Join(testRoot, "daemon.log"),
		stateRoot:   filepath.Join(testRoot, "state"),
		configRoot:  filepath.Join(testRoot, "config"),
		cacheRoot:   filepath.Join(testRoot, "cache"),
		runtimeRoot: filepath.Join(testRoot, "run"),
		stdout:      stdout,
		stderr:      stderr,
		child:       nil,
	}
	defer func() {
		if cleanupErr := check.cleanup(); cleanupErr != nil {
			if runErr == nil {
				runErr = cleanupErr
				return
			}
			fmt.Fprintf(stderr, "auto-update cleanup failed: %v\n", cleanupErr)
		}
	}()
	if err := check.prepareDirectories(); err != nil {
		return err
	}

	selection, err := check.fetchReleaseSelection(ctx)
	if err != nil {
		return err
	}
	targetCommit, err := check.resolveTagCommit(ctx, selection.target)
	if err != nil {
		return err
	}
	previousCommit, err := check.resolveTagCommit(ctx, selection.previous)
	if err != nil {
		return err
	}
	if targetCommit != env.commit {
		return fmt.Errorf(
			"target release %s resolves to %s, expected %s",
			selection.target,
			targetCommit,
			env.commit,
		)
	}
	fmt.Fprintf(stdout, "testing automatic update from %s to %s\n", selection.previous, selection.target)
	if err := check.buildOldBinary(ctx, selection.previous, previousCommit); err != nil {
		return err
	}
	if err := check.installFakeServiceManagers(); err != nil {
		return err
	}
	if err := check.startDaemon(ctx); err != nil {
		return err
	}
	if err := check.waitForUpdate(ctx, selection.target); err != nil {
		check.printDiagnostics()
		return err
	}
	fmt.Fprintf(stdout, "automatic update applied: %s\n", selection.target)
	return nil
}

func loadEnvironment(getenv func(string) string) (environment, error) {
	env := environment{
		repository: strings.TrimSpace(getenv("GITHUB_REPOSITORY")),
		commit:     strings.TrimSpace(getenv("GITHUB_SHA")),
		refType:    strings.TrimSpace(getenv("GITHUB_REF_TYPE")),
		refName:    strings.TrimSpace(getenv("GITHUB_REF_NAME")),
		token:      strings.TrimSpace(getenv("GH_TOKEN")),
		manual:     strings.TrimSpace(getenv("GITHUB_EVENT_NAME")) == "workflow_dispatch",
	}
	required := []struct {
		name  string
		value string
	}{
		{name: "GITHUB_REPOSITORY", value: env.repository},
		{name: "GITHUB_SHA", value: env.commit},
		{name: "GITHUB_REF_TYPE", value: env.refType},
		{name: "GITHUB_REF_NAME", value: env.refName},
		{name: "GH_TOKEN", value: env.token},
	}
	for _, variable := range required {
		if variable.value == "" {
			return environment{}, fmt.Errorf("%s is required", variable.name)
		}
	}
	if len(env.commit) < 8 {
		return environment{}, fmt.Errorf("GITHUB_SHA must contain at least eight characters")
	}
	return env, nil
}

func selectReleases(releases []githubRelease, env environment) (releaseSelection, error) {
	eligible := make([]githubRelease, 0, len(releases))
	for _, release := range releases {
		if !release.Draft {
			eligible = append(eligible, release)
		}
	}
	sort.SliceStable(eligible, func(i int, j int) bool {
		return eligible[i].PublishedAt.After(eligible[j].PublishedAt)
	})
	if env.manual {
		if len(eligible) < 2 {
			return releaseSelection{}, fmt.Errorf("manual run requires at least two published releases")
		}
		return releaseSelection{target: eligible[0].TagName, previous: eligible[1].TagName}, nil
	}
	target := ""
	if env.refType == "tag" {
		target = env.refName
	} else {
		commitSuffix := env.commit[:8]
		for _, release := range eligible {
			if strings.HasSuffix(release.TagName, "-"+commitSuffix) {
				target = release.TagName
				break
			}
		}
	}
	if target == "" {
		return releaseSelection{}, fmt.Errorf("published release for commit %s was not found", env.commit)
	}
	for i, release := range eligible {
		if release.TagName != target {
			continue
		}
		if i+1 >= len(eligible) {
			return releaseSelection{}, fmt.Errorf("release %s has no preceding release", target)
		}
		return releaseSelection{target: target, previous: eligible[i+1].TagName}, nil
	}
	return releaseSelection{}, fmt.Errorf("target release %s was not found", target)
}

func (check *updateCheck) fetchReleaseSelection(ctx context.Context) (releaseSelection, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= maxReleaseAttempts; attempt++ {
		releases, err := fetchGitHubReleases(ctx, client, check.environment)
		if err == nil {
			selection, selectErr := selectReleases(releases, check.environment)
			if selectErr == nil {
				return selection, nil
			}
			lastErr = selectErr
		} else {
			lastErr = err
		}
		fmt.Fprintf(check.stderr, "release selection attempt %d failed: %v\n", attempt, lastErr)
		if err := wait(ctx, pollInterval); err != nil {
			return releaseSelection{}, err
		}
	}
	slog.WarnContext(ctx, "ci.auto_update.release_selection_failed", "component", "ci.auto_update", "err", lastErr)
	return releaseSelection{}, fmt.Errorf("resolve target and preceding releases: %w", lastErr)
}

func fetchGitHubReleases(
	ctx context.Context,
	client *http.Client,
	env environment,
) ([]githubRelease, error) {
	slog.DebugContext(ctx, "ci.auto_update.release_fetch_started", "component", "ci.auto_update", "repository", env.repository)
	requestURL := githubAPIBaseURL + "/repos/" + env.repository + "/releases?per_page=100"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		slog.WarnContext(ctx, "ci.auto_update.release_request_failed", "component", "ci.auto_update", "repository", env.repository, "err", err)
		return nil, fmt.Errorf("build GitHub releases request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+env.token)
	request.Header.Set("X-Github-Api-Version", "2022-11-28")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query GitHub releases: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query GitHub releases: HTTP %d", response.StatusCode)
	}
	var releases []githubRelease
	reader := io.LimitReader(response.Body, maxGitHubBodyBytes)
	if err := json.NewDecoder(reader).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decode GitHub releases: %w", err)
	}
	return releases, nil
}

func (check *updateCheck) prepareDirectories() error {
	for _, directory := range []string{
		check.testHome,
		check.fakeBinDir,
		filepath.Dir(check.installBin),
		check.stateRoot,
		check.configRoot,
		check.cacheRoot,
		check.runtimeRoot,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			slog.Warn("ci.auto_update.directory_create_failed", "component", "ci.auto_update", "path", directory, "err", err)
			return fmt.Errorf("create test directory %s: %w", directory, err)
		}
	}
	configPath := filepath.Join(check.configRoot, "clyde", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return fmt.Errorf("create daemon config directory: %w", err)
	}
	if err := os.WriteFile(configPath, []byte(daemonConfig), 0o600); err != nil {
		return fmt.Errorf("write daemon config: %w", err)
	}
	return nil
}

func (check *updateCheck) resolveTagCommit(ctx context.Context, tag string) (string, error) {
	commit, err := commandOutput(ctx, check.repository, nil, "git", "rev-list", "-n", "1", tag)
	if err != nil {
		return "", err
	}
	if commit == "" {
		return "", fmt.Errorf("tag %s does not resolve to a commit", tag)
	}
	return commit, nil
}

func (check *updateCheck) buildOldBinary(
	ctx context.Context,
	previousTag string,
	previousCommit string,
) error {
	linkFlags := strings.Join([]string{
		"-X goodkind.io/gklog/version.Version=" + previousTag,
		"-X goodkind.io/gklog/version.Commit=" + previousCommit,
		"-X goodkind.io/gklog/version.Dirty=false",
		"-X goodkind.io/gklog/version.BinHash=ci-old-build",
	}, " ")
	env := replaceEnvironment(os.Environ(), map[string]string{"CGO_ENABLED": "1"})
	if _, err := commandOutput(
		ctx,
		check.repository,
		env,
		"go",
		"build",
		"-trimpath",
		"-ldflags",
		linkFlags,
		"-o",
		check.installBin,
		"./cmd/clyde",
	); err != nil {
		return err
	}
	version, err := binaryVersion(ctx, check.repository, check.installBin)
	if err != nil {
		return err
	}
	wantVersion := "clyde version " + previousTag
	if version != wantVersion {
		return fmt.Errorf("installed version is %q, expected %q", version, wantVersion)
	}
	return nil
}

func (check *updateCheck) installFakeServiceManagers() error {
	executable, err := os.Executable()
	if err != nil {
		slog.Warn("ci.auto_update.executable_resolve_failed", "component", "ci.auto_update", "err", err)
		return fmt.Errorf("resolve CI executable: %w", err)
	}
	for _, name := range []string{"launchctl", "systemctl"} {
		path := filepath.Join(check.fakeBinDir, name)
		if err := os.Symlink(executable, path); err != nil {
			return fmt.Errorf("create fake service manager %s: %w", name, err)
		}
	}
	return nil
}

func isServiceManagerInvocation(path string) bool {
	name := filepath.Base(path)
	return name == "launchctl" || name == "systemctl"
}

func (check *updateCheck) startDaemon(ctx context.Context) error {
	logFile, err := os.OpenFile(check.daemonLog, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		slog.WarnContext(ctx, "ci.auto_update.daemon_log_create_failed", "component", "ci.auto_update", "path", check.daemonLog, "err", err)
		return fmt.Errorf("create daemon log: %w", err)
	}
	// #nosec G204 -- installBin is created under the validated temporary root.
	command := exec.CommandContext(context.WithoutCancel(ctx), check.installBin, "daemon", "run")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	path := check.fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH")
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"HOME":            check.testHome,
		"PATH":            path,
		"XDG_CACHE_HOME":  check.cacheRoot,
		"XDG_CONFIG_HOME": check.configRoot,
		"XDG_RUNTIME_DIR": check.runtimeRoot,
		"XDG_STATE_HOME":  check.stateRoot,
	})
	command.Stdout = logFile
	command.Stderr = logFile
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		var waitErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.ErrorContext(ctx, "ci.auto_update.daemon_wait_panic", "component", "ci.auto_update", "err", fmt.Sprintf("panic: %v", recovered))
				waitErr = fmt.Errorf("wait for daemon panic: %v", recovered)
			}
			if closeErr := logFile.Close(); waitErr == nil {
				waitErr = closeErr
			}
			done <- waitErr
		}()
		waitErr = command.Wait()
	}()
	check.child = &childProcess{
		process:  command.Process,
		done:     done,
		finished: false,
		result:   nil,
	}
	return nil
}

func (process *childProcess) status() (error, bool) {
	if process == nil {
		return nil, true
	}
	if process.finished {
		return process.result, true
	}
	select {
	case process.result = <-process.done:
		process.finished = true
		return process.result, true
	default:
		return nil, false
	}
}

func (process *childProcess) stop() error {
	if process == nil || process.finished {
		return nil
	}
	processGroupID := -process.process.Pid
	if err := syscall.Kill(processGroupID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		slog.Warn("ci.auto_update.daemon_signal_failed", "component", "ci.auto_update", "pid", process.process.Pid, "err", err)
		return fmt.Errorf("signal daemon process group: %w", err)
	}
	timer := time.NewTimer(processStopTimeout)
	defer timer.Stop()
	select {
	case process.result = <-process.done:
		process.finished = true
		return nil
	case <-timer.C:
	}
	if err := syscall.Kill(processGroupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill daemon process group: %w", err)
	}
	process.result = <-process.done
	process.finished = true
	return nil
}

func (check *updateCheck) waitForUpdate(ctx context.Context, targetTag string) error {
	statePath := check.updateStatePath()
	wantVersion := "clyde version " + targetTag
	for attempt := 1; attempt <= maxUpdateAttempts; attempt++ {
		version, versionErr := binaryVersion(ctx, check.repository, check.installBin)
		applied, stateErr := stateReportsApplied(statePath)
		if versionErr == nil && stateErr == nil && version == wantVersion && applied {
			return nil
		}
		if processErr, exited := check.child.status(); exited {
			if processErr == nil {
				return fmt.Errorf("daemon exited before applying the update")
			}
			slog.WarnContext(ctx, "ci.auto_update.daemon_exited", "component", "ci.auto_update", "err", processErr)
			return fmt.Errorf("daemon exited before applying the update: %w", processErr)
		}
		if err := wait(ctx, pollInterval); err != nil {
			return err
		}
	}
	return fmt.Errorf("automatic update did not apply within ten minutes")
}

func stateReportsApplied(path string) (bool, error) {
	state, err := selfupdate.LoadState(path)
	if err != nil {
		slog.Warn("ci.auto_update.state_load_failed", "component", "ci.auto_update", "path", path, "err", err)
		return false, fmt.Errorf("load update state: %w", err)
	}
	return state.LastResult == "applied", nil
}

func (check *updateCheck) updateStatePath() string {
	return filepath.Join(check.stateRoot, "clyde", "update-state.json")
}

func (check *updateCheck) printDiagnostics() {
	fmt.Fprintln(check.stderr, "daemon output:")
	lines, err := tailLines(check.daemonLog, diagnosticLineCount)
	if err != nil {
		fmt.Fprintf(check.stderr, "(unavailable: %v)\n", err)
	} else {
		for _, line := range lines {
			fmt.Fprintln(check.stderr, line)
		}
	}
	fmt.Fprintln(check.stderr, "daemon structured log:")
	structuredLog := filepath.Join(check.stateRoot, "clyde", "clyde-daemon.jsonl")
	lines, err = tailLines(structuredLog, diagnosticLineCount)
	if err != nil {
		fmt.Fprintf(check.stderr, "(unavailable: %v)\n", err)
	} else {
		for _, line := range lines {
			fmt.Fprintln(check.stderr, line)
		}
	}
	fmt.Fprintln(check.stderr, "update state:")
	content, err := os.ReadFile(check.updateStatePath())
	if err != nil {
		fmt.Fprintf(check.stderr, "(unavailable: %v)\n", err)
		return
	}
	_, _ = check.stderr.Write(content)
}

func tailLines(path string, count int) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		slog.Warn("ci.auto_update.diagnostic_open_failed", "component", "ci.auto_update", "path", path, "err", err)
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	lines := make([]string, 0, count)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if len(lines) == count {
			copy(lines, lines[1:])
			lines[count-1] = scanner.Text()
			continue
		}
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return lines, nil
}

func (check *updateCheck) cleanup() error {
	var cleanupErrors []error
	if check.child != nil {
		if err := check.child.stop(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
		}
	}
	if err := removeTestRoot(check.testRoot); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	err := errors.Join(cleanupErrors...)
	if err != nil {
		slog.Warn("ci.auto_update.cleanup_failed", "component", "ci.auto_update", "err", err)
	}
	return err
}

func removeTestRoot(path string) error {
	cleaned := filepath.Clean(path)
	relative, err := filepath.Rel(testRootParent, cleaned)
	if err != nil {
		slog.Warn("ci.auto_update.root_resolve_failed", "component", "ci.auto_update", "path", path, "err", err)
		return fmt.Errorf("resolve test root %s: %w", path, err)
	}
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("refuse to remove test root outside the temp directory: %s", path)
	}
	if !strings.HasPrefix(filepath.Base(cleaned), testRootPattern) {
		return fmt.Errorf("refuse to remove unexpected test root %s", path)
	}
	if err := os.RemoveAll(cleaned); err != nil {
		return fmt.Errorf("remove test root %s: %w", cleaned, err)
	}
	return nil
}

func commandOutput(
	ctx context.Context,
	directory string,
	environment []string,
	name string,
	args ...string,
) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	output, err := command.CombinedOutput()
	if err != nil {
		slog.WarnContext(ctx, "ci.auto_update.command_failed", "component", "ci.auto_update", "command", name, "err", err)
		return "", fmt.Errorf("run %s: %w: %s", name, err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func binaryVersion(ctx context.Context, directory string, binary string) (string, error) {
	command := exec.CommandContext(ctx, binary, "--version")
	command.Dir = directory
	var stdout strings.Builder
	var stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		slog.WarnContext(ctx, "ci.auto_update.version_failed", "component", "ci.auto_update", "binary", binary, "err", err)
		return "", fmt.Errorf("run %s --version: %w: %s", binary, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func replaceEnvironment(current []string, replacements map[string]string) []string {
	result := make([]string, 0, len(current)+len(replacements))
	for _, entry := range current {
		name, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := replacements[name]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		slog.WarnContext(ctx, "ci.auto_update.wait_interrupted", "component", "ci.auto_update", "err", ctx.Err())
		return fmt.Errorf("wait interrupted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}
