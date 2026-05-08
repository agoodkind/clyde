package sessionrename

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

// recordingRenamer captures Apply calls for assertion.
type recordingRenamer struct {
	mu    sync.Mutex
	calls []recordedRename
	err   error
}

type recordedRename struct {
	OldName string
	NewName string
	Source  session.AutoNameSource
	Hash    string
}

func (r *recordingRenamer) Apply(_ context.Context, oldName, newName string, source session.AutoNameSource, hash string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, recordedRename{OldName: oldName, NewName: newName, Source: source, Hash: hash})
	return nil
}

func (r *recordingRenamer) Calls() []recordedRename {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedRename, len(r.calls))
	copy(out, r.calls)
	return out
}

// fakeStore captures UpdateMetadata calls.
type fakeStore struct {
	mu       sync.Mutex
	updates  map[string]session.Metadata
	failWith error
}

func newFakeStore() *fakeStore {
	return &fakeStore{updates: map[string]session.Metadata{}}
}

func (s *fakeStore) UpdateMetadata(name string, mutate func(*session.Metadata)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil {
		return s.failWith
	}
	current := s.updates[name]
	mutate(&current)
	s.updates[name] = current
	return nil
}

func (s *fakeStore) Get(name string) session.Metadata {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates[name]
}

// writeTranscript writes a JSONL transcript with the given user
// messages and returns the file path. The schema mirrors the subset
// ExtractFromTranscript reads.
func writeTranscript(t *testing.T, dir string, userMessages []string) string {
	t.Helper()
	path := filepath.Join(dir, "transcript.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	defer func() { _ = f.Close() }()
	for _, m := range userMessages {
		if _, err := f.WriteString(`{"type":"user","timestamp":"2026-05-08T12:00:00Z","content":"` + m + `"}` + "\n"); err != nil {
			t.Fatalf("write transcript: %v", err)
		}
	}
	return path
}

func newTestWorker(cfg Config, rename Renamer, leases LeaseChecker, store MetadataStore, now time.Time) *Worker {
	return NewWorker(cfg, rename, leases, store, func() time.Time { return now }, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestEvaluate_AppliesRenameOnHappyPath(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{
		"Investigate cloudflare bot fight mode triggering 1010 errors",
		"and also check waf rules",
		"plus the rate limits",
	})
	sess := &session.Session{
		Name:     "chat-20260508-044508",
		Metadata: session.Metadata{Name: "chat-20260508-044508"},
	}
	renamer := &recordingRenamer{}
	store := newFakeStore()
	worker := newTestWorker(Config{Enabled: true, MinUserMessages: 3, Cooldown: 30 * time.Minute, MaxCallsPerHour: 6}, renamer, LeaseCheckerFunc(func(string) bool { return false }), store, time.Now())

	res, err := worker.Evaluate(context.Background(), sess, transcript, map[string]bool{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !res.Applied {
		t.Fatalf("Applied=false, want true (reason=%q)", res.Reason)
	}
	if res.Hash == "" {
		t.Fatalf("Hash empty, want fingerprint of redacted source")
	}
	calls := renamer.Calls()
	if len(calls) != 1 {
		t.Fatalf("len(calls)=%d want 1", len(calls))
	}
	if calls[0].Source != session.AutoNameSourceTranscript {
		t.Fatalf("Source=%q want transcript", calls[0].Source)
	}
	updated := store.Get(res.NewName)
	if updated.AutoNameState != session.AutoNameStateApplied {
		t.Fatalf("State=%q want applied", updated.AutoNameState)
	}
	if updated.AutoNameSource != session.AutoNameSourceTranscript {
		t.Fatalf("Source=%q want transcript", updated.AutoNameSource)
	}
	if updated.AutoNameSourceHash != res.Hash {
		t.Fatalf("Hash mismatch: store=%q result=%q", updated.AutoNameSourceHash, res.Hash)
	}
	if updated.LastAutoNameAt.IsZero() {
		t.Fatalf("LastAutoNameAt not stamped")
	}
}

func TestEvaluate_DefersWhenLeaseHeld(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{"first", "second", "third"})
	sess := &session.Session{Name: "demo", Metadata: session.Metadata{Name: "demo"}}
	renamer := &recordingRenamer{}
	worker := newTestWorker(Config{Enabled: true, MinUserMessages: 3}, renamer, LeaseCheckerFunc(func(string) bool { return true }), newFakeStore(), time.Now())

	res, err := worker.Evaluate(context.Background(), sess, transcript, map[string]bool{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Applied {
		t.Fatalf("Applied=true, want false on live lease")
	}
	if res.Reason != "live_lease_held" {
		t.Fatalf("Reason=%q want live_lease_held", res.Reason)
	}
	if len(renamer.Calls()) != 0 {
		t.Fatalf("renamer called despite live lease")
	}
}

func TestEvaluate_SkipsWhenSourceUnchanged(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{"investigate redis lag", "looks slow", "across primary"})
	sess := &session.Session{
		Name:     "demo",
		Metadata: session.Metadata{Name: "demo"},
	}
	src, err := ExtractFromTranscript(transcript)
	if err != nil {
		t.Fatalf("ExtractFromTranscript: %v", err)
	}
	sess.Metadata.AutoNameSourceHash = src.Hash

	renamer := &recordingRenamer{}
	worker := newTestWorker(Config{Enabled: true, MinUserMessages: 3}, renamer, LeaseCheckerFunc(func(string) bool { return false }), newFakeStore(), time.Now())

	res, err := worker.Evaluate(context.Background(), sess, transcript, map[string]bool{})
	if !errors.Is(err, errSourceUnchanged) {
		t.Fatalf("err=%v want errSourceUnchanged", err)
	}
	if res.Applied {
		t.Fatalf("Applied=true, want false on unchanged source")
	}
	if res.Reason != "source_unchanged" {
		t.Fatalf("Reason=%q want source_unchanged", res.Reason)
	}
	if len(renamer.Calls()) != 0 {
		t.Fatalf("renamer called despite unchanged source")
	}
}

func TestEvaluate_RateLimitBlocksAfterBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	worker := newTestWorker(Config{Enabled: true, MinUserMessages: 3, MaxCallsPerHour: 2}, &recordingRenamer{}, LeaseCheckerFunc(func(string) bool { return false }), newFakeStore(), now)
	for index := 0; index < 2; index++ {
		if !worker.consumeRateLimit(now) {
			t.Fatalf("consumeRateLimit returned false within budget at index=%d", index)
		}
	}
	if worker.consumeRateLimit(now) {
		t.Fatalf("consumeRateLimit returned true after budget exhausted")
	}

	// transcript is unused for this isolated rate-limit assertion;
	// keep the helper warm so future evolutions of Evaluate that
	// rely on it stay compatible.
	_ = writeTranscript(t, dir, nil)
}

func TestEvaluate_SkipsWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	transcript := writeTranscript(t, dir, []string{"a", "b", "c"})
	sess := &session.Session{Name: "demo", Metadata: session.Metadata{Name: "demo"}}
	worker := newTestWorker(Config{Enabled: false}, &recordingRenamer{}, nil, nil, time.Now())
	res, err := worker.Evaluate(context.Background(), sess, transcript, map[string]bool{})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if res.Applied || res.Reason != "disabled" {
		t.Fatalf("got Applied=%v Reason=%q want false/disabled", res.Applied, res.Reason)
	}
}
