package clispec

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/loginventory"
)

type logsInventoryInput struct {
	StateRoot        string
	LargestFileLimit int
	Deep             bool
}

func (logsInventoryInput) isClispecInput() {}

type logsInventoryPayload struct {
	StateRoot        string
	LargestFileLimit int
	Deep             bool
}

func (logsInventoryPayload) isClispecPrepared() {}

type logsInventoryOutput struct {
	loginventory.Inventory
}

func (logsInventoryOutput) isClispecStructuredPayload() {}

var logsGroup = &Group{
	Use:     "logs",
	Short:   "Inspect Clyde log metadata",
	Long:    "Inspect Clyde log metadata. The daemon performs the inventory scan; these commands render the daemon's reply without reading log bodies.",
	Example: "clyde logs inventory --largest 5",
	Parent:  nil,
}

func logsInventoryOp() Operation[logsInventoryInput, logsInventoryPayload] {
	return Operation[logsInventoryInput, logsInventoryPayload]{
		Name:       Name{Canonical: "logs_inventory", CLIOverride: "inventory"},
		Group:      logsGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Inventory Clyde log files by metadata",
		Long:       "Inventory the daemon's log files grouped by category, with per-category counts, sizes, and the largest files. The daemon performs the scan; --output-format text renders a table and json emits a typed document.",
		Examples:   []string{"clyde logs inventory --largest 5"},
		Args:       nil,
		Params: []Param[logsInventoryInput]{
			StringParam("state_root", "Override the Clyde state root to inventory.", "", false,
				func(in *logsInventoryInput, v string) { in.StateRoot = v }),
			IntParam("largest", "Largest file count to show per category.", 3,
				func(in *logsInventoryInput, v int) { in.LargestFileLimit = v }),
			BoolParam("deep", "Perform an exact filesystem scan instead of the indexed inventory view.", false,
				func(in *logsInventoryInput, v bool) { in.Deep = v }),
		},
		New: func() logsInventoryInput {
			return logsInventoryInput{
				StateRoot:        "",
				LargestFileLimit: 3,
				Deep:             false,
			}
		},
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(in logsInventoryInput) (logsInventoryPayload, error) {
			if in.LargestFileLimit <= 0 {
				return logsInventoryPayload{}, fmt.Errorf("--largest must be greater than 0")
			}
			return logsInventoryPayload(in), nil
		},
		Run: nil,
		runResult: func(ctx context.Context, p logsInventoryPayload) (Result, error) {
			inventory, err := daemon.LogsInventory(ctx, p.StateRoot, p.LargestFileLimit, p.Deep)
			if err != nil {
				slog.WarnContext(ctx, "cli.logs.inventory.failed", "concern", "cli.logs", "component", "clispec", "err", err)
				return nil, fmt.Errorf("logs inventory: %w", err)
			}
			text, err := loginventory.RenderText(inventory)
			if err != nil {
				slog.WarnContext(ctx, "clispec.logs_inventory.render_failed", "concern", "cli.logs", "component", "clispec", "err", err)
				return nil, fmt.Errorf("render logs inventory: %w", err)
			}
			return valueResult{
				Payload: logsInventoryOutput{Inventory: inventory},
				Text:    text,
			}, nil
		},
	}
}
