package capture_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
)

type retentionTestStore struct {
	*capture.Store
	path string
}

func openRetentionStore(t *testing.T, cfg capture.Config) *retentionTestStore {
	t.Helper()
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(t.TempDir(), "capture.db")
	}
	store, err := capture.Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	return &retentionTestStore{Store: store, path: cfg.DBPath}
}

func openRetentionVerifier(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func waitRetentionRows(t *testing.T, store *retentionTestStore, want int) {
	t.Helper()
	waitRetention(t, store, testCountRequestsSQL, want)
}

func captureFileBytes(t *testing.T, path string) int64 {
	t.Helper()
	var total int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if os.IsNotExist(err) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

func runTestRetention(t *testing.T, store *retentionTestStore) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); store.RunRetention(ctx) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func waitRetention(t *testing.T, store *retentionTestStore, query string, want int) {
	t.Helper()
	database := openRetentionVerifier(t, store.path)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var count int
		if err := database.QueryRow(query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("retention count = %d, want %d for %s", count, want, query)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInitialRetentionResumesCutoffAndPreservesBaseline(t *testing.T) {
	seed := openRetentionStore(t, capture.Config{QueueDepth: 1024})
	path := seed.path
	for index := range 300 {
		seed.Record(capture.Record{Timestamp: time.Now(), RequestID: fmt.Sprintf("old-%d", index), RequestBody: []byte("raw"), DecodedRequest: &capture.DecodedBody{RepresentationJSON: []byte(`{}`), ToolEvents: []capture.ToolEvent{{ToolName: "edit"}}}})
	}
	if err := seed.PutBaseline(context.Background(), capture.BaselineChange{Timestamp: time.Now(), Upstream: "test", Snapshot: json.RawMessage(`{"baseline":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	first := openRetentionStore(t, capture.Config{DBPath: path, InitializeRetention: true, RetentionInterval: time.Hour})
	first.Record(capture.Record{Timestamp: time.Now(), RequestID: "new", RequestBody: []byte("keep")})
	waitRetentionRows(t, first, 301)
	if err := first.Close(context.Background(), "restart"); err != nil {
		t.Fatal(err)
	}
	resumed := openRetentionStore(t, capture.Config{DBPath: path, RetentionInterval: time.Millisecond})
	runTestRetention(t, resumed)
	waitRetention(t, resumed, testInitialCompleteSQL, 1)
	waitRetention(t, resumed, testCountNewRequestsSQL, 1)
	waitRetention(t, resumed, testCountRequestsSQL, 1)
	waitRetention(t, resumed, testCountDecodedBodiesSQL, 0)
	waitRetention(t, resumed, testCountDecodedToolEventsSQL, 0)
	_, found, err := resumed.CurrentBaseline(context.Background(), "test")
	if err != nil || !found {
		t.Fatalf("baseline missing: found=%v err=%v", found, err)
	}
}

func TestRetentionReclaimsActualDatabaseFiles(t *testing.T) {
	seed := openRetentionStore(t, capture.Config{QueueDepth: 64})
	for range 20 {
		seed.Record(capture.Record{Timestamp: time.Now(), RequestBody: []byte(strings.Repeat("a", 256*1024))})
	}
	path := seed.path
	if err := seed.Close(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	store := openRetentionStore(t, capture.Config{DBPath: path, RetentionInterval: time.Millisecond})
	before := captureFileBytes(t, store.path)
	runTestRetention(t, store)
	waitRetention(t, store, testInitialCompleteSQL, 1)
	waitRetention(t, store, testFreelistCountSQL, 0)
	after := captureFileBytes(t, store.path)
	if after >= before/2 {
		t.Fatalf("database files did not shrink enough: before=%d after=%d", before, after)
	}
	t.Logf("actual database+WAL+SHM bytes: before=%d after=%d", before, after)
}

func TestSizeRetentionWithoutAgePreservesNewestExchange(t *testing.T) {
	store := openRetentionStore(t, capture.Config{RetentionResolved: true, RetentionMaxBytes: 700_000, RetentionInterval: time.Millisecond})
	stop := runTestRetention(t, store)
	waitRetention(t, store, testInitialCompleteSQL, 1)
	stop()
	store.Record(capture.Record{Timestamp: time.Now().Add(-time.Hour), RequestID: "old", RequestBody: []byte(strings.Repeat("a", 512*1024))})
	store.Record(capture.Record{Timestamp: time.Now(), RequestID: "new", RequestBody: []byte(strings.Repeat("b", 128*1024))})
	waitRetentionRows(t, store, 2)
	runTestRetention(t, store)
	waitRetention(t, store, testCountOldRequestsSQL, 0)
	waitRetention(t, store, testCountNewRequestsSQL, 1)
}

func TestRetentionRepairsExistingDecodedOrphans(t *testing.T) {
	seed := openRetentionStore(t, capture.Config{})
	path := seed.path
	seed.Record(capture.Record{Timestamp: time.Now(), RequestBody: []byte("raw"), DecodedRequest: &capture.DecodedBody{RepresentationJSON: []byte(`{}`), ToolEvents: []capture.ToolEvent{{ToolName: "edit"}}}})
	if err := seed.Close(context.Background(), "seed"); err != nil {
		t.Fatal(err)
	}
	database := openRetentionVerifier(t, path)
	if _, err := database.Exec(testDeleteRequestsSQL); err != nil {
		t.Fatal(err)
	}
	store := openRetentionStore(t, capture.Config{DBPath: path, RetentionInterval: time.Millisecond})
	runTestRetention(t, store)
	waitRetention(t, store, testCountDecodedToolEventsSQL, 0)
	waitRetention(t, store, testCountDecodedBodiesSQL, 0)
	waitRetention(t, store, testCountBodiesSQL, 0)
}

func TestSizeRetentionConvergesAfterConcurrentCaptureBurst(t *testing.T) {
	const byteLimit int64 = 1_000_000
	store := openRetentionStore(t, capture.Config{RetentionResolved: true, RetentionMaxBytes: byteLimit, RetentionInterval: time.Second, QueueDepth: 1024})
	stop := runTestRetention(t, store)
	waitRetention(t, store, testInitialCompleteSQL, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for index := range 100 {
			store.Record(capture.Record{Timestamp: time.Now(), RequestID: fmt.Sprintf("burst-%03d", index), RequestBody: []byte(strings.Repeat("b", 64*1024))})
			time.Sleep(5 * time.Millisecond)
		}
		store.Record(capture.Record{Timestamp: time.Now(), RequestID: "burst-complete", RequestBody: []byte("keep")})
	}()
	<-finished
	waitRetention(t, store, testCountBurstCompleteSQL, 1)
	deadline := time.Now().Add(10 * time.Second)
	for {
		bytes := captureFileBytes(t, store.path)
		if bytes <= byteLimit {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("database files total %d bytes after 100 captures at 200 captures/second; limit=%d", bytes, byteLimit)
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	waitRetention(t, store, testCountBurstCompleteSQL, 1)
}

func TestRetentionDefersPinnedReaderWithoutBlockingAndResumes(t *testing.T) {
	const byteLimit int64 = 500_000
	directory := t.TempDir()
	logPath := filepath.Join(directory, "retention.jsonl")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	databasePath := filepath.Join(directory, "capture.db")
	opened, err := capture.Open(context.Background(), capture.Config{DBPath: databasePath, RetentionResolved: true, RetentionMaxBytes: byteLimit, RetentionInterval: time.Millisecond}, slog.New(slog.NewJSONHandler(logFile, nil)))
	if err != nil {
		t.Fatal(err)
	}
	store := &retentionTestStore{Store: opened, path: databasePath}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	stop := runTestRetention(t, store)
	waitRetention(t, store, testInitialCompleteSQL, 1)
	stop()
	store.Record(capture.Record{Timestamp: time.Now().Add(-time.Hour), RequestID: "old", RequestBody: []byte(strings.Repeat("a", 512*1024))})
	store.Record(capture.Record{Timestamp: time.Now(), RequestID: "new", RequestBody: []byte("keep")})
	waitRetentionRows(t, store, 2)
	database := openRetentionVerifier(t, store.path)
	reader, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Rollback() })
	var count int
	if err := reader.QueryRow(testCountRequestsSQL).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reader snapshot contains %d exchanges, want 2", count)
	}
	stopPinned := runTestRetention(t, store)
	started := time.Now()
	deadline := started.Add(500 * time.Millisecond)
	var pinDuration time.Duration
	for {
		contents, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), `"pinned_readers":true`) {
			pinDuration = time.Since(started)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("maintenance did not report reader pin within 500 milliseconds")
		}
		time.Sleep(time.Millisecond)
	}
	stopPinned()
	waitRetention(t, store, testCountRequestsSQL, 2)
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	runTestRetention(t, store)
	waitRetention(t, store, testCountOldRequestsSQL, 0)
	deadline = time.Now().Add(5 * time.Second)
	for {
		bytes := captureFileBytes(t, store.path)
		if bytes <= byteLimit {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reclamation did not resume: total_bytes=%d limit=%d", bytes, byteLimit)
		}
		time.Sleep(time.Millisecond)
	}
	waitRetention(t, store, testCountNewRequestsSQL, 1)
	t.Logf("maintenance reported reader pin after %s", pinDuration)
}

func TestPruneExpiresDriftHistoryAndPreservesBaseline(t *testing.T) {
	store := openRetentionStore(t, capture.Config{RetentionMaxAge: time.Hour})
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)

	store.RecordShape(capture.DriftShape{Timestamp: old, Upstream: "claude-code", Fingerprint: "fp-old"})
	store.RecordCheck(capture.DriftCheck{Timestamp: old, Upstream: "claude-code", Diverged: false, Summary: "ok"})
	waitRetention(t, store, testCountDriftShapesSQL, 1)
	if err := store.PutBaseline(ctx, capture.BaselineChange{
		Timestamp: old,
		Upstream:  "claude-code",
		Snapshot:  json.RawMessage(`{"upstream":{"name":"claude-code"},"flavors":[]}`),
		Diffs:     []capture.BaselineDiff{{Upstream: "claude-code", Flavor: "f", Category: "header_extra", Field: "x", FirstSeen: old, LastSeen: old, SeenCount: 1}},
	}); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}

	waitRetention(t, store, testCountDriftChecksSQL, 1)
	runTestRetention(t, store)
	waitRetention(t, store, testCountDriftShapesSQL, 0)

	db := openRetentionVerifier(t, store.path)
	for _, table := range []string{"drift_shapes", "baselines", "baseline_diffs", "drift_checks"} {
		var n int
		if err := db.QueryRow(strings.ReplaceAll(testCountTableSQL, "{{table}}", table)).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		want := 0
		if table == "baselines" {
			want = 1
		}
		if n != want {
			t.Fatalf("%s row count = %d, want %d", table, n, want)
		}
	}
}
