// Package sessioncmd implements `clyde session list` and
// `clyde session show <name>`. Both subcommands call the daemon
// session RPCs and emit typed payloads through the universal
// output.Encoder so JSON and text consumers share one code path.
package sessioncmd

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/cli/output"
	"goodkind.io/clyde/internal/daemon"
)

// NewCmd returns the cobra command for `clyde session`. The command
// itself does nothing; the two child commands `list` and `show` do
// the work.
func NewCmd(f *cli.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Inspect Clyde sessions",
	}
	cmd.AddCommand(newListCmd(f))
	cmd.AddCommand(newShowCmd(f))
	return cmd
}

func newListCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List Clyde sessions known to the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runList(cmd, f)
		},
	}
}

func newShowCmd(f *cli.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show a single Clyde session as a typed record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, f, args[0])
		},
	}
}

// ListRow is the typed shape one row of `session list` emits. The
// fields cover the columns the dashboard renders without exposing
// the entire proto envelope, so jq consumers can rely on a stable
// surface.
type ListRow struct {
	Name          string `json:"name"`
	SessionID     string `json:"session_id"`
	Model         string `json:"model"`
	ProviderID    string `json:"provider_id"`
	WorkDir       string `json:"work_dir"`
	WorkspaceRoot string `json:"workspace_root"`
	Summary       string `json:"summary,omitempty"`
	LastActive    string `json:"last_active,omitempty"`
	MessageCount  int    `json:"message_count"`
}

// IsOutputPayload marks ListRow as a valid output encoder payload.
func (ListRow) IsOutputPayload() {}

// ListRows is the array shape `session list --output-format json`
// emits at the top level. JSON consumers iterate the array directly.
type ListRows []ListRow

// IsOutputPayload marks ListRows as a valid output encoder payload.
func (ListRows) IsOutputPayload() {}

// ShowDetail is the typed shape `session show` emits. The fields
// mirror ListRow so consumers can union the two surfaces if they
// ever need to.
type ShowDetail struct {
	Name          string `json:"name"`
	SessionID     string `json:"session_id"`
	Model         string `json:"model"`
	ProviderID    string `json:"provider_id"`
	WorkDir       string `json:"work_dir"`
	WorkspaceRoot string `json:"workspace_root"`
	Summary       string `json:"summary,omitempty"`
	LastActive    string `json:"last_active,omitempty"`
	MessageCount  int    `json:"message_count"`
}

// IsOutputPayload marks ShowDetail as a valid output encoder payload.
func (ShowDetail) IsOutputPayload() {}

func runList(cmd *cobra.Command, f *cli.Factory) error {
	ctx := cmd.Context()
	resp, err := daemon.ListSessionsViaDaemon(ctx)
	if err != nil {
		slog.WarnContext(ctx, "cli.session.list.failed",
			"component", "cli",
			"subcomponent", "session",
			"err", err.Error(),
		)
		return fmt.Errorf("list sessions: %w", err)
	}
	rows := make(ListRows, 0, len(resp.GetSessions()))
	for _, s := range resp.GetSessions() {
		rows = append(rows, sessionToRow(s))
	}
	enc, err := output.From(cmd, f.IOStreams.Out)
	if err != nil {
		slog.WarnContext(ctx, "cli.session.list.encoder_failed",
			"component", "cli",
			"subcomponent", "session",
			"err", err.Error(),
		)
		return fmt.Errorf("resolve output encoder: %w", err)
	}
	emitErr := enc.Emit(rows, func(w io.Writer) error {
		writeListText(w, rows)
		return nil
	})
	if emitErr != nil {
		slog.WarnContext(ctx, "cli.session.list.emit_failed",
			"component", "cli",
			"subcomponent", "session",
			"err", emitErr.Error(),
		)
		return fmt.Errorf("emit session list: %w", emitErr)
	}
	return nil
}

