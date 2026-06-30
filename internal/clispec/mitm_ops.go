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

type mitmBaselineSeedInput struct {
	Upstream  string
	IncludeUA []string
	ExcludeUA []string
}

func (mitmBaselineSeedInput) isClispecInput() {}

type mitmBaselineSeedPayload struct {
	Upstream  string
	IncludeUA []string
	ExcludeUA []string
}

func (mitmBaselineSeedPayload) isClispecPrepared() {}

type mitmBaselineSeedOutput struct {
	Upstream string `json:"upstream"`
	Flavors  int    `json:"flavors"`
}

func (mitmBaselineSeedOutput) isClispecStructuredPayload() {}

var mitmGroup = &Group{
	Use:     "mitm",
	Short:   "Inspect the daemon-owned MITM proxy",
	Long:    "Inspect the daemon-owned MITM capture proxy: report listener status, show captured exchanges by request id, manage wire baselines, and manage the OS trust store for the MITM CA.",
	Example: "clyde mitm status\nclyde mitm show chatcmpl-abc123",
	Parent:  nil,
}

var mitmBaselineGroup = &Group{
	Use:     "baseline",
	Short:   "Manage MITM wire baselines",
	Long:    "Manage MITM wire baselines that Clyde learns from captured native traffic.",
	Example: "clyde mitm baseline seed --upstream claude-code",
	Parent:  mitmGroup,
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

func mitmBaselineSeedOp() Operation[mitmBaselineSeedInput, mitmBaselineSeedPayload] {
	return Operation[mitmBaselineSeedInput, mitmBaselineSeedPayload]{
		Name:       Name{Canonical: "mitm_baseline_seed", CLIOverride: "seed"},
		Group:      mitmBaselineGroup,
		Surfaces:   SurfaceSet{CLI: true, MCP: false},
		outputKind: resultKindValue,
		Short:      "Write a MITM baseline from the capture store's deduped shape corpus",
		Long: "Seed builds a wire baseline from the deduped native-request " +
			"shapes already captured in the MITM capture store and writes it " +
			"as the current baseline for the given upstream. The provider " +
			"filter is derived from the upstream name. Use --include-ua / " +
			"--exclude-ua to scope which captured caller flavor seeds the " +
			"baseline (for example --include-ua claude-cli). The daemon " +
			"performs the build and write against its capture store.",
		Examples: []string{"clyde mitm baseline seed --upstream claude-code --include-ua claude-cli"},
		Args:     nil,
		Params: []Param[mitmBaselineSeedInput]{
			StringParam("upstream", "Upstream name, e.g. claude-code or codex-cli.", "", true,
				func(in *mitmBaselineSeedInput, v string) { in.Upstream = v }),
			StringSliceParam("include_ua", "Only seed from shapes whose User-Agent contains one of these substrings.", nil,
				func(in *mitmBaselineSeedInput, v []string) { in.IncludeUA = append(in.IncludeUA, v...) }),
			StringSliceParam("exclude_ua", "Drop shapes whose User-Agent contains one of these substrings.", nil,
				func(in *mitmBaselineSeedInput, v []string) { in.ExcludeUA = append(in.ExcludeUA, v...) }),
		},
		New: func() mitmBaselineSeedInput {
			return mitmBaselineSeedInput{Upstream: "", IncludeUA: nil, ExcludeUA: nil}
		},
		Children:       nil,
		MCPTaskSupport: "",
		MCPTaskRun:     nil,
		mcpTaskResult:  nil,
		Prepare: func(in mitmBaselineSeedInput) (mitmBaselineSeedPayload, error) {
			if strings.TrimSpace(in.Upstream) == "" {
				return mitmBaselineSeedPayload{}, fmt.Errorf("upstream is required")
			}
			return mitmBaselineSeedPayload(in), nil
		},
		Run: nil,
		runResult: func(ctx context.Context, p mitmBaselineSeedPayload) (Result, error) {
			result, err := daemon.SeedBaseline(ctx, p.Upstream, p.IncludeUA, p.ExcludeUA)
			if err != nil {
				slog.WarnContext(ctx, "cli.mitm.seed_baseline.failed", "concern", "cli.mitm", "component", "clispec", "upstream", p.Upstream, "err", err)
				return nil, fmt.Errorf("baseline seed: %w", err)
			}
			var out strings.Builder
			fmt.Fprintf(&out, "upstream: %s\n", result.Upstream)
			fmt.Fprintf(&out, "flavors: %d\n", result.Flavors)
			return valueResult{
				Payload: mitmBaselineSeedOutput{Upstream: result.Upstream, Flavors: result.Flavors},
				Text:    out.String(),
			}, nil
		},
	}
}
