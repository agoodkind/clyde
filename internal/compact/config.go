// Package compact rebuilds the `clyde compact` command on top of an
// append-only model that mirrors Claude Code's own /compact writer.
//
// Every public function in this package is pure or scoped to one
// well-defined IO boundary so the orchestrator (plan.go) can drive it
// without holding shared state. See the rebuild plan for the design.
package compact

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	clydeconfig "goodkind.io/clyde/internal/config"
)

// AnthropicAPIKey reads the Anthropic API key from the user's global
// clyde config at ~/.config/clyde/config.toml under
// [defaults] anthropic_api_key.
//
// IMPORTANT: This key is NOT used to authenticate claude -p invocations.
// Claude Code authenticates via OAuth tokens (cached under ~/.claude/)
// against the user's Claude Max subscription buckets, not via direct
// Anthropic API key billing. This key is used solely for the free
// /v1/messages/count_tokens endpoint, which the local token-count
// helpers and the transcript verifier hit to get an authoritative
// figure that matches what `/context` reports inside Claude Code.
// Removing it disables exact token counts and falls back to local
// tiktoken estimates; it does NOT break adapter or session spawning.
//
// The value is never logged or returned
// in error messages so accidental tracing cannot leak it.
//
// Returns ErrNoAPIKey when the config exists but the key is empty so
// callers can distinguish "user must configure" from "transient IO
// error" without parsing strings.
func AnthropicAPIKey() (string, error) {
	path := clydeconfig.GlobalConfigPath()
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoConfig
		}
		slog.Error("compact.config.stat_failed", "component", "compact", "path", path, "err", err)
		return "", fmt.Errorf("stat global config: %w", err)
	}
	cfg, err := clydeconfig.LoadGlobalOrDefault()
	if err != nil {
		slog.Error("compact.config.load_failed", "component", "compact", "path", path, "err", err)
		return "", fmt.Errorf("load global config: %w", err)
	}
	key := strings.TrimSpace(cfg.Defaults.AnthropicAPIKeySecret())
	if key == "" {
		return "", ErrNoAPIKey
	}
	return key, nil
}

// ErrNoConfig is returned when ~/.config/clyde/config.toml does not
// exist. Callers can present a setup hint instead of a stack trace.
var ErrNoConfig = fmt.Errorf("clyde global config not found")

// ErrNoAPIKey is returned when the config exists but defaults
// anthropic_api_key is empty.
var ErrNoAPIKey = fmt.Errorf("anthropic_api_key not set in clyde global config")
