package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"goodkind.io/clyde/internal/binaryhandoff"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
	claudeprovider "goodkind.io/clyde/internal/providers/claude"
	claudeartifacts "goodkind.io/clyde/internal/providers/claude/lifecycle/artifacts"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/terminalcontrol"
	"goodkind.io/clyde/internal/util"
)

// VerboseFunc is a function that returns whether verbose mode is enabled.
// This is set by the cmd package.
var VerboseFunc func() bool = func() bool { return false }

// SessionUsedFunc checks if a Claude Code session was actually used (has a transcript).
// Can be overridden in tests where the fake claude binary doesn't create transcripts.
var (
	SessionUsedFunc       = DefaultSessionUsed
	wrapperExecutablePath = os.Executable
	execWrapperProcess    = syscall.Exec
)

const (
	envEnableSelfReload = "CLYDE_ENABLE_SELF_RELOAD"
	envDisable1MContext = "CLAUDE_CODE_DISABLE_1M_CONTEXT"
	envUnsetValue       = "\x00clyde-unset\x00"
)

type contextWindowSetting string

const (
	contextWindowUnset contextWindowSetting = ""
	contextWindow200K  contextWindowSetting = "200k"
	contextWindow1M    contextWindowSetting = "1m"
)

type monitorState struct {
	sawConnectionError bool
	reloadRequested    atomic.Bool
}

type SessionSettingsStore interface {
	LoadSettings(name string) (*session.Settings, error)
	SaveSettings(name string, settings *session.Settings) error
}

type ResumeOptions struct {
	CurrentWorkDir   string
	AdditionalArgs   []string
	ExtraEnvironment map[string]string
	EnableSelfReload bool
}

var (
	startNewInteractiveFunc     = StartNewInteractive
	resumeInteractiveFunc       = Resume
	resumeOpaqueInteractiveFunc = ResumeByName
)

// Lifecycle keeps Claude-specific session follow-through below the generic
// session launch contract.
type Lifecycle struct {
	settingsStore SessionSettingsStore
}

var (
	_ session.SessionLauncher           = (*Lifecycle)(nil)
	_ session.SessionResumer            = (*Lifecycle)(nil)
	_ session.OpaqueSessionResumer      = (*Lifecycle)(nil)
	_ session.ResumeInstructionProvider = (*Lifecycle)(nil)
	_ session.ContextMessageProvider    = (*Lifecycle)(nil)
	_ session.ArtifactCleaner           = (*Lifecycle)(nil)
)

func NewLifecycle(settingsStore SessionSettingsStore) *Lifecycle {
	return &Lifecycle{settingsStore: settingsStore}
}

func (l *Lifecycle) StartInteractive(ctx context.Context, req session.StartRequest) error {
	if req.Launch.Intent != "" && req.Launch.Intent != session.LaunchIntentNewSession {
		return fmt.Errorf("unsupported launch intent for claude lifecycle: %q", req.Launch.Intent)
	}

	sessionID, err := util.GenerateUUIDE()
	if err != nil {
		claudeLog.WarnContext(ctx, "claude.session.start.uuid_failed",
			"component", "claude",
			"session", req.SessionName,
			"err", err,
		)
		return err
	}
	env := map[string]string{
		"CLYDE_SESSION_NAME": req.SessionName,
	}
	if strings.TrimSpace(req.Launch.WorkDir) != "" {
		env["CLYDE_LAUNCH_CWD"] = req.Launch.WorkDir
	}

	if err := startNewInteractiveFunc(env, "", req.Launch.WorkDir, req.Launch.EnableRemoteControl, sessionID); err != nil {
		return err
	}
	if !req.Launch.EnableRemoteControl || l.settingsStore == nil {
		return nil
	}
	if err := PersistRemoteControlSetting(l.settingsStore, req.SessionName); err != nil {
		claudeLog.WarnContext(ctx, "claude.session.start.persist_remote_control_failed",
			"component", "claude",
			"session", req.SessionName,
			"err", err,
		)
		return nil
	}
	claudeLifecycleLog.Logger().Info("claude.session.start.remote_control_persisted",
		"component", "claude",
		"session", req.SessionName,
		"remote_control", true,
	)
	return nil
}

