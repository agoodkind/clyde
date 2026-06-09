package mitm

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

func newBaselineCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "baseline",
		Short:   "Manage MITM wire baselines",
		Long:    "Manage MITM wire baselines that Clyde learns from captured native traffic.",
		Example: "clyde mitm baseline seed --upstream claude-code --from capture.jsonl",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newBaselineSeedCmd(f))
	return cmd
}
