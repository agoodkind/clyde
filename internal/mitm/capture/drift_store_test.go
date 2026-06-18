package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"
)

// waitForShapeCount polls a fresh verifier connection until drift_shapes holds
// at least want rows for the upstream, so a test can assert against the
// async-committed corpus without closing the live store.
func waitForShapeCount(t *testing.T, store *Store, upstream string, want int) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+store.cfg.DBPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open shape verifier: %v", err)
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if scanErr := db.QueryRow(`SELECT count(*) FROM drift_shapes WHERE upstream=?`, upstream).Scan(&n); scanErr == nil && n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d drift_shapes rows for upstream %q", want, upstream)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sampleShape(upstream, fingerprint string, ts time.Time) DriftShape {
	return DriftShape{
		Timestamp:          ts,
		Provider:           "claude",
		Upstream:           upstream,
		Fingerprint:        fingerprint,
		Method:             "POST",
		Path:               "/v1/messages",
		URL:                "https://api.anthropic.com/v1/messages",
		RequestHeaders:     map[string]string{"user-agent": "claude-cli/2.1.0"},
		RequestBody:        json.RawMessage(`{"body_type":"json_object","keys":["messages","model"]}`),
		BillingAttestation: "cch-token",
		RequestFeatures:    json.RawMessage(`{"model_id":"claude-sonnet"}`),
	}
}

func TestRecordShapeDedupBumpsSeenCount(t *testing.T) {
	store := openTestStore(t, Config{})
	ts := time.Unix(1700000000, 0)
	store.RecordShape(sampleShape("claude-code", "fp-1", ts))
	store.RecordShape(sampleShape("claude-code", "fp-1", ts.Add(time.Minute)))
	store.RecordShape(sampleShape("claude-code", "fp-1", ts.Add(2*time.Minute)))
	waitForShapeCount(t, store, "claude-code", 1)

	db := openVerifier(t, store.cfg.DBPath)
	var rows, seen int
	if err := db.QueryRow(`SELECT count(*), max(seen_count) FROM drift_shapes WHERE upstream='claude-code' AND fingerprint='fp-1'`).Scan(&rows, &seen); err != nil {
		t.Fatalf("scan shape: %v", err)
	}
	if rows != 1 {
		t.Fatalf("drift_shapes rows=%d want 1 (dedup)", rows)
	}
	// Three identical fingerprints: first insert sets seen_count=1, two upserts
	// bump it to 3.
	if seen != 3 {
		t.Fatalf("seen_count=%d want 3", seen)
	}
}

func TestRecordShapeDistinctFingerprintAddsRow(t *testing.T) {
	store := openTestStore(t, Config{})
	ts := time.Unix(1700000000, 0)
	store.RecordShape(sampleShape("claude-code", "fp-1", ts))
	store.RecordShape(sampleShape("claude-code", "fp-2", ts))
	waitForShapeCount(t, store, "claude-code", 2)

	db := openVerifier(t, store.cfg.DBPath)
	var rows int
	if err := db.QueryRow(`SELECT count(*) FROM drift_shapes WHERE upstream='claude-code'`).Scan(&rows); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if rows != 2 {
		t.Fatalf("drift_shapes rows=%d want 2 (distinct fingerprints)", rows)
	}
}

func TestShapesForUpstreamReturnsSeenCount(t *testing.T) {
	store := openTestStore(t, Config{})
	ts := time.Unix(1700000000, 0)
	store.RecordShape(sampleShape("claude-code", "fp-1", ts))
	store.RecordShape(sampleShape("claude-code", "fp-1", ts.Add(time.Minute)))
	waitForShapeCount(t, store, "claude-code", 1)

	shapes, err := store.ShapesForUpstream(context.Background(), "claude-code", time.Unix(0, 0))
	if err != nil {
		t.Fatalf("ShapesForUpstream: %v", err)
	}
	if len(shapes) != 1 {
		t.Fatalf("shapes=%d want 1", len(shapes))
	}
	if shapes[0].SeenCount != 2 {
		t.Fatalf("SeenCount=%d want 2", shapes[0].SeenCount)
	}
	if shapes[0].BillingAttestation != "cch-token" {
		t.Fatalf("BillingAttestation=%q want cch-token", shapes[0].BillingAttestation)
	}
}

