package mitm

import (
	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
)

// NewCmd builds the mitm command tree.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "mitm",
		Short:   "Inspect the daemon-owned MITM proxy",
		Long:    "Inspect the daemon-owned MITM capture proxy: report listener status, show captured exchanges by request id, manage wire baselines, and manage the OS trust store for the MITM CA.",
		Example: "clyde mitm status\nclyde mitm show chatcmpl-abc123",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newStatusCmd(f))
	cmd.AddCommand(newBaselineCmd(f))
	cmd.AddCommand(newTrustCmd(f))
	return cmd
}
