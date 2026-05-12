package contextusage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"goodkind.io/clyde/internal/contextusage"
)

// ProbeOptions configures a get_context_usage spawn against the
// Claude CLI. The spawn is Claude-specific (control_request envelope,
// stream-json IO format, --resume semantics) and intentionally lives
// in the Claude provider package so the generic contextusage package
// stays provider-neutral.
type ProbeOptions struct {
	// SessionID resumes the live session so the probe measures the
	// actual per-session overhead (model choice, agent, custom system
	// prompt, injected context). Required.
	SessionID string

	// WorkDir is the cwd used when spawning claude. Must match the
	// session's original workspace so memory files (CLAUDE.md) and
	// skills resolve to the same set the live session sees.
	WorkDir string

	// Binary is the path to the claude CLI. Defaults to "claude" on
	// $PATH when empty. Tests inject a fake binary path here.
	Binary string

	// Timeout caps the probe. claude cold-starts plus MCP servers can
	// take 15-30 seconds on a large workspace, so callers should
	// budget at least 60 seconds for the default.
	Timeout time.Duration

	// ForkSession, when true, resumes the target session through a
	// disposable fork so the probe does not append /context noise to
	// the real transcript that compaction is about to mutate.
	ForkSession bool
}

// ProbeContextUsage spawns claude in SDK stream-json mode against a
// specific session, sends a get_context_usage control request over
// stdin, reads stdout until the matching control_response arrives, and
// returns the parsed Snapshot. The spawned claude process is closed
// when the function returns. No session persistence side effects are
// written because the probe passes --no-session-persistence.
func ProbeContextUsage(ctx context.Context, opts ProbeOptions) (contextusage.Snapshot, error) {
	if opts.SessionID == "" {
		return contextusage.Snapshot{}, errors.New("probe: session id required")
	}
	binary := opts.Binary
	if binary == "" {
		binary = "claude"
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	const requestID = "clyde-auto-calibrate-r1"
	reqLine, err := marshalControlRequest(ctx, requestID)
	if err != nil {
		return contextusage.Snapshot{}, err
	}

	cmd, stdin, stdout, stderr, err := startProbeCmd(probeCtx, ctx, binary, opts)
	if err != nil {
		return contextusage.Snapshot{}, err
	}

	started := currentTime()
	sessionContextLog.Logger().InfoContext(ctx, "compact.probe.spawned",
		"component", "compact",
		"subcomponent", "probe",
		"binary", binary,
		"session_id", opts.SessionID,
		"work_dir", opts.WorkDir,
		"timeout_s", int(timeout.Seconds()),
	)

	// Drain stderr concurrently so a noisy claude does not block on a
	// full pipe buffer. Keep the tail for diagnostics on failure.
	var stderrTail strings.Builder
	var stderrWG sync.WaitGroup
	stderrWG.Go(func() { drainStderr(stderr, &stderrTail) })

	if _, err := stdin.Write(reqLine); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		stderrWG.Wait()
		slog.ErrorContext(ctx, "compact.probe.write_request_failed", "component", "compact", "subcomponent", "probe", "session_id", opts.SessionID, "err", err)
		return contextusage.Snapshot{}, fmt.Errorf("probe: write control request: %w", err)
	}
	// Close stdin so claude sees EOF and exits after the response.
	// Keeping stdin open would leave the process waiting for more
	// messages until the context deadline.
	if err := stdin.Close(); err != nil {
		sessionContextLog.Logger().Debug("compact.probe.stdin_close_warn",
			"component", "compact",
			"subcomponent", "probe",
			"err", err,
		)
	}

	usage, parseErr := scanForUsage(probeCtx, stdout, requestID)
	waitErr := cmd.Wait()
	stderrWG.Wait()
	return finalizeProbeResult(ctx, opts, usage, parseErr, waitErr, stderrTail.String(), time.Since(started).Milliseconds())
}

