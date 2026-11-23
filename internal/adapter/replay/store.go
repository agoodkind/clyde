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

	if _, err := database.ExecContext(ctx, schemaSQL); err != nil {
		return storeError(ctx, "initialize database", err)
	}

	if len(record.Blocks) == 0 {
		record.Blocks = nil
	}

	blocks, err := json.Marshal(record.Blocks)
	if err != nil {
		return storeError(ctx, "encode blocks", err)
	}

	result, err := database.ExecContext(
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

	count, err := result.RowsAffected()
	if err != nil {
		return storeError(ctx, "check insert", err)
	}

	if count == 0 {
		return errConflictingReply
	}

	return nil
}

// Load reads a reply without creating the database.
//
// A missing database or an unknown reference returns found=false.
//
//	record, found, err := store.Load(ctx, ref)
func (store Store) Load(ctx context.Context, ref Reference) (Record, bool, error) {
	var record Record
	if store.Path == "" {
		return record, false, errMissingPath
	}

	if _, err := os.Stat(store.Path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return record, false, nil
		}

		return record, false, storeError(ctx, "inspect database", err)
	}

	database, err := store.open(ctx, false)
	if err != nil {
		return record, false, err
	}

	defer func() { _ = database.Close() }()

	var blocks []byte
	err = database.QueryRowContext(ctx, loadSQL, ref.ConversationID, ref.ReplyID).
		Scan(
			&record.Provider,
			&record.Digest,
			&record.VisibleDigest,
			&blocks,
		)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}

	if err != nil {
		return record, false, storeError(ctx, "read reply", err)
	}

	if err := json.Unmarshal(blocks, &record.Blocks); err != nil {
		return record, false, storeError(ctx, "decode blocks", err)
	}

	record.Reference = ref

	return record, true, nil
}

func (store Store) open(ctx context.Context, writable bool) (*sql.DB, error) {
	if store.Path == "" {
		return nil, errMissingPath
	}

	path, err := filepath.Abs(store.Path)
	if err != nil {
		return nil, storeError(ctx, "resolve database path", err)
	}

	mode := "ro"
	if writable {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, storeError(ctx, "create database directory", err)
		}

		mode = "rwc"
	}

	location := &url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=" + mode + "&_busy_timeout=5000",
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
