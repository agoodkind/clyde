package sessionrename

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/session"
)

type fakeRenamer struct {
	calls []renameCall
	err   error
}

type renameCall struct {
	old  string
	next string
	hash string
}

func (f *fakeRenamer) ApplyAutoRename(_ context.Context, oldName, newName, sourceHash string) error {
	f.calls = append(f.calls, renameCall{old: oldName, next: newName, hash: sourceHash})
	return f.err
}

type fakeLeases struct {
	held map[string]bool
}

func (f *fakeLeases) HasLiveLease(name string) bool { return f.held[name] }

type fakeStore struct {
	all []*session.Session
}

func (f *fakeStore) List() ([]*session.Session, error) {
	return f.all, nil
}

func writeTestTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func boolPtr(value bool) *bool {
	return &value
}

func newDefaultShapedSession(t *testing.T, provider session.ProviderID, name, transcriptPath string) *session.Session {
	t.Helper()
	sess := session.NewSession(name, "uuid")
	sess.Metadata.Provider = provider
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{
			Provider: provider,
			ID:       "uuid",
		},
	}
	sess.Metadata.SetProviderTranscriptPath(transcriptPath)
	return sess
}

func TestWorkerSkipsWhenDisabled(t *testing.T) {
	w := NewWorker(config.AutoNameConfig{Enabled: boolPtr(false)}, &fakeRenamer{}, &fakeLeases{}, &fakeStore{}, nil, nil)
	d := w.Evaluate(context.Background(), newDefaultShapedSession(t, session.ProviderClaude, "myrepo-12345678", ""))
	if d.Code != decisionDisabled {
		t.Fatalf("code=%q", d.Code)
	}
}

func TestWorkerSkipsUserLocked(t *testing.T) {
	w := NewWorker(config.AutoNameConfig{}, &fakeRenamer{}, &fakeLeases{}, &fakeStore{}, nil, nil)
	sess := newDefaultShapedSession(t, session.ProviderClaude, "anything", "")
	sess.Metadata.AutoNameState = session.AutoNameStateUserLocked
	sess.Metadata.AutoNameSource = session.AutoNameSourceUser
	d := w.Evaluate(context.Background(), sess)
	if d.Code != decisionUserRenamed {
		t.Fatalf("code=%q", d.Code)
	}
}

func TestWorkerSkipsNonDefaultName(t *testing.T) {
	w := NewWorker(config.AutoNameConfig{}, &fakeRenamer{}, &fakeLeases{}, &fakeStore{}, nil, nil)
	sess := newDefaultShapedSession(t, session.ProviderClaude, "user-typed-name", "")
	d := w.Evaluate(context.Background(), sess)
	if d.Code != decisionNotDefaultShape {
		t.Fatalf("code=%q", d.Code)
	}
}

func TestWorkerSkipsLiveLease(t *testing.T) {
	w := NewWorker(config.AutoNameConfig{}, &fakeRenamer{}, &fakeLeases{held: map[string]bool{"myrepo-12345678": true}}, &fakeStore{}, nil, nil)
	d := w.Evaluate(context.Background(), newDefaultShapedSession(t, session.ProviderClaude, "myrepo-12345678", ""))
	if d.Code != decisionLiveLeaseHeld {
		t.Fatalf("code=%q", d.Code)
	}
}

