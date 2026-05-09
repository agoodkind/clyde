package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/config"
	codexstore "goodkind.io/clyde/internal/providers/codex/store"
	"goodkind.io/clyde/internal/session"
)

func TestApplyAutoRenameMarksTranscriptStateAndHash(t *testing.T) {
	setupDaemonTestHome(t)
	srv := newTestServer(t)

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	sess := session.NewSession("repo-12345678", "uuid")
	transcriptPath := filepath.Join(t.TempDir(), "session.jsonl")
	transcriptFile, err := os.Create(transcriptPath)
	if err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	encoder := json.NewEncoder(transcriptFile)
	if err := encoder.Encode(map[string]string{
		"type":      "summary",
		"summary":   "repo-12345678",
		"sessionId": "uuid",
	}); err != nil {
		t.Fatalf("encode transcript: %v", err)
	}
	if err := transcriptFile.Close(); err != nil {
		t.Fatalf("close transcript: %v", err)
	}
	sess.Metadata.SetProviderTranscriptPath(transcriptPath)
	if err := store.Create(sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := srv.ApplyAutoRename(context.Background(), "repo-12345678", "ship-rename-funnel", "abc123"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	updated, err := store.Get("ship-rename-funnel")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Metadata.AutoNameState != session.AutoNameStateApplied {
		t.Fatalf("state=%q", updated.Metadata.AutoNameState)
	}
	if updated.Metadata.AutoNameSource != session.AutoNameSourceTranscript {
		t.Fatalf("source=%q", updated.Metadata.AutoNameSource)
	}
	if updated.Metadata.AutoNameSourceHash != "abc123" {
		t.Fatalf("hash=%q", updated.Metadata.AutoNameSourceHash)
	}
}

func TestRunAutoRenamePassBackfillsLegacySessions(t *testing.T) {
	setupDaemonTestHome(t)
	srv := newTestServer(t)

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	legacy := session.NewSession("legacy-typed", "uuid-legacy")
	if err := store.Create(legacy); err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	autoShape := session.NewSession("repo-deadbeef", "uuid-auto")
	if err := store.Create(autoShape); err != nil {
		t.Fatalf("create auto: %v", err)
	}

	srv.runAutoRenamePass(context.Background())

	gotLegacy, _ := store.Get("legacy-typed")
	if gotLegacy.Metadata.AutoNameState != session.AutoNameStateUserLocked {
		t.Fatalf("legacy state=%q", gotLegacy.Metadata.AutoNameState)
	}
	if gotLegacy.Metadata.AutoNameSource != session.AutoNameSourceUser {
		t.Fatalf("legacy source=%q", gotLegacy.Metadata.AutoNameSource)
	}
	gotAuto, _ := store.Get("repo-deadbeef")
	if gotAuto.Metadata.AutoNameState == session.AutoNameStateUserLocked {
		t.Fatalf("auto-shaped state=%q want not user_locked", gotAuto.Metadata.AutoNameState)
	}
}

func TestRunAutoRenamePassAppliesToCodexSession(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	srv := newTestServer(t)
	srv.configureAutoName(config.AutoNameConfig{MinUserMessages: 1})

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	transcript := filepath.Join(tmp, "codex-rollout.jsonl")
	body := []byte(
		`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}` + "\n" +
			`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"please promote the codex thread title"}]}}` + "\n",
	)
	if err := os.WriteFile(transcript, body, 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	sess := session.NewSession("repo-deadbeef", "codex-thread")
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{
			Provider: session.ProviderCodex,
			ID:       "codex-thread",
		},
	}
	sess.Metadata.SetProviderTranscriptPath(transcript)
	if err := store.Create(sess); err != nil {
		t.Fatalf("create: %v", err)
	}

	srv.runAutoRenamePass(context.Background())

	if _, err := store.Get("repo-deadbeef"); err == nil {
		t.Fatalf("old name still resolves")
	}
	renamed, err := store.Get("promote-codex-thread-title")
	if err != nil {
		t.Fatalf("get renamed: %v", err)
	}
	if renamed.Metadata.AutoNameState != session.AutoNameStateApplied {
		t.Fatalf("state=%q", renamed.Metadata.AutoNameState)
	}
	if renamed.Metadata.AutoNameSource != session.AutoNameSourceTranscript {
		t.Fatalf("source=%q", renamed.Metadata.AutoNameSource)
	}
	if renamed.Metadata.AutoNameSourceHash == "" {
		t.Fatalf("hash empty")
	}
	paths, err := codexstore.ResolveStorePathsFromEnv()
	if err != nil {
		t.Fatalf("resolve codex paths: %v", err)
	}
	index, err := codexstore.ReadSessionIndex(paths.SessionIndexPath)
	if err != nil {
		t.Fatalf("read codex index: %v", err)
	}
	if got := index.ThreadName("codex-thread"); got != "promote-codex-thread-title" {
		t.Fatalf("codex thread name=%q", got)
	}
}
