package compactsummary

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/session"
)

const (
	defaultBinary        = "claude"
	defaultMaxInputBytes = 200_000
	defaultTimeout       = 120 * time.Second
)

// Options configures the provider-owned summarization command.
type Options struct {
	Binary        string
	Model         string
	MaxInputBytes int
	Timeout       time.Duration
}

// Adapter summarizes dropped compact content through the provider runtime.
type Adapter struct {
	Options Options
}

func init() {
	if err := compact.RegisterSummarizeAdapter(session.ProviderClaude, func() compact.SummarizeAdapter {
		return Adapter{
			Options: Options{
				Binary:        "",
				Model:         "",
				MaxInputBytes: 0,
				Timeout:       0,
			},
		}
	}); err != nil {
		panic(err)
	}
}

// SummarizeDropped summarizes the dropped content for a compact run.
func (a Adapter) SummarizeDropped(ctx context.Context, req compact.SummarizeRequest) (string, error) {
	options := a.Options
	if options.Model == "" {
		options.Model = req.Model
	}
	return summarizeDropped(ctx, req.DroppedText, options)
}

func summarizeDropped(ctx context.Context, droppedText string, options Options) (string, error) {
	if strings.TrimSpace(droppedText) == "" {
		return "", nil
	}
	binary := options.Binary
	if binary == "" {
		binary = defaultBinary
	}
	maxBytes := options.MaxInputBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxInputBytes
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	trimmedDroppedText := trimDroppedText(droppedText, maxBytes)
	prompt := summarizePrompt(trimmedDroppedText)

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"-p", "--no-session-persistence"}
	if options.Model != "" {
		args = append(args, "--model", options.Model)
	}
	args = append(args, prompt)

	started := currentTime()
	slog.InfoContext(ctx, "compact.summarize.spawned",
		"component", "compact",
		"subcomponent", "summarize",
		"binary", binary,
		"prompt_bytes", len(prompt),
		"timeout_s", int(timeout.Seconds()),
	)

	cmd := exec.CommandContext(callCtx, binary, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tail := stderr.String()
		if len(tail) > 1024 {
			tail = tail[len(tail)-1024:]
		}
		slog.WarnContext(ctx, "compact.summarize.failed",
			"component", "compact",
			"subcomponent", "summarize",
			"duration_ms", currentTime().Sub(started).Milliseconds(),
			"stderr_tail", tail,
			"err", err,
		)
		return "", fmt.Errorf("summarize: %w", err)
	}
	summary := strings.TrimSpace(stdout.String())
	slog.InfoContext(ctx, "compact.summarize.completed",
		"component", "compact",
		"subcomponent", "summarize",
		"duration_ms", currentTime().Sub(started).Milliseconds(),
		"summary_bytes", len(summary),
	)
	return summary, nil
}

func trimDroppedText(droppedText string, maxBytes int) string {
	if len(droppedText) <= maxBytes {
		return droppedText
	}
	excess := len(droppedText) - maxBytes
	return "[...earlier dropped content elided for length...]\n\n" + droppedText[excess:]
}

func summarizePrompt(droppedText string) string {
	return "The following is a transcript excerpt that is being removed from a long running agent conversation during context compaction. Write a concise recap (under 400 words) that preserves everything a continuing agent needs to know: user goals, decisions made, files touched, tools invoked, outcomes, and any in flight commitments. Use bullet points grouped by topic. Do not summarize the mechanics of the tools themselves; summarize what was accomplished. Do not greet the user. Output the recap only.\n\n" + droppedText
}
