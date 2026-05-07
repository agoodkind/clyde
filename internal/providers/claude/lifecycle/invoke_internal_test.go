package claude

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/session"
)

type stubSessionSettingsStore struct {
	settings *session.Settings
	loadErr  error
	saveErr  error
	saved    *session.Settings
}

func (s *stubSessionSettingsStore) LoadSettings(_ string) (*session.Settings, error) {
	return s.settings, s.loadErr
}

func (s *stubSessionSettingsStore) SaveSettings(_ string, settings *session.Settings) error {
	if settings != nil {
		copyValue := *settings
		s.saved = &copyValue
	}
	return s.saveErr
}

func TestSessionSettingsFile(t *testing.T) {
	clydeRoot := t.TempDir()
	sessionsDir := config.GetSessionsDir(clydeRoot)
	if err := os.MkdirAll(filepath.Join(sessionsDir, "chat-1"), 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	if got := sessionSettingsFile(clydeRoot, "chat-1"); got != "" {
		t.Fatalf("sessionSettingsFile without file = %q, want empty", got)
	}

	settingsPath := filepath.Join(sessionsDir, "chat-1", "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"remoteControl":true}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if got := sessionSettingsFile(clydeRoot, "chat-1"); got != settingsPath {
		t.Fatalf("sessionSettingsFile with file = %q, want %q", got, settingsPath)
	}
}

func TestResumeAdditionalArgs(t *testing.T) {
	sess := &session.Session{
		Name: "chat-1",
		Metadata: session.Metadata{
			WorkspaceRoot: "/tmp/workspace",
		},
	}
	if got := resumeAdditionalArgs(sess, "/tmp/workspace"); len(got) != 0 {
		t.Fatalf("resumeAdditionalArgs same workspace = %v, want empty", got)
	}
	if got := resumeAdditionalArgs(sess, "/tmp/other"); len(got) != 2 || got[0] != "--add-dir" || got[1] != "/tmp/other" {
		t.Fatalf("resumeAdditionalArgs other workspace = %v, want [--add-dir /tmp/other]", got)
	}
	if got := resumeAdditionalArgs(sess, ""); len(got) != 0 {
		t.Fatalf("resumeAdditionalArgs empty cwd = %v, want empty", got)
	}
}

func TestPersistRemoteControlSetting(t *testing.T) {
	store := &stubSessionSettingsStore{}
	if err := PersistRemoteControlSetting(store, "chat-1"); err != nil {
		t.Fatalf("PersistRemoteControlSetting nil settings: %v", err)
	}
	if store.saved == nil || !store.saved.RemoteControl {
		t.Fatalf("PersistRemoteControlSetting saved = %#v, want remoteControl=true", store.saved)
	}

	store = &stubSessionSettingsStore{
		settings: &session.Settings{Model: "sonnet"},
	}
	if err := PersistRemoteControlSetting(store, "chat-2"); err != nil {
		t.Fatalf("PersistRemoteControlSetting existing settings: %v", err)
	}
	if store.saved == nil || !store.saved.RemoteControl || store.saved.Model != "sonnet" {
		t.Fatalf("PersistRemoteControlSetting preserved settings = %#v", store.saved)
	}
}

func TestApplyMITMEnvAddsAnthropicBaseURLForWrapperLaunch(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := []byte("[mitm]\nenabled_default = true\nproviders = [\"claude\"]\nbody_mode = \"summary\"\ncapture_dir = \"" + t.TempDir() + "\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	env := map[string]string{
		"ANTHROPIC_BASE_URL": "https://old.example",
		"KEEP":               "1",
	}
	applyMITMEnv(env)

	if env["KEEP"] != "1" {
		t.Fatalf("KEEP env = %q, want 1", env["KEEP"])
	}
	baseURL := env["ANTHROPIC_BASE_URL"]
	if !strings.HasPrefix(baseURL, "http://[::1]:") {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want local MITM proxy", baseURL)
	}
}

func TestLifecycleStartInteractiveReturnsUUIDAllocationError(t *testing.T) {
	uuid.DisableRandPool()
	uuid.SetRand(strings.NewReader(""))
	t.Cleanup(func() {
		uuid.SetRand(nil)
		uuid.DisableRandPool()
	})

	original := startNewInteractiveFunc
	t.Cleanup(func() {
		startNewInteractiveFunc = original
	})
	startNewInteractiveFunc = func(map[string]string, string, string, bool, string) error {
		t.Fatal("startNewInteractiveFunc was called after UUID allocation failed")
		return nil
	}

	lifecycle := NewLifecycle(nil)
	err := lifecycle.StartInteractive(context.Background(), session.StartRequest{
		SessionName: "chat-1",
		Launch: session.LaunchOptions{
			Intent: session.LaunchIntentNewSession,
		},
	})
	if err == nil {
		t.Fatal("StartInteractive returned nil error, want UUID allocation error")
	}
	if !strings.Contains(err.Error(), "generate uuid") {
		t.Fatalf("StartInteractive error = %q, want generate uuid error", err.Error())
	}
}

func TestLifecycleStartInteractivePersistsRemoteControl(t *testing.T) {
	original := startNewInteractiveFunc
	t.Cleanup(func() {
		startNewInteractiveFunc = original
	})

	var capturedEnv map[string]string
	var capturedWorkDir string
	var capturedForceRemoteControl bool
	var capturedSessionID string
	startNewInteractiveFunc = func(env map[string]string, _ string, workDir string, forceRemoteControl bool, sessionID string) error {
		capturedEnv = map[string]string{}
		maps.Copy(capturedEnv, env)
		capturedWorkDir = workDir
		capturedForceRemoteControl = forceRemoteControl
		capturedSessionID = sessionID
		return nil
	}

	store := &stubSessionSettingsStore{}
	lifecycle := NewLifecycle(store)
	err := lifecycle.StartInteractive(context.Background(), session.StartRequest{
		SessionName: "chat-1",
		Launch: session.LaunchOptions{
			WorkDir:             "/tmp/workspace",
			Intent:              session.LaunchIntentNewSession,
			EnableRemoteControl: true,
		},
	})
	if err != nil {
		t.Fatalf("StartInteractive returned error: %v", err)
	}
	if capturedEnv["CLYDE_SESSION_NAME"] != "chat-1" {
		t.Fatalf("captured session name = %q, want chat-1", capturedEnv["CLYDE_SESSION_NAME"])
	}
	if capturedEnv["CLYDE_LAUNCH_CWD"] != "/tmp/workspace" {
		t.Fatalf("captured launch cwd = %q, want /tmp/workspace", capturedEnv["CLYDE_LAUNCH_CWD"])
	}
	if capturedWorkDir != "/tmp/workspace" {
		t.Fatalf("captured workDir = %q, want /tmp/workspace", capturedWorkDir)
	}
	if !capturedForceRemoteControl {
		t.Fatalf("captured forceRemoteControl = false, want true")
	}
	if capturedSessionID == "" {
		t.Fatal("captured sessionID is empty, want generated id")
	}
	if store.saved == nil || !store.saved.RemoteControl {
		t.Fatalf("persisted settings = %#v, want remoteControl=true", store.saved)
	}
}

func TestLifecycleStartInteractiveSkipsRemoteControlPersistenceWhenDisabled(t *testing.T) {
	original := startNewInteractiveFunc
	t.Cleanup(func() {
		startNewInteractiveFunc = original
	})

	startNewInteractiveFunc = func(_ map[string]string, _ string, _ string, _ bool, _ string) error {
		return nil
	}

	store := &stubSessionSettingsStore{}
	lifecycle := NewLifecycle(store)
	err := lifecycle.StartInteractive(context.Background(), session.StartRequest{
		SessionName: "chat-1",
		Launch: session.LaunchOptions{
			WorkDir:             "/tmp/workspace",
			Intent:              session.LaunchIntentNewSession,
			EnableRemoteControl: false,
		},
	})
	if err != nil {
		t.Fatalf("StartInteractive returned error: %v", err)
	}
	if store.saved != nil {
		t.Fatalf("persisted settings = %#v, want nil", store.saved)
	}
}

func TestLifecycleResumeInteractive(t *testing.T) {
	original := resumeInteractiveFunc
	t.Cleanup(func() {
		resumeInteractiveFunc = original
	})

	var capturedRoot string
	var capturedSession *session.Session
	var capturedOptions ResumeOptions
	resumeInteractiveFunc = func(clydeRoot string, sess *session.Session, opts ResumeOptions) error {
		capturedRoot = clydeRoot
		capturedSession = sess
		capturedOptions = opts
		return nil
	}

	sess := &session.Session{Name: "chat-1"}
	lifecycle := NewLifecycle(nil)
	err := lifecycle.ResumeInteractive(context.Background(), session.ResumeRequest{
		Session: sess,
		Options: session.ResumeOptions{
			CurrentWorkDir:   "/tmp/workspace",
			EnableSelfReload: true,
		},
	})
	if err != nil {
		t.Fatalf("ResumeInteractive returned error: %v", err)
	}
	if capturedRoot != config.GlobalDataDir() {
		t.Fatalf("captured root = %q, want %q", capturedRoot, config.GlobalDataDir())
	}
	if capturedSession != sess {
		t.Fatalf("captured session = %#v, want %#v", capturedSession, sess)
	}
	if capturedOptions.CurrentWorkDir != "/tmp/workspace" {
		t.Fatalf("captured currentWorkDir = %q, want /tmp/workspace", capturedOptions.CurrentWorkDir)
	}
	if !capturedOptions.EnableSelfReload {
		t.Fatalf("captured enableSelfReload = false, want true")
	}
}

func TestLifecycleResumeOpaqueInteractive(t *testing.T) {
	original := resumeOpaqueInteractiveFunc
	t.Cleanup(func() {
		resumeOpaqueInteractiveFunc = original
	})

	var capturedQuery string
	var capturedArgs []string
	resumeOpaqueInteractiveFunc = func(query string, additionalArgs []string) error {
		capturedQuery = query
		capturedArgs = append([]string(nil), additionalArgs...)
		return nil
	}

	lifecycle := NewLifecycle(nil)
	err := lifecycle.ResumeOpaqueInteractive(context.Background(), session.OpaqueResumeRequest{
		Query:          "native-chat",
		AdditionalArgs: []string{"--verbose"},
	})
	if err != nil {
		t.Fatalf("ResumeOpaqueInteractive returned error: %v", err)
	}
	if capturedQuery != "native-chat" {
		t.Fatalf("captured query = %q, want native-chat", capturedQuery)
	}
	if len(capturedArgs) != 1 || capturedArgs[0] != "--verbose" {
		t.Fatalf("captured additionalArgs = %v, want [--verbose]", capturedArgs)
	}
}

func TestLifecycleResumeInstructions(t *testing.T) {
	lifecycle := NewLifecycle(nil)
	sess := &session.Session{
		Name: "chat-1",
		Metadata: session.Metadata{
			SessionID: "session-123",
		},
	}
	lines := lifecycle.ResumeInstructions(sess)
	if len(lines) != 1 || lines[0] != "claude --resume session-123" {
		t.Fatalf("ResumeInstructions returned %v, want [claude --resume session-123]", lines)
	}
}

func TestSelfReloadCurrentProcessRejectsBadBinaryBeforeExec(t *testing.T) {
	badBinary := filepath.Join(t.TempDir(), "clyde")
	script := "#!/usr/bin/env bash\nprintf 'not-clyde\\n'\n"
	if err := os.WriteFile(badBinary, []byte(script), 0o755); err != nil {
		t.Fatalf("write bad binary: %v", err)
	}

	oldExecutablePath := wrapperExecutablePath
	oldExecWrapperProcess := execWrapperProcess
	t.Cleanup(func() {
		wrapperExecutablePath = oldExecutablePath
		execWrapperProcess = oldExecWrapperProcess
	})

	execCalled := false
	wrapperExecutablePath = func() (string, error) {
		return badBinary, nil
	}
	execWrapperProcess = func(string, []string, []string) error {
		execCalled = true
		return nil
	}

	if err := selfReloadCurrentProcess(); err != nil {
		t.Fatalf("selfReloadCurrentProcess returned error: %v", err)
	}
	if execCalled {
		t.Fatalf("exec was called for invalid binary")
	}
}
