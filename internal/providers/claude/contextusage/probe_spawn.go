package contextusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"goodkind.io/clyde/internal/contextusage"
)

// ProbeOptions configures a /context spawn against the Claude CLI.
// The spawn is Claude-specific (slash-command argv, JSON envelope
// output) and lives in the Claude provider package so the generic
// contextusage package stays provider-neutral.
//
// The Snapshot ProbeContextUsage returns is workspace and session
// scoped: System tools, MCP tools, Memory files, Skills, and Compact
// buffer reflect the workspace WorkDir points at, and Messages
// reflects the resumed transcript identified by SessionID.
type ProbeOptions struct {
	// SessionID is the provider's session identifier. The probe
	// hands it to claude as `--resume <SessionID>` so the Snapshot
	// reflects that session's transcript and not a fresh session.
	SessionID string

	// WorkDir is the cwd used when spawning claude. Must match the
	// session's original workspace so memory files (CLAUDE.md),
	// skills, agents, and MCP servers resolve to the same set the
	// live session sees.
	WorkDir string

	// Model is the model name handed to claude as `--model <Model>`.
	// The Snapshot's MaxTokens (and the per-model context budget the
	// planner targets) follows the model claude resolves the flag
	// to. An empty value omits the flag and lets claude pick its
	// default model, which is rarely the right one for compaction.
	Model string

	// Binary is the path to the claude CLI. Defaults to "claude" on
	// $PATH when empty. Tests inject a fake binary path here.
	Binary string

	// Timeout caps the probe. claude cold-starts plus MCP servers
	// can take 15-30 seconds on a large workspace, so callers
	// should budget at least 60 seconds for the default.
	Timeout time.Duration
}

