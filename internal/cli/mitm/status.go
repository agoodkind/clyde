package mitm

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/daemon"
)

func newStatusCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Print each configured MITM listen address, CA paths, and listener liveness",
		Long:    "Report each configured MITM listener address and whether the daemon currently has it bound, along with the CA certificate and key paths. The daemon answers from the listeners it bound rather than by dialing.",
		Example: "clyde mitm status",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := daemon.GetMITMStatus(cmd.Context())
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.status.failed", "concern", "cli.mitm", "component", "cli", "err", err)
				return fmt.Errorf("mitm status: %w", err)
			}
			out := f.IOStreams.Out
			for _, listener := range status.Listeners {
				fmt.Fprintf(out, "listener[%s] listen_address: %s\n", listener.ID, listener.Address)
				fmt.Fprintf(out, "listener[%s] listener_up: %t\n", listener.ID, listener.Up)
			}
			fmt.Fprintf(out, "ca_cert_path: %s\n", status.CACertPath)
			fmt.Fprintf(out, "ca_key_path: %s\n", status.CAKeyPath)
			return nil
		},
	}
}
