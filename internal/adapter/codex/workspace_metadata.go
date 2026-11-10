package codex

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WorkspaceProbe inspects a workspace path with three small git
// commands and returns the typed metadata Codex Desktop ships in
// `x-codex-turn-metadata.workspaces`. Results are cached for
// `cacheTTL` so a burst of Cursor turns on the same workspace does
// not fork-bomb git.
//
// The probe is best-effort. Any git failure leaves the field
// empty rather than failing the request. The most-common case is
// a workspace that is not a git repo at all; we return an entry
// with the path but no origin or commit.
type WorkspaceProbe struct {
	mu       sync.Mutex
	cache    map[string]workspaceCacheEntry
	cacheTTL time.Duration
	now      func() time.Time
	runner   workspaceCommandRunner
}

type workspaceCacheEntry struct {
	value   TurnMetadataWorkspace
	fetched time.Time
}

type workspaceCommandRunner interface {
	run(ctx context.Context, workdir string, args ...string) ([]byte, error)
}

// workspaceGitCommand enumerates the three git invocations the
// workspace probe issues. The case values are the canonical NUL-joined
// `args` slice each one produces.
type workspaceGitCommand string

const (
	workspaceGitConfigOriginURL workspaceGitCommand = "config\x00\x2d\x2dget\x00remote.origin.url"
	workspaceGitRevParseHEAD    workspaceGitCommand = "rev-parse\x00HEAD"
	workspaceGitStatusPorcelain workspaceGitCommand = "status\x00\x2d\x2dporcelain"
)

type workspaceGitRunner struct{}

func (workspaceGitRunner) run(ctx context.Context, workdir string, args ...string) ([]byte, error) {
	cleanWorkdir, err := cleanWorkspaceGitDir(workdir)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	switch workspaceGitCommand(strings.Join(args, "\x00")) {
	case workspaceGitConfigOriginURL:
		cmd = exec.CommandContext(ctx, "git", "config", "\x2d\x2dget", "remote.origin.url")
	case workspaceGitRevParseHEAD:
		cmd = exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	case workspaceGitStatusPorcelain:
		cmd = exec.CommandContext(ctx, "git", "status", "\x2d\x2dporcelain")
	default:
		return nil, fmt.Errorf("unsupported workspace git command: %q", strings.Join(args, " "))
	}
	var stdout, stderr bytes.Buffer
	cmd.Dir = cleanWorkdir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.WarnContext(ctx, "adapter.codex.workspace_metadata.git_failed", "concern", "adapter.providers.codex.request", "component", "adapter",
			"subcomponent", "codex",
			"workdir", cleanWorkdir,
			"args", strings.Join(args, " "),
			"err", err,
		)
		return nil, fmt.Errorf("workspace git command %q: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

func cleanWorkspaceGitDir(workdir string) (string, error) {
	workdir = strings.TrimSpace(workdir)
	if workdir == "" {
		return "", fmt.Errorf("empty workspace git dir")
	}
	if strings.ContainsRune(workdir, 0) {
		return "", fmt.Errorf("workspace git dir contains NUL")
	}
	clean, err := filepath.Abs(workdir)
	if err != nil {
		slog.Warn("adapter.codex.workspace_metadata.abs_failed", "concern", "adapter.providers.codex.request", "workdir", workdir, "err", err)
		return "", fmt.Errorf("absolute workspace git dir: %w", err)
	}
	return clean, nil
}

const defaultWorkspaceProbeTTL = 30 * time.Second

// NewWorkspaceProbe constructs a probe with a 30s cache window.
func NewWorkspaceProbe() *WorkspaceProbe {
	return newWorkspaceProbeWith(workspaceGitRunner{}, defaultWorkspaceProbeTTL, time.Now)
}

func newWorkspaceProbeWith(runner workspaceCommandRunner, ttl time.Duration, now func() time.Time) *WorkspaceProbe {
	if now == nil {
		now = time.Now
	}
	return &WorkspaceProbe{
		cache:    map[string]workspaceCacheEntry{},
		cacheTTL: ttl,
		now:      now,
		runner:   runner, mu: sync.
				Mutex{},
	}
}

// Probe returns the cached TurnMetadataWorkspace for path, refreshing
// it when the cache entry is older than cacheTTL.
func (p *WorkspaceProbe) Probe(ctx context.Context, path string) TurnMetadataWorkspace {
	path = strings.TrimSpace(path)
	if path == "" {
		return TurnMetadataWorkspace{
			AssociatedRemoteURLs: TurnMetadataRemoteURLs{Origin: ""},

			LatestGitCommitHash: "", HasChanges: false,
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.cache[path]; ok && p.now().Sub(entry.fetched) < p.cacheTTL {
		return entry.value
	}
	value := p.fetch(ctx, path)
	p.cache[path] = workspaceCacheEntry{value: value, fetched: p.now()}
	return value
}

func (p *WorkspaceProbe) fetch(ctx context.Context, path string) TurnMetadataWorkspace {
	out := TurnMetadataWorkspace{
		AssociatedRemoteURLs: TurnMetadataRemoteURLs{Origin: ""},

		LatestGitCommitHash: "", HasChanges: false,
	}
	if origin, err := p.runner.run(ctx, path, "config", "--get", "remote.origin.url"); err == nil {
		if v := strings.TrimSpace(string(origin)); v != "" {
			out.AssociatedRemoteURLs.Origin = v
		}
	}
	if head, err := p.runner.run(ctx, path, "rev-parse", "HEAD"); err == nil {
		if v := strings.TrimSpace(string(head)); v != "" {
			out.LatestGitCommitHash = v
		}
	}
	if status, err := p.runner.run(ctx, path, "status", "--porcelain"); err == nil {
		out.HasChanges = strings.TrimSpace(string(status)) != ""
	}
	return out
}
