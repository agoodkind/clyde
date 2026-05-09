package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/session"
)

// newTestServerWithLogger builds a Server backed by a JSON slog handler
// writing into the provided buffer. The buffer's contents are decoded
// as one JSON object per line so tests can assert on attribute keys.
func newTestServerWithLogger(t *testing.T, buf *bytes.Buffer) *Server {
	t.Helper()
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	srv, err := New(slog.New(handler))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// createTestSession persists a session with the given name and provider
// session id so RenameSession can locate it through the global file store.
func createTestSession(t *testing.T, name, providerSessionID string) *session.Session {
	t.Helper()
	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess := session.NewSession(name, providerSessionID)
	setTestProviderIdentity(sess, session.ProviderClaude, providerSessionID)
	if err := store.Create(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func TestRenameSessionRejectsLiveLeasedSession(t *testing.T) {
	tmp := setupDaemonTestHome(t)
	_ = tmp
	srv := newTestServer(t)
	createTestSession(t, "live-chat", "live-session")

	// Insert a fake live-runtime record keyed on the provider session id.
	srv.liveSessions["live-session"] = &liveRuntimeSession{
		provider:  session.ProviderClaude,
		name:      "live-chat",
		id:        "live-session",
		basedir:   filepath.Join(tmp, "work"),
		status:    "running",
		startedAt: daemonNow(),
	}

	resp, err := srv.RenameSession(context.Background(), &clydev1.RenameSessionRequest{
		OldName: "live-chat",
		NewName: "renamed-chat",
	})
	if err == nil {
		t.Fatalf("rename returned no error, want FailedPrecondition (resp=%v)", resp)
	}
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("rename status code = %v, want %v", got, codes.FailedPrecondition)
	}
	if strings.Contains(err.Error(), "live-session") {
		t.Fatalf("error message leaks raw session id: %q", err.Error())
	}

	store, storeErr := session.NewGlobalFileStore()
	if storeErr != nil {
		t.Fatalf("reopen store: %v", storeErr)
	}
	if !store.Exists("live-chat") {
		t.Fatalf("original session directory was renamed despite live lease")
	}
	if store.Exists("renamed-chat") {
		t.Fatalf("renamed session directory exists despite live lease")
	}
}

func TestRenameSessionRecordsUserAttributionAndCooldown(t *testing.T) {
	_ = setupDaemonTestHome(t)
	srv := newTestServer(t)
	createTestSession(t, "default-001", "session-001")

	before := daemonNow()
	if _, err := srv.RenameSession(context.Background(), &clydev1.RenameSessionRequest{
		OldName: "default-001",
		NewName: "team-discussion",
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	store, err := session.NewGlobalFileStore()
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	renamed, err := store.Get("team-discussion")
	if err != nil {
		t.Fatalf("load renamed session: %v", err)
	}
	if renamed == nil {
		t.Fatalf("renamed session not found")
	}
	if renamed.Metadata.AutoNameSource != session.AutoNameSourceUser {
		t.Fatalf("AutoNameSource = %q, want %q",
			renamed.Metadata.AutoNameSource, session.AutoNameSourceUser)
	}
	if renamed.Metadata.AutoNameState != session.AutoNameStateUserLocked {
		t.Fatalf("AutoNameState = %q, want %q",
			renamed.Metadata.AutoNameState, session.AutoNameStateUserLocked)
	}
	if renamed.Metadata.AutoNameSourceHash != "" {
		t.Fatalf("AutoNameSourceHash = %q, want empty", renamed.Metadata.AutoNameSourceHash)
	}
	if renamed.Metadata.LastAutoNameAt.IsZero() {
		t.Fatalf("LastAutoNameAt is zero, want a timestamp >= test start")
	}
	if renamed.Metadata.LastAutoNameAt.Before(before.Add(-time.Second)) {
		t.Fatalf("LastAutoNameAt = %v, want >= %v", renamed.Metadata.LastAutoNameAt, before)
	}
}

// renameAppliedRecord captures the typed shape of the
// daemon.rename.applied slog event for assertion.
type renameAppliedRecord struct {
	Msg          string `json:"msg"`
	Component    string `json:"component"`
	Subcomponent string `json:"subcomponent"`
	PriorName    string `json:"prior_name"`
	NewName      string `json:"new_name"`
	Source       string `json:"source"`
	SessionID    string `json:"session_id"`
	TraceID      string `json:"trace_id"`
	DurationMs   int64  `json:"duration_ms"`
}

// hasDurationField reports whether a JSON line carries the duration_ms
// key. The decoder treats a missing key as zero, which is a valid
// duration value, so the caller asserts presence by re-scanning.
func hasDurationField(line string) bool {
	return strings.Contains(line, "\"duration_ms\"")
}

func TestRenameSessionEmitsAppliedSlogEvent(t *testing.T) {
	_ = setupDaemonTestHome(t)
	var buf bytes.Buffer
	srv := newTestServerWithLogger(t, &buf)
	createTestSession(t, "default-007", "session-007")

	if _, err := srv.RenameSession(context.Background(), &clydev1.RenameSessionRequest{
		OldName: "default-007",
		NewName: "infra-runbook",
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	line, applied := findRenameAppliedRecord(t, &buf)
	if applied.Component != "daemon" {
		t.Fatalf("component = %q, want daemon", applied.Component)
	}
	if applied.Subcomponent != "session" {
		t.Fatalf("subcomponent = %q, want session", applied.Subcomponent)
	}
	if applied.PriorName != "default-007" {
		t.Fatalf("prior_name = %q, want default-007", applied.PriorName)
	}
	if applied.NewName != "infra-runbook" {
		t.Fatalf("new_name = %q, want infra-runbook", applied.NewName)
	}
	if applied.Source != string(session.AutoNameSourceUser) {
		t.Fatalf("source = %q, want %q", applied.Source, session.AutoNameSourceUser)
	}
	if applied.SessionID != "session-007" {
		t.Fatalf("session_id = %q, want session-007", applied.SessionID)
	}
	// trace_id is empty by default in unit tests because the request
	// context carries no correlation. The field must still be emitted.
	if !strings.Contains(line, "\"trace_id\"") {
		t.Fatalf("slog line missing trace_id key: %s", line)
	}
	if !hasDurationField(line) {
		t.Fatalf("slog line missing duration_ms key: %s", line)
	}
}

// findRenameAppliedRecord scans the buffer for the daemon.rename.applied
// JSON line and returns the raw line plus a decoded typed record.
func findRenameAppliedRecord(t *testing.T, buf *bytes.Buffer) (string, renameAppliedRecord) {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec renameAppliedRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Msg == "daemon.rename.applied" {
			return line, rec
		}
	}
	t.Fatalf("slog record daemon.rename.applied not found in buffer:\n%s", buf.String())
	return "", renameAppliedRecord{}
}
