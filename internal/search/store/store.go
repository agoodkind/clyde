// Package store persists conversation search jobs to a local SQLite database so
// an async search started by one request can be polled, analyzed, and canceled
// by later requests, and so job state survives a daemon reload. It mirrors the
// single-writer WAL discipline of internal/mitm/capture: one connection, a WAL
// DSN, and a checkpoint into the main database file on open and close so a
// reader that opens the file after a reload sees every committed row.
//
// The store keeps the serialized result set opaque (a [json.RawMessage]) so it
// does not import the conversation package; the conversation layer owns the
// encoding of its result-set DTO into that field.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"

	"goodkind.io/clyde/internal/clock"
)

const (
	defaultRetentionMaxAge   = 24 * time.Hour
	defaultRetentionInterval = 10 * time.Minute
	dbFileMode               = os.FileMode(0o600)
)

// Status is the lifecycle state of a search job.
type Status string

const (
	// StatusPending marks a job that has been created but not started.
	StatusPending Status = "pending"
	// StatusRunning marks a job whose search pipeline is executing.
	StatusRunning Status = "running"
	// StatusComplete marks a job that finished and stored its result set.
	StatusComplete Status = "complete"
	// StatusFailed marks a job that ended with an error.
	StatusFailed Status = "failed"
	// StatusCanceled marks a job that was canceled before completion.
	StatusCanceled Status = "canceled"
)

// Progress is the live advancement of a running search across its chunk sweep
// and its rerank layers.
type Progress struct {
	ChunksDone  int
	ChunksTotal int
	LayerIndex  int
	LayerTotal  int
	LayerName   string
}

// Job is one row of the search_jobs table.
type Job struct {
	ResultID       string
	ConversationID string
	Query          string
	Depth          string
	Status         Status
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Progress       Progress
	ResultText     string
	// ResultSetJSON carries the caller's serialized result-set DTO. The store
	// keeps it opaque to avoid importing the conversation package; the caller
	// owns the encoding.
	ResultSetJSON json.RawMessage
	Error         string
}

// Config controls the search job store. Zero values fall back to package
// defaults so callers can pass a near-empty Config with only DBPath set.
type Config struct {
	// DBPath is the SQLite database file path. Required.
	DBPath string
	// RetentionMaxAge deletes terminal jobs older than this. Defaults to 24h.
	RetentionMaxAge time.Duration
	// RetentionInterval is the prune cadence. Defaults to 10 minutes.
	RetentionInterval time.Duration
}

func (c Config) withDefaults() Config {
	if c.RetentionMaxAge <= 0 {
		c.RetentionMaxAge = defaultRetentionMaxAge
	}
	if c.RetentionInterval <= 0 {
		c.RetentionInterval = defaultRetentionInterval
	}
	return c
}

