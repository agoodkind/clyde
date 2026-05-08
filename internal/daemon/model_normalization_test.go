package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/session"
)

func TestResolveSessionSettingsNormalizesStoredModel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("chat-1", "uuid-1")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.SaveSettings("chat-1", &session.Settings{
		Model:       "clyde-gpt-5.4-1m-medium",
		EffortLevel: "medium",
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	srv := &Server{
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		globalSettings: map[string]json.RawMessage{},
	}

	_, model, effort := srv.resolveSessionSettings(context.Background(), "chat-1", "")
	if model != "clyde-gpt-5.4-1m-medium" {
		t.Fatalf("model=%q want %q", model, "clyde-gpt-5.4-1m-medium")
	}
	if effort != "medium" {
		t.Fatalf("effort=%q want %q", effort, "medium")
	}
}

func TestReadSessionSettingsNormalizesRuntimeModel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	if err := config.EnsureRuntimeDir(); err != nil {
		t.Fatalf("ensure runtime dir: %v", err)
	}
	wrapperID := "wrapper-1"
	sessionDir := config.SessionRuntimeDir(wrapperID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir session runtime: %v", err)
	}
	settingsPath := filepath.Join(sessionDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{"model":"clyde-codex-5.5-xhigh","effortLevel":"xhigh"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	srv := &Server{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		settingsLocks: make(map[string]*sync.Mutex),
	}

	model, effort := srv.readSessionSettings(wrapperID)
	if model != "clyde-codex-5.5-xhigh" {
		t.Fatalf("model=%q want %q", model, "clyde-codex-5.5-xhigh")
	}
	if effort != "xhigh" {
		t.Fatalf("effort=%q want %q", effort, "xhigh")
	}
}

func TestSessionIsActiveChecksOpenWrapperSessions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("open-chat", "open-session-id")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	srv := &Server{
		sessions: map[string]*wrapperSession{
			"wrapper-1": {wrapperID: "wrapper-1", sessionName: "open-chat", sessionID: "open-session-id"},
			"wrapper-2": {wrapperID: "wrapper-2", sessionName: ""},
		},
	}

	if !srv.sessionIsActive("open-chat") {
		t.Fatalf("expected named wrapper session to be active")
	}
	if srv.sessionIsActive("closed-chat") {
		t.Fatalf("expected unknown session to be inactive")
	}
	if err := store.Rename("open-chat", "renamed-chat"); err != nil {
		t.Fatalf("rename session: %v", err)
	}
	if !srv.sessionIsActive("renamed-chat") {
		t.Fatalf("expected renamed session to stay active by session id")
	}
	if srv.sessionIsActive("") {
		t.Fatalf("expected empty session name to be inactive")
	}
}

func TestUpdateSessionSettingsNormalizesModelBeforePersisting(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("chat-2", "uuid-2")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	srv := &Server{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		settingsLocks: make(map[string]*sync.Mutex),
	}

	_, err = srv.UpdateSessionSettings(context.Background(), &clydev1.UpdateSessionSettingsRequest{
		Name: "chat-2",
		Settings: &clydev1.Settings{
			Model:       "clyde-gpt-5.4-1m-medium",
			EffortLevel: "medium",
		},
		UpdateMask: []string{"model", "effort_level"},
	})
	if err != nil {
		t.Fatalf("update session settings: %v", err)
	}

	settings, err := store.LoadSettings("chat-2")
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if settings == nil {
		t.Fatalf("settings missing")
	}
	if settings.Model != "clyde-gpt-5.4-1m-medium" {
		t.Fatalf("settings.Model=%q want %q", settings.Model, "clyde-gpt-5.4-1m-medium")
	}
	if settings.EffortLevel != "medium" {
		t.Fatalf("settings.EffortLevel=%q want %q", settings.EffortLevel, "medium")
	}
}

func TestUpdateSessionSettingsContextWindowPreservesExistingFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("chat-context-window", "uuid-context-window")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.SaveSettings("chat-context-window", &session.Settings{
		Model:         "clyde-gpt-5.4-1m-medium",
		EffortLevel:   "medium",
		OutputStyle:   "concise",
		RemoteControl: true,
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	srv := &Server{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		settingsLocks: make(map[string]*sync.Mutex),
	}

	_, err = srv.UpdateSessionSettings(context.Background(), &clydev1.UpdateSessionSettingsRequest{
		Name: "chat-context-window",
		Settings: &clydev1.Settings{
			ContextWindow: "1m",
		},
		UpdateMask: []string{"context_window"},
	})
	if err != nil {
		t.Fatalf("update session settings: %v", err)
	}

	settings, err := store.LoadSettings("chat-context-window")
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if settings == nil {
		t.Fatalf("settings missing")
	}
	if settings.ContextWindow != "1m" {
		t.Fatalf("settings.ContextWindow=%q want %q", settings.ContextWindow, "1m")
	}
	if settings.Model != "clyde-gpt-5.4-1m-medium" {
		t.Fatalf("settings.Model=%q want %q", settings.Model, "clyde-gpt-5.4-1m-medium")
	}
	if settings.EffortLevel != "medium" {
		t.Fatalf("settings.EffortLevel=%q want %q", settings.EffortLevel, "medium")
	}
	if settings.OutputStyle != "concise" {
		t.Fatalf("settings.OutputStyle=%q want %q", settings.OutputStyle, "concise")
	}
	if !settings.RemoteControl {
		t.Fatalf("settings.RemoteControl=false want true")
	}
}

func TestWriteSettingsJSONPersistsNormalizedModel(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	srv := &Server{
		log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		globalSettings: map[string]json.RawMessage{},
	}

	settingsPath, err := srv.writeSettingsJSON(context.Background(), "wrapper-2", "clyde-codex-5.5-xhigh", "xhigh")
	if err != nil {
		t.Fatalf("writeSettingsJSON: %v", err)
	}

	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal settings.json: %v", err)
	}
	if payload["model"] != "clyde-codex-5.5-xhigh" {
		t.Fatalf("persisted model=%v want %q", payload["model"], "clyde-codex-5.5-xhigh")
	}
}

func TestSessionSummaryNormalizesSettingsModelFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("chat-3", "uuid-3")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.SaveSettings("chat-3", &session.Settings{
		Model: "clyde-gpt-5.4-1m-medium",
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	srv := &Server{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		bridges:       make(map[string]*clydev1.Bridge),
		contextStates: make(map[string]sessionContextState),
	}

	summary := srv.sessionSummary(context.Background(), store, sess)
	if summary.GetModel() != "clyde-gpt-5.4-1m-medium" {
		t.Fatalf("summary.Model=%q want %q", summary.GetModel(), "clyde-gpt-5.4-1m-medium")
	}
}

func TestSessionDetailNormalizesSettingsModelFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmp, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(tmp, "run"))

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession("chat-4", "uuid-4")
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.SaveSettings("chat-4", &session.Settings{
		Model: "clyde-codex-5.5-xhigh",
	}); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	srv := &Server{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	detail := srv.sessionDetail(context.Background(), store, sess)
	if detail.GetModel() != "clyde-codex-5.5-xhigh" {
		t.Fatalf("detail.Model=%q want %q", detail.GetModel(), "clyde-codex-5.5-xhigh")
	}
}
