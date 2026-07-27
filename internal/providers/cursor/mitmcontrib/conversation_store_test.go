package mitmcontrib

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/mitm/capture"
)

// These tests drive the real capture store with the real Cursor resolver over
// real protobuf bodies, so they fail if either the extraction or the stored
// join breaks. Only identifiers cross the boundary; no fixture carries prompt
// or response content.

func openConversationStore(t *testing.T) (*capture.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "capture.db")
	store, err := capture.Open(context.Background(), capture.Config{DBPath: dbPath}, nil)
	if err != nil {
		t.Fatalf("capture.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background(), "test cleanup") })
	return store, dbPath
}

// recordCursorRequest tags a request the way the MITM proxy does, by asking the
// registered provider's resolver, then stores it.
func recordCursorRequest(store *capture.Store, request RequestCapture, at time.Time, requestID, traceID string) capture.ConversationRef {
	ref, supported := routeProvider{}.ResolveConversation(capture.RequestConversationInput{
		Path:        request.Path,
		ContentType: request.Headers.Get("Content-Type"),
		Headers:     request.Headers,
		Body:        request.Body,
	})
	if !supported {
		ref = capture.UnresolvedConversation()
	}
	store.Record(capture.Record{
		Timestamp:      at,
		Client:         "app.cursor",
		Provider:       "cursor",
		Concern:        "cursor.agent",
		Host:           "agentn.global.api5.cursor.sh",
		Method:         http.MethodPost,
		Path:           request.Path,
		Status:         http.StatusOK,
		RequestID:      requestID,
		TraceID:        traceID,
		RequestHeaders: request.Headers,
		RequestBody:    request.Body,
		RequestType:    request.Headers.Get("Content-Type"),
		Conversation:   ref,
	})
	return ref
}

func waitForCapturedRows(t *testing.T, dbPath string, want int) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open wait verifier: %v", err)
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if scanErr := db.QueryRow(`SELECT count(*) FROM requests`).Scan(&count); scanErr == nil && count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d captured rows", want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCapturedCursorRequestAndItsChatResolveEachOther(t *testing.T) {
	store, dbPath := openConversationStore(t)
	base := time.Unix(1785082765, 0)

	// Two turns of one chat, the second a subagent run, plus a metadata write
	// for a different chat that has no local transcript, plus one unrelated
	// non-chat request on the same host.
	recordCursorRequest(store, runRequest(t, testConversationUUID, ""), base, "req-turn-1", "trace-turn-1")
	recordCursorRequest(store, runRequest(t, testSubagentUUID, testConversationUUID), base.Add(time.Second), "req-turn-2", "trace-turn-2")
	recordCursorRequest(store, metadataRequest(testUnknownChatUUID), base.Add(2*time.Second), "req-other-chat", "trace-other-chat")
	telemetry := metadataRequest(testConversationUUID)
	telemetry.Path = "/aiserver.v1.AnalyticsService/Batch"
	recordCursorRequest(store, telemetry, base.Add(3*time.Second), "req-telemetry", "trace-telemetry")
	waitForCapturedRows(t, dbPath, 4)

	ctx := context.Background()
	wantConversation := "cursor:" + testConversationUUID

	// Request to chat, by request id and by trace id.
	byRequestID, err := store.ConversationForRequest(ctx, capture.RequestLookup{RequestID: "req-turn-2"}, nil)
	if err != nil {
		t.Fatalf("ConversationForRequest by request id: %v", err)
	}
	if byRequestID.ConversationID != wantConversation {
		t.Fatalf("by request id = %q, want %q", byRequestID.ConversationID, wantConversation)
	}
	if byRequestID.Source != capture.ConversationSourceCursorRunParent {
		t.Fatalf("by request id source = %q, want %q", byRequestID.Source, capture.ConversationSourceCursorRunParent)
	}
	byTraceID, err := store.ConversationForRequest(ctx, capture.RequestLookup{TraceID: "trace-turn-1"}, nil)
	if err != nil {
		t.Fatalf("ConversationForRequest by trace id: %v", err)
	}
	if byTraceID.ConversationID != wantConversation {
		t.Fatalf("by trace id = %q, want %q", byTraceID.ConversationID, wantConversation)
	}

	// Chat to requests, in the order they happened.
	captured, err := store.CapturedRequestsForConversation(ctx, wantConversation)
	if err != nil {
		t.Fatalf("CapturedRequestsForConversation: %v", err)
	}
	gotRequestIDs := make([]string, 0, len(captured))
	for _, record := range captured {
		gotRequestIDs = append(gotRequestIDs, record.RequestID)
	}
	if len(gotRequestIDs) != 2 || gotRequestIDs[0] != "req-turn-1" || gotRequestIDs[1] != "req-turn-2" {
		t.Fatalf("captured request ids = %v, want [req-turn-1 req-turn-2]", gotRequestIDs)
	}
	if !captured[0].Timestamp.Before(captured[1].Timestamp) {
		t.Fatalf("captured requests are not in chronological order: %v then %v", captured[0].Timestamp, captured[1].Timestamp)
	}
	if captured[0].ConversationSource != capture.ConversationSourceCursorRunThread {
		t.Fatalf("first turn source = %q, want %q", captured[0].ConversationSource, capture.ConversationSourceCursorRunThread)
	}

	// The other chat keeps its own requests, and the telemetry row belongs to
	// no chat at all.
	otherChat, err := store.CapturedRequestsForConversation(ctx, "cursor:"+testUnknownChatUUID)
	if err != nil {
		t.Fatalf("CapturedRequestsForConversation for the other chat: %v", err)
	}
	if len(otherChat) != 1 || otherChat[0].RequestID != "req-other-chat" {
		t.Fatalf("other chat captured %d requests, want only req-other-chat", len(otherChat))
	}
	telemetryRef, err := store.ConversationForRequest(ctx, capture.RequestLookup{RequestID: "req-telemetry"}, nil)
	if err != nil {
		t.Fatalf("ConversationForRequest for telemetry: %v", err)
	}
	if telemetryRef.Resolved() {
		t.Fatalf("telemetry row resolved to %q, want no conversation", telemetryRef.ConversationID)
	}
}

