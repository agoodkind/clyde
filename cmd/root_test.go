package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/ui"
)

func TestSessionEventFromProtoMapsBinaryUpdate(t *testing.T) {
	ev := sessionEventFromProto(&clydev1.SubscribeRegistryResponse{
		Kind:         clydev1.SubscribeRegistryResponse_KIND_CLYDE_BINARY_UPDATED,
		BinaryPath:   "/tmp/clyde",
		BinaryReason: "mtime_changed",
		BinaryHash:   "abc123",
	})

	if ev.Kind != "CLYDE_BINARY_UPDATED" {
		t.Fatalf("kind=%q want CLYDE_BINARY_UPDATED", ev.Kind)
	}
	if ev.BinaryPath != "/tmp/clyde" {
		t.Fatalf("binary path=%q want /tmp/clyde", ev.BinaryPath)
	}
	if ev.BinaryReason != "mtime_changed" {
		t.Fatalf("binary reason=%q want mtime_changed", ev.BinaryReason)
	}
	if ev.BinaryHash != "abc123" {
		t.Fatalf("binary hash=%q want abc123", ev.BinaryHash)
	}
}

func TestConsumeTUIReturnSessionRestoresAndClearsEnv(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := session.NewFileStore(config.GlobalDataDir())
	want := session.NewSession("chat-one", "session-uuid")
	if err := store.Create(want); err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Setenv(ui.EnvTUIReturnSessionID, "session-uuid")
	t.Setenv(ui.EnvTUIReturnSessionName, "chat-one")

	got := consumeTUIReturnSession()

	if got == nil {
		t.Fatalf("consumeTUIReturnSession returned nil")
	}
	if got.Name != "chat-one" || got.Metadata.ProviderSessionID() != "session-uuid" {
		t.Fatalf("restored session = %s/%s, want chat-one/session-uuid", got.Name, got.Metadata.ProviderSessionID())
	}
	if value := os.Getenv(ui.EnvTUIReturnSessionID); value != "" {
		t.Fatalf("%s still set to %q", ui.EnvTUIReturnSessionID, value)
	}
	if value := os.Getenv(ui.EnvTUIReturnSessionName); value != "" {
		t.Fatalf("%s still set to %q", ui.EnvTUIReturnSessionName, value)
	}
}

func TestNextChatSessionNameUsesDisplayNameCollisionSuffix(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	store := session.NewFileStore(config.GlobalDataDir())
	oldCurrentTime := currentTime
	currentTime = func() time.Time {
		return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	}
	t.Cleanup(func() {
		currentTime = oldCurrentTime
	})

	base := "chat-20260508-120000"
	if err := store.Create(session.NewSession(base, "uuid-1")); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	got, err := nextChatSessionName(store)
	if err != nil {
		t.Fatalf("next chat session name: %v", err)
	}
	if got != "chat-20260508-120000 (2)" {
		t.Fatalf("name = %q want display-name suffix", got)
	}
}

func TestApplyPassthroughBootstrapIdentityAssignsNameAndSessionID(t *testing.T) {
	store := session.NewFileStore(t.TempDir())
	oldCurrentTime := currentTime
	currentTime = func() time.Time {
		return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	}
	t.Cleanup(func() {
		currentTime = oldCurrentTime
	})

	args, env := applyPassthroughBootstrapIdentity(
		context.Background(),
		[]string{"--model", "sonnet"},
		[]string{"KEEP=1"},
		store,
	)

	if !envContains(env, "CLYDE_SESSION_NAME", "chat-20260508-120000") {
		t.Fatalf("env missing generated display name: %v", env)
	}
	sessionID, ok := envValue(env, "CLYDE_SESSION_ID")
	if !ok || strings.TrimSpace(sessionID) == "" {
		t.Fatalf("env missing generated session id: %v", env)
	}
	if len(args) < 4 || args[0] != "--session-id" || args[1] != sessionID {
		t.Fatalf("args = %v, want leading --session-id %q", args, sessionID)
	}
	if args[2] != "--model" || args[3] != "sonnet" {
		t.Fatalf("args = %v, want original args after session id", args)
	}
}

