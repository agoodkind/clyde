package cursorstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
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
	// RowID is SQLite's own row identifier. Cursor's key-value tables declare
	// `key TEXT UNIQUE ON CONFLICT REPLACE`, so a rewritten row is reinserted and
	// takes a new, higher rowid. That makes the rowid the store's record of write
	// order, which is the only ordering signal left when two rows share a
	// millisecond-coarse timestamp.
	RowID int64
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

// tableExistsQuery and tableColumnsQuery are the statements that answer what a
// database's schema holds, written once so the pooled read and the snapshot read
// ask them the same way.
const (
	tableExistsQuery  = "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?"
	tableColumnsQuery = "SELECT name FROM pragma_table_info(?)"
)

// readSnapshot is one read transaction over a Cursor database, which pins a
// single connection and a single view of the data for every statement inside it.
//
// A read whose answer depends on two statements agreeing needs this. Cursor
// writes while Clyde reads; a pooled handle hands consecutive statements
// different connections, and each takes its own snapshot of a WAL database, so a
// row appended between them can make a count and a classification of the same
// range disagree in a way that looks like everything reconciled.
type readSnapshot struct {
	tx *sql.Tx
}

// beginReadSnapshot opens the read transaction. The caller must roll it back,
// which for a read is how it is closed.
func beginReadSnapshot(ctx context.Context, db *sql.DB) (readSnapshot, error) {
	var empty readSnapshot

	if ctx == nil {
		return empty, fmt.Errorf("begin cursor read snapshot: nil context")
	}
	if db == nil {
		return empty, fmt.Errorf("begin cursor read snapshot: nil database")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelDefault, ReadOnly: true})
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.read_snapshot_failed", "concern", concern, "err", err)
		return empty, fmt.Errorf("begin cursor read snapshot: %w", err)
	}
	return readSnapshot{tx: tx}, nil
}

func (snapshot readSnapshot) rollback() {
	if snapshot.tx == nil {
		return
	}
	_ = snapshot.tx.Rollback()
}

// tableColumns answers the same question as [readTableColumns] against this
// snapshot.
func (snapshot readSnapshot) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := snapshot.tx.QueryContext(ctx, tableColumnsQuery, table)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.table_columns_query_failed", "concern", concern, "table", table, "err", err)
		return nil, fmt.Errorf("query cursor %s columns: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	return scanTableColumns(ctx, rows, table)
}

// tableExists answers the same question as [TableExists] against this snapshot.
func (snapshot readSnapshot) tableExists(ctx context.Context, table string) (bool, error) {
	var count int
	if err := snapshot.tx.QueryRowContext(ctx, tableExistsQuery, table).Scan(&count); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.sqlite_master_query_failed", "concern", concern, "table", table, "err", err)
		return false, fmt.Errorf("query sqlite_master for %s: %w", table, err)
	}
	return count > 0, nil
}

// queryRange runs one statement over a key range on this snapshot, binding the
// range bounds the same way the pooled read does.
func (snapshot readSnapshot) queryRange(
	ctx context.Context,
	query string,
	bounds keyRange,
	reads string,
) (*sql.Rows, error) {
	var rows *sql.Rows
	var err error
	if bounds.HasUpper {
		rows, err = snapshot.tx.QueryContext(ctx, query, bounds.Lower, bounds.Upper)
	} else {
		rows, err = snapshot.tx.QueryContext(ctx, query, bounds.Lower)
	}
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.kv_range_query_failed", "concern", concern, "reads", reads, "key_lower", bounds.Lower, "err", err)
		return nil, fmt.Errorf("query cursor %s in key range %q: %w", reads, bounds.Lower, err)
	}
	return rows, nil
}

// countRange counts the rows of one `cursorDiskKV` key range on this snapshot,
// from the key index alone.
//
// The table is named here rather than taken as an argument because counting a key
// range is a `cursorDiskKV` operation. Cursor's other key-value table, `ItemTable`,
// stores one row per exact item key and is only ever read by that key, so it has
// no ranges to count.
func (snapshot readSnapshot) countRange(ctx context.Context, bounds keyRange) (int, error) {
	sqlTableName, err := KVTableCursorDiskKV.sqlTableName()
	if err != nil {
		return 0, err
	}
	exists, err := snapshot.tableExists(ctx, sqlTableName)
	if err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}

	query := "SELECT count(*) FROM " + sqlTableName + keyRangePredicate(bounds, "")
	var count int
	if bounds.HasUpper {
		err = snapshot.tx.QueryRowContext(ctx, query, bounds.Lower, bounds.Upper).Scan(&count)
	} else {
		err = snapshot.tx.QueryRowContext(ctx, query, bounds.Lower).Scan(&count)
	}
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.kv_rows_count_failed", "concern", concern, "table", sqlTableName, "key_lower", bounds.Lower, "err", err)
		return 0, fmt.Errorf("count cursor %s rows in key range %q: %w", sqlTableName, bounds.Lower, err)
	}
	return count, nil
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
	err := db.QueryRowContext(ctx, tableExistsQuery, table).Scan(&count)
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

	return readKVValueInKnownTable(ctx, db, tableName, key)
}

