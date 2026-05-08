package mitm

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// NewCmd builds the mitm command tree.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mitm",
		Short: "Inspect the daemon-owned MITM proxy",
	}
	cmd.AddCommand(newStatusCmd(f))
	cmd.AddCommand(newShowCmd(f))
	return cmd
}