func TestApplyPassthroughBootstrapIdentityUsesExistingExactNameAndID(t *testing.T) {
	args, env := applyPassthroughBootstrapIdentity(
		context.Background(),
		[]string{"--model", "sonnet"},
		[]string{"CLYDE_SESSION_NAME=Merry Swan", "CLYDE_SESSION_ID=stable-uuid"},
		nil,
	)

	if !envContains(env, "CLYDE_SESSION_NAME", "Merry Swan") {
		t.Fatalf("env changed display name: %v", env)
	}
	if !envContains(env, "CLYDE_SESSION_ID", "stable-uuid") {
		t.Fatalf("env changed session id: %v", env)
	}
	if len(args) < 2 || args[0] != "--session-id" || args[1] != "stable-uuid" {
		t.Fatalf("args = %v, want leading --session-id stable-uuid", args)
	}
}

func TestApplyPassthroughBootstrapIdentitySkipsResumeLaunches(t *testing.T) {
	store := session.NewFileStore(t.TempDir())
	args, env := applyPassthroughBootstrapIdentity(
		context.Background(),
		[]string{"--resume", "existing-uuid"},
		[]string{"KEEP=1"},
		store,
	)

	if len(args) != 2 || args[0] != "--resume" || args[1] != "existing-uuid" {
		t.Fatalf("args = %v, want unchanged resume args", args)
	}
	if _, ok := envValue(env, "CLYDE_SESSION_NAME"); ok {
		t.Fatalf("env unexpectedly got display name: %v", env)
	}
	if _, ok := envValue(env, "CLYDE_SESSION_ID"); ok {
		t.Fatalf("env unexpectedly got session id: %v", env)
	}
}

func TestApplyClaudeMITMEnvAddsAnthropicBaseURLForPassthrough(t *testing.T) {
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
	oldLaunchEnvironment := claudeLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		claudeLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	claudeLaunchEnvironmentViaDaemon = func(_ context.Context, provider string) (daemon.ProviderLaunchEnvironment, error) {
		switch provider {
		case "claude":
			return daemon.ProviderLaunchEnvironment{
				Environment: []*clydev1.EnvironmentVariable{
					{Key: "ANTHROPIC_BASE_URL", Value: "http://daemon.example"},
					{Key: "CLYDE_MITM_ANTHROPIC_BASE_URL", Value: "1"},
				},
			}, nil
		case "codex":
			return daemon.ProviderLaunchEnvironment{}, nil
		default:
			t.Fatalf("unexpected provider %q", provider)
			return daemon.ProviderLaunchEnvironment{}, nil
		}
	}

	got := applyClaudeMITMEnv(context.Background(), []string{"ANTHROPIC_BASE_URL=https://old.example", "KEEP=1"})

	if !envContains(got, "KEEP", "1") {
		t.Fatalf("KEEP env missing: %v", got)
	}
	baseURL, ok := envValue(got, "ANTHROPIC_BASE_URL")
	if !ok {
		t.Fatalf("ANTHROPIC_BASE_URL missing: %v", got)
	}
	if baseURL != "http://daemon.example" {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want daemon-provided MITM proxy", baseURL)
	}
	if !envContains(got, "CLYDE_MITM_ANTHROPIC_BASE_URL", "1") {
		t.Fatalf("CLYDE_MITM_ANTHROPIC_BASE_URL marker missing: %v", got)
	}
}

func TestApplyClaudeMITMEnvFailsOpenWhenDaemonUnavailable(t *testing.T) {
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
	oldLaunchEnvironment := claudeLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		claudeLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	claudeLaunchEnvironmentViaDaemon = func(context.Context, string) (daemon.ProviderLaunchEnvironment, error) {
		return daemon.ProviderLaunchEnvironment{}, errors.New("daemon unavailable")
	}

	got := applyClaudeMITMEnv(context.Background(), []string{"ANTHROPIC_BASE_URL=https://old.example", "KEEP=1"})

	if !envContains(got, "KEEP", "1") {
		t.Fatalf("KEEP env missing: %v", got)
	}
	baseURL, ok := envValue(got, "ANTHROPIC_BASE_URL")
	if !ok {
		t.Fatalf("ANTHROPIC_BASE_URL missing: %v", got)
	}
	if baseURL != "https://old.example" {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want original value after fail open", baseURL)
	}
}

