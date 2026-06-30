package capture

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T, cfg Config) *Store {
	t.Helper()
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(t.TempDir(), "capture.db")
	}
	store, err := Open(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	return store
}

func openVerifier(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRecordTextAndBinaryBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	store := openTestStore(t, Config{DBPath: path})

	store.Record(Record{
		Timestamp:      time.Now(),
		Client:         "cursor",
		Provider:       "anthropic",
		Concern:        "providers.mitm.wire",
		Host:           "api.anthropic.com",
		Method:         "POST",
		Path:           "/v1/messages",
		Status:         200,
		RequestID:      "req-text",
		RequestHeaders: http.Header{"Content-Type": []string{"application/json"}},
		RequestBody:    []byte(`{"model":"claude"}`),
		RequestType:    "application/json",
		ResponseBody:   []byte(`{"ok":true}`),
		ResponseType:   "application/json",
		Duration:       42 * time.Millisecond,
	})
	store.Record(Record{
		Timestamp:    time.Now(),
		Client:       "cursor",
		Host:         "api2.cursor.sh",
		Method:       "POST",
		Path:         "/proto",
		Status:       200,
		RequestID:    "req-binary",
		RequestBody:  []byte{0xff, 0xfe, 0x00, 0x01, 0x80},
		RequestType:  "application/proto",
		ResponseBody: []byte{0x00, 0xff},
		ResponseType: "application/proto",
	})
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openVerifier(t, path)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM requests`).Scan(&count); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if count != 2 {
		t.Fatalf("requests count = %d, want 2", count)
	}

	var isText int
	if err := db.QueryRow(`SELECT is_text FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id='req-text') AND which='request'`).Scan(&isText); err != nil {
		t.Fatalf("text body is_text: %v", err)
	}
	if isText != 1 {
		t.Fatalf("text body is_text = %d, want 1", isText)
	}
	if err := db.QueryRow(`SELECT is_text FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id='req-binary') AND which='request'`).Scan(&isText); err != nil {
		t.Fatalf("binary body is_text: %v", err)
	}
	if isText != 0 {
		t.Fatalf("binary body is_text = %d, want 0", isText)
	}

	var headers string
	if err := db.QueryRow(`SELECT req_headers FROM requests WHERE request_id='req-text'`).Scan(&headers); err != nil {
		t.Fatalf("req_headers: %v", err)
	}
	if headers == "" {
		t.Fatalf("req_headers is empty, want JSON")
	}
}

func TestTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	store := openTestStore(t, Config{DBPath: path, MaxBodyBytes: 10})
	full := make([]byte, 100)
	for i := range full {
		full[i] = 'a'
	}
	store.Record(Record{
		Timestamp:   time.Now(),
		Host:        "h",
		RequestID:   "trunc",
		RequestBody: full,
		RequestType: "text/plain",
	})
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openVerifier(t, path)
	var reqBytes, truncated, storedLen int
	row := db.QueryRow(`SELECT r.req_bytes, b.truncated, length(b.data) FROM requests r JOIN bodies b ON b.request_row_id=r.id WHERE r.request_id='trunc' AND b.which='request'`)
	if err := row.Scan(&reqBytes, &truncated, &storedLen); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reqBytes != 100 {
		t.Fatalf("req_bytes = %d, want 100", reqBytes)
	}
	if truncated != 1 {
		t.Fatalf("truncated = %d, want 1", truncated)
	}
	if storedLen != 10 {
		t.Fatalf("stored length = %d, want 10", storedLen)
	}
}

func TestRetentionByAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	store := openTestStore(t, Config{DBPath: path, RetentionMaxAge: time.Hour})
	store.Record(Record{
		Timestamp:   time.Now().Add(-2 * time.Hour),
		Host:        "old",
		RequestID:   "old",
		RequestBody: []byte("x"),
	})
	store.Record(Record{
		Timestamp:   time.Now(),
		Host:        "fresh",
		RequestID:   "fresh",
		RequestBody: []byte("y"),
	})
	waitForRowCount(t, store, 2)
	store.prune(context.Background())

	db := openVerifier(t, path)
	var hasOld, hasFresh int
	if err := db.QueryRow(`SELECT count(*) FROM requests WHERE request_id='old'`).Scan(&hasOld); err != nil {
		t.Fatalf("count old: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM requests WHERE request_id='fresh'`).Scan(&hasFresh); err != nil {
		t.Fatalf("count fresh: %v", err)
	}
	if hasOld != 0 {
		t.Fatalf("old row survived prune (count=%d)", hasOld)
	}
	if hasFresh != 1 {
		t.Fatalf("fresh row pruned (count=%d), want 1", hasFresh)
	}
	var orphanBodies int
	if err := db.QueryRow(`SELECT count(*) FROM bodies WHERE request_row_id NOT IN (SELECT id FROM requests)`).Scan(&orphanBodies); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphanBodies != 0 {
		t.Fatalf("orphan bodies after prune = %d, want 0", orphanBodies)
	}
}

func TestConcurrentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	store := openTestStore(t, Config{DBPath: path, QueueDepth: 4096})
	const writers = 8
	const perWriter = 200
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := range writers {
		go func(w int) {
			defer wg.Done()
			for i := range perWriter {
				store.Record(Record{
					Timestamp:   time.Now(),
					Host:        "h",
					RequestID:   fmt.Sprintf("w%d-i%d", w, i),
					RequestBody: []byte("payload"),
				})
			}
		}(w)
	}
	wg.Wait()
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openVerifier(t, path)
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM requests`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != writers*perWriter {
		t.Fatalf("requests count = %d, want %d", count, writers*perWriter)
	}
}

// waitForRowCount polls a fresh verifier connection until the requests table
// holds at least want rows, so a test can assert against committed state
// without closing the live store. The store checkpoints its schema at Open, so
// the main database file carries a header and a new reader sees WAL commits.
func waitForRowCount(t *testing.T, store *Store, want int) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+store.cfg.DBPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open wait verifier: %v", err)
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if scanErr := db.QueryRow(`SELECT count(*) FROM requests`).Scan(&n); scanErr == nil && n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d rows in requests", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
