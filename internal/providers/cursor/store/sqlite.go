package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"

	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"
)

// KVTableName names one Cursor key-value table that this package can query.
type KVTableName string

const (
	concern = "providers.cursor.store"

	// KVTableItemTable names Cursor's legacy ItemTable key-value table.
	KVTableItemTable KVTableName = "ItemTable"
	// KVTableCursorDiskKV names Cursor's cursorDiskKV key-value table.
	KVTableCursorDiskKV KVTableName = "cursorDiskKV"
)

// KVRow is one key-value row from a Cursor SQLite table.
type KVRow struct {
	Key   string
	Value []byte
}

// UnknownKVTableNameError reports a request for a table outside Cursor's known
// key-value table set.
type UnknownKVTableNameError struct {
	TableName KVTableName
}

// Error renders the unsupported key-value table name.
func (err UnknownKVTableNameError) Error() string {
	return fmt.Sprintf("unsupported cursor kv table %q", err.TableName)
}

// OpenReadOnlyDatabase opens one Cursor SQLite database in read-only mode.
func OpenReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	if ctx == nil {
		return nil, fmt.Errorf("open cursor sqlite database %s: nil context", path)
	}
	dsn := readOnlyDatabaseDSN(path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.sqlite_open_failed", "concern", concern, "path", path, "err", err)
		return nil, fmt.Errorf("open cursor sqlite database %s: %w", path, err)
	}
	if pingErr := db.PingContext(ctx); pingErr != nil {
		_ = db.Close()
		slog.WarnContext(ctx, "providers.cursor.store.sqlite_ping_failed", "concern", concern, "path", path, "err", pingErr)
		return nil, fmt.Errorf("ping cursor sqlite database %s: %w", path, pingErr)
	}
	return db, nil
}

func readOnlyDatabaseDSN(path string) string {
	normalizedPath := strings.ReplaceAll(path, `\`, "/")
	normalizedPath = filepath.ToSlash(normalizedPath)
	if len(normalizedPath) >= 2 && normalizedPath[1] == ':' {
		normalizedPath = "/" + normalizedPath
	}
	return (&url.URL{
		Scheme:   "file",
		Path:     normalizedPath,
		RawQuery: "mode=ro&_busy_timeout=5000",
	}).String()
}

// TableExists reports whether one named table exists in the SQLite database.
func TableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("query sqlite_master for %s: nil context", table)
	}
	if db == nil {
		return false, fmt.Errorf("query sqlite_master for %s: nil database", table)
	}
	var count int
	err := db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		table,
	).Scan(&count)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.sqlite_master_query_failed", "concern", concern, "table", table, "err", err)
		return false, fmt.Errorf("query sqlite_master for %s: %w", table, err)
	}
	return count > 0, nil
}

// ReadKVValue reads one exact key from a known Cursor key-value table.
func ReadKVValue(ctx context.Context, db *sql.DB, tableName KVTableName, key string) ([]byte, bool, error) {
	if ctx == nil {
		return nil, false, fmt.Errorf("read cursor value for key %q: nil context", key)
	}
	if db == nil {
		return nil, false, fmt.Errorf("read cursor value for key %q: nil database", key)
	}
	sqlTableName, err := tableName.sqlTableName()
	if err != nil {
		return nil, false, err
	}
	exists, err := TableExists(ctx, db, sqlTableName)
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}

	query, err := tableName.selectValueByKeyQuery()
	if err != nil {
		return nil, false, err
	}
	var value []byte
	err = db.QueryRowContext(ctx, query, key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		slog.WarnContext(ctx, "providers.cursor.store.kv_value_scan_failed", "concern", concern, "table", sqlTableName, "key", key, "err", err)
		return nil, false, fmt.Errorf("scan cursor %s value for key %q: %w", sqlTableName, key, err)
	}
	return append([]byte(nil), value...), true, nil
}

// ReadKVRowsByPrefix reads key-value rows whose keys begin with one prefix from
// a known Cursor key-value table.
func ReadKVRowsByPrefix(ctx context.Context, db *sql.DB, tableName KVTableName, keyPrefix string) ([]KVRow, error) {
	if ctx == nil {
		return nil, fmt.Errorf("read cursor rows by prefix %q: nil context", keyPrefix)
	}
	if db == nil {
		return nil, fmt.Errorf("read cursor rows by prefix %q: nil database", keyPrefix)
	}
	sqlTableName, err := tableName.sqlTableName()
	if err != nil {
		return nil, err
	}
	exists, err := TableExists(ctx, db, sqlTableName)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	query, err := tableName.selectRowsByPrefixQuery()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, query, keyPrefix, keyPrefix)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.kv_rows_query_failed", "concern", concern, "table", sqlTableName, "key_prefix", keyPrefix, "err", err)
		return nil, fmt.Errorf("query cursor %s rows by prefix %q: %w", sqlTableName, keyPrefix, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]KVRow, 0)
	for rows.Next() {
		var row KVRow
		if err := rows.Scan(&row.Key, &row.Value); err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.kv_row_scan_failed", "concern", concern, "table", sqlTableName, "key_prefix", keyPrefix, "err", err)
			return nil, fmt.Errorf("scan cursor %s row by prefix %q: %w", sqlTableName, keyPrefix, err)
		}
		row.Value = append([]byte(nil), row.Value...)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.kv_rows_iterate_failed", "concern", concern, "table", sqlTableName, "key_prefix", keyPrefix, "err", err)
		return nil, fmt.Errorf("iterate cursor %s rows by prefix %q: %w", sqlTableName, keyPrefix, err)
	}
	return out, nil
}

func (tableName KVTableName) sqlTableName() (string, error) {
	switch tableName {
	case KVTableItemTable:
		return string(KVTableItemTable), nil
	case KVTableCursorDiskKV:
		return string(KVTableCursorDiskKV), nil
	default:
		return "", UnknownKVTableNameError{TableName: tableName}
	}
}

func (tableName KVTableName) selectValueByKeyQuery() (string, error) {
	switch tableName {
	case KVTableItemTable:
		return "SELECT value FROM ItemTable WHERE key = ?", nil
	case KVTableCursorDiskKV:
		return "SELECT value FROM cursorDiskKV WHERE key = ?", nil
	default:
		return "", UnknownKVTableNameError{TableName: tableName}
	}
}

func (tableName KVTableName) selectRowsByPrefixQuery() (string, error) {
	switch tableName {
	case KVTableItemTable:
		return "SELECT key, value FROM ItemTable WHERE substr(key, 1, length(?)) = ? ORDER BY key", nil
	case KVTableCursorDiskKV:
		return "SELECT key, value FROM cursorDiskKV WHERE substr(key, 1, length(?)) = ? ORDER BY key", nil
	default:
		return "", UnknownKVTableNameError{TableName: tableName}
	}
}
