//go:build live

package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/config"
)

func TestEmbeddedMilvusConfig(t *testing.T) {
	address := os.Getenv("CLYDE_TEST_MILVUS_ADDRESS")
	if address == "" {
		t.Fatal("CLYDE_TEST_MILVUS_ADDRESS is required")
	}
	for _, mode := range []string{"", "large_topk"} {
		t.Run("mode_"+mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			admin, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeLiveMilvusAdmin(t, admin) })
			random := make([]byte, 16)
			if _, err := rand.Read(random); err != nil {
				t.Fatal(err)
			}
			database := "clyde_query_" + hex.EncodeToString(random)
			existing, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
			if err != nil || slices.Contains(existing, database) {
				t.Fatalf("isolated database absence = %v, %v", existing, err)
			}
			registerLiveMilvusDatabase(t, database, address)
			t.Logf("isolated database %s was absent before creation", database)
			t.Cleanup(func() { dropLiveMilvusDatabase(t, admin, database) })
			if err := admin.CreateDatabase(ctx, milvusclient.NewCreateDatabaseOption(database)); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			endpoint := "http://" + listener.Addr().String() + "/v1"
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", root)
			attempts := 1
			cfg := config.NewConfig()
			cfg.Conversation.Semantic = config.ConversationSemanticConfig{
				ProjectionProfile: config.ConversationProjectionProfileSourceSpan,
				SearchEnabled:     true, CollectionID: "clyde-config",
				CatalogPath: filepath.Join(root, "catalog.sqlite"), LockPath: filepath.Join(root, "catalog.lock"), PoolID: "config-live",
				MilvusAddress: address, MilvusDatabase: database, MilvusCollection: "config_vectors",
				MilvusQueryMode: config.ConversationSemanticMilvusQueryMode(mode), MilvusMaxVerifyBatchRows: 512,
				EmbeddingBaseURL: endpoint, EmbeddingModel: "config-live", EmbeddingRevision: "config-live",
				EmbeddingMaxAttempts: &attempts, VectorDimension: 4, Normalization: "l2",
				QueryTimeout: config.Duration(30 * time.Second), QueryBlockSize: 16384,
			}
			if mode == "large_topk" {
				cfg.Conversation.Semantic.MilvusMaxScoreWindow = 32768
				cfg.Conversation.Semantic.QueryBlockSize = 32768
			}
			data, err := toml.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			path := config.GlobalConfigPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := config.LoadGlobalOrDefault()
			if err != nil {
				t.Fatal(err)
			}
			outboxPath := filepath.Join(root, "outbox.sqlite")
			lock, err := lockConversationSemanticOutbox(ctx, outboxPath)
			if err != nil {
				t.Fatal(err)
			}
			store, err := openEmbeddedConversationStore(ctx, loaded.Conversation.Semantic, outboxPath, lock, slog.Default())
			if err != nil {
				if closeErr := lock.Close(); closeErr != nil {
					t.Errorf("close failed opener lock: %v", closeErr)
				}
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
				defer stop()
				if err := store.close(cleanupCtx); err != nil {
					t.Errorf("close isolated store: %v", err)
				}
			})
			collection, err := store.milvusClient.DescribeCollection(ctx, milvusclient.NewDescribeCollectionOption("config_vectors"))
			if err != nil {
				t.Fatal(err)
			}
			actualMode := collection.Properties["query_mode"]
			if actualMode == "" {
				actualMode = "normal"
			}
			wantedMode := mode
			if wantedMode == "" {
				wantedMode = "normal"
			}
			if actualMode != wantedMode {
				t.Fatalf("collection query mode = %q, want %q", actualMode, wantedMode)
			}
		})
	}
}
