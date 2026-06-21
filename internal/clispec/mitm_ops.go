package clispec

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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

type mitmStatusInput struct{}

func (mitmStatusInput) isClispecInput() {}

type mitmStatusPayload struct{}

func (mitmStatusPayload) isClispecPrepared() {}

type mitmListenerStatusOutput struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Up      bool   `json:"up"`
}

type mitmStatusOutput struct {
	Listeners  []mitmListenerStatusOutput `json:"listeners"`
	CACertPath string                     `json:"ca_cert_path"`
	CAKeyPath  string                     `json:"ca_key_path"`
}

func (mitmStatusOutput) isClispecStructuredPayload() {}

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

func mitmStatusOp() Operation[mitmStatusInput, mitmStatusPayload] {
	return Operation[mitmStatusInput, mitmStatusPayload]{
		Name:           Name{Canonical: "mitm_status", CLIOverride: "status"},
		Group:          mitmGroup,
		Surfaces:       SurfaceSet{CLI: true, MCP: false},
		outputKind:     resultKindValue,
		Short:          "Print each configured MITM listen address, CA paths, and listener liveness",
		Long:           "Report each configured MITM listener address and whether the daemon currently has it bound, along with the CA certificate and key paths. The daemon answers from the listeners it bound rather than by dialing.",
		Examples:       []string{"clyde mitm status"},
		Args:           nil,
		Params:         nil,
		New:            func() mitmStatusInput { return mitmStatusInput{} },
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare:        func(in mitmStatusInput) (mitmStatusPayload, error) { return mitmStatusPayload{}, nil },
		Run:            nil,
		runResult: func(ctx context.Context, _ mitmStatusPayload) (Result, error) {
			status, err := daemon.GetMITMStatus(ctx)
			if err != nil {
				slog.WarnContext(ctx, "cli.mitm.status.failed", "concern", "cli.mitm", "component", "clispec", "err", err)
				return nil, fmt.Errorf("mitm status: %w", err)
			}
			listeners := make([]mitmListenerStatusOutput, 0, len(status.Listeners))
			var out strings.Builder
			for _, listener := range status.Listeners {
				listeners = append(listeners, mitmListenerStatusOutput{
					ID:      listener.ID,
					Address: listener.Address,
					Up:      listener.Up,
				})
				fmt.Fprintf(&out, "listener[%s] listen_address: %s\n", listener.ID, listener.Address)
				fmt.Fprintf(&out, "listener[%s] listener_up: %t\n", listener.ID, listener.Up)
			}
			fmt.Fprintf(&out, "ca_cert_path: %s\n", status.CACertPath)
			fmt.Fprintf(&out, "ca_key_path: %s\n", status.CAKeyPath)
			return valueResult{
				Payload: mitmStatusOutput{
					Listeners:  listeners,
					CACertPath: status.CACertPath,
					CAKeyPath:  status.CAKeyPath,
				},
				Text: out.String(),
			}, nil
		},
	}
}
