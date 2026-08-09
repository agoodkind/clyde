package capture

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
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

func TestRecordPersistsConversationIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	store := openTestStore(t, Config{DBPath: path})

	store.Record(Record{
		Timestamp:          time.Now(),
		Client:             "cli.claude-code",
		Provider:           "claude",
		Host:               "api.anthropic.com",
		Method:             "POST",
		Path:               "/v1/messages",
		Status:             200,
		RequestID:          "req-conv",
		SessionID:          "sess-native",
		ConversationID:     "claude:sess-native",
		ConversationSource: "header",
	})
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openVerifier(t, path)
	var sessionID, conversationID, conversationSource string
	if err := db.QueryRow(`SELECT session_id, conversation_id, conversation_source FROM requests WHERE request_id='req-conv'`).
		Scan(&sessionID, &conversationID, &conversationSource); err != nil {
		t.Fatalf("scan identity columns: %v", err)
	}
	if sessionID != "sess-native" {
		t.Fatalf("session_id = %q", sessionID)
	}
	if conversationID != "claude:sess-native" {
		t.Fatalf("conversation_id = %q", conversationID)
	}
	if conversationSource != "header" {
		t.Fatalf("conversation_source = %q", conversationSource)
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

func TestRecordPersistsLinkedDecodedRequestAndToolEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	store := openTestStore(t, Config{DBPath: path})
	raw := []byte{0x0a, 0x03, 'r', 'a', 'w'}
	store.Record(Record{
		Timestamp:   time.Now(),
		Host:        "api2.cursor.sh",
		Path:        "/aiserver.v1.AiService/BidiAppend",
		RequestID:   "decoded-request",
		RequestBody: raw,
		RequestType: "application/protobuf",
		DecodedRequest: &DecodedBody{
			Format:             "cursor.bidi_append.protobuf_hex",
			Status:             DecodeStatusSuccess,
			RepresentationJSON: []byte(`{"fields":[{"field_number":1,"wire_type":"bytes","data_base64":"cmF3"}]}`),
			ToolEvents: []ToolEvent{
				{Ordering: 7, CallID: "call-7", ToolName: "edit_file", InputRepresentation: `{"patch":"hello"}`, InputEncoding: ToolInputEncodingJSON},
			},
		},
	})
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openVerifier(t, path)
	var storedRaw []byte
	if err := db.QueryRow(`SELECT data FROM bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id='decoded-request') AND which='request'`).Scan(&storedRaw); err != nil {
		t.Fatalf("raw body: %v", err)
	}
	if string(storedRaw) != string(raw) {
		t.Fatalf("raw body = %x want %x", storedRaw, raw)
	}
	var format, status, representation string
	if err := db.QueryRow(`SELECT format, status, representation_json FROM decoded_bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id='decoded-request') AND which='request'`).Scan(&format, &status, &representation); err != nil {
		t.Fatalf("decoded body: %v", err)
	}
	if format != "cursor.bidi_append.protobuf_hex" {
		t.Fatalf("format = %q", format)
	}
	if status != string(DecodeStatusSuccess) {
		t.Fatalf("status = %q", status)
	}
	if representation == "" {
		t.Fatal("representation is empty")
	}
	var ordering int
	var callID, toolName, input, inputEncoding string
	if err := db.QueryRow(`SELECT ordering, call_id, tool_name, input_representation, input_encoding FROM decoded_tool_events WHERE decoded_body_id=(SELECT id FROM decoded_bodies WHERE request_row_id=(SELECT id FROM requests WHERE request_id='decoded-request') AND which='request')`).Scan(&ordering, &callID, &toolName, &input, &inputEncoding); err != nil {
		t.Fatalf("tool event: %v", err)
	}
	if ordering != 7 || callID != "call-7" || toolName != "edit_file" || input != `{"patch":"hello"}` || inputEncoding != string(ToolInputEncodingJSON) {
		t.Fatalf("tool event = (%d, %q, %q, %q, %q)", ordering, callID, toolName, input, inputEncoding)
	}
}

func TestRecordPersistsFullUnsignedToolEventOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	store := openTestStore(t, Config{DBPath: path})
	ordering := ^uint64(0)
	store.Record(Record{
		Timestamp: time.Now(),
		Host:      "api2.cursor.sh",
		Path:      "/aiserver.v1.AiService/BidiAppend",
		RequestID: "decoded-large-ordering",
		DecodedRequest: &DecodedBody{
			Format:             "cursor.bidi_append.protobuf_hex",
			Status:             DecodeStatusSuccess,
			RepresentationJSON: []byte(`{}`),
			ToolEvents: []ToolEvent{{
				Ordering:            ordering,
				CallID:              "call-large-ordering",
				ToolName:            "apply_patch",
				InputRepresentation: "raw patch",
				InputEncoding:       ToolInputEncodingBase64,
			}},
		},
	})
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db := openVerifier(t, path)
	var storedOrdering int64
	var storedOrderingText string
	row := db.QueryRow(`SELECT ordering, ordering_text FROM decoded_tool_events WHERE call_id='call-large-ordering'`)
	if err := row.Scan(&storedOrdering, &storedOrderingText); err != nil {
		t.Fatalf("tool event ordering: %v", err)
	}
	if storedOrdering != 0 {
		t.Fatalf("legacy ordering = %d, want 0 for out-of-range value", storedOrdering)
	}
	if storedOrderingText != strconv.FormatUint(ordering, 10) {
		t.Fatalf("ordering text = %q, want %q", storedOrderingText, strconv.FormatUint(ordering, 10))
	}
}

func TestOpenAddsDecodedTablesToExistingCaptureDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE requests (
			id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, client TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '', concern TEXT NOT NULL DEFAULT '', host TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 0, request_id TEXT NOT NULL DEFAULT '', upstream_request_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', trace_id TEXT NOT NULL DEFAULT '', req_headers TEXT NOT NULL DEFAULT '', resp_headers TEXT NOT NULL DEFAULT '', req_content_type TEXT NOT NULL DEFAULT '', resp_content_type TEXT NOT NULL DEFAULT '', req_bytes INTEGER NOT NULL DEFAULT 0, resp_bytes INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE bodies (request_row_id INTEGER NOT NULL, which TEXT NOT NULL, content_type TEXT NOT NULL DEFAULT '', is_text INTEGER NOT NULL DEFAULT 0, truncated INTEGER NOT NULL DEFAULT 0, data BLOB, PRIMARY KEY (request_row_id, which));
		INSERT INTO requests (ts, request_id) VALUES (1, 'legacy-request');
	`)
	if err != nil {
		_ = db.Close()
		t.Fatalf("create legacy schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store := openTestStore(t, Config{DBPath: path})
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	verifier := openVerifier(t, path)
	var tableCount int
	if err := verifier.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('decoded_bodies', 'decoded_tool_events')`).Scan(&tableCount); err != nil {
		t.Fatalf("query decoded tables: %v", err)
	}
	if tableCount != 2 {
		t.Fatalf("decoded table count = %d, want 2", tableCount)
	}
	var requestID string
	if err := verifier.QueryRow(`SELECT request_id FROM requests`).Scan(&requestID); err != nil {
		t.Fatalf("read legacy request: %v", err)
	}
	if requestID != "legacy-request" {
		t.Fatalf("legacy request id = %q", requestID)
	}
}

func TestOpenAddsInputEncodingToExistingDecodedToolEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE decoded_tool_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			decoded_body_id INTEGER NOT NULL,
			ordering INTEGER NOT NULL DEFAULT 0,
			call_id TEXT NOT NULL DEFAULT '',
			tool_name TEXT NOT NULL DEFAULT '',
			input_representation TEXT NOT NULL DEFAULT ''
		);
	`)
	if err != nil {
		_ = db.Close()
		t.Fatalf("create legacy decoded tool events: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store := openTestStore(t, Config{DBPath: path})
	if err := store.Close(context.Background(), "test"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	verifier := openVerifier(t, path)
	var columnCount int
	if err := verifier.QueryRow(`SELECT count(*) FROM pragma_table_info('decoded_tool_events') WHERE name='input_encoding'`).Scan(&columnCount); err != nil {
		t.Fatalf("query input encoding column: %v", err)
	}
	if columnCount != 1 {
		t.Fatalf("input encoding columns=%d want 1", columnCount)
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
