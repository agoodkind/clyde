package logs

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
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
					slog.WarnContext(cmd.Context(), "cli.logs.inventory.config_failed", "concern", "cli.logs", "component", "cli", "err", err)
					return fmt.Errorf("load config for log inventory: %w", err)
				}
				if loaded != nil {
					loadedConfig = loaded
				}
			}
			mode := inventoryModeIndexed
			if deep {
				mode = inventoryModeDeep
			} else if configuredMode := strings.TrimSpace(loadedConfig.Logging.Inventory.Mode); configuredMode != "" {
				mode = inventoryMode(configuredMode)
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
				slog.WarnContext(cmd.Context(), "cli.logs.inventory.failed", "concern", "cli.logs", "component", "cli", "state_root", root, "err", err)
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