// Store is the SQLite-backed search job store. Construct it with [Open] and
// release it with [Close]; it is safe to share across goroutines.
type Store struct {
	cfg    Config
	log    *slog.Logger
	db     *sql.DB
	done   chan struct{}
	closed atomic.Bool
	wg     sync.WaitGroup
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS search_jobs (
	result_id        TEXT PRIMARY KEY,
	conversation_id  TEXT NOT NULL DEFAULT '',
	query            TEXT NOT NULL DEFAULT '',
	depth            TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL DEFAULT '',
	created_at       INTEGER NOT NULL DEFAULT 0,
	updated_at       INTEGER NOT NULL DEFAULT 0,
	chunks_total     INTEGER NOT NULL DEFAULT 0,
	chunks_done      INTEGER NOT NULL DEFAULT 0,
	layer_index      INTEGER NOT NULL DEFAULT 0,
	layer_total      INTEGER NOT NULL DEFAULT 0,
	layer_name       TEXT NOT NULL DEFAULT '',
	result_text      TEXT NOT NULL DEFAULT '',
	result_set_json  BLOB,
	error            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_search_jobs_status ON search_jobs(status);
CREATE INDEX IF NOT EXISTS idx_search_jobs_updated ON search_jobs(updated_at);`

// Open creates or opens the search job database at cfg.DBPath, applies the
// schema, and starts the retention goroutine. ctx is the store-lifetime context
// the pruner attaches its database calls to. The log is used verbatim, so the
// caller should scope it to the desired concern before passing it.
func Open(ctx context.Context, cfg Config, log *slog.Logger) (*Store, error) {
	cfg = cfg.withDefaults()
	if cfg.DBPath == "" {
		return nil, fmt.Errorf("search store: DBPath is required")
	}
	if log == nil {
		log = slog.Default()
	}
	dsn := "file:" + cfg.DBPath + "?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate&_synchronous=NORMAL&_auto_vacuum=incremental"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("search store: open sqlite %s: %w", cfg.DBPath, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("search store: apply schema: %w", err)
	}
	// Checkpoint the schema write into the main database file so it carries a
	// valid header immediately. In WAL mode the CREATE TABLE statements
	// otherwise live only in the -wal sidecar, leaving the main file
	// zero-length; a reader (the daemon after reload) that opens the empty main
	// file would discard the WAL and see no schema.
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.WarnContext(ctx, "search.store.initial_checkpoint_failed", "concern", "conversation.search", "path", cfg.DBPath, "err", err)
	}
	if err := os.Chmod(cfg.DBPath, dbFileMode); err != nil {
		log.WarnContext(ctx, "search.store.chmod_failed", "concern", "conversation.search", "path", cfg.DBPath, "err", err)
	}
	s := &Store{
		cfg:    cfg,
		log:    log,
		db:     db,
		done:   make(chan struct{}),
		closed: atomic.Bool{},
		wg:     sync.WaitGroup{},
	}
	s.wg.Add(1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "search.store.retention_loop_panic", "concern", "conversation.search", "err", fmt.Errorf("panic: %v", r))
			}
		}()
		s.retentionLoop(ctx)
	}()
	return s, nil
}

// Insert writes a new job row. The job's CreatedAt and UpdatedAt are stamped
// from the clock if zero.
func (s *Store) Insert(ctx context.Context, job Job) error {
	now := clock.Now()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}
	if job.Status == "" {
		job.Status = StatusPending
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO search_jobs (
			result_id, conversation_id, query, depth, status,
			created_at, updated_at,
			chunks_total, chunks_done, layer_index, layer_total, layer_name,
			result_text, result_set_json, error
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		job.ResultID, job.ConversationID, job.Query, job.Depth, string(job.Status),
		job.CreatedAt.UnixNano(), job.UpdatedAt.UnixNano(),
		job.Progress.ChunksTotal, job.Progress.ChunksDone, job.Progress.LayerIndex, job.Progress.LayerTotal, job.Progress.LayerName,
		job.ResultText, []byte(job.ResultSetJSON), job.Error,
	)
	if err != nil {
		s.log.WarnContext(ctx, "search.store.insert_failed", "concern", "conversation.search", "result_id", job.ResultID, "err", err)
		return fmt.Errorf("search store: insert job %s: %w", job.ResultID, err)
	}
	return nil
}

// Start flips a job to running and stamps updated_at.
func (s *Store) Start(ctx context.Context, resultID string) error {
	return s.setStatus(ctx, resultID, StatusRunning, "")
}

// UpdateProgress writes the live chunk and layer counters for a running job.
func (s *Store) UpdateProgress(ctx context.Context, resultID string, p Progress) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE search_jobs SET chunks_total=?, chunks_done=?, layer_index=?, layer_total=?, layer_name=?, updated_at=? WHERE result_id=?`,
		p.ChunksTotal, p.ChunksDone, p.LayerIndex, p.LayerTotal, p.LayerName, clock.Now().UnixNano(), resultID,
	)
	if err != nil {
		s.log.WarnContext(ctx, "search.store.update_progress_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return fmt.Errorf("search store: update progress %s: %w", resultID, err)
	}
	return nil
}

// Complete marks a job complete and stores its rendered text and serialized
// result set.
func (s *Store) Complete(ctx context.Context, resultID, text string, resultSet json.RawMessage) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE search_jobs SET status=?, result_text=?, result_set_json=?, error='', updated_at=? WHERE result_id=?`,
		string(StatusComplete), text, []byte(resultSet), clock.Now().UnixNano(), resultID,
	)
	if err != nil {
		s.log.WarnContext(ctx, "search.store.complete_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return fmt.Errorf("search store: complete job %s: %w", resultID, err)
	}
	return nil
}

// Fail marks a job failed and records the error message.
func (s *Store) Fail(ctx context.Context, resultID, errMsg string) error {
	return s.setStatus(ctx, resultID, StatusFailed, errMsg)
}

// Cancel marks a job canceled.
func (s *Store) Cancel(ctx context.Context, resultID string) error {
	return s.setStatus(ctx, resultID, StatusCanceled, "")
}

func (s *Store) setStatus(ctx context.Context, resultID string, status Status, errMsg string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE search_jobs SET status=?, error=?, updated_at=? WHERE result_id=?`,
		string(status), errMsg, clock.Now().UnixNano(), resultID,
	)
	if err != nil {
		s.log.WarnContext(ctx, "search.store.set_status_failed", "concern", "conversation.search", "result_id", resultID, "status", string(status), "err", err)
		return fmt.Errorf("search store: set status %s on %s: %w", status, resultID, err)
	}
	return nil
}

