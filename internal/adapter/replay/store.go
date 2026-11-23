package replay

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3" // Register the SQLite database/sql driver.
	"goodkind.io/clyde/internal/config"
)

//go:embed sql/schema.sql
var schemaSQL string

//go:embed sql/save.sql
var saveSQL string

//go:embed sql/load.sql
var loadSQL string

//go:embed sql/tool_schema.sql
var toolSchemaSQL string

//go:embed sql/save_tool.sql
var saveToolSQL string

var (
	errIncompleteRecord = errors.New(
		"save Cursor replay: conversation ID, reply ID and both digests are required",
	)
	errConflictingReply = errors.New(
		"save Cursor replay: the reply ID already identifies different metadata",
	)
	errMissingPath = errors.New("open Cursor replay: database path is required")
)

// Store opens a private SQLite database for each operation. Daemon generations
// can read and write committed replies while existing requests finish.
type Store struct {
	Path string
}

// DefaultStore locates the Cursor replay database in Clyde's state directory.
//
// The adapter and transcript readers use this path to share reply metadata.
//
//	store := replay.DefaultStore()
func DefaultStore() Store {
	return Store{Path: filepath.Join(config.DefaultStateDir(), "adapter", "cursor", "replay.db")}
}

// Save inserts one completed reply.
//
// Repeating an identical save succeeds. A conflicting save returns an error.
//
//	err := store.Save(ctx, record)
func (store Store) Save(ctx context.Context, record Record) error {
	if record.Reference.ConversationID == "" || record.Reference.ReplyID == "" ||
		record.Digest == "" || record.VisibleDigest == "" {
		return errIncompleteRecord
	}

	database, err := store.open(ctx, true)
	if err != nil {
		return err
	}

	defer func() { _ = database.Close() }()

	if err := database.PingContext(ctx); err != nil {
		return storeError(ctx, "create or open database", err)
	}

	if err := os.Chmod(store.Path, 0o600); err != nil {
		return storeError(ctx, "restrict database permissions", err)
	}

	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return storeError(ctx, "begin reply save", err)
	}

	defer func() { _ = transaction.Rollback() }()

	if _, err := transaction.ExecContext(ctx, schemaSQL); err != nil {
		return storeError(ctx, "initialize database", err)
	}

	if len(record.Blocks) == 0 {
		record.Blocks = nil
	}

	blocks, err := json.Marshal(record.Blocks)
	if err != nil {
		return storeError(ctx, "encode blocks", err)
	}

	result, err := transaction.ExecContext(
		ctx,
		saveSQL,
		record.Reference.ConversationID,
		record.Reference.ReplyID,
		record.Provider,
		record.Digest,
		record.VisibleDigest,
		blocks,
	)
	if err != nil {
		return storeError(ctx, "save reply", err)
	}

	if err := checkInsert(ctx, result); err != nil {
		return err
	}

	if record.ToolDigest != "" {
		if err := saveToolIndex(ctx, transaction, record); err != nil {
			return err
		}
	}

	if err := transaction.Commit(); err != nil {
		return storeError(ctx, "commit reply", err)
	}

	return nil
}

func saveToolIndex(ctx context.Context, transaction *sql.Tx, record Record) error {
	if _, err := transaction.ExecContext(ctx, toolSchemaSQL); err != nil {
		return storeError(ctx, "initialize tool reply index", err)
	}

	result, err := transaction.ExecContext(
		ctx,
		saveToolSQL,
		record.Reference.ConversationID,
		record.Reference.ReplyID,
		record.ToolDigest,
	)
	if err != nil {
		return storeError(ctx, "index tool reply", err)
	}

	return checkInsert(ctx, result)
}

func checkInsert(ctx context.Context, result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return storeError(ctx, "check insert", err)
	}

	if count == 0 {
		return errConflictingReply
	}

	return nil
}

func (store Store) openExisting(ctx context.Context) (*sql.DB, error) {
	if store.Path == "" {
		return nil, errMissingPath
	}

	if _, err := os.Stat(store.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, storeError(ctx, "inspect database", err)
	}

	return store.open(ctx, false)
}

func (store Store) open(ctx context.Context, writable bool) (*sql.DB, error) {
	if store.Path == "" {
		return nil, errMissingPath
	}

	path, err := filepath.Abs(store.Path)
	if err != nil {
		return nil, storeError(ctx, "resolve database path", err)
	}

	query := "mode=ro&_busy_timeout=5000"
	if writable {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, storeError(ctx, "create database directory", err)
		}

		// Take the write lock before checking the schema. SQLite cannot wait
		// when two deferred transactions try to upgrade their read locks.
		query = "mode=rwc&_busy_timeout=5000&_txlock=immediate"
	}

	location := &url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: query,
	}
	db, err := sql.Open("sqlite3", location.String())
	if err != nil {
		return nil, storeError(ctx, "open database", err)
	}

	return db, nil
}

func storeError(ctx context.Context, operation string, err error) error {
	slog.WarnContext(ctx, "adapter.cursor.replay.store_failed", "operation", operation, "err", err)

	return fmt.Errorf("cursor replay: %s: %w", operation, err)
}
