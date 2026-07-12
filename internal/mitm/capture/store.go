// Package capture persists full MITM request/response exchanges to a local
// SQLite database. It replaces the previous per-leg `.raw` sidecar files with
// one correlatable row per request plus a side table for the request and
// response bodies, so a human or an LLM can query exchanges with plain SQL and
// join them by request_id or trace_id.
//
// The store is single-writer by construction: callers hand records to
// [Store.Record], which enqueues them onto a buffered channel drained by one
// writer goroutine. Capture is best-effort diagnostics, so a full queue drops
// the record with a warning rather than blocking the proxy's hot path. A
// background goroutine prunes rows past the configured age and total-size caps.
package capture

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	sq "github.com/Masterminds/squirrel"
	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"
)

// DefaultMaxBodyBytes is the default per-body storage cap (8 MiB). Callers
// outside the package (the proxy's response buffer) read it so their in-memory
// buffer limit matches what the store will persist.
const DefaultMaxBodyBytes = 8 * 1024 * 1024

const (
	defaultRetentionMaxAge   = 7 * 24 * time.Hour
	defaultRetentionMaxBytes = int64(2 * 1024 * 1024 * 1024)
	defaultRetentionInterval = 10 * time.Minute
	defaultQueueDepth        = 256
	dbFileMode               = os.FileMode(0o600)
	// readPoolMaxConns bounds the read-only handle's connection pool so
	// per-request baseline reads cannot open an unbounded number of SQLite
	// connections. WAL allows several concurrent readers, so a small fixed
	// pool is enough.
	readPoolMaxConns = 4
)

// BodySide names which half of an exchange a body row holds.
type BodySide string

const (
	// BodyRequest marks the captured request body.
	BodyRequest BodySide = "request"
	// BodyResponse marks the captured response body.
	BodyResponse BodySide = "response"
)

// Config controls the SQLite capture store. Zero values fall back to package
// defaults so callers can pass a near-empty Config with only DBPath set.
type Config struct {
	// DBPath is the SQLite database file path. Required.
	DBPath string
	// MaxBodyBytes caps how many bytes of each body are stored. Bodies past
	// the cap are truncated and flagged; the full byte count is still recorded
	// on the request row. Defaults to 8 MiB.
	MaxBodyBytes int
	// RetentionMaxAge deletes rows older than this. Defaults to 7 days.
	RetentionMaxAge time.Duration
	// RetentionMaxBytes deletes the oldest rows once the database exceeds this
	// on-disk size. Defaults to 2 GiB.
	RetentionMaxBytes int64
	// RetentionInterval is the prune cadence. Defaults to 10 minutes.
	RetentionInterval time.Duration
	// QueueDepth bounds the in-flight record channel. Defaults to 256.
	QueueDepth int
}

func (c Config) withDefaults() Config {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.RetentionMaxAge <= 0 {
		c.RetentionMaxAge = defaultRetentionMaxAge
	}
	if c.RetentionMaxBytes <= 0 {
		c.RetentionMaxBytes = defaultRetentionMaxBytes
	}
	if c.RetentionInterval <= 0 {
		c.RetentionInterval = defaultRetentionInterval
	}
	if c.QueueDepth <= 0 {
		c.QueueDepth = defaultQueueDepth
	}
	return c
}

// Record is one captured request/response exchange. Headers are typed
// [http.Header] values serialized to JSON at write time; bodies are the raw
// (decoded) bytes the proxy observed.
type Record struct {
	Timestamp         time.Time
	Client            string
	Provider          string
	Concern           string
	Host              string
	Method            string
	Path              string
	Status            int
	RequestID         string
	UpstreamRequestID string
	SessionID         string
	TraceID           string
	RequestHeaders    http.Header
	ResponseHeaders   http.Header
	RequestBody       []byte
	ResponseBody      []byte
	RequestType       string
	ResponseType      string
	DecodedRequest    *DecodedBody
	Duration          time.Duration
}

// DecodeStatus identifies whether a provider-specific body decoder understood
// the captured transport bytes. The raw body remains the authoritative record.
type DecodeStatus string

