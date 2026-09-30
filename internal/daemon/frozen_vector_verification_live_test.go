//go:build live

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/column"
	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/lm-semantic-search/library"
	librarymilvus "goodkind.io/lm-semantic-search/library/milvus"
	"goodkind.io/lm-semantic-search/library/observation"
)

type frozenVectorObservation struct {
	mutex  sync.Mutex
	events []observation.Event
}

func (observer *frozenVectorObservation) Observe(event observation.Event) {
	observer.mutex.Lock()
	defer observer.mutex.Unlock()
	observer.events = append(observer.events, event)
}

func (observer *frozenVectorObservation) requests(run string) []int {
	observer.mutex.Lock()
	defer observer.mutex.Unlock()
	var requests []int
	for _, event := range observer.events {
		if event.Scope.RunID == run && event.Operation == observation.StrongVerification && event.Phase == observation.Completed {
			requests = append(requests, event.Data.Vector.Requested)
		}
	}
	return requests
}

// TestFrozenVectorVerificationBatches requires an explicitly registered isolated database.
func TestFrozenVectorVerificationBatches(t *testing.T) {
	root := os.Getenv("CLYDE_FROZEN_FIXTURE_EVIDENCE_ROOT")
	if root == "" {
		return
	}
	database := os.Getenv("CLYDE_FROZEN_FIXTURE_DATABASE")
	address, err := frozenCorpusMilvusAddress()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	client := openFrozenVectorFixture(t, ctx, root, database, address)
	observer := &frozenVectorObservation{}
	store, err := librarymilvus.New(client, librarymilvus.Config{Database: database, Collection: "frozen_vectors", Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := json.Marshal(struct {
		CatalogUUID string `json:"catalog_uuid"`
		Dimension   int    `json:"dimension"`
	}{CatalogUUID: database, Dimension: liveEmbeddingDimension})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.BindCatalog(ctx, string(binding)); err != nil {
		t.Fatal(err)
	}
	identities := seedFrozenVerificationVectors(t, ctx, client)
	defer func() {
		observer.mutex.Lock()
		defer observer.mutex.Unlock()
		file, openErr := os.OpenFile(filepath.Join(root, "vector-verification-operations.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr != nil {
			t.Error(openErr)
			return
		}
		for _, event := range observer.events {
			if encodeErr := json.NewEncoder(file).Encode(event); encodeErr != nil {
				t.Error(encodeErr)
			}
		}
		if closeErr := errors.Join(file.Sync(), file.Close()); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	verifiedCtx := observation.WithScope(ctx, observation.Scope{RunID: "verified"})
	if err = verifyFrozenVectorBatches(verifiedCtx, store, identities); err != nil {
		t.Fatal(err)
	}
	if requests := observer.requests("verified"); !slices.Equal(requests, []int{256, 1}) {
		t.Fatalf("actual strong-read requests=%v, want [256 1]", requests)
	}
	missing := slices.Clone(identities)
	missing[256].ID = strings.Repeat("f", 64)
	if err = verifyFrozenVectorBatches(observation.WithScope(ctx, observation.Scope{RunID: "missing"}), store, missing); !errors.Is(err, library.ErrVectorMissing) {
		t.Fatalf("second-batch missing vector error=%v", err)
	}
	corrupt := slices.Clone(identities)
	corrupt[256].Checksum = strings.Repeat("0", 64)
	if err = verifyFrozenVectorBatches(observation.WithScope(ctx, observation.Scope{RunID: "corrupt"}), store, corrupt); !errors.Is(err, library.ErrVectorCorrupt) {
		t.Fatalf("second-batch corrupt vector error=%v", err)
	}
	cancelledCtx, stop := context.WithCancel(observation.WithScope(ctx, observation.Scope{RunID: "cancelled"}))
	stop()
	if err = verifyFrozenVectorBatches(cancelledCtx, store, identities); !errors.Is(err, context.Canceled) || len(observer.requests("cancelled")) != 0 {
		t.Fatalf("cancelled verification error=%v requests=%v", err, observer.requests("cancelled"))
	}
	t.Logf("verified 257 actual canonical rows with requests=%v; second-batch missing/corrupt and cancellation passed without embedding", observer.requests("verified"))
}

func seedFrozenVerificationVectors(t *testing.T, ctx context.Context, client *milvusclient.Client) []library.VectorIdentity {
	t.Helper()
	values := make([]float32, liveEmbeddingDimension)
	values[0] = 1
	encoded := make([]byte, binary.Size(values))
	_, err := binary.Encode(encoded, binary.LittleEndian, values)
	if err != nil {
		t.Fatal(err)
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256(encoded))
	ids := make([]string, 257)
	digests := make([]string, 257)
	checksums := make([]string, 257)
	vectors := make([][]float32, 257)
	identities := make([]library.VectorIdentity, 257)
	for index := range identities {
		ids[index] = fmt.Sprintf("%x", sha256.Sum256(fmt.Appendf(nil, "frozen-vector-%d", index)))
		digests[index] = ids[index]
		checksums[index] = checksum
		vectors[index] = values
		identities[index] = library.VectorIdentity{ID: ids[index], IdentityDigest: digests[index], Checksum: checksum}
	}
	result, err := client.Upsert(ctx, milvusclient.NewColumnBasedInsertOption("frozen_vectors",
		column.NewColumnVarChar("vector_id", ids), column.NewColumnVarChar("identity_digest", digests),
		column.NewColumnVarChar("vector_checksum", checksums), column.NewColumnFloatVector("vector", liveEmbeddingDimension, vectors)))
	if err != nil {
		t.Fatalf("actual canonical row upsert failed: %v", err)
	}
	if result.UpsertCount != int64(len(identities)) {
		t.Fatalf("actual canonical row upsert count=%d, want %d", result.UpsertCount, len(identities))
	}
	return identities
}

func openFrozenVectorFixture(t *testing.T, ctx context.Context, root, database, address string) *milvusclient.Client {
	t.Helper()
	if filepath.Dir(root) != "/private/tmp" || !strings.HasPrefix(filepath.Base(root), "lms-frozen-fixture-") || !strings.HasPrefix(database, "clyde_frozen_") {
		t.Fatal("vector fixture requires a private frozen evidence root and exact fresh database")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private physical evidence directory required: %v", err)
	}
	admin, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeFrozenAdmin(t.Context(), admin); closeErr != nil {
			t.Error(closeErr)
		}
	})
	existing, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil || slices.Contains(existing, database) {
		t.Fatalf("exact vector database prior absence failed: %v", err)
	}
	registryPath := filepath.Join(root, "database-registration.jsonl")
	registryInfo, err := os.Lstat(registryPath)
	if err != nil || !registryInfo.Mode().IsRegular() || registryInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("private parent database registry required: %v", err)
	}
	registry, err := os.OpenFile(registryPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	opened, statErr := registry.Stat()
	if statErr != nil || !os.SameFile(registryInfo, opened) {
		t.Fatal(errors.Join(statErr, registry.Close(), errors.New("database registry changed before append")))
	}
	entry := struct {
		Database        string `json:"database"`
		Address         string `json:"address"`
		AbsenceVerified bool   `json:"absence_verified"`
	}{Database: database, Address: address, AbsenceVerified: true}
	encodeErr := json.NewEncoder(registry).Encode(entry)
	if err = errors.Join(encodeErr, registry.Sync(), registry.Close()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer stop()
		databases, inspectErr := admin.ListDatabase(cleanupCtx, milvusclient.NewListDatabaseOption())
		if inspectErr != nil {
			t.Error(inspectErr)
			return
		}
		if slices.Contains(databases, database) {
			if dropErr := dropFrozenDatabase(cleanupCtx, admin, address, database); dropErr != nil {
				t.Error(dropErr)
			}
		}
	})
	if err = admin.CreateDatabase(ctx, milvusclient.NewCreateDatabaseOption(database)); err != nil {
		t.Fatal(err)
	}
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: database})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := closeFrozenAdmin(t.Context(), client); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return client
}
