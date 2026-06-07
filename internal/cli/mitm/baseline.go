package mitm

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

func newBaselineCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "baseline",
		Short: "Manage MITM wire baselines",
		Long:  "Manage MITM wire baselines that Clyde learns from captured native traffic.",
	}
	cmd.AddCommand(newBaselineSeedCmd(f))
	return cmd
}