func runShow(cmd *cobra.Command, f *cli.Factory, name string) error {
	ctx := cmd.Context()
	resp, err := daemon.GetSessionDetailViaDaemon(ctx, name)
	if err != nil {
		slog.WarnContext(ctx, "cli.session.show.failed",
			"component", "cli",
			"subcomponent", "session",
			"session", name,
			"err", err.Error(),
		)
		return fmt.Errorf("get session detail %q: %w", name, err)
	}
	// GetSessionDetailResponse does not carry the full SessionSummary,
	// so back-fill the list entry to populate the typed payload. The
	// list call is cheap (daemon-cached) and gives us the same row
	// shape the dashboard uses.
	list, listErr := daemon.ListSessionsViaDaemon(ctx)
	if listErr != nil {
		slog.WarnContext(ctx, "cli.session.show.list_failed",
			"component", "cli",
			"subcomponent", "session",
			"session", name,
			"err", listErr.Error(),
		)
		return fmt.Errorf("list sessions for show backfill: %w", listErr)
	}
	var summary *clydev1.SessionSummary
	for _, s := range list.GetSessions() {
		if s.GetName() == name || s.GetMetadataName() == name {
			summary = s
			break
		}
	}
	detail := buildShowDetail(name, summary, resp)
	enc, err := output.From(cmd, f.IOStreams.Out)
	if err != nil {
		slog.WarnContext(ctx, "cli.session.show.encoder_failed",
			"component", "cli",
			"subcomponent", "session",
			"session", name,
			"err", err.Error(),
		)
		return fmt.Errorf("resolve output encoder: %w", err)
	}
	emitErr := enc.Emit(detail, func(w io.Writer) error {
		writeShowText(w, detail)
		return nil
	})
	if emitErr != nil {
		slog.WarnContext(ctx, "cli.session.show.emit_failed",
			"component", "cli",
			"subcomponent", "session",
			"session", name,
			"err", emitErr.Error(),
		)
		return fmt.Errorf("emit session detail: %w", emitErr)
	}
	return nil
}

func sessionToRow(s *clydev1.SessionSummary) ListRow {
	return ListRow{
		Name:          s.GetName(),
		SessionID:     s.GetSessionId(),
		Model:         s.GetModel(),
		ProviderID:    s.GetProvider(),
		WorkDir:       s.GetWorkDir(),
		WorkspaceRoot: s.GetWorkspaceRoot(),
		Summary:       s.GetDisplayTitle(),
		LastActive:    formatNanos(s.GetLastActivityNanos()),
		MessageCount:  int(s.GetMessageCount()),
	}
}

func buildShowDetail(name string, summary *clydev1.SessionSummary, detail *clydev1.GetSessionDetailResponse) ShowDetail {
	out := ShowDetail{
		Name:          name,
		SessionID:     "",
		Model:         detail.GetModel(),
		ProviderID:    "",
		WorkDir:       "",
		WorkspaceRoot: "",
		Summary:       "",
		LastActive:    "",
		MessageCount:  int(detail.GetTotalMessages()),
	}
	if summary != nil {
		out.Name = summary.GetName()
		out.SessionID = summary.GetSessionId()
		if out.Model == "" {
			out.Model = summary.GetModel()
		}
		out.ProviderID = summary.GetProvider()
		out.WorkDir = summary.GetWorkDir()
		out.WorkspaceRoot = summary.GetWorkspaceRoot()
		out.Summary = summary.GetDisplayTitle()
		out.LastActive = formatNanos(summary.GetLastActivityNanos())
		if out.MessageCount == 0 {
			out.MessageCount = int(summary.GetMessageCount())
		}
	}
	return out
}

// formatNanos renders a Unix-nanosecond timestamp as an RFC3339
// UTC string. Zero is treated as unknown and returns the empty
// string so the JSON omitempty tag drops the field.
func formatNanos(ns int64) string {
	if ns <= 0 {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

func writeListText(out io.Writer, rows ListRows) {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(out, "(no sessions)")
		return
	}
	for _, row := range rows {
		_, _ = fmt.Fprintf(out, "%s\tmodel=%s\tprovider=%s\tmsgs=%d\tlast=%s\n",
			row.Name, row.Model, row.ProviderID, row.MessageCount, row.LastActive)
	}
}

func writeShowText(out io.Writer, d ShowDetail) {
	_, _ = fmt.Fprintf(out, "name           %s\n", d.Name)
	_, _ = fmt.Fprintf(out, "session_id     %s\n", d.SessionID)
	_, _ = fmt.Fprintf(out, "provider       %s\n", d.ProviderID)
	_, _ = fmt.Fprintf(out, "model          %s\n", d.Model)
	_, _ = fmt.Fprintf(out, "work_dir       %s\n", d.WorkDir)
	_, _ = fmt.Fprintf(out, "workspace_root %s\n", d.WorkspaceRoot)
	_, _ = fmt.Fprintf(out, "summary        %s\n", d.Summary)
	_, _ = fmt.Fprintf(out, "last_active    %s\n", d.LastActive)
	_, _ = fmt.Fprintf(out, "message_count  %d\n", d.MessageCount)
}
