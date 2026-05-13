// Package mitmcontrib registers Codex's MITM launch-environment
// contributor with the generic mitmcontrib registry. Generic callers
// reach the spawn through mitmcontrib.Get(codexContributorID) or
// mitmcontrib.Contributors() and never import this package directly.
package mitmcontrib

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/mitm"
	codexprovider "goodkind.io/clyde/internal/providers/codex"
	"goodkind.io/clyde/internal/providers/mitmcontrib"
)

// codexContributorID names this provider in the registry. The string
// matches session.ProviderCodex; duplicated here to keep the
// dependency direction one-way.
const codexContributorID = "codex"

type codexContributor struct{}

// Env satisfies mitmcontrib.Contributor. The contributor always emits
// the OpenAI-compatible base URL (Codex needs it whether MITM is on
// or off) and only adds proxy/cert env when MITM is enabled for codex.
func (codexContributor) Env(ctx context.Context, cfg *config.Config, proxy *mitm.Proxy) (map[string]string, error) {
	if cfg == nil {
		return nil, nil
	}
	port := cfg.Adapter.Port
	if port == 0 {
		port = adapter.DefaultPort
	}
	env := map[string]string{
		codexprovider.OpenAIBaseURLEnv: fmt.Sprintf("http://[::1]:%d/v1", port),
	}
	if !cfg.MITM.EnabledDefault || !cfg.MITM.EnabledFor(codexContributorID) {
		return env, nil
	}
	proxyEnv, err := mitm.CodexEnv(ctx, cfg.MITM, proxy)
	if err != nil {
		slog.WarnContext(ctx, "provider.codex.mitm_env_failed",
			"component", "providers",
			"provider", codexContributorID,
			"err", err,
		)
		return nil, fmt.Errorf("codex mitm environment: %w", err)
	}
	maps.Copy(env, proxyEnv)
	certPath := strings.TrimSpace(cfg.MITM.CA.CertPath)
	if certPath != "" {
		env[codexprovider.SSLCertFileEnv] = certPath
		env[codexprovider.NodeExtraCACertsEnv] = certPath
	}
	return env, nil
}

func init() {
	mitmcontrib.Register(codexContributorID, codexContributor{})
}
