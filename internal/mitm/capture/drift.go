package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	sq "github.com/Masterminds/squirrel"
)

// DriftShape is one deduped native-request shape captured for baseline learning.
// The proxy derives the fingerprint and masks the headers before handing the
// shape to the store; identical fingerprints collapse to one row whose
// seen_count and last_seen advance. RequestBody is the field-set summary
// (no prompt text); RequestFeatures is the prompt-free feature vector JSON.
type DriftShape struct {
	Timestamp          time.Time
	Provider           string
	Upstream           string
	Fingerprint        string
	Method             string
	Path               string
	URL                string
	RequestHeaders     map[string]string
	RequestBody        json.RawMessage
	BillingAttestation string
	RequestFeatures    json.RawMessage
	// SeenCount is populated on read (the dedup weight); writers leave it zero.
	SeenCount int
}

// DriftCheck is one periodic drift-check run outcome. The durable per-difference
// detail lives in baseline_diffs; this records cadence and the diverged flag.
type DriftCheck struct {
	Timestamp time.Time
	Upstream  string
	Diverged  bool
	Summary   string
}

// BaselineDiff is one distinct baseline difference (deduped on
// upstream+flavor+category+field+before+after). Re-observing it bumps last_seen
// and seen_count rather than adding a row.
type BaselineDiff struct {
	Upstream  string
	Flavor    string
	Category  string
	Field     string
	Before    string
	After     string
	Reason    string
	FirstSeen time.Time
	LastSeen  time.Time
	SeenCount int
}

// BaselineChange carries one baseline update: the new authoritative snapshot for
// the upstream plus the flattened set of differences against the previous
// baseline to fold into the difference matrix.
type BaselineChange struct {
	Timestamp time.Time
	Upstream  string
	Snapshot  json.RawMessage
	Diffs     []BaselineDiff
}

// driftShapeColumns is the column order both the corpus insert and the
// shape read share, so the builder and the row scan stay aligned.
var driftShapeColumns = []string{
	"provider", "upstream", "fingerprint", "last_seen", "seen_count",
	"method", "path", "url", "request_headers", "request_body",
	"billing_attestation", "request_features",
}

// baselineDiffColumns is the column order the difference-matrix read shares
// with the BaselineDiff scan.
var baselineDiffColumns = []string{
	"upstream", "flavor", "category", "field", "before_value", "after_value",
	"reason", "first_seen", "last_seen", "seen_count",
}

// RecordShape enqueues a native-request shape for asynchronous upsert. It never
// blocks: a full queue drops the shape with a warning, since corpus capture is
// best-effort. A nil receiver or closed store is a no-op.
func (s *Store) RecordShape(shape DriftShape) {
	if s == nil || s.closed.Load() {
		return
	}
	select {
	case s.shapes <- shape:
	default:
		s.log.Warn("mitm.capture.drift_shape_dropped", "reason", "queue_full", "upstream", shape.Upstream)
	}
}

// RecordCheck enqueues a drift-check outcome for asynchronous insert. Non-blocking.
func (s *Store) RecordCheck(check DriftCheck) {
	if s == nil || s.closed.Load() {
		return
	}
	select {
	case s.checks <- check:
	default:
		s.log.Warn("mitm.capture.drift_check_dropped", "reason", "queue_full", "upstream", check.Upstream)
	}
}

func (s *Store) insertShape(ctx context.Context, shape DriftShape) {
	ts := shape.Timestamp.UnixNano()
	query, args, err := sq.Insert("drift_shapes").
		Columns("provider", "upstream", "fingerprint", "first_seen", "last_seen", "seen_count",
			"method", "path", "url", "request_headers", "request_body", "billing_attestation", "request_features").
		Values(shape.Provider, shape.Upstream, shape.Fingerprint, ts, ts, 1,
			shape.Method, shape.Path, shape.URL,
			encodeStringMap(shape.RequestHeaders), jsonOrEmpty(shape.RequestBody),
			shape.BillingAttestation, jsonOrEmpty(shape.RequestFeatures)).
		Suffix(`ON CONFLICT(upstream, fingerprint) DO UPDATE SET
			last_seen=excluded.last_seen,
			seen_count=drift_shapes.seen_count+1,
			method=excluded.method, path=excluded.path, url=excluded.url,
			request_headers=excluded.request_headers, request_body=excluded.request_body,
			billing_attestation=excluded.billing_attestation, request_features=excluded.request_features`).
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_shape_insert_failed", "upstream", shape.Upstream, "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.insert_shape_failed", "upstream", shape.Upstream, "err", err)
	}
}