// ProbeContextUsage spawns claude in print mode with the /context
// slash command and returns the workspace+session scoped Snapshot.
// The flow is:
//
//  1. Spawn claude with `-p /context --resume <session-id> --model <model>
//     --no-session-persistence --output-format json --max-turns 1`.
//  2. Read the full stdout as a JSON list of stream envelopes.
//  3. Find the envelope whose `type` field is `result` and whose
//     `subtype` is `success`; extract the `result` field, which
//     carries the markdown payload.
//  4. Parse the markdown header (`**Model:**`, `**Tokens:** N / M (P%)`)
//     and the "Estimated usage by category" table into a Snapshot.
//
// The probe runs claude as a one-shot non-interactive turn. Because
// the slash command does not call a model, the spawn does not
// consume API tokens and `--no-session-persistence` keeps the
// transcript untouched.
func ProbeContextUsage(ctx context.Context, opts ProbeOptions) (contextusage.Snapshot, error) {
	binary := opts.Binary
	if binary == "" {
		binary = "claude"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, stdout, stderr, err := startProbeCmd(probeCtx, ctx, binary, opts)
	if err != nil {
		return contextusage.Snapshot{}, err
	}

	started := currentTime()
	sessionContextLog.Logger().InfoContext(ctx, "compact.probe.spawned",
		"component", "compact",
		"subcomponent", "probe",
		"binary", binary,
		"session_id", opts.SessionID,
		"model", opts.Model,
		"work_dir", opts.WorkDir,
		"timeout_s", int(timeout.Seconds()),
	)

	var stderrTail strings.Builder
	var stderrWG sync.WaitGroup
	stderrWG.Go(func() { drainStderr(stderr, &stderrTail) })

	stdoutBytes, readErr := io.ReadAll(stdout)
	waitErr := cmd.Wait()
	stderrWG.Wait()

	var usage contextusage.Snapshot
	var parseErr error
	switch {
	case readErr != nil:
		parseErr = fmt.Errorf("probe: read stdout: %w", readErr)
	case waitErr != nil:
		parseErr = fmt.Errorf("probe: claude exited with error: %w", waitErr)
	default:
		usage, parseErr = decodeProbeStdout(ctx, stdoutBytes)
	}

	return finalizeProbeResult(ctx, opts, usage, parseErr, stderrTail.String(), time.Since(started).Milliseconds())
}

// finalizeProbeResult emits the terminal log line for the probe and
// returns the parsed snapshot or the parse error. Split out so
// ProbeContextUsage stays under the function-length budget.
func finalizeProbeResult(
	ctx context.Context,
	opts ProbeOptions,
	usage contextusage.Snapshot,
	parseErr error,
	stderrTail string,
	durationMs int64,
) (contextusage.Snapshot, error) {
	if parseErr != nil {
		slog.WarnContext(ctx, "compact.probe.parse_failed",
			"component", "compact",
			"subcomponent", "probe",
			"session_id", opts.SessionID,
			"model", opts.Model,
			"duration_ms", durationMs,
			"stderr_tail", stderrTail,
			"err", parseErr,
		)
		return contextusage.Snapshot{}, parseErr
	}
	sessionContextLog.Logger().InfoContext(ctx, "compact.probe.completed",
		"component", "compact",
		"subcomponent", "probe",
		"session_id", opts.SessionID,
		"duration_ms", durationMs,
		"total_tokens", usage.TotalTokens,
		"max_tokens", usage.MaxTokens,
		"percentage", usage.Percentage,
		"model", usage.Model,
		"categories", len(usage.Categories),
	)
	return usage, nil
}

// startProbeCmd configures the claude command, attaches stdout and
// stderr pipes, and starts the process. The probe does not write to
// stdin; the slash command runs as a single non-interactive turn,
// so claude closes its own input pipeline. The logCtx argument is
// the caller's context used for structured logging; probeCtx is the
// bounded execution context the command runs under.
func startProbeCmd(probeCtx, logCtx context.Context, binary string, opts ProbeOptions) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	args := buildProbeArgs(opts)
	cmd := exec.CommandContext(probeCtx, binary, args...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	cmd.Env = append(os.Environ(), "CLYDE_SUPPRESS_HOOKS=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.ErrorContext(logCtx, "compact.probe.stdout_pipe_failed", "component", "compact", "subcomponent", "probe", "err", err)
		return nil, nil, nil, fmt.Errorf("probe: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		slog.ErrorContext(logCtx, "compact.probe.stderr_pipe_failed", "component", "compact", "subcomponent", "probe", "err", err)
		return nil, nil, nil, fmt.Errorf("probe: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		slog.ErrorContext(logCtx, "compact.probe.start_failed", "component", "compact", "subcomponent", "probe", "binary", binary, "session_id", opts.SessionID, "err", err)
		return nil, nil, nil, fmt.Errorf("probe: start claude: %w", err)
	}
	return cmd, stdout, stderr, nil
}

// drainStderr reads stderr to EOF, keeping the first 2048 bytes as
// a tail for diagnostics. The probe never writes its own bytes to
// stderr; the tail exists so a failure inside claude (a missing
// session, a refused workspace) surfaces on the parent log.
func drainStderr(stderr io.Reader, tail *strings.Builder) {
	buf := make([]byte, 4096)
	for {
		n, readErr := stderr.Read(buf)
		if n > 0 {
			if tail.Len() < 2048 {
				remaining := 2048 - tail.Len()
				if n > remaining {
					tail.Write(buf[:remaining])
				} else {
					tail.Write(buf[:n])
				}
			}
		}
		if readErr != nil {
			return
		}
	}
}

// buildProbeArgs assembles the claude argv for the probe. The probe
// runs the /context slash command in print JSON mode against the
// resumed session. claude resolves the workspace (memory files, MCP
// servers, skills, agents) from cmd.Dir, which the caller sets to
// ProbeOptions.WorkDir. `--resume` re-hydrates the transcript so the
// Messages bucket reflects the live session. `--max-turns 1` caps
// the call at one non-model turn so the spawn returns as soon as the
// slash command renders. `--no-session-persistence` suppresses
// transcript writes through claude's session storage.
func buildProbeArgs(opts ProbeOptions) []string {
	args := []string{
		"-p", "/context",
		"--no-session-persistence",
		"--output-format", "json",
		"--max-turns", "1",
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return args
}

// sdkResultEnvelope mirrors the `result` entry in claude's print
// JSON output. The remaining envelopes (`system`, `assistant`) carry
// metadata the probe does not consume.
type sdkResultEnvelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	IsError   bool   `json:"is_error"`
	Result    string `json:"result"`
	SessionID string `json:"session_id"`
}

// decodeProbeStdout parses the full stdout payload into a Snapshot.
// claude in `--output-format json` emits a top-level JSON list of
// stream envelopes; only one carries the markdown the probe needs.
func decodeProbeStdout(ctx context.Context, stdout []byte) (contextusage.Snapshot, error) {
	var envelopes []json.RawMessage
	if err := json.Unmarshal(stdout, &envelopes); err != nil {
		slog.ErrorContext(ctx, "compact.probe.decode_envelope_failed",
			"component", "compact",
			"subcomponent", "probe",
			"err", err,
			"stdout_bytes", len(stdout),
		)
		return contextusage.Snapshot{}, fmt.Errorf("probe: decode stdout envelope: %w", err)
	}
	for _, raw := range envelopes {
		var env sdkResultEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}
		if env.Type != "result" {
			continue
		}
		if env.IsError {
			return contextusage.Snapshot{}, fmt.Errorf("probe: claude reported is_error on result envelope (subtype=%q)", env.Subtype)
		}
		if env.Subtype != "success" {
			return contextusage.Snapshot{}, fmt.Errorf("probe: claude returned result subtype %q", env.Subtype)
		}
		if env.Result == "" {
			return contextusage.Snapshot{}, fmt.Errorf("probe: empty result payload from claude")
		}
		return unmarshalContextMarkdown(env.Result)
	}
	return contextusage.Snapshot{}, fmt.Errorf("probe: no result envelope in stdout (envelopes=%d, bytes=%d)", len(envelopes), len(stdout))
}
