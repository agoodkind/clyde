// Package logs implements the metadata-only Clyde log inspection command. The
// daemon performs the inventory; this command renders the daemon's reply.
package logs

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/daemon"
)

const defaultLargestFileLimit = 3

// NewCmd returns the logs command tree.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Inspect Clyde log metadata",
	}
	cmd.AddCommand(newInventoryCmd(f))
	return cmd
}

func newInventoryCmd(f *cli.Factory) *cobra.Command {
	var stateRoot string
	var largestFileLimit int
	var deep bool
	cmd := &cobra.Command{
		Use:     "inventory",
		Short:   "Inventory Clyde log files by metadata",
		Long:    "Inventory the daemon's log files grouped by category, with per-category counts, sizes, and the largest files. The daemon performs the scan; --output-format text renders a table and json emits a typed document.",
		Example: "clyde logs inventory --largest 5",
		RunE: func(cmd *cobra.Command, _ []string) error {
			formatRaw, err := cmd.Flags().GetString(output.FlagName)
			if err != nil {
				return fmt.Errorf("read output format: %w", err)
			}
			format, err := output.ParseFormat(formatRaw)
			if err != nil {
				return fmt.Errorf("parse output format: %w", err)
			}
			result, err := daemon.LogsInventory(cmd.Context(), stateRoot, largestFileLimit, deep, format == output.FormatJSON)
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.logs.inventory.failed", "concern", "cli.logs", "component", "cli", "err", err)
				return fmt.Errorf("logs inventory: %w", err)
			}
			_, _ = fmt.Fprint(f.IOStreams.Out, result)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateRoot, "state-root", "", "Override the Clyde state root to inventory")
	cmd.Flags().IntVar(&largestFileLimit, "largest", defaultLargestFileLimit, "Largest file count to show per category")
	cmd.Flags().BoolVar(&deep, "deep", false, "Perform an exact filesystem scan instead of the indexed inventory view")
	return cmd
}