func (l *Lifecycle) ResumeInteractive(_ context.Context, req session.ResumeRequest) error {
	if req.Session == nil {
		return fmt.Errorf("nil session")
	}
	return resumeInteractiveFunc(config.GlobalDataDir(), req.Session, ResumeOptions{
		CurrentWorkDir:   req.Options.CurrentWorkDir,
		EnableSelfReload: req.Options.EnableSelfReload,
	})
}

func (l *Lifecycle) ResumeOpaqueInteractive(_ context.Context, req session.OpaqueResumeRequest) error {
	return resumeOpaqueInteractiveFunc(req.Query, req.AdditionalArgs)
}

func (l *Lifecycle) ResumeInstructions(sess *session.Session) []string {
	if sess == nil {
		return nil
	}
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID == "" {
		return nil
	}
	return []string{fmt.Sprintf("claude --resume %s", sessionID)}
}

func (l *Lifecycle) RecentContextMessages(sess *session.Session, limit, maxLen int) []session.ContextMessage {
	if sess == nil || strings.TrimSpace(sess.Metadata.ProviderTranscriptPath()) == "" {
		return nil
	}
	recent := ExtractRecentMessages(sess.Metadata.ProviderTranscriptPath(), limit, maxLen)
	out := make([]session.ContextMessage, 0, len(recent))
	for _, msg := range recent {
		out = append(out, session.ContextMessage{
			Role: msg.Role,
			Text: msg.Text,
		})
	}
	return out
}

func (l *Lifecycle) DeleteArtifacts(_ context.Context, req session.DeleteArtifactsRequest) (*session.DeletedArtifacts, error) {
	if req.Session == nil {
		return nil, fmt.Errorf("nil session")
	}
	deleted, err := deleteSessionArtifacts(req.ClydeRoot, req.Session)
	if err != nil {
		return nil, err
	}
	claudeLifecycleLog.Logger().Info("claude.session.artifacts_deleted",
		"component", "claude",
		"session", req.Session.Name,
		"transcript_count", len(deleted.Transcript),
		"agent_log_count", len(deleted.AgentLogs),
	)
	return &session.DeletedArtifacts{
		Transcripts: deleted.Transcript,
		AgentLogs:   deleted.AgentLogs,
	}, nil
}

// appendCommonArgs adds settings flags and global defaults to the arg list.
func appendCommonArgs(args []string, settingsFile string) []string {
	if settingsFile != "" && util.FileExists(settingsFile) {
		args = append(args, "--settings", settingsFile)
	}
	if remoteControlEnabled(settingsFile) {
		args = append(args, "--remote-control")
	}
	return args
}

// remoteControlEnabled decides whether to pass --remote-control to
// claude. Per session settings.json wins. The global config default
// fills in when the session has no explicit value. The two layers
// allow a user to opt one session in without forcing the flag on
// every other session.
func remoteControlEnabled(settingsFile string) bool {
	if settingsFile != "" && util.FileExists(settingsFile) {
		var s session.Settings
		if err := util.ReadJSON(settingsFile, &s); err == nil && s.RemoteControl {
			return true
		}
	}
	cfg, err := config.LoadGlobalOrDefault()
	return err == nil && cfg.Defaults.RemoteControl
}

func sessionSettingsFile(clydeRoot string, sessionName string) string {
	if strings.TrimSpace(clydeRoot) == "" || strings.TrimSpace(sessionName) == "" {
		return ""
	}
	settingsPath := filepath.Join(config.GetSessionDir(clydeRoot, sessionName), "settings.json")
	if !util.FileExists(settingsPath) {
		return ""
	}
	return settingsPath
}

