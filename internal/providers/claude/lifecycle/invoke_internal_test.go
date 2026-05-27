package claude

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
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
	store := session.NewFileStore(clydeRoot)
	sess := session.NewSession("chat-1", "uuid-1")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if got := sessionSettingsFileForSession(clydeRoot, sess); got != "" {
		t.Fatalf("sessionSettingsFileForSession without file = %q, want empty", got)
	}

	settingsPath := filepath.Join(config.GetSessionDir(clydeRoot, sess.StorageKey()), "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"remoteControl":true}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	if got := sessionSettingsFileForSession(clydeRoot, sess); got != settingsPath {
		t.Fatalf("sessionSettingsFileForSession with file = %q, want %q", got, settingsPath)
	}
	sess.Name = "renamed-chat"
	if got := sessionSettingsFileForSession(clydeRoot, sess); got != settingsPath {
		t.Fatalf("sessionSettingsFileForSession after rename = %q, want %q", got, settingsPath)
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

func TestLaunchSessionIDPrefersExplicitAndFallsBackToEnv(t *testing.T) {
	env := map[string]string{"CLYDE_SESSION_ID": " env-uuid "}

	if got := launchSessionID(env, " explicit-uuid "); got != "explicit-uuid" {
		t.Fatalf("launchSessionID explicit = %q, want explicit-uuid", got)
	}
	if got := launchSessionID(env, ""); got != "env-uuid" {
		t.Fatalf("launchSessionID env fallback = %q, want env-uuid", got)
	}
	if got := launchSessionID(nil, ""); got != "" {
		t.Fatalf("launchSessionID nil env = %q, want empty", got)
	}
}

func TestLifecycleRenameSessionAppendsClaudeRenameEntries(t *testing.T) {
	transcriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(`{"type":"system","sessionId":"uuid-1","timestamp":"2026-04-12T23:52:12Z","entrypoint":"cli","cwd":"/tmp"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	lifecycle := NewLifecycle(nil)
	sess := &session.Session{
		Name: "chat-1",
		Metadata: session.Metadata{
			SessionID:      "uuid-1",
			TranscriptPath: transcriptPath,
		},
	}
	if err := lifecycle.RenameSession(context.Background(), sess, "provider-rename"); err != nil {
		t.Fatalf("RenameSession returned error: %v", err)
	}

	content, err := os.ReadFile(transcriptPath)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	got := string(content)
	if !strings.Contains(got, `"type":"custom-title"`) || !strings.Contains(got, `"customTitle":"provider-rename"`) {
		t.Fatalf("transcript missing custom-title entry: %s", got)
	}
	if !strings.Contains(got, `"type":"agent-name"`) || !strings.Contains(got, `"agentName":"provider-rename"`) {
		t.Fatalf("transcript missing agent-name entry: %s", got)
	}
}

func TestLifecycleReadCurrentTitleReturnsEmptyWithoutCustomTitle(t *testing.T) {
	transcriptPath := writeClaudeTitleTranscript(t, ""+
		`{"type":"system","sessionId":"uuid-1","timestamp":"2026-04-12T23:52:12Z"}`+"\n"+
		`{"type":"assistant","sessionId":"uuid-1"}`+"\n")

	got, err := NewLifecycle(nil).ReadCurrentTitle(transcriptPath)
	if err != nil {
		t.Fatalf("ReadCurrentTitle returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("ReadCurrentTitle = %q, want empty", got)
	}
}

func TestLifecycleReadCurrentTitleReturnsSingleCustomTitle(t *testing.T) {
	transcriptPath := writeClaudeTitleTranscript(t, ""+
		`{"type":"system","sessionId":"uuid-1","timestamp":"2026-04-12T23:52:12Z"}`+"\n"+
		`{"type":"custom-title","customTitle":"one title","sessionId":"uuid-1"}`+"\n")

	got, err := NewLifecycle(nil).ReadCurrentTitle(transcriptPath)
	if err != nil {
		t.Fatalf("ReadCurrentTitle returned error: %v", err)
	}
	if got != "one title" {
		t.Fatalf("ReadCurrentTitle = %q, want %q", got, "one title")
	}
}

func TestLifecycleReadCurrentTitleReturnsLatestCustomTitle(t *testing.T) {
	transcriptPath := writeClaudeTitleTranscript(t, ""+
		`{"type":"custom-title","customTitle":"first title","sessionId":"uuid-1"}`+"\n"+
		`{"type":"assistant","sessionId":"uuid-1"}`+"\n"+
		`{"type":"custom-title","customTitle":"final title","sessionId":"uuid-1"}`+"\n")

	got, err := NewLifecycle(nil).ReadCurrentTitle(transcriptPath)
	if err != nil {
		t.Fatalf("ReadCurrentTitle returned error: %v", err)
	}
	if got != "final title" {
		t.Fatalf("ReadCurrentTitle = %q, want %q", got, "final title")
	}
}

func TestLifecycleReadCurrentTitleFallsBackToHeadWindow(t *testing.T) {
	transcriptPath := writeClaudeTitleTranscript(t, ""+
		`{"type":"custom-title","customTitle":"head title","sessionId":"uuid-1"}`+"\n"+
		strings.Repeat(`{"type":"assistant","sessionId":"uuid-1","message":"padding"}`+"\n", 1500))

	got, err := NewLifecycle(nil).ReadCurrentTitle(transcriptPath)
	if err != nil {
		t.Fatalf("ReadCurrentTitle returned error: %v", err)
	}
	if got != "head title" {
		t.Fatalf("ReadCurrentTitle = %q, want %q", got, "head title")
	}
}

func writeClaudeTitleTranscript(t *testing.T, body string) string {
	t.Helper()
	transcriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return transcriptPath
}

func TestApplyMITMEnvAddsAnthropicBaseURLForWrapperLaunch(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := []byte("[mitm]\nenabled_default = true\nproviders = [\"claude\"]\ncapture_dir = \"" + t.TempDir() + "\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(_ context.Context, provider string) (daemon.ProviderLaunchEnvironment, error) {
		if provider != "claude" {
			t.Fatalf("provider = %q, want claude", provider)
		}
		return daemon.ProviderLaunchEnvironment{
			Environment: []*clydev1.EnvironmentVariable{
				{Key: "ANTHROPIC_BASE_URL", Value: "http://daemon.example"},
			},
		}, nil
	}

	env := map[string]string{
		"ANTHROPIC_BASE_URL": "https://old.example",
		"KEEP":               "1",
	}
	applyDaemonLaunchEnv(env)

	if env["KEEP"] != "1" {
		t.Fatalf("KEEP env = %q, want 1", env["KEEP"])
	}
	baseURL := env["ANTHROPIC_BASE_URL"]
	if baseURL != "http://daemon.example" {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want daemon-provided MITM proxy", baseURL)
	}
	if env["CLYDE_MITM_ANTHROPIC_BASE_URL"] != "1" {
		t.Fatalf("CLYDE_MITM_ANTHROPIC_BASE_URL=%q, want 1", env["CLYDE_MITM_ANTHROPIC_BASE_URL"])
	}
}

func TestApplyMITMEnvFailsOpenWhenDaemonUnavailable(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := []byte("[mitm]\nenabled_default = true\nproviders = [\"claude\"]\ncapture_dir = \"" + t.TempDir() + "\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(context.Context, string) (daemon.ProviderLaunchEnvironment, error) {
		return daemon.ProviderLaunchEnvironment{}, errors.New("daemon unavailable")
	}

	env := map[string]string{
		"ANTHROPIC_BASE_URL": "https://old.example",
		"KEEP":               "1",
	}
	applyDaemonLaunchEnv(env)

	if env["KEEP"] != "1" {
		t.Fatalf("KEEP env = %q, want 1", env["KEEP"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://old.example" {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want original value after fail open", env["ANTHROPIC_BASE_URL"])
	}
}

func TestApplyMITMEnvDropsInheritedLoopbackWhenDaemonUnavailable(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := []byte("[mitm]\nenabled_default = true\nproviders = [\"claude\"]\ncapture_dir = \"" + t.TempDir() + "\"\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(context.Context, string) (daemon.ProviderLaunchEnvironment, error) {
		return daemon.ProviderLaunchEnvironment{}, errors.New("daemon unavailable")
	}

	env := map[string]string{
		"ANTHROPIC_BASE_URL": "http://[::1]:50067",
		"KEEP":               "1",
	}
	applyDaemonLaunchEnv(env)

	if env["KEEP"] != "1" {
		t.Fatalf("KEEP env = %q, want 1", env["KEEP"])
	}
	// SanitizeMITMMap strips the stale loopback override, and the daemon-fail
	// branch then fills the adapter loopback default so a daemon-spawned claude
	// still reaches the adapter rather than calling Anthropic directly.
	if env["ANTHROPIC_BASE_URL"] != defaultAnthropicAdapterURL {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want adapter loopback default %q", env["ANTHROPIC_BASE_URL"], defaultAnthropicAdapterURL)
	}
}

func TestLaunchEnvDefaultsAnthropicBaseURLWhenMissing(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := []byte("[mitm]\nenabled_default = false\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(context.Context, string) (daemon.ProviderLaunchEnvironment, error) {
		return daemon.ProviderLaunchEnvironment{}, nil
	}

	env := map[string]string{
		"KEEP": "1",
	}
	applyDaemonLaunchEnv(env)

	if env["KEEP"] != "1" {
		t.Fatalf("KEEP env = %q, want 1", env["KEEP"])
	}
	if env["ANTHROPIC_BASE_URL"] != defaultAnthropicAdapterURL {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want adapter loopback default %q", env["ANTHROPIC_BASE_URL"], defaultAnthropicAdapterURL)
	}
}

func TestLaunchEnvPreservesAnthropicBaseURLOverride(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	cfgDir := filepath.Join(configHome, "clyde")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	cfg := []byte("[mitm]\nenabled_default = false\n")
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), cfg, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	oldLaunchEnvironment := providerLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		providerLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	providerLaunchEnvironmentViaDaemon = func(context.Context, string) (daemon.ProviderLaunchEnvironment, error) {
		return daemon.ProviderLaunchEnvironment{}, nil
	}

	env := map[string]string{
		"ANTHROPIC_BASE_URL": "https://my.override.example",
		"KEEP":               "1",
	}
	applyDaemonLaunchEnv(env)

	if env["KEEP"] != "1" {
		t.Fatalf("KEEP env = %q, want 1", env["KEEP"])
	}
	if env["ANTHROPIC_BASE_URL"] != "https://my.override.example" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q, want caller-supplied override", env["ANTHROPIC_BASE_URL"])
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
	if capturedEnv["CLYDE_SESSION_ID"] != capturedSessionID {
		t.Fatalf("captured session id env = %q, want %q", capturedEnv["CLYDE_SESSION_ID"], capturedSessionID)
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
