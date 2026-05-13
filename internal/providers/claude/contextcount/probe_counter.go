package contextcount

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	generic "goodkind.io/clyde/internal/contextcount"
)

const (
	defaultProbeBinary  = "claude"
	defaultProbeTimeout = 60 * time.Second
	contextPrompt       = "/context"
)

var messagesRowRegexp = regexp.MustCompile(`(?m)(?:^|[│|])\s*Messages\s*(?:[│|])\s*([0-9][0-9,]*)`)

var _ generic.Counter = (*ProbeCounter)(nil)

type probeExecFunc func(context.Context, probeCommand) (probeOutput, error)

type probeCommand struct {
	Binary  string
	Args    []string
	WorkDir string
	Timeout time.Duration
}

type probeOutput struct {
	Stdout string
	Stderr string
}

// ProbeCounterOptions configures a Claude CLI /context probe counter.
type ProbeCounterOptions struct {
	Binary  string
	WorkDir string
	Timeout time.Duration
	Exec    probeExecFunc
	Clock   generic.Clock
}

// ProbeCounter counts by parsing the Messages row from Claude CLI /context output.
type ProbeCounter struct {
	binary  string
	workDir string
	timeout time.Duration
	run     probeExecFunc
	clock   generic.Clock
}

// NewProbeCounter creates a Claude CLI /context probe counter.
func NewProbeCounter(options ProbeCounterOptions) *ProbeCounter {
	binary := options.Binary
	if binary == "" {
		binary = defaultProbeBinary
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	run := options.Exec
	if run == nil {
		run = runProbeCommand
	}
	return &ProbeCounter{
		binary:  binary,
		workDir: options.WorkDir,
		timeout: timeout,
		run:     run,
		clock:   options.Clock,
	}
}

// Source returns the registered source name.
func (c *ProbeCounter) Source() generic.CounterSource {
	return generic.CounterSourceProbe
}

// Count runs Claude /context and returns the Messages row token count.
func (c *ProbeCounter) Count(ctx context.Context, transcript generic.Transcript) (int, error) {
	started := c.currentTime()
	command := probeCommand{
		Binary:  c.binary,
		Args:    buildContextProbeArgs(),
		WorkDir: c.workDir,
		Timeout: c.timeout,
	}
	output, err := c.run(ctx, command)
	if err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.probe.exec_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "probe",
			"model", transcript.Model,
			"stderr_tail", tailString(output.Stderr),
			"err", err,
		)
		return 0, fmt.Errorf("claude contextcount probe: exec: %w", err)
	}
	tokens, err := readProbeMessagesTokens(output.Stdout)
	if err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.probe.parse_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "probe",
			"model", transcript.Model,
			"stdout_tail", tailString(output.Stdout),
			"err", err,
		)
		return 0, err
	}
	slog.InfoContext(ctx, "providers.claude.contextcount.probe.completed",
		"component", "providers.claude.contextcount",
		"subcomponent", "probe",
		"model", transcript.Model,
		"input_tokens", tokens,
		"duration_ms", c.currentTime().Sub(started).Milliseconds(),
	)
	return tokens, nil
}

func buildContextProbeArgs() []string {
	args := []string{
		"-p",
		contextPrompt,
		"--no-session-persistence",
	}
	return args
}

func runProbeCommand(ctx context.Context, command probeCommand) (probeOutput, error) {
	runCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()

	if command.Binary != defaultProbeBinary {
		err := fmt.Errorf("unsupported probe binary %q", command.Binary)
		slog.WarnContext(ctx, "providers.claude.contextcount.probe.unsupported_binary",
			"component", "providers.claude.contextcount",
			"subcomponent", "probe",
			"binary", command.Binary,
			"err", err,
		)
		return probeOutput{Stdout: "", Stderr: ""}, err
	}
	slog.DebugContext(ctx, "providers.claude.contextcount.probe.exec_start",
		"component", "providers.claude.contextcount",
		"subcomponent", "probe",
		"binary", defaultProbeBinary,
		"work_dir", command.WorkDir,
		"timeout_s", int(command.Timeout.Seconds()),
	)
	cmd := exec.CommandContext(runCtx, defaultProbeBinary, "-p", contextPrompt, "--no-session-persistence")
	if command.WorkDir != "" {
		cmd.Dir = command.WorkDir
	}
	cmd.Env = append(os.Environ(), "CLYDE_SUPPRESS_HOOKS=1")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.probe.command_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "probe",
			"binary", defaultProbeBinary,
			"stderr_tail", tailString(stderr.String()),
			"err", err,
		)
		return probeOutput{
			Stdout: stdout.String(),
			Stderr: stderr.String(),
		}, fmt.Errorf("run %s %s: %w", command.Binary, strings.Join(command.Args, " "), err)
	}
	return probeOutput{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}, nil
}

func readProbeMessagesTokens(output string) (int, error) {
	match := messagesRowRegexp.FindStringSubmatch(output)
	if len(match) != 2 {
		return 0, fmt.Errorf("claude contextcount probe: Messages row not found")
	}
	tokenText := strings.ReplaceAll(match[1], ",", "")
	tokens, err := strconv.Atoi(tokenText)
	if err != nil {
		slog.Warn("providers.claude.contextcount.probe.messages_tokens_parse_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "probe",
			"value", match[1],
			"err", err,
		)
		return 0, fmt.Errorf("claude contextcount probe: parse Messages tokens %q: %w", match[1], err)
	}
	return tokens, nil
}

func (c *ProbeCounter) currentTime() time.Time {
	if c.clock == nil {
		return time.Time{}
	}
	return c.clock.Now()
}

func tailString(value string) string {
	const maxBytes = 2048
	if len(value) <= maxBytes {
		return value
	}
	return value[len(value)-maxBytes:]
}