const (
	// DecodeStatusSuccess indicates the provider decoder understood the body.
	DecodeStatusSuccess DecodeStatus = "success"
	// DecodeStatusFailed indicates the provider decoder retained an error.
	DecodeStatusFailed DecodeStatus = "failed"
)

// ToolEvent is one semantic tool event recovered from a decoded provider body.
type ToolEvent struct {
	Ordering            uint64
	CallID              string
	ToolName            string
	InputRepresentation string
	InputEncoding       ToolInputEncoding
}

// ToolInputEncoding identifies how InputRepresentation stores recovered tool
// input bytes.
type ToolInputEncoding string

const (
	// ToolInputEncodingJSON stores valid JSON text unchanged.
	ToolInputEncodingJSON ToolInputEncoding = "json"
	// ToolInputEncodingBase64 stores non-JSON tool input as standard base64.
	ToolInputEncodingBase64 ToolInputEncoding = "base64"
)

// DecodedBody is a durable provider-specific representation linked to a raw
// capture body. RepresentationJSON must preserve the decoder's field tree.
type DecodedBody struct {
	Format             string
	Status             DecodeStatus
	Error              string
	RepresentationJSON []byte
	ToolEvents         []ToolEvent
}

// Store is the SQLite-backed capture sink. Construct it with [Open] and release
// it with [Close]; it is safe to share across goroutines.
type Store struct {
	cfg     Config
	log     *slog.Logger
	db      *sql.DB
	rdb     *sql.DB
	records chan queuedRecord
	shapes  chan DriftShape
	checks  chan DriftCheck
	done    chan struct{}
	closed  atomic.Bool
	wg      sync.WaitGroup
}

// Open creates or opens the capture database at cfg.DBPath, applies the schema,
// and starts the writer and retention goroutines. ctx is the store-lifetime
// context the background writer and pruner attach their database calls to; the
// goroutines capture it so it flows as a parameter rather than living on the
// struct. The log is used verbatim, so the caller should scope it to the
// desired concern before passing it.
func Open(ctx context.Context, cfg Config, log *slog.Logger) (*Store, error) {
	cfg = cfg.withDefaults()
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("capture: DBPath is required")
	}
	if log == nil {
		log = slog.Default()
	}
	dsn := "file:" + cfg.DBPath + "?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate&_synchronous=NORMAL&_auto_vacuum=incremental"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("capture: open sqlite %s: %w", cfg.DBPath, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("capture: apply schema: %w", err)
	}
	if err := ensureToolEventEncodingColumn(ctx, db, log); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Checkpoint the schema write into the main database file so it carries a
	// valid header immediately. In WAL mode the CREATE TABLE statements
	// otherwise live only in the -wal sidecar, leaving the main file
	// zero-length; a concurrent reader (a query tool, the daemon after reload)
	// that opens the empty main file would discard the WAL and see no schema.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.WarnContext(ctx, "mitm.capture.initial_checkpoint_failed", "path", cfg.DBPath, "err", err)
	}
	if err := os.Chmod(cfg.DBPath, dbFileMode); err != nil {
		log.WarnContext(ctx, "mitm.capture.chmod_failed", "path", cfg.DBPath, "err", err)
	}
	// A separate read-only handle serves the per-request adapter baseline reads
	// (and drift/baseline queries) without contending on the single-writer
	// connection. WAL lets readers see committed writes concurrently.
	rdsn := "file:" + cfg.DBPath + "?mode=ro&_journal_mode=WAL&_busy_timeout=5000"
	rdb, err := sql.Open("sqlite3", rdsn)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("capture: open read-only sqlite %s: %w", cfg.DBPath, err)
	}
	rdb.SetMaxOpenConns(readPoolMaxConns)
	rdb.SetMaxIdleConns(readPoolMaxConns)
	s := &Store{
		cfg:     cfg,
		log:     log,
		db:      db,
		rdb:     rdb,
		records: make(chan queuedRecord, cfg.QueueDepth),
		shapes:  make(chan DriftShape, cfg.QueueDepth),
		checks:  make(chan DriftCheck, cfg.QueueDepth),
		done:    make(chan struct{}),
		closed:  atomic.Bool{},
		wg:      sync.WaitGroup{},
	}
	s.wg.Add(2)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "mitm.capture.write_loop_panic", "err", fmt.Errorf("panic: %v", r))
			}
		}()
		s.writeLoop(ctx)
	}()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "mitm.capture.retention_loop_panic", "err", fmt.Errorf("panic: %v", r))
			}
		}()
		s.retentionLoop(ctx)
	}()
	return s, nil
}