// readKVValueInKnownTable reads one exact key without re-proving that the table
// exists. It is for a caller that has already read the table through the same
// database handle, where a per-row `sqlite_master` query would be pure overhead:
// streaming one chat's bubbles issues thousands of these reads.
func readKVValueInKnownTable(
	ctx context.Context,
	db *sql.DB,
	tableName KVTableName,
	key string,
) ([]byte, bool, error) {
	value, _, found, err := readKVRowInKnownTable(ctx, db, tableName, key)
	return value, found, err
}

// readKVRowInKnownTable reads one exact key and the row's rowid, which is what a
// caller comparing this read against an earlier one needs: a rewritten row is
// reinserted under a new rowid, so an unchanged rowid is the store's own evidence
// that the value is the same one.
func readKVRowInKnownTable(
	ctx context.Context,
	db *sql.DB,
	tableName KVTableName,
	key string,
) ([]byte, int64, bool, error) {
	sqlTableName, err := tableName.sqlTableName()
	if err != nil {
		return nil, 0, false, err
	}
	query, err := tableName.selectRowByKeyQuery()
	if err != nil {
		return nil, 0, false, err
	}
	var value []byte
	var writeOrder int64
	err = db.QueryRowContext(ctx, query, key).Scan(&writeOrder, &value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, false, nil
		}
		slog.WarnContext(ctx, "providers.cursor.store.kv_value_scan_failed", "concern", concern, "table", sqlTableName, "key", key, "err", err)
		return nil, 0, false, fmt.Errorf("scan cursor %s value for key %q: %w", sqlTableName, key, err)
	}
	return append([]byte(nil), value...), writeOrder, true, nil
}