func TestWorkerAppliesFromCustomTitle(t *testing.T) {
	transcript := writeTestTranscript(t,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"custom-title","customTitle":"Auto rename rollout"}`,
	)
	sess := newDefaultShapedSession(t, session.ProviderClaude, "myrepo-12345678", transcript)
	rename := &fakeRenamer{}
	w := NewWorker(config.AutoNameConfig{MinUserMessages: 1}, rename, &fakeLeases{}, &fakeStore{}, nil, nil)
	d := w.Evaluate(context.Background(), sess)
	if d.Code != decisionApplied {
		t.Fatalf("code=%q reason=%q", d.Code, d.Reason)
	}
	if d.NewName != "auto-rename-rollout" {
		t.Fatalf("new=%q", d.NewName)
	}
	if len(rename.calls) != 1 {
		t.Fatalf("calls=%d", len(rename.calls))
	}
	if rename.calls[0].hash != d.SourceHash {
		t.Fatalf("hash mismatch")
	}
}

func TestWorkerAppliesFromCodexFirstUserMessage(t *testing.T) {
	transcript := writeTestTranscript(t,
		`{"timestamp":"2026-05-02T17:09:04.407Z","type":"session_meta","payload":{"id":"019de9aa-3a00-7010-bd9f-a6ee71559357","timestamp":"2026-05-02T17:09:00.555Z","cwd":"/repo","originator":"codex-tui","cli_version":"0.128.0","source":"cli","model_provider":"openai"}}`,
		`{"timestamp":"2026-05-02T17:09:05.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"please promote the codex thread title"}]}}`,
	)
	sess := newDefaultShapedSession(t, session.ProviderCodex, "myrepo-12345678", transcript)
	rename := &fakeRenamer{}
	w := NewWorker(config.AutoNameConfig{MinUserMessages: 1}, rename, &fakeLeases{}, &fakeStore{}, nil, nil)
	d := w.Evaluate(context.Background(), sess)
	if d.Code != decisionApplied {
		t.Fatalf("code=%q reason=%q", d.Code, d.Reason)
	}
	if d.NewName != "promote-codex-thread-title" {
		t.Fatalf("new=%q", d.NewName)
	}
}

func TestWorkerSkipsWhenSourceHashUnchanged(t *testing.T) {
	transcript := writeTestTranscript(t,
		`{"type":"custom-title","customTitle":"Auto rename rollout"}`,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
	)
	sess := newDefaultShapedSession(t, session.ProviderClaude, "myrepo-12345678", transcript)
	src, err := extractFromSession(sess)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	sess.Metadata.AutoNameSourceHash = src.Hash
	w := NewWorker(config.AutoNameConfig{MinUserMessages: 1}, &fakeRenamer{}, &fakeLeases{}, &fakeStore{}, nil, nil)
	d := w.Evaluate(context.Background(), sess)
	if d.Code != decisionSourceUnchanged {
		t.Fatalf("code=%q", d.Code)
	}
}

func TestWorkerRespectsRateLimit(t *testing.T) {
	transcript := writeTestTranscript(t,
		`{"type":"custom-title","customTitle":"Hello world"}`,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
	)
	now := time.Now()
	rename := &fakeRenamer{}
	w := NewWorker(
		config.AutoNameConfig{MinUserMessages: 1, MaxCallsPerHour: 1},
		rename,
		&fakeLeases{},
		&fakeStore{},
		nil,
		func() time.Time { return now },
	)
	first := w.Evaluate(context.Background(), newDefaultShapedSession(t, session.ProviderClaude, "first-12345678", transcript))
	if first.Code != decisionApplied {
		t.Fatalf("first code=%q", first.Code)
	}
	second := w.Evaluate(context.Background(), newDefaultShapedSession(t, session.ProviderClaude, "second-12345678", transcript))
	if second.Code != decisionRateLimited {
		t.Fatalf("second code=%q", second.Code)
	}
}

func TestWorkerSurfacesApplyError(t *testing.T) {
	transcript := writeTestTranscript(t,
		`{"type":"custom-title","customTitle":"My session"}`,
		`{"type":"user","timestamp":"2025-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
	)
	rename := &fakeRenamer{err: errors.New("boom")}
	w := NewWorker(config.AutoNameConfig{MinUserMessages: 1}, rename, &fakeLeases{}, &fakeStore{}, nil, nil)
	d := w.Evaluate(context.Background(), newDefaultShapedSession(t, session.ProviderClaude, "stuff-12345678", transcript))
	if d.Code != decisionApplyFailed {
		t.Fatalf("code=%q", d.Code)
	}
	if d.ApplyError == nil {
		t.Fatalf("expected ApplyError")
	}
}
