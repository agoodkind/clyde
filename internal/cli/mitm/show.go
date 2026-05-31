package mitm

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/daemon"
)

func newShowCmd(f *cli.Factory) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Print log lines and MITM capture-store rows that correlate to one request id",
		Long: "Show searches Clyde adapter, daemon, and MITM per-concern wire logs and the SQLite " +
			"capture store for any line that mentions the given id. The id may be a Clyde request id " +
			"(chatcmpl-<hex>), a Cursor request id (UUID), or an Anthropic upstream request id " +
			"(req_<token>); the kind is detected by shape. The daemon owns the logs and capture store " +
			"and performs the lookup.",
		Example: "clyde mitm show chatcmpl-abc123",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			output, err := daemon.ShowCapture(cmd.Context(), args[0], asJSON)
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.show.failed", "concern", "cli.mitm", "component", "cli", "err", err)
				return fmt.Errorf("mitm show: %w", err)
			}
			_, _ = fmt.Fprint(f.IOStreams.Out, output)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a typed JSON document instead of human-readable text")
	return cmd
}
