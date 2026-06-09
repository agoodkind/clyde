// Package clisearch is the Claude CLI search backend. It shells out to
// `claude -p` for cheap conversation-search summarization and self-registers
// with internal/search at package load, so the generic search package never
// imports a provider package. The composition root (cmd/clyde) blank-imports
// this package to wire the backend, the same way the MITM provider packages
// register themselves.
package clisearch

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"goodkind.io/clyde/internal/search"
)

const (
	// backendName is the search backend key this package registers under. It
	// matches the default backend name resolved by internal/search.
	backendName = "claude"
	// defaultModel is used when the search request supplies no model.
	defaultModel = "haiku"
	// completeTimeout bounds one `claude -p` invocation.
	completeTimeout = 30 * time.Second
)

func init() {
	search.RegisterBackend(backendName, func(model string) search.Client {
		return &client{model: model}
	})
}

// client shells out to `claude -p` for one search completion.
type client struct {
	model string
}

// Complete runs `claude -p --model <model> <prompt>` and returns the trimmed
// stdout. It satisfies [search.Client].
func (c *client) Complete(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()

	model, err := cleanModelArg(c.model)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "claude")
	cmd.Args = []string{"claude", "-p", "--model", model, prompt}
	output, err := cmd.Output()
	if err != nil {
		slog.ErrorContext(ctx, "claude search request failed", "concern", "search", "model", model, "err", err)
		return "", fmt.Errorf("claude -p failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// cleanModelArg validates the model passed to the `claude` CLI, defaulting an
// empty model to [defaultModel] and rejecting a NUL byte that would corrupt the
// argument vector.
func cleanModelArg(model string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = defaultModel
	}
	if strings.ContainsRune(model, 0) {
		return "", fmt.Errorf("claude model contains NUL")
	}
	return model, nil
}