// schemaSQL is the capture store's DDL, kept in schema.sql so the schema is
// well-defined SQL rather than a Go string literal, and embedded at build time.
//
//go:embed schema.sql
var schemaSQL string

func ensureToolEventEncodingColumn(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(decoded_tool_events)`)
	if err != nil {
		log.WarnContext(ctx, "mitm.capture.inspect_decoded_tool_event_columns_failed", "err", err)
		return fmt.Errorf("capture: inspect decoded tool event columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var columnID int
		var name string
		var columnType string
		var notNull int
		var defaultValue *string
		var primaryKey int
		if err := rows.Scan(&columnID, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			log.WarnContext(ctx, "mitm.capture.scan_decoded_tool_event_column_failed", "err", err)
			return fmt.Errorf("capture: scan decoded tool event column: %w", err)
		}
		if name == "input_encoding" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		log.WarnContext(ctx, "mitm.capture.inspect_decoded_tool_event_columns_failed", "err", err)
		return fmt.Errorf("capture: inspect decoded tool event columns: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE decoded_tool_events ADD COLUMN input_encoding TEXT NOT NULL DEFAULT ''`); err != nil {
		log.WarnContext(ctx, "mitm.capture.add_decoded_tool_input_encoding_column_failed", "err", err)
		return fmt.Errorf("capture: add decoded tool input encoding column: %w", err)
	}
	return nil
}

// Record enqueues an exchange for asynchronous persistence. It never blocks: if
// the writer queue is full or the store is closed, the record is dropped with a
// warning, since capture is best-effort diagnostics and must not stall the
// proxy.
func (s *Store) Record(rec Record) {
	s.recordWithMetadata(rec, len(rec.RequestBody), len(rec.ResponseBody), false, false)
}

type queuedRecord struct {
	record            Record
	requestBytes      int
	responseBytes     int
	requestTruncated  bool
	responseTruncated bool
}

func (s *Store) recordWithMetadata(rec Record, requestBytes, responseBytes int, requestTruncated, responseTruncated bool) {
	if s == nil || s.closed.Load() {
		return
	}
	queued := queuedRecord{record: rec, requestBytes: requestBytes, responseBytes: responseBytes, requestTruncated: requestTruncated, responseTruncated: responseTruncated}
	select {
	case s.records <- queued:
	default:
		s.log.Warn("mitm.capture.dropped", "reason", "queue_full", "host", rec.Host, "path", rec.Path)
	}
}

func (s *Store) writeLoop(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case rec := <-s.records:
			s.insert(ctx, rec)
		case shape := <-s.shapes:
			s.insertShape(ctx, shape)
		case check := <-s.checks:
			s.insertCheck(ctx, check)
		case <-s.done:
			s.drain(ctx)
			return
		}
	}
}

func (s *Store) drain(ctx context.Context) {
	for {
		select {
		case rec := <-s.records:
			s.insert(ctx, rec)
		case shape := <-s.shapes:
			s.insertShape(ctx, shape)
		case check := <-s.checks:
			s.insertCheck(ctx, check)
		default:
			return
		}
	}
}

