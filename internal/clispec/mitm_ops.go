package clispec

import (
	"context"
	"fmt"
	"log/slog"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/mitmshow"
)

type mitmShowInput struct {
	ID string
}

func (mitmShowInput) isClispecInput() {}

type mitmShowPayload struct {
	ID string
}

func (mitmShowPayload) isClispecPrepared() {}

type mitmShowOutput struct {
	mitmshow.ShowOutput
}

func (mitmShowOutput) isClispecStructuredPayload() {}

var mitmGroup = &Group{
	Use:     "mitm",
	Short:   "Inspect the daemon-owned MITM proxy",
	Long:    "Inspect the daemon-owned MITM capture proxy: report listener status, show captured exchanges by request id, manage wire baselines, and manage the OS trust store for the MITM CA.",
	Example: "clyde mitm status\nclyde mitm show chatcmpl-abc123",
	Parent:  nil,
}

func mitmShowOp() Operation[mitmShowInput, mitmShowPayload] {
	return Operation[mitmShowInput, mitmShowPayload]{
		Name:       Name{Canonical: "mitm_show", CLIOverride: "show"},
		Group:      mitmGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Print log lines and MITM capture-store rows that correlate to one request id",
		Long: "Show searches Clyde adapter, daemon, and MITM per-concern wire logs and the SQLite " +
			"capture store for any line that mentions the given id. The id may be a Clyde request id " +
			"(chatcmpl-<hex>), a Cursor request id (UUID), or an Anthropic upstream request id " +
			"(req_<token>); the kind is detected by shape. The daemon owns the logs and capture store " +
			"and performs the lookup. Use --output-format json for structured output.",
		Examples: []string{"clyde mitm show chatcmpl-abc123"},
		Args: []Arg[mitmShowInput]{
			PositionalArg("id", "Correlation id to inspect.",
				func(in *mitmShowInput, v string) { in.ID = v }),
		},
		Params:         nil,
		New:            func() mitmShowInput { return mitmShowInput{ID: ""} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare:        func(in mitmShowInput) (mitmShowPayload, error) { return mitmShowPayload(in), nil },
		Run:            nil,
		runResult: func(ctx context.Context, p mitmShowPayload) (Result, error) {
			output, err := daemon.ShowCapture(ctx, p.ID)
			if err != nil {
				slog.WarnContext(ctx, "cli.mitm.show.failed", "concern", "cli.mitm", "component", "clispec", "err", err)
				return nil, fmt.Errorf("mitm show: %w", err)
			}
			return valueResult{
				Payload: mitmShowOutput{ShowOutput: output},
				Text:    mitmshow.RenderText(output),
			}, nil
		},
	}
}
