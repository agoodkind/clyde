package codex

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRunner struct {
	calls atomic.Int32
	out   map[string][]byte
	err   map[string]error
}

func (f *fakeRunner) run(_ context.Context, _ string, args ...string) ([]byte, error) {
	f.calls.Add(1)
	key := args[0]
	if e, ok := f.err[key]; ok {
		return nil, e
	}
	if v, ok := f.out[key]; ok {
		return v, nil
	}
	return nil, errors.New("no fixture for " + key)
}

func TestWorkspaceProbeReturnsAllFields(t *testing.T) {
	r := &fakeRunner{
		out: map[string][]byte{
			"config":    []byte("git@github.com:agoodkind/clyde.git\n"),
			"rev-parse": []byte("95d28dd0fdef4d87a64b29283e39605ce759c4cc\n"),
			"status":    []byte(" M Makefile\n"),
		},
	}
	now := time.Unix(0, 0)
	p := newWorkspaceProbeWith(r, time.Minute, func() time.Time { return now })
	got := p.Probe(context.Background(), "/some/path")
	if got.AssociatedRemoteURLs.Origin != "git@github.com:agoodkind/clyde.git" {
		t.Errorf("origin: %q", got.AssociatedRemoteURLs.Origin)
	}
	if got.LatestGitCommitHash != "95d28dd0fdef4d87a64b29283e39605ce759c4cc" {
		t.Errorf("commit: %q", got.LatestGitCommitHash)
	}
	if !got.HasChanges {
		t.Errorf("has_changes should be true")
	}
}

func TestWorkspaceProbeCachesWithinTTL(t *testing.T) {
	r := &fakeRunner{
		out: map[string][]byte{
			"config":    []byte("origin\n"),
			"rev-parse": []byte("abc\n"),
			"status":    []byte(""),
		},
	}
	now := time.Unix(100, 0)
	p := newWorkspaceProbeWith(r, time.Minute, func() time.Time { return now })
	p.Probe(context.Background(), "/a")
	p.Probe(context.Background(), "/a")
	if r.calls.Load() != 3 {
		t.Errorf("expected 3 git calls (one probe), got %d", r.calls.Load())
	}
}

func TestWorkspaceProbeRefreshesAfterTTL(t *testing.T) {
	r := &fakeRunner{
		out: map[string][]byte{
			"config":    []byte("origin\n"),
			"rev-parse": []byte("abc\n"),
			"status":    []byte(""),
		},
	}
	now := time.Unix(100, 0)
	p := newWorkspaceProbeWith(r, time.Second, func() time.Time { return now })
	p.Probe(context.Background(), "/a")
	now = now.Add(2 * time.Second)
	p.Probe(context.Background(), "/a")
	if r.calls.Load() != 6 {
		t.Errorf("expected 6 git calls (two probes), got %d", r.calls.Load())
	}
}

func TestWorkspaceProbeEmptyPathReturnsZero(t *testing.T) {
	p := NewWorkspaceProbe()
	got := p.Probe(context.Background(), "")
	if got.AssociatedRemoteURLs.Origin != "" || got.LatestGitCommitHash != "" || got.HasChanges {
		t.Errorf("expected zero, got %#v", got)
	}
}

func TestWorkspaceProbeIgnoresGitFailures(t *testing.T) {
	r := &fakeRunner{
		err: map[string]error{
			"config":    errors.New("not a repo"),
			"rev-parse": errors.New("not a repo"),
			"status":    errors.New("not a repo"),
		},
	}
	now := time.Unix(0, 0)
	p := newWorkspaceProbeWith(r, time.Minute, func() time.Time { return now })
	got := p.Probe(context.Background(), "/not-a-repo")
	if got.AssociatedRemoteURLs.Origin != "" {
		t.Errorf("origin should be empty on git failure")
	}
	if got.LatestGitCommitHash != "" {
		t.Errorf("commit should be empty on git failure")
	}
	if got.HasChanges {
		t.Errorf("has_changes should be false on git failure")
	}
}