// ReadKVRowsByPrefix reads key-value rows whose keys begin with one prefix from
// a known Cursor key-value table.
//
// The predicate is a half-open key range rather than a computed prefix test, so
// SQLite seeks the table's primary key index instead of scanning every row. That
// distinction is load bearing: Cursor's global `cursorDiskKV` table is tens of
// gigabytes and its key index is the only index it has.
func ReadKVRowsByPrefix(ctx context.Context, db *sql.DB, tableName KVTableName, keyPrefix string) ([]KVRow, error) {
	rows, err := readKVRowsInKeyRange(ctx, db, tableName, keyRangeForPrefix(keyPrefix), "")
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// keyRange is the half-open key interval `[Lower, Upper)` that selects one key
// prefix through a Cursor key-value table's primary key index. HasUpper is false
// only when the prefix has no successor, which leaves the range open ended.
type keyRange struct {
	Lower    string
	Upper    string
	HasUpper bool
}

// keyRangeForPrefix converts one key prefix into the half-open range that
// selects exactly the keys carrying it, under SQLite's default BINARY collation.
func keyRangeForPrefix(keyPrefix string) keyRange {
	upper, hasUpper := keyPrefixUpperBound(keyPrefix)
	return keyRange{Lower: keyPrefix, Upper: upper, HasUpper: hasUpper}
}

// keyPrefixUpperBound returns the first key that sorts after every key carrying
// the prefix. It increments the last byte below 0xFF and drops the rest, which
// is the exclusive bound for a byte-ordered index. The second result is false
// when no such bound exists, meaning the range runs to the end of the table.
func keyPrefixUpperBound(keyPrefix string) (string, bool) {
	raw := []byte(keyPrefix)
	for index, value := range slices.Backward(raw) {
		if value == 0xFF {
			continue
		}
		bound := make([]byte, index+1)
		copy(bound, raw[:index+1])
		bound[index]++
		return string(bound), true
	}
	return "", false
}

// readKVRowsInKeyRange reads the rows of one key range, optionally keeping only
// the rows whose value contains valueSubstring as a raw byte sequence.
//
// The substring filter is a cheap prefilter, not a match: it runs inside SQLite
// so a non-matching row is never transferred, and the caller still has to decode
// the surviving rows and compare the field it cares about for exact equality.
// Passing an empty valueSubstring disables the filter.
func readKVRowsInKeyRange(
	ctx context.Context,
	db *sql.DB,
	tableName KVTableName,
	bounds keyRange,
	valueSubstring string,
) ([]KVRow, error) {
	out := make([]KVRow, 0)
	err := forEachKVRowInKeyRange(ctx, db, tableName, bounds, valueSubstring, func(row KVRow) error {
		out = append(out, row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// forEachKVRowInKeyRange streams the rows of one key range through visit, so a
// caller that only needs a projection of each value never holds the whole range
// in memory. Cursor's bubble payloads carry large attached context, and one
// chat's range can be tens of megabytes.
func forEachKVRowInKeyRange(
	ctx context.Context,
	db *sql.DB,
	tableName KVTableName,
	bounds keyRange,
	valueSubstring string,
	visit func(KVRow) error,
) error {
	if ctx == nil {
		return fmt.Errorf("read cursor rows in key range %q: nil context", bounds.Lower)
	}
	if db == nil {
		return fmt.Errorf("read cursor rows in key range %q: nil database", bounds.Lower)
	}
	sqlTableName, err := tableName.sqlTableName()
	if err != nil {
		return err
	}
	exists, err := TableExists(ctx, db, sqlTableName)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	rows, err := queryKVRangeContext(
		ctx, db,
		selectRowsInKeyRangeQuery(sqlTableName, bounds, valueSubstring),
		bounds, valueSubstring, sqlTableName+" rows",
	)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var row KVRow
		if err := rows.Scan(&row.RowID, &row.Key, &row.Value); err != nil {
			slog.WarnContext(ctx, "providers.cursor.store.kv_row_scan_failed", "concern", concern, "table", sqlTableName, "key_lower", bounds.Lower, "err", err)
			return fmt.Errorf("scan cursor %s row in key range %q: %w", sqlTableName, bounds.Lower, err)
		}
		row.Value = append([]byte(nil), row.Value...)
		if err := visit(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.kv_rows_iterate_failed", "concern", concern, "table", sqlTableName, "key_lower", bounds.Lower, "err", err)
		return fmt.Errorf("iterate cursor %s rows in key range %q: %w", sqlTableName, bounds.Lower, err)
	}
	return nil
}

// selectRowsInKeyRangeQuery builds the range query. Placeholders bind the range
// bounds and the substring in that order, and the table name is a validated
// constant from [KVTableName] rather than caller text. The rowid comes along
// because it is the only record of write order these tables keep.
func selectRowsInKeyRangeQuery(sqlTableName string, bounds keyRange, valueSubstring string) string {
	return "SELECT rowid, key, value FROM " + sqlTableName +
		keyRangePredicate(bounds, valueSubstring) + " ORDER BY key"
}

// keyRangePredicate renders the half-open key predicate every range read shares,
// plus the optional byte search that narrows it, so the placeholders are written
// in one order by one piece of code and bound in that order by
// [queryKVRangeContext].
func keyRangePredicate(bounds keyRange, valueSubstring string) string {
	var predicate strings.Builder
	predicate.WriteString(" WHERE key >= ?")
	if bounds.HasUpper {
		predicate.WriteString(" AND key < ?")
	}
	if valueSubstring != "" {
		// CAST both sides to BLOB so the search compares raw bytes whatever
		// storage class Cursor wrote the value with.
		predicate.WriteString(" AND instr(CAST(value AS BLOB), CAST(? AS BLOB)) > 0")
	}
	return predicate.String()
}

// queryKVRangeContext runs one statement over a key range, binding the range
// bounds and the optional byte search that [keyRangePredicate] wrote. It exists
// so a caller adding its own projection to a range read repeats neither the
// placeholder-arity branch nor the failure report, and reads is what the read was
// for.
func queryKVRangeContext(
	ctx context.Context,
	db *sql.DB,
	query string,
	bounds keyRange,
	valueSubstring string,
	reads string,
) (*sql.Rows, error) {
	var rows *sql.Rows
	var err error
	switch {
	case bounds.HasUpper && valueSubstring != "":
		rows, err = db.QueryContext(ctx, query, bounds.Lower, bounds.Upper, valueSubstring)
	case bounds.HasUpper:
		rows, err = db.QueryContext(ctx, query, bounds.Lower, bounds.Upper)
	case valueSubstring != "":
		rows, err = db.QueryContext(ctx, query, bounds.Lower, valueSubstring)
	default:
		rows, err = db.QueryContext(ctx, query, bounds.Lower)
	}
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.kv_range_query_failed", "concern", concern, "reads", reads, "key_lower", bounds.Lower, "err", err)
		return nil, fmt.Errorf("query cursor %s in key range %q: %w", reads, bounds.Lower, err)
	}
	return rows, nil
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

func (tableName KVTableName) selectRowByKeyQuery() (string, error) {
	switch tableName {
	case KVTableItemTable:
		return "SELECT rowid, value FROM ItemTable WHERE key = ?", nil
	case KVTableCursorDiskKV:
		return "SELECT rowid, value FROM cursorDiskKV WHERE key = ?", nil
	default:
		return "", UnknownKVTableNameError{TableName: tableName}
	}
}
