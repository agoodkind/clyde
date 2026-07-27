package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	sq "github.com/Masterminds/squirrel"
)

// ErrEmptyRequestLookup is returned when a lookup names neither a request id
// nor a trace id. An empty lookup would otherwise match the many stored rows
// that carry neither, so it is refused rather than answered.
var ErrEmptyRequestLookup = errors.New("capture: request lookup needs a request id or a trace id")

// RequestLookup names one captured request by correlation id. RequestID wins
// when both are set, since it identifies a single exchange while a trace can
// span several.
type RequestLookup struct {
	RequestID string
	TraceID   string
}

// CapturedRequest is the queryable summary of one stored exchange. It carries
// no body and no headers, so listing a conversation's requests never moves
// captured prompt or response content.
type CapturedRequest struct {
	RowID              int64
	Timestamp          time.Time
	Client             string
	Provider           string
	Host               string
	Method             string
	Path               string
	Status             int
	RequestID          string
	TraceID            string
	ConversationSource ConversationSource
}

// requestConversationColumn names one additive column on the requests table and
// carries its own literal DDL. The statement is a constant per column rather
// than a string built from a name, so no identifier is ever concatenated into
// SQL.
type requestConversationColumn struct {
	Name string
	DDL  string
}

// requestConversationColumns are the join columns added to an existing requests
// table. Fresh databases already get them from schema.sql.
var requestConversationColumns = []requestConversationColumn{
	{Name: "conversation_id", DDL: `ALTER TABLE requests ADD COLUMN conversation_id TEXT NOT NULL DEFAULT ''`},
	{Name: "conversation_source", DDL: `ALTER TABLE requests ADD COLUMN conversation_source TEXT NOT NULL DEFAULT ''`},
}

// ensureRequestConversationColumns adds the conversation join columns to an
// existing requests table and indexes them. It is additive: it only ever adds
// missing columns and a missing index, and never rewrites or drops a row. The
// index lives here rather than in schema.sql because schema.sql runs before
// these columns exist on a database created without them.
func ensureRequestConversationColumns(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	existing, err := readRequestColumnNames(ctx, db, log)
	if err != nil {
		return err
	}
	for _, column := range requestConversationColumns {
		if existing[column.Name] {
			continue
		}
		if _, err := db.ExecContext(ctx, column.DDL); err != nil {
			log.WarnContext(ctx, "mitm.capture.add_request_conversation_column_failed", "column", column.Name, "err", err)
			return fmt.Errorf("capture: add request column %s: %w", column.Name, err)
		}
	}
	const createIndex = `CREATE INDEX IF NOT EXISTS idx_requests_conversation_id ON requests(conversation_id, ts)`
	if _, err := db.ExecContext(ctx, createIndex); err != nil {
		log.WarnContext(ctx, "mitm.capture.create_request_conversation_index_failed", "err", err)
		return fmt.Errorf("capture: create request conversation index: %w", err)
	}
	return nil
}