func TestApplyClaudeMITMEnvDropsInheritedLoopbackWhenDaemonUnavailable(t *testing.T) {
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
	oldLaunchEnvironment := claudeLaunchEnvironmentViaDaemon
	t.Cleanup(func() {
		claudeLaunchEnvironmentViaDaemon = oldLaunchEnvironment
	})
	claudeLaunchEnvironmentViaDaemon = func(context.Context, string) (daemon.ProviderLaunchEnvironment, error) {
		return daemon.ProviderLaunchEnvironment{}, errors.New("daemon unavailable")
	}

	got := applyClaudeMITMEnv(context.Background(), []string{"ANTHROPIC_BASE_URL=http://[::1]:50067", "KEEP=1"})

	if !envContains(got, "KEEP", "1") {
		t.Fatalf("KEEP env missing: %v", got)
	}
	if baseURL, ok := envValue(got, "ANTHROPIC_BASE_URL"); ok {
		t.Fatalf("ANTHROPIC_BASE_URL=%q, want stale Clyde proxy removed", baseURL)
	}
}

func envContains(env []string, key, want string) bool {
	got, ok := envValue(env, key)
	return ok && got == want
}

func envValue(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, item := range env {
		if value, ok := strings.CutPrefix(item, prefix); ok {
			return value, true
		}
	}
	return "", false
}

// TestSessionDetailFromProtoMapsContextUsage round-trips a proto
// detail response with the new context usage fields and asserts the
// UI struct ends up with the matching values. The TUI relies on these
// fields being populated to escape the persistent "loading..." state
// in the stats pane.
func TestSessionDetailFromProtoMapsContextUsage(t *testing.T) {
	resp := &clydev1.GetSessionDetailResponse{
		SessionName:           "chat-x",
		Model:                 "claude-sonnet-4-5",
		Provider:              "codex",
		ContextTotalTokens:    42000,
		ContextMaxTokens:      200000,
		ContextPercentage:     21,
		ContextMessagesTokens: 31000,
		ContextUsageLoaded:    true,
		ContextUsageStatus:    "",
		ResumeInstructions:    []string{"codex resume codex-123"},
	}

	got := sessionDetailFromProto(resp)

	if !got.ContextUsageLoaded {
		t.Fatalf("ContextUsageLoaded=false want true")
	}
	if got.ContextUsage.TotalTokens != 42000 {
		t.Fatalf("ContextUsage.TotalTokens=%d want 42000", got.ContextUsage.TotalTokens)
	}
	if got.ContextUsage.MaxTokens != 200000 {
		t.Fatalf("ContextUsage.MaxTokens=%d want 200000", got.ContextUsage.MaxTokens)
	}
	if got.ContextUsage.Percentage != 21 {
		t.Fatalf("ContextUsage.Percentage=%d want 21", got.ContextUsage.Percentage)
	}
	if got.ContextUsage.MessagesTokens != 31000 {
		t.Fatalf("ContextUsage.MessagesTokens=%d want 31000", got.ContextUsage.MessagesTokens)
	}
	if got.ContextUsageStatus != "" {
		t.Fatalf("ContextUsageStatus=%q want empty", got.ContextUsageStatus)
	}
	if got.Provider != "codex" {
		t.Fatalf("Provider=%q want codex", got.Provider)
	}
	if len(got.ResumeInstructions) != 1 || got.ResumeInstructions[0] != "codex resume codex-123" {
		t.Fatalf("ResumeInstructions=%v want [codex resume codex-123]", got.ResumeInstructions)
	}
}

// TestSessionDetailFromProtoCarriesProbingStatus asserts the lazy-cold
// path: daemon returns ContextUsageLoaded=false with Status="probing"
// and the mapper preserves it so the TUI can render a non-blocking
// placeholder rather than the legacy permanent "loading..." string.
func TestSessionDetailFromProtoCarriesProbingStatus(t *testing.T) {
	resp := &clydev1.GetSessionDetailResponse{
		SessionName:        "chat-y",
		ContextUsageLoaded: false,
		ContextUsageStatus: "probing",
	}

	got := sessionDetailFromProto(resp)

	if got.ContextUsageLoaded {
		t.Fatalf("ContextUsageLoaded=true want false")
	}
	if got.ContextUsageStatus != "probing" {
		t.Fatalf("ContextUsageStatus=%q want %q", got.ContextUsageStatus, "probing")
	}
	if got.ContextUsage != (ui.SessionContextUsage{}) {
		t.Fatalf("ContextUsage=%+v want zero value", got.ContextUsage)
	}
}

