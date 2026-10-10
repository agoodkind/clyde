// Package status owns the top-level clyde status command: one raw snapshot of
// the daemon, feeder, provider, and MITM state, auto-refreshed on a terminal
// and printed once everywhere else. The lm-semantic-search status package
// renders both forms.
package status

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	lmstatus "goodkind.io/lm-semantic-search/status"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation"
	daemonsvc "goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/response"
)

const (
	defaultInterval = 2 * time.Second
	minimumInterval = 500 * time.Millisecond
)

// NewCmd builds the top-level status command. On a terminal it runs the
// auto-refreshing view; when stdout is not a terminal, or with --once or JSON
// output, it prints one snapshot and exits.
func NewCmd(f *cli.Factory) *cobra.Command {
	var once bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show live daemon, feeder, provider, and MITM status",
		Long: "Show one raw snapshot of the daemon process, the conversation feeder " +
			"freshness, the provider counters, and the MITM listeners. On a terminal " +
			"the view refreshes on an interval until q or Ctrl-C; when stdout is not " +
			"a terminal it prints the same lines once and exits.",
		Example: "clyde status",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < minimumInterval {
				interval = minimumInterval
			}
			return run(cmd.Context(), f, cmd, once, interval)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "print one snapshot even on a terminal")
	cmd.Flags().DurationVar(&interval, "interval", defaultInterval, "refresh interval for the terminal view")
	return cmd
}

func run(ctx context.Context, f *cli.Factory, cmd *cobra.Command, once bool, interval time.Duration) error {
	format := resolveFormat(cmd)
	if format == output.FormatJSON {
		if err := lmstatus.WriteJSON(f.IOStreams.Out, buildSnapshot(gatherSnapshot(ctx), f.Build.Version)); err != nil {
			slog.ErrorContext(ctx, "cli.status.write_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
			return fmt.Errorf("write status snapshot: %w", err)
		}
		return nil
	}
	stdout, isFile := f.IOStreams.Out.(*os.File)
	live := !once && isFile && term.IsTerminal(int(stdout.Fd()))
	if !live {
		body := lmstatus.Dump(buildSnapshot(gatherSnapshot(ctx), f.Build.Version)) + "\n"
		if err := response.WriteResult(ctx, f.IOStreams.Out, f.IOStreams.Err, body); err != nil {
			slog.ErrorContext(ctx, "cli.status.write_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
			return fmt.Errorf("write status snapshot: %w", err)
		}
		return nil
	}
	source := func() (lmstatus.Snapshot, error) {
		return buildSnapshot(gatherSnapshot(ctx), f.Build.Version), nil
	}
	if err := lmstatus.Run(source, lmstatus.Options{Interval: interval, Once: false, Now: clock.Now}); err != nil {
		slog.ErrorContext(ctx, "cli.status.view_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("run the status view: %w", err)
	}
	return nil
}

// resolveFormat reads the persistent output format flag; an unset or invalid
// value is the text default.
func resolveFormat(cmd *cobra.Command) output.Format {
	flag := cmd.InheritedFlags().Lookup(output.FlagName)
	if flag == nil {
		return output.FormatText
	}
	format, err := output.ParseFormat(flag.Value.String())
	if err != nil {
		return output.FormatText
	}
	return format
}

// statusSnapshot is everything one refresh reads. Each section carries its own
// error so a dead surface renders as an error line instead of hiding the rest.
type statusSnapshot struct {
	readAt       time.Time
	report       daemonsvc.StatusReport
	freshness    conversation.SearchFreshness
	freshnessErr error
	providers    daemonsvc.ProviderStatsSnapshot
	providersErr error
	mitm         daemonsvc.MITMStatus
	mitmErr      error
}

func gatherSnapshot(ctx context.Context) statusSnapshot {
	snapshot := statusSnapshot{
		readAt:       clock.Now(),
		report:       daemonsvc.InspectStatus(ctx),
		freshness:    conversation.SearchFreshness{Manifest: 0, Needed: 0, Embedded: 0, Pending: 0, LastSyncUnix: 0},
		freshnessErr: nil,
		providers:    daemonsvc.ProviderStatsSnapshot{Providers: nil, LoadedAtUnix: 0},
		providersErr: nil,
		mitm:         daemonsvc.MITMStatus{Listeners: nil, CACertPath: "", CAKeyPath: ""},
		mitmErr:      nil,
	}
	snapshot.freshness, snapshot.freshnessErr = daemonsvc.GetSearchFreshness(ctx)
	snapshot.providers, snapshot.providersErr = daemonsvc.CurrentProviderStats(ctx)
	snapshot.mitm, snapshot.mitmErr = daemonsvc.GetMITMStatus(ctx)
	return snapshot
}