func (s *Store) insertCheck(ctx context.Context, check DriftCheck) {
	query, args, err := sq.Insert("drift_checks").
		Columns("upstream", "ts", "diverged", "summary").
		Values(check.Upstream, check.Timestamp.UnixNano(), boolToInt(check.Diverged), check.Summary).
		ToSql()
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.build_check_insert_failed", "upstream", check.Upstream, "err", err)
		return
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		s.log.WarnContext(ctx, "mitm.capture.insert_check_failed", "upstream", check.Upstream, "err", err)
	}
}

// PutBaseline upserts the current baseline row for the upstream and folds each
// difference into the deduped matrix, in one transaction. Unlike the corpus and
// check writes, this is synchronous and returns an error because the refresh
// path needs to know it persisted. It runs on the single-writer connection, so
// database/sql serializes it behind any in-flight corpus insert.
func (s *Store) PutBaseline(ctx context.Context, change BaselineChange) error {
	if s == nil {
		return fmt.Errorf("capture: PutBaseline on nil store")
	}
	ts := change.Timestamp.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.baseline_tx_begin_failed", "upstream", change.Upstream, "err", err)
		return fmt.Errorf("capture: begin baseline tx: %w", err)
	}

	baselineQuery, baselineArgs, err := sq.Insert("baselines").
		Columns("upstream", "snapshot", "updated_at").
		Values(change.Upstream, jsonOrEmpty(change.Snapshot), ts).
		Suffix(`ON CONFLICT(upstream) DO UPDATE SET snapshot=excluded.snapshot, updated_at=excluded.updated_at`).
		ToSql()
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("capture: build baseline upsert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, baselineQuery, baselineArgs...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("capture: upsert baseline: %w", err)
	}

	for _, d := range change.Diffs {
		diffQuery, diffArgs, err := sq.Insert("baseline_diffs").
			Columns("upstream", "flavor", "category", "field", "before_value", "after_value", "reason",
				"first_seen", "last_seen", "seen_count").
			Values(d.Upstream, d.Flavor, d.Category, d.Field, d.Before, d.After, d.Reason, ts, ts, 1).
			Suffix(`ON CONFLICT(upstream, flavor, category, field, before_value, after_value) DO UPDATE SET
				last_seen=excluded.last_seen, seen_count=baseline_diffs.seen_count+1, reason=excluded.reason`).
			ToSql()
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("capture: build baseline diff upsert: %w", err)
		}
		if _, err := tx.ExecContext(ctx, diffQuery, diffArgs...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("capture: upsert baseline diff: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("capture: commit baseline: %w", err)
	}
	return nil
}

