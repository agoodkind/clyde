package capture_test

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

//go:embed testdata/recording_counts.sql
var recordingCountsSQL string

func TestDisabledRecordingPreservesBaselineLearning(t *testing.T) {
	for _, test := range []struct {
		name     string
		disabled bool
		until    time.Time
	}{
		{name: "disabled", disabled: true},
		{name: "expired", until: time.Unix(1, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "capture.db")
			store := openRecordingStore(t, capture.Config{DBPath: path, RecordingDisabled: test.disabled, RecordingUntil: test.until})
			if store.RecordingEnabled() {
				t.Fatal("disabled exchange recording accepted captures")
			}
			recordDiagnosticExchange(store, "disabled")
			store.RecordExchange(correlation.Context{}, capture.Exchange{RequestBody: []byte("disabled request")})
			store.RecordCappedExchange(correlation.Context{}, capture.Exchange{ResponseBody: []byte("disabled response")}, 17, true)
			store.RecordShape(capture.DriftShape{
				Timestamp: time.Now(), Upstream: "test", Fingerprint: "shape",
				RequestBody: json.RawMessage(`{"body_type":"object"}`),
			})
			store.RecordCheck(capture.DriftCheck{Timestamp: time.Now(), Upstream: "test"})
			snapshot := json.RawMessage(`{"upstream":"test"}`)
			if err := store.PutBaseline(context.Background(), capture.BaselineChange{Timestamp: time.Now(), Upstream: "test", Snapshot: snapshot}); err != nil {
				t.Fatal(err)
			}
			baseline, found, err := store.CurrentBaseline(context.Background(), "test")
			if err != nil || !found || string(baseline) != string(snapshot) {
				t.Fatalf("baseline = %s, found=%t, error=%v", baseline, found, err)
			}
			if err := store.Close(context.Background(), "test"); err != nil {
				t.Fatal(err)
			}
			assertRecordingCounts(t, path, 0, 0, 0, 0, 1, 1)
		})
	}
}

func TestRecordingExpiresWithoutReloadAndStaysExpiredAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.db")
	deadline := time.Now().Add(2 * time.Second)
	configuration := capture.Config{DBPath: path, RecordingUntil: deadline}
	store := openRecordingStore(t, configuration)
	if !store.RecordingEnabled() {
		t.Fatal("future recording deadline did not enable recording")
	}
	recordDiagnosticExchange(store, "before")
	timer := time.NewTimer(time.Until(deadline) + time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if store.RecordingEnabled() {
		t.Fatal("recording continued after its deadline")
	}
	recordDiagnosticExchange(store, "after")
	if err := store.Close(context.Background(), "test expiration"); err != nil {
		t.Fatal(err)
	}
	assertRecordingCounts(t, path, 1, 2, 1, 1, 0, 0)
	reopened := openRecordingStore(t, configuration)
	recordDiagnosticExchange(reopened, "reopened")
	if err := reopened.Close(context.Background(), "test reopen"); err != nil {
		t.Fatal(err)
	}
	assertRecordingCounts(t, path, 1, 2, 1, 1, 0, 0)
}

func recordDiagnosticExchange(store *capture.Store, requestID string) {
	store.Record(capture.Record{
		Timestamp: time.Now(), RequestID: requestID,
		RequestBody: []byte("request"), ResponseBody: []byte("response"),
		DecodedRequest: &capture.DecodedBody{
			Format: "test", Status: capture.DecodeStatusSuccess,
			RepresentationJSON: []byte(`{"request":"decoded"}`),
			ToolEvents:         []capture.ToolEvent{{CallID: "call", ToolName: "tool", InputRepresentation: "arguments"}},
		},
	})
}

func openRecordingStore(t *testing.T, configuration capture.Config) *capture.Store {
	t.Helper()
	store, err := capture.Open(context.Background(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	return store
}

func assertRecordingCounts(t *testing.T, path string, expected ...int) {
	t.Helper()
	database, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var requests, bodies, decodedBodies, toolEvents, shapes, checks int
	if err := database.QueryRow(recordingCountsSQL).Scan(&requests, &bodies, &decodedBodies, &toolEvents, &shapes, &checks); err != nil {
		t.Fatal(err)
	}
	for index, count := range []int{requests, bodies, decodedBodies, toolEvents, shapes, checks} {
		if count != expected[index] {
			t.Fatalf("table %d count = %d, want %d", index, count, expected[index])
		}
	}
}