// finalizeProbeResult emits the terminal log line for the probe and
// returns the parsed snapshot or the parse error. Split out so
// ProbeContextUsage stays under the function-length budget.
func finalizeProbeResult(
	ctx context.Context,
	opts ProbeOptions,
	usage contextusage.Snapshot,
	parseErr, waitErr error,
	stderrTail string,
	durationMs int64,
) (contextusage.Snapshot, error) {
	if parseErr != nil {
		waitErrMsg := ""
		if waitErr != nil {
			waitErrMsg = waitErr.Error()
		}
		slog.WarnContext(ctx, "compact.probe.parse_failed",
			"component", "compact",
			"subcomponent", "probe",
			"session_id", opts.SessionID,
			"duration_ms", durationMs,
			"stderr_tail", stderrTail,
			"err", parseErr,
			"wait_err", waitErrMsg,
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

// startProbeCmd configures the claude command, attaches stdio pipes,
// and starts the process. Split out so ProbeContextUsage stays under
// the function-length budget. The logCtx argument is the caller's
// context used for structured logging; probeCtx is the bounded
// execution context the command runs under.
func startProbeCmd(probeCtx, logCtx context.Context, binary string, opts ProbeOptions) (*exec.Cmd, io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	args := buildProbeArgs(opts)
	cmd := exec.CommandContext(probeCtx, binary, args...)
	if opts.WorkDir != "" {
		cmd.Dir = opts.WorkDir
	}
	cmd.Env = append(os.Environ(), "CLYDE_SUPPRESS_HOOKS=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.ErrorContext(logCtx, "compact.probe.stdin_pipe_failed", "component", "compact", "subcomponent", "probe", "err", err)
		return nil, nil, nil, nil, fmt.Errorf("probe: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		slog.ErrorContext(logCtx, "compact.probe.stdout_pipe_failed", "component", "compact", "subcomponent", "probe", "err", err)
		return nil, nil, nil, nil, fmt.Errorf("probe: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		slog.ErrorContext(logCtx, "compact.probe.stderr_pipe_failed", "component", "compact", "subcomponent", "probe", "err", err)
		return nil, nil, nil, nil, fmt.Errorf("probe: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		slog.ErrorContext(logCtx, "compact.probe.start_failed", "component", "compact", "subcomponent", "probe", "binary", binary, "session_id", opts.SessionID, "err", err)
		return nil, nil, nil, nil, fmt.Errorf("probe: start claude: %w", err)
	}
	return cmd, stdin, stdout, stderr, nil
}

// marshalControlRequest encodes the get_context_usage control request
// envelope claude expects on stdin. Split out so ProbeContextUsage
// stays under the function-length budget without inlining the type
// declarations.
func marshalControlRequest(ctx context.Context, requestID string) ([]byte, error) {
	type controlRequestPayload struct {
		Subtype string `json:"subtype"`
	}
	type controlRequestEnvelope struct {
		Type      string                `json:"type"`
		RequestID string                `json:"request_id"`
		Request   controlRequestPayload `json:"request"`
	}
	controlReq := controlRequestEnvelope{
		Type:      "control_request",
		RequestID: requestID,
		Request:   controlRequestPayload{Subtype: "get_context_usage"},
	}
	reqLine, err := json.Marshal(controlReq)
	if err != nil {
		slog.ErrorContext(ctx, "compact.probe.marshal_request_failed", "component", "compact", "subcomponent", "probe", "err", err)
		return nil, fmt.Errorf("probe: marshal control request: %w", err)
	}
	reqLine = append(reqLine, '\n')
	return reqLine, nil
}

// drainStderr reads stderr to EOF, keeping the first 2048 bytes as a
// tail for diagnostics. Split out so ProbeContextUsage stays under
// the function-length budget.
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

func buildProbeArgs(opts ProbeOptions) []string {
	args := []string{
		"-p",
		"--resume", opts.SessionID,
	}
	if opts.ForkSession {
		probeID := uuid.NewString()
		args = append(args,
			"--fork-session",
			"--session-id", probeID,
			"-n", "clyde-probe-"+probeID[:8],
		)
	}
	args = append(args,
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--no-session-persistence",
	)
	return args
}

// scanForUsage reads stdout line by line looking for a control_response
// whose request_id matches the one we sent. The stream may also carry
// hook events, session events, and other SDK messages that we ignore.
// Returns an error if stdout closes before the matching response is
// seen or if the response is an error rather than success.
func scanForUsage(ctx context.Context, stdout io.Reader, requestID string) (contextusage.Snapshot, error) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 128*1024), 8*1024*1024)

	type controlInner struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Error     string          `json:"error,omitempty"`
		Response  json.RawMessage `json:"response,omitempty"`
	}
	type controlEnvelope struct {
		Type     string       `json:"type"`
		Response controlInner `json:"response"`
	}

	lines := 0
	for scanner.Scan() {
		lines++
		line := scanner.Bytes()
		if !hasControlResponsePrefix(line) {
			continue
		}
		var env controlEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		if env.Type != "control_response" {
			continue
		}
		if env.Response.RequestID != requestID {
			continue
		}
		if env.Response.Subtype != "success" {
			return contextusage.Snapshot{}, fmt.Errorf("probe: claude returned error: %s", env.Response.Error)
		}
		var usage contextusage.Snapshot
		if err := json.Unmarshal(env.Response.Response, &usage); err != nil {
			slog.ErrorContext(ctx, "compact.probe.decode_usage_failed", "component", "compact", "subcomponent", "probe", "request_id", requestID, "err", err)
			return contextusage.Snapshot{}, fmt.Errorf("probe: decode usage payload: %w", err)
		}
		sessionContextLog.Logger().Debug("compact.probe.response_received",
			"component", "compact",
			"subcomponent", "probe",
			"lines_scanned", lines,
			"request_id", requestID,
		)
		return usage, nil
	}
	if err := scanner.Err(); err != nil {
		slog.ErrorContext(ctx, "compact.probe.scan_stdout_failed", "component", "compact", "subcomponent", "probe", "request_id", requestID, "lines_scanned", lines, "err", err)
		return contextusage.Snapshot{}, fmt.Errorf("probe: scan stdout: %w (lines=%d)", err, lines)
	}
	return contextusage.Snapshot{}, fmt.Errorf("probe: no control_response before stdout closed (lines=%d)", lines)
}

// hasControlResponsePrefix avoids the JSON parse cost on the vast
// majority of stream lines that are not control_response messages.
// Stream-json events always start with `{"type":` so a cheap substring
// check rules out user messages, hook events, and partial-message
// chunks before the heavier Unmarshal.
func hasControlResponsePrefix(line []byte) bool {
	return len(line) > 0 && line[0] == '{' && bytes.Contains(line, []byte(`"control_response"`))
}