// insert writes one record and its bodies in a single transaction on ctx. It is
// the store's sole write error boundary: every failure is logged here and the
// transaction rolled back, so the writer goroutine never propagates an error.
func (s *Store) insert(ctx context.Context, queued queuedRecord) {
	rec := queued.record
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.tx_begin_failed", "err", err)
		return
	}
	requestSQL, requestArgs, err := sq.Insert("requests").
		Columns("ts", "client", "provider", "concern", "host", "method", "path", "status",
			"request_id", "upstream_request_id", "session_id", "trace_id",
			"req_headers", "resp_headers", "req_content_type", "resp_content_type",
			"req_bytes", "resp_bytes", "duration_ms").
		Values(rec.Timestamp.UnixNano(), rec.Client, rec.Provider, rec.Concern, rec.Host, rec.Method, rec.Path, rec.Status,
			rec.RequestID, rec.UpstreamRequestID, rec.SessionID, rec.TraceID,
			encodeHeaders(rec.RequestHeaders), encodeHeaders(rec.ResponseHeaders), rec.RequestType, rec.ResponseType,
			queued.requestBytes, queued.responseBytes, rec.Duration.Milliseconds()).
		ToSql()
	if err != nil {
		_ = tx.Rollback()
		s.log.WarnContext(ctx, "mitm.capture.build_request_insert_failed", "host", rec.Host, "path", rec.Path, "err", err)
		return
	}
	res, err := tx.ExecContext(ctx, requestSQL, requestArgs...)
	if err != nil {
		_ = tx.Rollback()
		s.log.WarnContext(ctx, "mitm.capture.insert_request_failed", "host", rec.Host, "path", rec.Path, "err", err)
		return
	}
	rowID, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		s.log.WarnContext(ctx, "mitm.capture.last_insert_id_failed", "host", rec.Host, "path", rec.Path, "err", err)
		return
	}
	if !s.insertBody(ctx, tx, rowID, BodyRequest, rec.RequestType, rec.RequestBody, queued.requestTruncated) {
		_ = tx.Rollback()
		return
	}
	if !s.insertBody(ctx, tx, rowID, BodyResponse, rec.ResponseType, rec.ResponseBody, queued.responseTruncated) {
		_ = tx.Rollback()
		return
	}
	if !s.insertDecodedBody(ctx, tx, rowID, BodyRequest, rec.DecodedRequest) {
		_ = tx.Rollback()
		return
	}
	if err := tx.Commit(); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.commit_failed", "host", rec.Host, "path", rec.Path, "err", err)
	}
}

func (s *Store) insertDecodedBody(ctx context.Context, tx *sql.Tx, rowID int64, which BodySide, decoded *DecodedBody) bool {
	if decoded == nil {
		return true
	}
	decodedSQL, decodedArgs, err := sq.Insert("decoded_bodies").
		Columns("request_row_id", "which", "format", "status", "decode_error", "representation_json").
		Values(rowID, string(which), decoded.Format, string(decoded.Status), decoded.Error, decoded.RepresentationJSON).
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_decoded_body_insert_failed", "which", string(which), "err", err)
		return false
	}
	result, err := tx.ExecContext(ctx, decodedSQL, decodedArgs...)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.insert_decoded_body_failed", "which", string(which), "err", err)
		return false
	}
	decodedBodyID, err := result.LastInsertId()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.decoded_body_last_insert_id_failed", "which", string(which), "err", err)
		return false
	}
	for _, event := range decoded.ToolEvents {
		eventSQL, eventArgs, buildErr := sq.Insert("decoded_tool_events").
			Columns("decoded_body_id", "ordering", "call_id", "tool_name", "input_representation", "input_encoding").
			Values(decodedBodyID, event.Ordering, event.CallID, event.ToolName, event.InputRepresentation, string(event.InputEncoding)).
			ToSql()
		if buildErr != nil {
			s.log.WarnContext(ctx, "mitm.capture.build_decoded_tool_event_insert_failed", "err", buildErr)
			return false
		}
		if _, execErr := tx.ExecContext(ctx, eventSQL, eventArgs...); execErr != nil {
			s.log.WarnContext(ctx, "mitm.capture.insert_decoded_tool_event_failed", "err", execErr)
			return false
		}
	}
	return true
}

// insertBody writes one body row, returning false (after logging) on failure so
// the caller can roll back. An empty body is a no-op success.
func (s *Store) insertBody(ctx context.Context, tx *sql.Tx, rowID int64, which BodySide, contentType string, body []byte, alreadyTruncated bool) bool {
	if len(body) == 0 {
		return true
	}
	stored, truncated := s.capBody(body)
	truncated = truncated || alreadyTruncated
	isText := utf8.Valid(stored)
	bodySQL, bodyArgs, err := sq.Insert("bodies").
		Columns("request_row_id", "which", "content_type", "is_text", "truncated", "data").
		Values(rowID, string(which), contentType, boolToInt(isText), boolToInt(truncated), stored).
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_body_insert_failed", "which", string(which), "err", err)
		return false
	}
	if _, err := tx.ExecContext(ctx, bodySQL, bodyArgs...); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.insert_body_failed", "which", string(which), "err", err)
		return false
	}
	return true
}