// A row stored without the join, which is every row captured before the column
// existed, still resolves from its own stored body. The store is never
// rewritten to make that work.
func TestRowStoredWithoutTheJoinResolvesFromItsStoredBody(t *testing.T) {
	store, dbPath := openConversationStore(t)
	request := runRequest(t, testSubagentUUID, testConversationUUID)

	store.Record(capture.Record{
		Timestamp:      time.Unix(1785082765, 0),
		Client:         "app.cursor",
		Provider:       "cursor",
		Host:           "agentn.global.api5.cursor.sh",
		Method:         http.MethodPost,
		Path:           request.Path,
		Status:         http.StatusOK,
		RequestID:      "req-untagged",
		RequestHeaders: request.Headers,
		RequestBody:    request.Body,
		RequestType:    request.Headers.Get("Content-Type"),
		Conversation:   capture.UnresolvedConversation(),
	})
	waitForCapturedRows(t, dbPath, 1)

	ctx := context.Background()
	lookup := capture.RequestLookup{RequestID: "req-untagged"}

	columnOnly, err := store.ConversationForRequest(ctx, lookup, nil)
	if err != nil {
		t.Fatalf("column-only lookup: %v", err)
	}
	if columnOnly.Resolved() {
		t.Fatalf("column-only lookup resolved to %q, want no conversation", columnOnly.ConversationID)
	}

	fromBody, err := store.ConversationForRequest(ctx, lookup, ConversationResolver{})
	if err != nil {
		t.Fatalf("body-fallback lookup: %v", err)
	}
	if fromBody.ConversationID != "cursor:"+testConversationUUID {
		t.Fatalf("body-fallback lookup = %q, want %q", fromBody.ConversationID, "cursor:"+testConversationUUID)
	}
	if fromBody.Source != capture.ConversationSourceCursorRunParent {
		t.Fatalf("body-fallback source = %q, want %q", fromBody.Source, capture.ConversationSourceCursorRunParent)
	}

	// The fallback read must not have written the join back onto the row.
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open verifier: %v", err)
	}
	defer func() { _ = db.Close() }()
	var storedConversationID string
	if scanErr := db.QueryRow(`SELECT conversation_id FROM requests WHERE request_id='req-untagged'`).Scan(&storedConversationID); scanErr != nil {
		t.Fatalf("read stored conversation id: %v", scanErr)
	}
	if storedConversationID != "" {
		t.Fatalf("stored conversation_id = %q, want the row left untouched", storedConversationID)
	}
}

func TestLookupWithoutAnyCorrelationIDIsRefused(t *testing.T) {
	store, _ := openConversationStore(t)

	_, err := store.ConversationForRequest(context.Background(), capture.RequestLookup{}, ConversationResolver{})

	if !errors.Is(err, capture.ErrEmptyRequestLookup) {
		t.Fatalf("err = %v, want ErrEmptyRequestLookup", err)
	}
}

func TestUnknownRequestResolvesToNoConversation(t *testing.T) {
	store, _ := openConversationStore(t)

	ref, err := store.ConversationForRequest(context.Background(),
		capture.RequestLookup{RequestID: "never-captured"}, ConversationResolver{})
	if err != nil {
		t.Fatalf("ConversationForRequest: %v", err)
	}
	if ref.Resolved() {
		t.Fatalf("resolved to %q, want no conversation", ref.ConversationID)
	}
}
