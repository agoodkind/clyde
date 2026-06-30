package cursorstore

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"
)

// KVTableName names one Cursor key-value table that this package can query.
type KVTableName string

const (
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

// OpenReadOnlyDatabase opens one Cursor SQLite database in read-only immutable
// mode.
func OpenReadOnlyDatabase(ctx context.Context, path string) (*sql.DB, error) {
	if ctx == nil {
		return nil, fmt.Errorf("open cursor sqlite database %s: nil context", path)
	}
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=ro&immutable=1&_busy_timeout=5000",
	}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open cursor sqlite database %s: %w", path, err)
	}
	if pingErr := db.PingContext(ctx); pingErr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping cursor sqlite database %s: %w", path, pingErr)
	}
	return db, nil
}

// TableExists reports whether one named table exists in the SQLite database.
func TableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		table,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("query sqlite_master for %s: %w", table, err)
	}
	return count > 0, nil
}

// ReadKVValue reads one exact key from a known Cursor key-value table.
func ReadKVValue(ctx context.Context, db *sql.DB, tableName KVTableName, key string) ([]byte, bool, error) {
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

	query := fmt.Sprintf("SELECT value FROM %s WHERE key = ?", sqlTableName)
	var value []byte
	err = db.QueryRowContext(ctx, query, key).Scan(&value)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("scan cursor %s value for key %q: %w", sqlTableName, key, err)
	}
	return append([]byte(nil), value...), true, nil
}

// ReadKVRowsByPrefix reads key-value rows whose keys begin with one prefix from
// a known Cursor key-value table.
func ReadKVRowsByPrefix(ctx context.Context, db *sql.DB, tableName KVTableName, keyPrefix string) ([]KVRow, error) {
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

	query := fmt.Sprintf("SELECT key, value FROM %s WHERE key LIKE ? || '%%' ORDER BY key", sqlTableName)
	rows, err := db.QueryContext(ctx, query, keyPrefix)
	if err != nil {
		return nil, fmt.Errorf("query cursor %s rows by prefix %q: %w", sqlTableName, keyPrefix, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]KVRow, 0)
	for rows.Next() {
		var row KVRow
		if err := rows.Scan(&row.Key, &row.Value); err != nil {
			return nil, fmt.Errorf("scan cursor %s row by prefix %q: %w", sqlTableName, keyPrefix, err)
		}
		row.Value = append([]byte(nil), row.Value...)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
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