// TestSessionSummaryFromProto_AutoNameFields round-trips a proto
// SessionSummary with the auto-rename fields through the mapper and
// asserts that the resulting Session metadata carries matching values.
// This guards the wire-to-domain hand-off the TUI consumes.
func TestSessionSummaryFromProto_AutoNameFields(t *testing.T) {
	lastAutoName := time.Unix(1700001234, 0).UTC()
	raw := &clydev1.SessionSummary{
		Name:              "chat-auto-name",
		MetadataName:      "chat-auto-name",
		SessionId:         "uuid-auto-name",
		AutoNameState:     string(session.AutoNameStateApplied),
		AutoNameSource:    string(session.AutoNameSourceLLM),
		LastAutoNameNanos: lastAutoName.UnixNano(),
	}

	sess, _, _, _, _ := sessionSummaryFromProto(raw)

	if sess.Metadata.AutoNameState != session.AutoNameStateApplied {
		t.Fatalf("AutoNameState=%q want %q", sess.Metadata.AutoNameState, session.AutoNameStateApplied)
	}
	if sess.Metadata.AutoNameSource != session.AutoNameSourceLLM {
		t.Fatalf("AutoNameSource=%q want %q", sess.Metadata.AutoNameSource, session.AutoNameSourceLLM)
	}
	if !sess.Metadata.LastAutoNameAt.Equal(lastAutoName) {
		t.Fatalf("LastAutoNameAt=%s want %s", sess.Metadata.LastAutoNameAt, lastAutoName)
	}
}

// TestSessionSummaryFromProto_AutoNameZeroFields asserts that a proto
// SessionSummary with no auto-rename fields decodes into the
// untouched/unspecified zero values rather than spurious data.
func TestSessionSummaryFromProto_AutoNameZeroFields(t *testing.T) {
	raw := &clydev1.SessionSummary{
		Name:         "chat-no-auto-name",
		MetadataName: "chat-no-auto-name",
		SessionId:    "uuid-no-auto-name",
	}

	sess, _, _, _, _ := sessionSummaryFromProto(raw)

	if sess.Metadata.AutoNameState != session.AutoNameStateUntouched {
		t.Fatalf("AutoNameState=%q want untouched", sess.Metadata.AutoNameState)
	}
	if sess.Metadata.AutoNameSource != session.AutoNameSourceUnspecified {
		t.Fatalf("AutoNameSource=%q want unspecified", sess.Metadata.AutoNameSource)
	}
	if !sess.Metadata.LastAutoNameAt.IsZero() {
		t.Fatalf("LastAutoNameAt=%s want zero", sess.Metadata.LastAutoNameAt)
	}
}

func TestSessionSummaryFromProto_RestoresProviderForResumeRouting(t *testing.T) {
	raw := &clydev1.SessionSummary{
		Name:         "codex-chat",
		MetadataName: "codex-chat",
		SessionId:    "codex-thread",
		Provider:     string(session.ProviderCodex),
		Runtime: &clydev1.ProviderRuntimeBoundary{
			History: &clydev1.SessionHistoryBoundary{
				Current: &clydev1.ProviderSessionIdentity{
					Provider:  string(session.ProviderCodex),
					SessionId: "codex-thread",
				},
			},
		},
	}

	sess, _, _, _, _ := sessionSummaryFromProto(raw)

	if sess.ProviderID() != session.ProviderCodex {
		t.Fatalf("provider=%q want %q", sess.ProviderID(), session.ProviderCodex)
	}
	if sess.Metadata.ProviderSessionID() != "codex-thread" {
		t.Fatalf("provider session id=%q want codex-thread", sess.Metadata.ProviderSessionID())
	}
}
