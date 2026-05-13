// Package mitmcontrib registers Claude's MITM launch-environment
// contributor with the generic mitmcontrib registry. Generic callers
// reach the spawn through mitmcontrib.Get(claudeContributorID) or
// mitmcontrib.Contributors() and never import this package directly.
package mitmcontrib

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
	claudeprovider "goodkind.io/clyde/internal/providers/claude"
	"goodkind.io/clyde/internal/providers/mitmcontrib"
)

// claudeContributorID names this provider in the registry. It matches
// session.ProviderClaude's string form but is duplicated here to keep
// the dependency direction one-way (mitmcontrib never imports session).
const claudeContributorID = "claude"

type claudeContributor struct{}

// Env satisfies mitmcontrib.Contributor. The MITM proxy and config
// gate whether claude-specific overrides are emitted; when MITM is
// disabled the contributor returns an empty map.
func (claudeContributor) Env(ctx context.Context, cfg *config.Config, proxy *mitm.Proxy) (map[string]string, error) {
	if cfg == nil {
		return nil, nil
	}
	if !cfg.MITM.EnabledDefault || !cfg.MITM.EnabledFor("claude") {
		return nil, nil
	}
	env, err := mitm.ClaudeEnv(ctx, cfg.MITM, proxy)
	if err != nil {
		slog.WarnContext(ctx, "provider.claude.mitm_env_failed",
			"component", "providers",
			"provider", claudeContributorID,
			"err", err,
		)
		return nil, fmt.Errorf("claude mitm environment: %w", err)
	}
	if env == nil {
		return nil, nil
	}
	if _, ok := env[claudeprovider.AnthropicBaseURLEnv]; ok {
		env[claudeprovider.ClydeMITMAnthropicBaseURLEnv] = "1"
	}
	return env, nil
}

// Sanitize satisfies mitmcontrib.Sanitizer. It scrubs stale
// Clyde-owned Anthropic MITM env from an inherited list so a fresh
// daemon-supplied value (or no value) can be applied on top.
func (claudeContributor) Sanitize(env []string) []string {
	return claudeprovider.SanitizeMITMList(env)
}

func init() {
	mitmcontrib.Register(claudeContributorID, claudeContributor{})
}