// ShapesForUpstream returns the deduped native-request shapes for an upstream
// last seen at or after since, each carrying its seen_count weight. A zero
// `since` returns all retained shapes.
func (s *Store) ShapesForUpstream(ctx context.Context, upstream string, since time.Time) ([]DriftShape, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	query, args, err := sq.Select(driftShapeColumns...).
		From("drift_shapes").
		Where("upstream = ?", upstream).
		Where("last_seen >= ?", since.UnixNano()).
		OrderBy("last_seen ASC").
		ToSql()
	if err != nil {
		return nil, fmt.Errorf("capture: build drift shapes query: %w", err)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.query_drift_shapes_failed", "upstream", upstream, "err", err)
		return nil, fmt.Errorf("capture: query drift shapes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DriftShape
	for rows.Next() {
		var (
			shape    DriftShape
			lastSeen int64
			headers  string
			body     string
			features string
		)
		if err := rows.Scan(
			&shape.Provider, &shape.Upstream, &shape.Fingerprint, &lastSeen, &shape.SeenCount,
			&shape.Method, &shape.Path, &shape.URL, &headers, &body, &shape.BillingAttestation, &features,
		); err != nil {
			return nil, fmt.Errorf("capture: scan drift shape: %w", err)
		}
		shape.Timestamp = time.Unix(0, lastSeen)
		shape.RequestHeaders = decodeStringMap(headers)
		shape.RequestBody = rawOrNil(body)
		shape.RequestFeatures = rawOrNil(features)
		out = append(out, shape)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("capture: iterate drift shapes: %w", err)
	}
	return out, nil
}

// CurrentBaseline returns the authoritative current baseline snapshot JSON for
// the upstream and whether a row exists.
func (s *Store) CurrentBaseline(ctx context.Context, upstream string) (json.RawMessage, bool, error) {
	if s == nil || s.rdb == nil {
		return nil, false, nil
	}
	query, args, err := sq.Select("snapshot").From("baselines").Where("upstream = ?", upstream).ToSql()
	if err != nil {
		return nil, false, fmt.Errorf("capture: build baseline query: %w", err)
	}
	var snapshot string
	switch err := s.rdb.QueryRowContext(ctx, query, args...).Scan(&snapshot); {
	case err == sql.ErrNoRows:
		return nil, false, nil
	case err != nil:
		s.log.WarnContext(ctx, "mitm.capture.query_baseline_failed", "upstream", upstream, "err", err)
		return nil, false, fmt.Errorf("capture: query baseline: %w", err)
	}
	return rawOrNil(snapshot), true, nil
}

// CurrentBaselineUpdatedAt returns the upstream baseline's updated_at (unix
// nanos), or 0 when no baseline row exists. It is the cheap cache token the
// adapter loaders poll per request in place of a file mtime.
func (s *Store) CurrentBaselineUpdatedAt(ctx context.Context, upstream string) (int64, error) {
	if s == nil || s.rdb == nil {
		return 0, nil
	}
	query, args, err := sq.Select("updated_at").From("baselines").Where("upstream = ?", upstream).ToSql()
	if err != nil {
		return 0, fmt.Errorf("capture: build baseline updated_at query: %w", err)
	}
	var updatedAt int64
	switch err := s.rdb.QueryRowContext(ctx, query, args...).Scan(&updatedAt); {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		s.log.WarnContext(ctx, "mitm.capture.query_baseline_updated_at_failed", "upstream", upstream, "err", err)
		return 0, fmt.Errorf("capture: query baseline updated_at: %w", err)
	}
	return updatedAt, nil
}

// BaselineDiffs returns the deduped difference matrix for an upstream, newest
// last-seen first.
func (s *Store) BaselineDiffs(ctx context.Context, upstream string) ([]BaselineDiff, error) {
	if s == nil || s.rdb == nil {
		return nil, nil
	}
	query, args, err := sq.Select(baselineDiffColumns...).
		From("baseline_diffs").
		Where("upstream = ?", upstream).
		OrderBy("last_seen DESC").
		ToSql()
	if err != nil {
		return nil, fmt.Errorf("capture: build baseline diffs query: %w", err)
	}
	rows, err := s.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		s.log.WarnContext(ctx, "mitm.capture.query_baseline_diffs_failed", "upstream", upstream, "err", err)
		return nil, fmt.Errorf("capture: query baseline diffs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BaselineDiff
	for rows.Next() {
		var (
			d         BaselineDiff
			firstSeen int64
			lastSeen  int64
		)
		if err := rows.Scan(
			&d.Upstream, &d.Flavor, &d.Category, &d.Field, &d.Before, &d.After, &d.Reason,
			&firstSeen, &lastSeen, &d.SeenCount,
		); err != nil {
			return nil, fmt.Errorf("capture: scan baseline diff: %w", err)
		}
		d.FirstSeen = time.Unix(0, firstSeen)
		d.LastSeen = time.Unix(0, lastSeen)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("capture: iterate baseline diffs: %w", err)
	}
	return out, nil
}

func encodeStringMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeStringMap(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func jsonOrEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw)
}

func rawOrNil(s string) json.RawMessage {
	if s == "" {
		return nil
	}
	return json.RawMessage(s)
}
