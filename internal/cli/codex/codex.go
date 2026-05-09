// Package codex owns the explicit `clyde codex` CLI override verb.
package codex

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	codexlifecycle "goodkind.io/clyde/internal/providers/codex/lifecycle"
)

// NewCmd returns the explicit `clyde codex` override verb.
func NewCmd(_ *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:                "codex",
		Short:              "Run Codex through Clyde",
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return codexlifecycle.InvokeCLI(cmd.Context(), args, "", "")
		},
	}
}
