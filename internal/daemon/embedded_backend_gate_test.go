package daemon

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/livetrack"
)

// embeddedGatePassTimeout bounds the wait for the first sync pass.
const embeddedGatePassTimeout = 10 * time.Second

// TestSemanticSyncStartLeavesEmbeddedStoreOffForProductionConfig loads two
// production-shaped [conversation.semantic] sections through the config
// loader and starts the semantic runtime and sync the way RunContext starts
// them, with a real lifecycle group and a real conversation index over a Codex
// rollout under temporary roots. Neither configuration may open the embedded
// runtime: no outbox directory, no catalog file, and no embedded runtime log
// record may appear. With ingestion enabled, the lm-semantic-search feeder
// must run its pass and skip it because the socket does not exist.
func TestSemanticSyncStartLeavesEmbeddedStoreOffForProductionConfig(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		ingestion      bool
		wantLMSSkipped bool
	}{
		{name: "ingestion_disabled", ingestion: false, wantLMSSkipped: false},
		{name: "ingestion_enabled_missing_socket", ingestion: true, wantLMSSkipped: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stores := isolateEmbeddedProjectionStores(t)
			ageLiveArtifactForGate(t, writeEmbeddedProjectionCodexRollout(t, stores))
			cfg := loadEmbeddedGateConfig(t, testCase.ingestion)
			if cfg.Conversation.Semantic.Backend != "" {
				t.Fatalf("loaded backend = %q, want empty", cfg.Conversation.Semantic.Backend)
			}
			capture := &semanticLogCapture{mu: sync.Mutex{}, records: nil}
			log := slog.New(capture)
			group := newLifecycleGroup(log)
			index := newEmbeddedProjectionIndex()
			if err := index.Refresh(t.Context()); err != nil {
				t.Fatalf("refresh conversation index: %v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			runtime := startConversationSemanticRuntime(ctx, cfg, log, group)
			var resolveClient conversationSemanticClientResolver
			if runtime != nil && cfg.Conversation.Semantic.FeedsEngine() {
				resolveClient = runtime.syncClient
			}
			if err := startConfiguredConversationSemanticSync(ctx, log, cfg, index, resolveClient, newConversationSemanticFreshness(), newEmbeddedSemanticStatus(), newEmbeddedReconcileGate(log), group); err != nil {
				t.Fatalf("start configured semantic sync: %v", err)
			}
			if testCase.wantLMSSkipped {
				awaitEmbeddedGateMessage(t, capture, "daemon.conversation_semantic_sync.pass_skipped_engine_unavailable")
			}
			group.Quiesce(context.Background(), "test", livetrack.Budget{Cap: 5 * time.Second, IdleGrace: 0})

			for _, message := range embeddedGateMessages(capture) {
				if strings.HasPrefix(message, "daemon.conversation_semantic_embedded.") {
					t.Fatalf("log record %q shows that the embedded runtime started", message)
				}
			}
			stateSemanticDir := filepath.Join(config.DefaultStateDir(), "conversation-semantic")
			if _, err := os.Stat(stateSemanticDir); !os.IsNotExist(err) {
				t.Fatalf("stat %s = %v, want no conversation-semantic directory", stateSemanticDir, err)
			}
			assertNoEmbeddedStoreFiles(t, filepath.Dir(filepath.Dir(stores.codexHome)))
		})
	}
}

// loadEmbeddedGateConfig writes a production-shaped semantic section to the
// global config path under the temporary XDG config root and loads it.
func loadEmbeddedGateConfig(t *testing.T, ingestion bool) *config.Config {
	t.Helper()
	body := "[conversation.semantic]\nsearch_enabled = false\ncollection_id = \"clyde-conversations\"\n"
	if ingestion {
		body += "ingestion_enabled = true\nsocket_path = \"" + filepath.Join(t.TempDir(), "missing-semantic.sock") + "\"\n"
	} else {
		body += "ingestion_enabled = false\n"
	}
	path := config.GlobalConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func ageLiveArtifactForGate(t *testing.T, path string) {
	t.Helper()
	aged := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

func embeddedGateMessages(capture *semanticLogCapture) []string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	messages := make([]string, 0, len(capture.records))
	for _, record := range capture.records {
		messages = append(messages, record.Message)
	}
	return messages
}

func awaitEmbeddedGateMessage(t *testing.T, capture *semanticLogCapture, want string) {
	t.Helper()
	deadline := time.After(embeddedGatePassTimeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		for _, message := range embeddedGateMessages(capture) {
			if message == want {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("no %q log record within %s; records: %q", want, embeddedGatePassTimeout, embeddedGateMessages(capture))
		case <-ticker.C:
		}
	}
}

// assertNoEmbeddedStoreFiles walks the temporary root and fails on any outbox
// or catalog file.
func assertNoEmbeddedStoreFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if strings.HasPrefix(name, "outbox-") || strings.HasPrefix(name, "catalog.") {
			t.Fatalf("found embedded store file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
