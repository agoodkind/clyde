package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

const (
	redactionMarker          = "redaction-sentinel-value"
	redactionEnvironmentName = "CLYDE_TEST_EMBEDDING_CREDENTIAL_7F3A"
	redactionOpenTimeout     = 5 * time.Second
)

// TestEmbeddedOpenNeverLogsCredentialReferences opens the embedded store
// through the daemon open path with an unreachable embedding URL and Milvus
// address, for an environment credential, a file credential, a missing file,
// and an empty environment variable. Every open fails. The slog output of a
// real text handler and the returned error must not contain the credential
// value, the environment variable name, or the key file path.
func TestEmbeddedOpenNeverLogsCredentialReferences(t *testing.T) {
	root := t.TempDir()
	keyFile := filepath.Join(root, "secrets", "embedding-credential.txt")
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(redactionMarker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingFile := filepath.Join(root, "secrets", "missing-credential.txt")
	for _, testCase := range []struct {
		name        string
		environment string
		setEnv      bool
		file        string
	}{
		{name: "environment", environment: redactionEnvironmentName, setEnv: true, file: ""},
		{name: "file", environment: "", setEnv: false, file: keyFile},
		{name: "missing_file", environment: "", setEnv: false, file: missingFile},
		{name: "empty_environment", environment: redactionEnvironmentName, setEnv: false, file: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.setEnv {
				t.Setenv(redactionEnvironmentName, redactionMarker)
			} else {
				t.Setenv(redactionEnvironmentName, "")
			}
			var logs bytes.Buffer
			original := slog.Default()
			logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			slog.SetDefault(logger)
			t.Cleanup(func() { slog.SetDefault(original) })
			semantic := config.ConversationSemanticConfig{
				IngestionEnabled:    true,
				CollectionID:        "clyde-conversations",
				Backend:             config.ConversationSemanticBackendEmbedded,
				CatalogPath:         filepath.Join(root, testCase.name, "catalog.sqlite"),
				LockPath:            filepath.Join(root, testCase.name, "catalog.lock"),
				PoolID:              "redaction-" + testCase.name,
				MilvusAddress:       "localhost:1",
				MilvusDatabase:      "clyde_redaction_test",
				MilvusCollection:    "vectors",
				EmbeddingBaseURL:    "http://localhost:1/v1",
				EmbeddingAPIKeyEnv:  testCase.environment,
				EmbeddingAPIKeyFile: testCase.file,
				EmbeddingModel:      "nvidia/NV-EmbedCode-7b-v1",
				EmbeddingRevision:   "redaction-test",
				VectorDimension:     4096,
				Normalization:       "l2",
			}
			ctx, cancel := context.WithTimeout(t.Context(), redactionOpenTimeout)
			defer cancel()
			outboxPath := filepath.Join(root, testCase.name, "outbox.sqlite")
			lock, err := lockConversationSemanticOutbox(ctx, outboxPath)
			if err != nil {
				t.Fatalf("lock outbox: %v", err)
			}
			store, err := openEmbeddedConversationStore(ctx, semantic, outboxPath, lock, logger)
			if err != nil {
				_ = lock.Close()
			}
			if err == nil {
				_ = store.close(context.WithoutCancel(ctx))
				t.Fatal("open succeeded against an unreachable Milvus address")
			}
			observed := logs.String() + "\n" + err.Error()
			for _, forbidden := range []string{redactionMarker, redactionEnvironmentName, keyFile, missingFile, filepath.Dir(keyFile)} {
				if strings.Contains(observed, forbidden) {
					t.Fatalf("log output or error contains %q:\n%s", forbidden, observed)
				}
			}
		})
	}
}