func applyContextWindowLaunchSettings(settingsFile string, env map[string]string) (string, func()) {
	contextWindow, model, rawSettings, ok := readContextWindowLaunchSettings(settingsFile)
	if !ok {
		return settingsFile, func() {}
	}

	effectiveModel := model
	switch contextWindow {
	case contextWindow200K:
		env[envDisable1MContext] = "1"
		effectiveModel = strings.TrimSuffix(strings.TrimSpace(model), "[1m]")
	case contextWindow1M:
		env[envDisable1MContext] = envUnsetValue
		if shouldUse1MModelSuffix(model) {
			effectiveModel = strings.TrimSpace(model) + "[1m]"
		}
	case contextWindowUnset:
		return settingsFile, func() {}
	default:
		return settingsFile, func() {}
	}

	if effectiveModel == model {
		return settingsFile, func() {}
	}
	effectiveFile, err := writeTemporaryLaunchSettings(rawSettings, effectiveModel)
	if err != nil {
		claudeLog.Warn("claude.context_window.settings_rewrite_failed",
			"component", "claude",
			"settings_file", settingsFile,
			"err", err,
		)
		return settingsFile, func() {}
	}
	return effectiveFile, func() { _ = os.Remove(effectiveFile) }
}

func readContextWindowLaunchSettings(settingsFile string) (contextWindowSetting, string, map[string]json.RawMessage, bool) {
	if settingsFile == "" || !util.FileExists(settingsFile) {
		return contextWindowUnset, "", nil, false
	}
	content, err := os.ReadFile(settingsFile)
	if err != nil {
		return contextWindowUnset, "", nil, false
	}
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(content, &rawSettings); err != nil {
		return contextWindowUnset, "", nil, false
	}
	contextWindow := rawSettingsString(rawSettings, "contextWindow")
	if contextWindow == "" {
		return contextWindowUnset, "", rawSettings, false
	}
	model := rawSettingsString(rawSettings, "model")
	return contextWindowSetting(strings.ToLower(contextWindow)), model, rawSettings, true
}

