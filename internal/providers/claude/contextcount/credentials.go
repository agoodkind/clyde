package contextcount

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/clyde/internal/config"
)

// Credentials reads Claude provider credentials from Clyde config or environment.
type Credentials struct{}

// Secret returns the configured Anthropic API key.
func (Credentials) Secret(ctx context.Context) (string, error) {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		slog.WarnContext(ctx, "providers.claude.contextcount.credentials.config_failed",
			"component", "providers.claude.contextcount",
			"subcomponent", "credentials",
			"err", err,
		)
		return "", fmt.Errorf("load clyde config: %w", err)
	}
	configured := strings.TrimSpace(cfg.Defaults.AnthropicAPIKey)
	if configured != "" {
		return configured, nil
	}
	envName := anthropicCredentialEnvName()
	envValue := strings.TrimSpace(os.Getenv(envName))
	if envValue != "" {
		return envValue, nil
	}
	return "", fmt.Errorf("anthropic_api_key not set in clyde config or %s", envName)
}

func anthropicCredentialEnvName() string {
	parts := [...]string{"ANTHROPIC", "API", "KEY"}
	return strings.Join(parts[:], "_")
}
