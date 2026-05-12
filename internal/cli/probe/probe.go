// Package probe implements `clyde probe <session>`, a thin wrapper
// over the registered contextusage.Prober that prints the live
// /context snapshot for a Clyde session in the user's chosen output
// format. The text mode is a one-line-per-category dump; the JSON
// mode emits the typed contextusage.Snapshot variant so jq users can
// consume the same data without parsing prose.
package probe

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/contextusage"
)

// NewCmd returns the probe subcommand bound to the supplied factory.
// Discovery, store access, and provider routing are deferred to RunE
// so the cobra tree assembles cheaply.
func NewCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "probe <session>",
		Short: "Probe a session's live /context snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, f, args[0])
		},
	}
}

func run(cmd *cobra.Command, f *cli.Factory, name string) error {
	ctx := cmd.Context()
	store, err := f.Store()
	if err != nil {
		slog.WarnContext(ctx, "cli.probe.store_failed",
			"component", "cli",
			"subcomponent", "probe",
			"session", name,
			"err", err,
		)
		return fmt.Errorf("open session store: %w", err)
	}
	sess, err := store.Resolve(name)
	if err != nil {
		slog.WarnContext(ctx, "cli.probe.resolve_failed",
			"component", "cli",
			"subcomponent", "probe",
			"session", name,
			"err", err,
		)
		return fmt.Errorf("resolve session %q: %w", name, err)
	}
	if sess == nil {
		return fmt.Errorf("session %q not found", name)
	}
	providerID := string(sess.ProviderID())
	prober, ok := contextusage.Get(providerID)
	if !ok {
		slog.WarnContext(ctx, "cli.probe.prober_missing",
			"component", "cli",
			"subcomponent", "probe",
			"provider", providerID,
		)
		return fmt.Errorf("no context-usage prober registered for provider %q", providerID)
	}
	snapshot, err := prober.Probe(ctx, sess.Metadata.ProviderSessionID(), contextusage.ProbeOptions{RefreshHint: false})
	if err != nil {
		slog.WarnContext(ctx, "cli.probe.failed",
			"component", "cli",
			"subcomponent", "probe",
			"session", name,
			"provider", providerID,
			"err", err,
		)
		return fmt.Errorf("probe session %q: %w", name, err)
	}
	enc, err := output.From(cmd, f.IOStreams.Out)
	if err != nil {
		slog.WarnContext(ctx, "cli.probe.encoder_failed",
			"component", "cli",
			"subcomponent", "probe",
			"session", name,
			"err", err,
		)
		return fmt.Errorf("resolve output encoder: %w", err)
	}
	emitErr := enc.Emit(snapshotPayload{Snapshot: snapshot}, func(w io.Writer) error {
		return writeSnapshotText(w, snapshot)
	})
	if emitErr != nil {
		slog.WarnContext(ctx, "cli.probe.emit_failed",
			"component", "cli",
			"subcomponent", "probe",
			"session", name,
			"err", emitErr,
		)
		return fmt.Errorf("emit probe snapshot: %w", emitErr)
	}
	return nil
}

// snapshotPayload wraps contextusage.Snapshot so the output.Payload
// marker stays in this package. The payload's JSON shape is the
// underlying Snapshot directly, matching Item 1's JSON tags.
type snapshotPayload struct {
	contextusage.Snapshot
}

// IsOutputPayload marks snapshotPayload as a valid output encoder
// payload variant. The marker is empty by design.
func (snapshotPayload) IsOutputPayload() {}

// writeSnapshotText emits the human-readable summary. The format is
// intentionally compact (one line per category plus a total) so
// piping to less or grep stays useful without column alignment.
func writeSnapshotText(out io.Writer, snapshot contextusage.Snapshot) error {
	_, _ = fmt.Fprintf(out, "model        %s\n", snapshot.Model)
	_, _ = fmt.Fprintf(out, "total        %d / %d  (%d%%)\n", snapshot.TotalTokens, snapshot.MaxTokens, snapshot.Percentage)
	for _, cat := range snapshot.Categories {
		_, _ = fmt.Fprintf(out, "  %-24s %d\n", cat.Name, cat.Tokens)
	}
	return nil
}