func rawSettingsString(rawSettings map[string]json.RawMessage, key string) string {
	rawValue, ok := rawSettings[key]
	if !ok {
		return ""
	}
	var value string
	if err := json.Unmarshal(rawValue, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func shouldUse1MModelSuffix(model string) bool {
	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" || strings.HasSuffix(trimmedModel, "[1m]") {
		return false
	}
	return strings.Contains(strings.ToLower(trimmedModel), "opus")
}

func writeTemporaryLaunchSettings(rawSettings map[string]json.RawMessage, model string) (string, error) {
	modelJSON, err := json.Marshal(model)
	if err != nil {
		claudeLog.Warn("claude.context_window.model_json_failed", "component", "claude", "err", err)
		return "", fmt.Errorf("marshal context window model: %w", err)
	}
	rawSettings["model"] = modelJSON
	content, err := json.MarshalIndent(rawSettings, "", "  ")
	if err != nil {
		claudeLog.Warn("claude.context_window.settings_json_failed", "component", "claude", "err", err)
		return "", fmt.Errorf("marshal context window settings: %w", err)
	}
	tmpFile, err := os.CreateTemp("", "clyde-claude-settings-*.json")
	if err != nil {
		claudeLog.Warn("claude.context_window.temp_create_failed", "component", "claude", "err", err)
		return "", fmt.Errorf("create context window settings temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(content); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		claudeLog.Warn("claude.context_window.temp_write_failed", "component", "claude", "path", tmpPath, "err", err)
		return "", fmt.Errorf("write context window settings temp file %q: %w", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		claudeLog.Warn("claude.context_window.temp_close_failed", "component", "claude", "path", tmpPath, "err", err)
		return "", fmt.Errorf("close context window settings temp file %q: %w", tmpPath, err)
	}
	return tmpPath, nil
}

func commandEnvironment(env map[string]string) []string {
	commandEnv := os.Environ()
	if _, ok := env[envDisable1MContext]; ok {
		commandEnv = withoutEnvironmentKey(commandEnv, envDisable1MContext)
	}
	for key, value := range env {
		if value == envUnsetValue {
			continue
		}
		commandEnv = append(commandEnv, fmt.Sprintf("%s=%s", key, value))
	}
	return commandEnv
}

func withoutEnvironmentKey(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func resumeAdditionalArgs(sess *session.Session, currentWorkDir string) []string {
	currentWorkDir = strings.TrimSpace(currentWorkDir)
	if currentWorkDir == "" {
		return nil
	}
	if sess == nil || sess.Metadata.WorkspaceRoot == "" {
		return nil
	}
	if currentWorkDir == sess.Metadata.WorkspaceRoot {
		return nil
	}
	return []string{"--add-dir", currentWorkDir}
}

func PersistRemoteControlSetting(store SessionSettingsStore, sessionName string) error {
	settings, err := store.LoadSettings(sessionName)
	if err != nil {
		return err
	}
	if settings == nil {
		settings = &session.Settings{
			Model:         "",
			EffortLevel:   "",
			OutputStyle:   "",
			RemoteControl: false,
			ContextWindow: "",
			Permissions: session.Permissions{
				Allow:                        nil,
				Ask:                          nil,
				Deny:                         nil,
				AdditionalDirectories:        nil,
				DefaultMode:                  "",
				DisableBypassPermissionsMode: "",
			},
		}
	}
	settings.RemoteControl = true
	return store.SaveSettings(sessionName, settings)
}

// Resume invokes claude CLI to resume an existing session.
func Resume(clydeRoot string, sess *session.Session, opts ResumeOptions) error {
	settingsFile := sessionSettingsFile(clydeRoot, sess.Name)
	env := map[string]string{
		"CLYDE_SESSION_NAME": sess.Name,
	}
	if opts.EnableSelfReload {
		env[envEnableSelfReload] = "1"
	}
	maps.Copy(env, opts.ExtraEnvironment)
	applyMITMEnv(env)

	effectiveSettingsFile, cleanupSettings := applyContextWindowLaunchSettings(settingsFile, env)
	defer cleanupSettings()

	args := []string{"--resume", sess.Metadata.ProviderSessionID(), "-n", sess.Name}
	args = appendCommonArgs(args, effectiveSettingsFile)
	args = append(args, resumeAdditionalArgs(sess, opts.CurrentWorkDir)...)
	args = append(args, opts.AdditionalArgs...)

	if sess.Metadata.IsIncognito {
		return invokeWithCleanup(clydeRoot, sess, args, env, sess.Metadata.WorkDir)
	}

	if remoteControlEnabled(effectiveSettingsFile) {
		return invokeInteractivePTY(args, env, sess.Metadata.WorkDir, sess.Metadata.ProviderSessionID())
	}
	return invokeInteractive(args, env, sess.Metadata.WorkDir)
}

// StartNewInteractive runs claude without --resume for a new named session.
// env must set CLYDE_SESSION_NAME so the SessionStart hook can adopt the row.
// settingsFile may be empty; remote-control and settings injection match Resume.
// When sessionID is non-empty it is pre-assigned to Claude at launch so the
// inject socket, metadata, and later resume flows all share one UUID.
func StartNewInteractive(env map[string]string, settingsFile string, workDir string, forceRemoteControl bool, sessionID string) error {
	effectiveSettingsFile, cleanupSettings := applyContextWindowLaunchSettings(settingsFile, env)
	defer cleanupSettings()

	args := []string{}
	args = appendCommonArgs(args, effectiveSettingsFile)
	if sessionID != "" {
		args = append(args, "--session-id", sessionID)
	}
	applyMITMEnv(env)
	if forceRemoteControl || remoteControlEnabled(effectiveSettingsFile) {
		return invokeInteractivePTY(args, env, workDir, sessionID)
	}
	return invokeInteractive(args, env, workDir)
}

func applyMITMEnv(env map[string]string) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return
	}
	if !cfg.MITM.EnabledDefault || !cfg.MITM.EnabledFor("claude") {
		return
	}
	claudeprovider.SanitizeMITMMap(env)
	extra, err := providerLaunchEnvironmentViaDaemon(context.Background(), "claude")
	if err != nil {
		claudeLog.Warn("wrapper.mitm.claude_env_failed", "component", "wrapper", "err", err)
		return
	}
	for _, item := range extra {
		if item.GetKey() == "" {
			continue
		}
		if item.GetKey() == claudeprovider.AnthropicBaseURLEnv {
			claudeprovider.ApplyMITMMap(env, item.GetValue())
			continue
		}
		env[item.GetKey()] = item.GetValue()
	}
}

var providerLaunchEnvironmentViaDaemon = daemon.ProviderLaunchEnvironmentViaDaemon

// ClaudeBinaryPathFunc is a function that returns the path to the claude binary.
// This is set by the cmd package to allow overriding for tests.
var ClaudeBinaryPathFunc func() string = func() string { return "claude" }

// displayCommand prints the command being executed (always shown) and verbose debug info (if verbose mode).
func displayCommand(claudeBin string, args []string, env map[string]string) {
	// Always display the command being executed
	cmdStr := claudeBin + " " + strings.Join(args, " ")
	fmt.Fprintf(os.Stderr, "→ %s\n", cmdStr)

	// Show additional debug info in verbose mode
	if VerboseFunc() {
		if len(env) > 0 {
			fmt.Fprintln(os.Stderr, "[DEBUG] Environment variables:")
			for k, v := range env {
				if v == envUnsetValue {
					continue
				}
				fmt.Fprintf(os.Stderr, "  %s=%s\n", k, v)
			}
		}
	}
}

// invokeInteractive executes the claude CLI command interactively.
// Stdin, stdout, and stderr are connected to the current process.
// If the daemon is reachable, it acquires a per-session settings file
// for model isolation and injects --settings. If the daemon is not
// running, claude is invoked directly (graceful degradation).
// workDir, if non-empty, sets the working directory for the subprocess.
func invokeInteractive(args []string, env map[string]string, workDir string) error {
	claudeBin := ClaudeBinaryPathFunc()

	// Try to connect to daemon for per-session model isolation.
	// If the daemon is not running, skip (non-fatal).
	ctx := context.Background()
	wrapperID := fmt.Sprintf("%d", os.Getpid())
	sessionName := env["CLYDE_SESSION_NAME"]

	if settingsFile := acquireDaemonSession(ctx, wrapperID, sessionName); settingsFile != "" {
		effectiveSettingsFile, cleanupSettings := applyContextWindowLaunchSettings(settingsFile, env)
		defer cleanupSettings()
		// Inject per-session settings before other args.
		args = append([]string{"--settings", effectiveSettingsFile}, args...)
	}

	displayCommand(claudeBin, args, env)

	cmd := exec.Command(claudeBin, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Restore working directory if stored (empty = inherit from parent process)
	if workDir != "" {
		cmd.Dir = workDir
	}

	// Set environment variables
	cmd.Env = commandEnvironment(env)

	// Start a background goroutine that monitors the daemon connection.
	// If the daemon restarts (e.g. after `make install`), this re-registers
	// the session with the new daemon so global settings sync continues.
	terminalRestorer := terminalcontrol.CaptureProcessRestorer(claudeLifecycleLog.Logger())
	terminalcontrol.WriteResetToTerminal()
	done := make(chan struct{})
	monitorStopped := make(chan struct{})
	monitor := &monitorState{}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				claudeLifecycleLog.Logger().Error("wrapper.daemon_monitor.panic",
					"component", "wrapper",
					"session", sessionName,
					"wrapper_id", wrapperID,
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		monitorDaemon(ctx, wrapperID, sessionName, done, monitor, monitorStopped)
	}()

	runErr := cmd.Run()
	if terminalRestorer != nil {
		terminalRestorer.Restore()
	} else {
		terminalcontrol.WriteResetToTerminal()
	}

	// Signal the monitor to stop and release the session.
	close(done)
	<-monitorStopped
	if shouldSelfReloadWrapper(env, runErr, monitor) {
		if reloadErr := selfReloadCurrentProcess(); reloadErr != nil {
			claudeLog.Warn("wrapper.self_reload.failed",
				"component", "wrapper",
				"session", sessionName,
				"error", reloadErr)
		}
	}

	return runErr
}

func shouldSelfReloadWrapper(env map[string]string, runErr error, state *monitorState) bool {
	if runErr != nil {
		return false
	}
	if env[envEnableSelfReload] != "1" {
		return false
	}
	return state.reloadRequested.Load()
}

func selfReloadCurrentProcess() error {
	executablePath, err := wrapperExecutablePath()
	if err != nil {
		claudeLog.Warn("wrapper.self_reload.executable_failed",
			"component", "wrapper",
			"err", err,
		)
		return fmt.Errorf("resolve executable path: %w", err)
	}
	if err := binaryhandoff.ValidateClydeExecutable(executablePath); err != nil {
		claudeLifecycleLog.Logger().Warn("wrapper.self_reload.rejected",
			"component", "wrapper",
			"path", executablePath,
			"err", err)
		return nil
	}
	claudeLifecycleLog.Logger().Info("wrapper.self_reload.exec",
		"component", "wrapper",
		"path", executablePath,
		"arg_count", len(os.Args))
	return execWrapperProcess(executablePath, os.Args, os.Environ())
}

// ResumeByName invokes claude with --resume <name>, letting Claude resolve
// the display name to a session UUID internally. Used when clyde doesn't
// have the session in its own store. The daemon wrapping in invokeInteractive
// still provides model isolation.
func ResumeByName(name string, additionalArgs []string) error {
	args := []string{"--resume", name}
	args = append(args, additionalArgs...)
	env := map[string]string{
		"CLYDE_SESSION_NAME": name,
	}
	return invokeInteractive(args, env, "")
}

// invokeWithCleanup runs claude and cleans up incognito session on exit.
// Uses defer to ensure cleanup runs even on panic or interrupt (Ctrl+C).
func invokeWithCleanup(clydeRoot string, sess *session.Session, args []string, env map[string]string, workDir string) error {
	// Setup cleanup to run after claude exits (even on panic/Ctrl+C)
	defer func() {
		deleted, err := cleanupIncognitoSession(clydeRoot, sess)
		if err != nil {
			claudeLog.Warn("claude.incognito.cleanup.failed", "session", sess.Name, "err", err)
		} else {
			claudeLifecycleLog.Logger().Info("claude.incognito.deleted", "session", sess.Name, "transcript_count", len(deleted.Transcript), "agent_log_count", len(deleted.AgentLogs))

			// Show detailed info in verbose mode
			if VerboseFunc() {
				transcriptCount := len(deleted.Transcript)
				agentLogCount := len(deleted.AgentLogs)
				claudeLifecycleLog.Logger().Debug("claude.incognito.cleanup.details",
					"session", sess.Name,
					"transcripts", transcriptCount,
					"agent_logs", agentLogCount,
					"transcript_paths", deleted.Transcript,
					"agent_log_paths", deleted.AgentLogs,
				)
			}
		}
	}()

	// Run claude (blocks until exit)
	return invokeInteractive(args, env, workDir)
}

// cleanupIncognitoSession deletes session folder and Claude data.
// Returns DeletedFiles with info about what was deleted.
func cleanupIncognitoSession(clydeRoot string, sess *session.Session) (*DeletedFiles, error) {
	deleted, err := deleteSessionArtifacts(clydeRoot, sess)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to delete Claude data: %v\n", err)
	}

	// Delete session folder
	store := session.NewFileStore(clydeRoot)
	if err := store.Delete(sess.Name); err != nil {
		return deleted, err
	}

	return deleted, nil
}

func deleteSessionArtifacts(clydeRoot string, sess *session.Session) (*DeletedFiles, error) {
	return claudeartifacts.DeleteSessionArtifacts(clydeRoot, sess)
}

// DefaultSessionUsed checks if a Claude Code session was actually used by looking
// for a transcript file. Sessions with no ID are considered unused.
func DefaultSessionUsed(globalRoot string, sess *session.Session) bool {
	sessionID := sess.Metadata.ProviderSessionID()
	if sessionID == "" {
		return false
	}

	// Prefer the transcript path saved by the hook (accurate even with symlinks).
	if sess.Metadata.ProviderTranscriptPath() != "" {
		return util.FileExists(sess.Metadata.ProviderTranscriptPath())
	}

	homeDir, err := util.HomeDir()
	if err != nil {
		return true // assume used if we can't check
	}

	// Derive project-specific clyde root from WorkspaceRoot in session metadata.
	// Sessions are stored globally, but transcripts live under the project directory.
	clydeRoot := globalRoot
	if sess.Metadata.WorkspaceRoot != "" {
		clydeRoot = filepath.Join(sess.Metadata.WorkspaceRoot, config.ClydeDir)
	}

	transcriptPath := claudeprovider.TranscriptPath(homeDir, clydeRoot, sessionID)
	return util.FileExists(transcriptPath)
}