// Get reads one job by result id. The second return is false when no row exists.
func (s *Store) Get(ctx context.Context, resultID string) (Job, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT result_id, conversation_id, query, depth, status,
			created_at, updated_at,
			chunks_total, chunks_done, layer_index, layer_total, layer_name,
			result_text, result_set_json, error
		FROM search_jobs WHERE result_id=?`, resultID)
	var (
		job             Job
		status          string
		createdAt       int64
		updatedAt       int64
		resultSetRawNul []byte
	)
	err := row.Scan(
		&job.ResultID, &job.ConversationID, &job.Query, &job.Depth, &status,
		&createdAt, &updatedAt,
		&job.Progress.ChunksTotal, &job.Progress.ChunksDone, &job.Progress.LayerIndex, &job.Progress.LayerTotal, &job.Progress.LayerName,
		&job.ResultText, &resultSetRawNul, &job.Error,
	)
	if err == sql.ErrNoRows {
		return job, false, nil
	}
	if err != nil {
		s.log.WarnContext(ctx, "search.store.get_failed", "concern", "conversation.search", "result_id", resultID, "err", err)
		return job, false, fmt.Errorf("search store: get job %s: %w", resultID, err)
	}
	job.Status = Status(status)
	job.CreatedAt = time.Unix(0, createdAt)
	job.UpdatedAt = time.Unix(0, updatedAt)
	if len(resultSetRawNul) > 0 {
		job.ResultSetJSON = json.RawMessage(resultSetRawNul)
	}
	return job, true, nil
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

// prune deletes terminal jobs older than the age cap. Running and pending jobs
// are kept regardless of age. The cutoff is computed in SQL so the pruner needs
// no Go wall clock. Failures are logged, not returned.
func (s *Store) prune(ctx context.Context) {
	ageSeconds := int64(s.cfg.RetentionMaxAge / time.Second)
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM search_jobs
			WHERE status IN ('complete','failed','canceled')
			AND updated_at < CAST(strftime('%s','now','-' || ? || ' seconds') AS INTEGER)*1000000000`,
		ageSeconds,
	); err != nil {
		s.log.WarnContext(ctx, "search.store.prune_failed", "concern", "conversation.search", "err", err)
	}
}

// Close stops the retention loop, checkpoints the WAL into the main database
// file on ctx, and closes the database. It satisfies a livetrack-style closer;
// reason is logged. Close is idempotent.
func (s *Store) Close(ctx context.Context, reason string) error {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(s.done)
	s.wg.Wait()
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		s.log.WarnContext(ctx, "search.store.checkpoint_failed", "concern", "conversation.search", "reason", reason, "err", err)
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("search store: close db: %w", err)
	}
	s.log.InfoContext(ctx, "search.store.closed", "concern", "conversation.search", "reason", reason)
	return nil
}