func TestPutBaselineUpsertsAndDedupsDiffs(t *testing.T) {
	store := openTestStore(t, Config{})
	ctx := context.Background()
	ts := time.Unix(1700000000, 0)

	change := BaselineChange{
		Timestamp: ts,
		Upstream:  "claude-code",
		Snapshot:  json.RawMessage(`{"upstream":{"name":"claude-code"},"flavors":[]}`),
		Diffs: []BaselineDiff{
			{Upstream: "claude-code", Flavor: "claude-cli", Category: "header_extra", Field: "x-new", Before: "", After: "", Reason: "extra", FirstSeen: ts, LastSeen: ts, SeenCount: 1},
		},
	}
	if err := store.PutBaseline(ctx, change); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}
	// Second baseline with the same diff bumps seen_count, not a new row; the
	// baselines row is upserted in place.
	change.Timestamp = ts.Add(time.Hour)
	change.Snapshot = json.RawMessage(`{"upstream":{"name":"claude-code","version":"v2"},"flavors":[]}`)
	change.Diffs[0].LastSeen = change.Timestamp
	if err := store.PutBaseline(ctx, change); err != nil {
		t.Fatalf("PutBaseline second: %v", err)
	}

	db := openVerifier(t, store.cfg.DBPath)
	var baselineRows int
	if err := db.QueryRow(`SELECT count(*) FROM baselines WHERE upstream='claude-code'`).Scan(&baselineRows); err != nil {
		t.Fatalf("count baselines: %v", err)
	}
	if baselineRows != 1 {
		t.Fatalf("baselines rows=%d want 1 (upsert in place)", baselineRows)
	}
	var diffRows, diffSeen int
	if err := db.QueryRow(`SELECT count(*), max(seen_count) FROM baseline_diffs WHERE upstream='claude-code'`).Scan(&diffRows, &diffSeen); err != nil {
		t.Fatalf("count diffs: %v", err)
	}
	if diffRows != 1 {
		t.Fatalf("baseline_diffs rows=%d want 1 (dedup)", diffRows)
	}
	if diffSeen != 2 {
		t.Fatalf("baseline_diffs seen_count=%d want 2", diffSeen)
	}
}

func TestCurrentBaselineAndUpdatedAt(t *testing.T) {
	store := openTestStore(t, Config{})
	ctx := context.Background()
	ts := time.Unix(1700000000, 0)

	if _, ok, err := store.CurrentBaseline(ctx, "claude-code"); err != nil || ok {
		t.Fatalf("CurrentBaseline empty: ok=%v err=%v want ok=false", ok, err)
	}
	if updatedAt, err := store.CurrentBaselineUpdatedAt(ctx, "claude-code"); err != nil || updatedAt != 0 {
		t.Fatalf("CurrentBaselineUpdatedAt empty=%d err=%v want 0", updatedAt, err)
	}

	snap := json.RawMessage(`{"upstream":{"name":"claude-code"},"flavors":[]}`)
	if err := store.PutBaseline(ctx, BaselineChange{Timestamp: ts, Upstream: "claude-code", Snapshot: snap, Diffs: nil}); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}
	raw, ok, err := store.CurrentBaseline(ctx, "claude-code")
	if err != nil || !ok {
		t.Fatalf("CurrentBaseline: ok=%v err=%v want ok=true", ok, err)
	}
	if string(raw) != string(snap) {
		t.Fatalf("CurrentBaseline=%q want %q", string(raw), string(snap))
	}
	updatedAt, err := store.CurrentBaselineUpdatedAt(ctx, "claude-code")
	if err != nil {
		t.Fatalf("CurrentBaselineUpdatedAt: %v", err)
	}
	if updatedAt != ts.UnixNano() {
		t.Fatalf("updated_at=%d want %d", updatedAt, ts.UnixNano())
	}
}

func TestRecordCheckInsertsRow(t *testing.T) {
	store := openTestStore(t, Config{})
	store.RecordCheck(DriftCheck{Timestamp: time.Unix(1700000000, 0), Upstream: "claude-code", Diverged: true, Summary: "drift"})

	db := openVerifier(t, store.cfg.DBPath)
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n, diverged int
		err := db.QueryRow(`SELECT count(*), coalesce(max(diverged),0) FROM drift_checks WHERE upstream='claude-code'`).Scan(&n, &diverged)
		if err == nil && n >= 1 {
			if diverged != 1 {
				t.Fatalf("drift_checks diverged=%d want 1", diverged)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for drift_checks row")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPruneNeverDeletesDriftTables(t *testing.T) {
	store := openTestStore(t, Config{RetentionMaxAge: time.Hour})
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)

	store.RecordShape(sampleShape("claude-code", "fp-old", old))
	store.RecordCheck(DriftCheck{Timestamp: old, Upstream: "claude-code", Diverged: false, Summary: "ok"})
	waitForShapeCount(t, store, "claude-code", 1)
	if err := store.PutBaseline(ctx, BaselineChange{
		Timestamp: old,
		Upstream:  "claude-code",
		Snapshot:  json.RawMessage(`{"upstream":{"name":"claude-code"},"flavors":[]}`),
		Diffs:     []BaselineDiff{{Upstream: "claude-code", Flavor: "f", Category: "header_extra", Field: "x", FirstSeen: old, LastSeen: old, SeenCount: 1}},
	}); err != nil {
		t.Fatalf("PutBaseline: %v", err)
	}

	store.prune(ctx)

	db := openVerifier(t, store.cfg.DBPath)
	for _, table := range []string{"drift_shapes", "baselines", "baseline_diffs"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n == 0 {
			t.Fatalf("prune deleted all rows from %s; drift tables must be retained forever", table)
		}
	}
}
