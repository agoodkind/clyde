package logs

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/config"
)

// NewCmd returns the logs command tree.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Inspect Clyde log metadata",
	}
	cmd.AddCommand(newInventoryCmd(f))
	cmd.AddCommand(newPurgeLegacyCmd(f))
	return cmd
}

func newInventoryCmd(f *cli.Factory) *cobra.Command {
	var stateRoot string
	var largestFileLimit int
	var deep bool
	cmd := &cobra.Command{
		Use:   "inventory",
		Short: "Inventory Clyde log files by metadata",
		RunE: func(cmd *cobra.Command, args []string) error {
			root := stateRoot
			if root == "" {
				root = config.DefaultStateDir()
			}
			loadedConfig := config.NewConfigWithDefaults()
			if f != nil && f.Config != nil {
				loaded, err := f.Config()
				if err != nil {
					slog.WarnContext(cmd.Context(), "cli.logs.inventory.config_failed", "component", "cli", "err", err)
					return fmt.Errorf("load config for log inventory: %w", err)
				}
				if loaded != nil {
					loadedConfig = loaded
				}
			}
			mode := inventoryModeIndexed
			if deep {
				mode = inventoryModeDeep
			}
			currentInventory, err := buildInventory(inventoryOptions{
				StateRoot:        root,
				LargestFileLimit: largestFileLimit,
				Now:              time.Time{},
				Mode:             mode,
				Logging:          loadedConfig.Logging,
				MITM:             loadedConfig.MITM,
			})
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.logs.inventory.failed", "component", "cli", "state_root", root, "err", err)
				return fmt.Errorf("build log inventory: %w", err)
			}
			enc, err := output.From(cmd, f.IOStreams.Out)
			if err != nil {
				return fmt.Errorf("resolve output encoder: %w", err)
			}
			return enc.Emit(currentInventory, func(w io.Writer) error {
				return writeInventoryTable(w, currentInventory)
			})
		},
	}
	cmd.Flags().StringVar(&stateRoot, "state-root", "", "Override the Clyde state root to inventory")
	cmd.Flags().IntVar(&largestFileLimit, "largest", defaultLargestFileLimit, "Largest file count to show per category")
	cmd.Flags().BoolVar(&deep, "deep", false, "Perform an exact filesystem scan instead of the indexed inventory view")
	return cmd
}

func newPurgeLegacyCmd(f *cli.Factory) *cobra.Command {
	var stateRoot string
	var apply bool
	cmd := &cobra.Command{
		Use:   "purge-legacy",
		Short: "Remove legacy logging surfaces left over from previous Clyde implementations",
		Long: "Walks the Clyde state root the same way `logs inventory --mode deep` does, " +
			"identifies legacy logging surfaces (currently pre-repair retained logs), and " +
			"either reports or removes them. The new-format file set is encoded explicitly " +
			"so the purge cannot drift into deleting current-format logs.\n\n" +
			"Default mode is --dry-run; pass --apply to actually remove the identified files.",
		RunE: func(cmd *cobra.Command, args []string) error {
			root := stateRoot
			if root == "" {
				root = config.DefaultStateDir()
			}
			loadedConfig := config.NewConfigWithDefaults()
			if f != nil && f.Config != nil {
				loaded, err := f.Config()
				if err != nil {
					slog.WarnContext(cmd.Context(), "cli.logs.purge.config_failed", "component", "cli", "err", err)
					return fmt.Errorf("load config for purge-legacy: %w", err)
				}
				if loaded != nil {
					loadedConfig = loaded
				}
			}
			mode := PurgeModeDryRun
			if apply {
				mode = PurgeModeApply
			}
			result, err := PurgeLegacyLogs(PurgeOptions{
				StateRoot: root,
				Mode:      mode,
				Now:       time.Time{},
				Logging:   loadedConfig.Logging,
				MITM:      loadedConfig.MITM,
			})
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.logs.purge.failed", "component", "cli", "state_root", root, "err", err)
				return fmt.Errorf("purge legacy logs: %w", err)
			}
			enc, err := output.From(cmd, f.IOStreams.Out)
			if err != nil {
				return fmt.Errorf("resolve output encoder: %w", err)
			}
			return enc.Emit(result, func(w io.Writer) error {
				return writePurgeTable(w, result)
			})
		},
	}
	cmd.Flags().StringVar(&stateRoot, "state-root", "", "Override the Clyde state root to purge")
	cmd.Flags().BoolVar(&apply, "apply", false, "Actually remove identified legacy files; default is --dry-run reporting")
	return cmd
}
