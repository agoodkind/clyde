//go:build live

package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"github.com/pelletier/go-toml/v2"
	"goodkind.io/clyde/internal/config"
)

func writeEmbeddedLifecycleConfig(t *testing.T, harness *harness, configuration config.Config) {
	t.Helper()
	contents, err := toml.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness.configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

func createEmbeddedLifecycleDatabase(t *testing.T) string {
	t.Helper()
	client, err := milvusclient.New(t.Context(), &milvusclient.ClientConfig{Address: "localhost:39530"})
	if err != nil {
		t.Fatal(err)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	database := "clyde_lifecycle_" + hex.EncodeToString(random[:])
	if err := client.CreateDatabase(t.Context(), milvusclient.NewCreateDatabaseOption(database)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		scoped, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: "localhost:39530", DBName: database})
		if err != nil {
			t.Error(err)
		} else {
			collections, err := scoped.ListCollections(ctx, milvusclient.NewListCollectionOption())
			if err != nil {
				t.Error(err)
			} else {
				for _, collection := range collections {
					if err := scoped.DropCollection(ctx, milvusclient.NewDropCollectionOption(collection)); err != nil {
						t.Error(err)
					}
				}
			}
			if err := scoped.Close(ctx); err != nil {
				t.Error(err)
			}
		}
		if err := client.DropDatabase(ctx, milvusclient.NewDropDatabaseOption(database)); err != nil {
			t.Error(err)
		}
		databases, err := client.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
		if err != nil {
			t.Error(err)
		}
		for _, remaining := range databases {
			if remaining == database {
				t.Errorf("isolated database %s remains after cleanup", database)
			}
		}
		if err := client.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return database
}