func readRequestColumnNames(ctx context.Context, db *sql.DB, log *slog.Logger) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(requests)`)
	if err != nil {
		log.WarnContext(ctx, "mitm.capture.inspect_request_columns_failed", "err", err)
		return nil, fmt.Errorf("capture: inspect request columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	names := make(map[string]bool)
	for rows.Next() {
		var columnID int
		var name string
		var columnType string
		var notNull int
		var defaultValue *string
		var primaryKey int
		if err := rows.Scan(&columnID, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			log.WarnContext(ctx, "mitm.capture.scan_request_column_failed", "err", err)
			return nil, fmt.Errorf("capture: scan request column: %w", err)
		}
		names[name] = true
	}
	if err := rows.Err(); err != nil {
		log.WarnContext(ctx, "mitm.capture.iterate_request_columns_failed", "err", err)
		return nil, fmt.Errorf("capture: iterate request columns: %w", err)
	}
	return names, nil
}

// ConversationForRequest resolves the conversation one captured request belongs
// to, from its request id or trace id.
//
// It reads the stored join column first. When that column is empty, which is
// the case for every row captured before the column existed, and resolver is
// non-nil, it re-reads the row's stored request body and asks the resolver.
// That fallback is read-only: history is resolved on read rather than migrated,
// so no existing row is ever rewritten. Pass a nil resolver for a column-only
// read.
//
// A request that names no conversation resolves to the unresolved ref, not to a
// nearby or guessed conversation.
func (s *Store) ConversationForRequest(ctx context.Context, lookup RequestLookup, resolver ConversationRefResolver) (ConversationRef, error) {
	if s == nil {
		return UnresolvedConversation(), nil
	}
	condition, err := lookupCondition(lookup)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.conversation_lookup_empty", "err", err)
		return UnresolvedConversation(), err
	}
	query, args, err := sq.Select("id", "conversation_id", "conversation_source").
		From("requests").
		Where(condition).
		OrderBy("ts DESC").
		Limit(1).
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_conversation_lookup_failed", "err", err)
		return UnresolvedConversation(), fmt.Errorf("capture: build conversation lookup: %w", err)
	}
	var rowID int64
	var conversationID string
	var source string
	scanErr := s.rdb.QueryRowContext(ctx, query, args...).Scan(&rowID, &conversationID, &source)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return UnresolvedConversation(), nil
	}
	if scanErr != nil {
		s.log.WarnContext(ctx, "mitm.capture.conversation_lookup_failed", "err", scanErr)
		return UnresolvedConversation(), fmt.Errorf("capture: resolve conversation for request: %w", scanErr)
	}
	if conversationID != "" {
		return ConversationRef{ConversationID: conversationID, Source: ConversationSource(source)}, nil
	}
	if resolver == nil {
		return UnresolvedConversation(), nil
	}
	return s.conversationFromStoredBody(ctx, rowID, resolver)
}

// conversationFromStoredBody re-runs a provider resolver over one stored
// request body. It reads; it never writes.
func (s *Store) conversationFromStoredBody(ctx context.Context, rowID int64, resolver ConversationRefResolver) (ConversationRef, error) {
	input, found, err := s.readStoredRequestInput(ctx, rowID)
	if err != nil {
		return UnresolvedConversation(), err
	}
	if !found {
		return UnresolvedConversation(), nil
	}
	ref, supported := resolver.ResolveConversation(input)
	if !supported {
		return UnresolvedConversation(), nil
	}
	return ref, nil
}

// readStoredRequestInput loads one row's path, request content type, request
// headers, and stored request bytes.
func (s *Store) readStoredRequestInput(ctx context.Context, rowID int64) (RequestConversationInput, bool, error) {
	query, args, err := sq.Select("r.path", "r.req_content_type", "r.req_headers", "b.data").
		From("requests r").
		LeftJoin("bodies b ON b.request_row_id = r.id AND b.which = ?", string(BodyRequest)).
		Where(sq.Eq{"r.id": rowID}).
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_stored_body_read_failed", "row_id", rowID, "err", err)
		return emptyRequestConversationInput(), false, fmt.Errorf("capture: build stored body read: %w", err)
	}
	var path string
	var contentType string
	var encodedHeaders string
	var body []byte
	scanErr := s.rdb.QueryRowContext(ctx, query, args...).Scan(&path, &contentType, &encodedHeaders, &body)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return emptyRequestConversationInput(), false, nil
	}
	if scanErr != nil {
		s.log.WarnContext(ctx, "mitm.capture.stored_body_read_failed", "row_id", rowID, "err", scanErr)
		return emptyRequestConversationInput(), false, fmt.Errorf("capture: read stored body for row %d: %w", rowID, scanErr)
	}
	return RequestConversationInput{
		Path:        path,
		ContentType: contentType,
		Headers:     decodeHeaders(encodedHeaders),
		Body:        body,
	}, true, nil
}

func emptyRequestConversationInput() RequestConversationInput {
	return RequestConversationInput{Path: "", ContentType: "", Headers: nil, Body: nil}
}

// decodeHeaders reverses encodeHeaders. A header blob that does not parse
// yields no headers rather than an error, since the resolver falls back to the
// stored content type.
func decodeHeaders(encoded string) http.Header {
	if encoded == "" {
		return nil
	}
	headers := http.Header{}
	if err := json.Unmarshal([]byte(encoded), &headers); err != nil {
		return nil
	}
	return headers
}

func lookupCondition(lookup RequestLookup) (sq.Eq, error) {
	if lookup.RequestID != "" {
		return sq.Eq{"request_id": lookup.RequestID}, nil
	}
	if lookup.TraceID != "" {
		return sq.Eq{"trace_id": lookup.TraceID}, nil
	}
	return nil, ErrEmptyRequestLookup
}

// CapturedRequestsForConversation lists every stored request tagged with
// conversationID, oldest first, so the caller sees the conversation's captured
// traffic in the order it happened. The match is exact: an id that merely
// contains conversationID as a substring is a different conversation.
//
// This direction reads the join column only. Rows captured before the column
// existed are absent from the listing, because finding them would mean decoding
// the stored body of every row in the store. [Store.ConversationForRequest] is
// the direction that resolves those rows, one at a time, from a correlation id
// the caller already has.
func (s *Store) CapturedRequestsForConversation(ctx context.Context, conversationID string) ([]CapturedRequest, error) {
	if s == nil || conversationID == "" {
		return nil, nil
	}
	query, args, err := sq.Select("id", "ts", "client", "provider", "host", "method", "path", "status",
		"request_id", "trace_id", "conversation_source").
		From("requests").
		Where(sq.Eq{"conversation_id": conversationID}).
		OrderBy("ts ASC", "id ASC").
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_conversation_requests_failed", "conversation_id", conversationID, "err", err)
		return nil, fmt.Errorf("capture: build conversation request listing: %w", err)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.conversation_requests_failed", "conversation_id", conversationID, "err", err)
		return nil, fmt.Errorf("capture: list requests for conversation %s: %w", conversationID, err)
	}
	defer func() { _ = rows.Close() }()
	captured := make([]CapturedRequest, 0)
	for rows.Next() {
		var record CapturedRequest
		var timestampNanos int64
		var source string
		scanErr := rows.Scan(&record.RowID, &timestampNanos, &record.Client, &record.Provider, &record.Host,
			&record.Method, &record.Path, &record.Status, &record.RequestID, &record.TraceID, &source)
		if scanErr != nil {
			s.log.WarnContext(ctx, "mitm.capture.scan_conversation_request_failed", "conversation_id", conversationID, "err", scanErr)
			return nil, fmt.Errorf("capture: scan request for conversation %s: %w", conversationID, scanErr)
		}
		record.Timestamp = time.Unix(0, timestampNanos)
		record.ConversationSource = ConversationSource(source)
		captured = append(captured, record)
	}
	if err := rows.Err(); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.iterate_conversation_requests_failed", "conversation_id", conversationID, "err", err)
		return nil, fmt.Errorf("capture: iterate requests for conversation %s: %w", conversationID, err)
	}
	return captured, nil
}