func (s *Store) capBody(body []byte) (stored []byte, truncated bool) {
	if len(body) <= s.cfg.MaxBodyBytes {
		return body, false
	}
	return body[:s.cfg.MaxBodyBytes], true
}

func (s *Store) retentionLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.RetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.prune(ctx)
		case <-s.done:
			return
		}
	}
}

// prune deletes rows past the age cap, then enforces the on-disk size cap. The
// age cutoff is computed in SQL via a strftime modifier so the store needs no Go
// wall clock. Every failure is logged here; prune never returns an error.
func (s *Store) prune(ctx context.Context) {
	ageSeconds := int64(s.cfg.RetentionMaxAge / time.Second)
	ageCutoff := "ts < CAST(strftime('%s','now','-' || ? || ' seconds') AS INTEGER)*1000000000"
	bodiesSQL, bodiesArgs, err := sq.Delete("bodies").
		Where("request_row_id IN (SELECT id FROM requests WHERE "+ageCutoff+")", ageSeconds).
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_prune_age_bodies_failed", "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, bodiesSQL, bodiesArgs...); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.prune_age_bodies_failed", "err", err)
		return
	}
	requestsSQL, requestsArgs, err := sq.Delete("requests").Where(ageCutoff, ageSeconds).ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_prune_age_requests_failed", "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, requestsSQL, requestsArgs...); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.prune_age_requests_failed", "err", err)
		return
	}
	s.enforceSizeCap(ctx)
}

// enforceSizeCap drops the oldest tenth of rows when the database exceeds the
// configured byte cap, then reclaims freed pages. Repeated prune ticks converge
// without holding a long delete transaction. Failures are logged, not returned.
func (s *Store) enforceSizeCap(ctx context.Context) {
	var pageCount, pageSize int64
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.page_count_failed", "err", err)
		return
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.page_size_failed", "err", err)
		return
	}
	if pageCount*pageSize <= s.cfg.RetentionMaxBytes {
		return
	}
	oldestTenth := "(SELECT id FROM requests ORDER BY ts ASC LIMIT max(1, (SELECT count(*) FROM requests)/10))"
	bodiesSQL, _, err := sq.Delete("bodies").Where("request_row_id IN " + oldestTenth).ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_prune_size_bodies_failed", "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, bodiesSQL); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.prune_size_bodies_failed", "err", err)
		return
	}
	requestsSQL, _, err := sq.Delete("requests").Where("id IN " + oldestTenth).ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_prune_size_requests_failed", "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, requestsSQL); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.prune_size_requests_failed", "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.incremental_vacuum_failed", "err", err)
	}
}

// Close stops accepting records, drains the writer queue, checkpoints the WAL
// into the main database file on ctx, and closes the database. It satisfies a
// livetrack-style closer; reason is logged. Close is idempotent.
func (s *Store) Close(ctx context.Context, reason string) error {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(s.done)
	s.wg.Wait()
	// Flush the WAL into the main database file so a freshly opened connection
	// (a query tool, or the daemon after reload) sees every committed row.
	// Without an explicit checkpoint the main file can stay zero-length while
	// all committed data sits in the -wal sidecar, which a new reader opening
	// the empty main file discards.
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.checkpoint_failed", "reason", reason, "err", err)
	}
	if s.rdb != nil {
		if err := s.rdb.Close(); err != nil {
			s.log.WarnContext(ctx, "mitm.capture.read_handle_close_failed", "reason", reason, "err", err)
		}
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("capture: close db: %w", err)
	}
	s.log.InfoContext(ctx, "mitm.capture.closed", "reason", reason)
	return nil
}

func encodeHeaders(h http.Header) string {
	if len(h) == 0 {
		return ""
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return string(raw)
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
