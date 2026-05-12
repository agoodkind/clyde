package compact

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli/output"
	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/session"
)

func runListBackups(cmd *cobra.Command, out io.Writer, sess *session.Session) error {
	cliCompactLog.Logger().Info("cli.compact.ledger.started",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
	)
	entries, err := compactengine.ReadLedger(sess.Metadata.ProviderSessionID())
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.ledger.failed",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			"err", err,
		)
		return err
	}
	sorted := compactengine.SortLedger(entries)
	payload := newLedgerListJSON(sorted)
	enc, err := output.From(cmd, out)
	if err != nil {
		slog.WarnContext(cmd.Context(), "cli.compact.ledger.encoder_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", sess.Name,
			"err", err,
		)
		return fmt.Errorf("resolve output encoder: %w", err)
	}
	emitErr := enc.Emit(payload, func(w io.Writer) error {
		return writeLedgerText(w, sorted)
	})
	if emitErr != nil {
		slog.WarnContext(cmd.Context(), "cli.compact.ledger.emit_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			"err", emitErr,
		)
		return fmt.Errorf("emit ledger: %w", emitErr)
	}
	cliCompactLog.Logger().Info("cli.compact.ledger.completed",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"entries", len(entries),
	)
	return nil
}

func writeLedgerText(out io.Writer, entries []compactengine.LedgerEntry) error {
	if len(entries) == 0 {
		if _, err := fmt.Fprintln(out, "(no ledger entries)"); err != nil {
			slog.Warn("cli.compact.ledger.write_empty_failed",
				"component", "cli",
				"subcomponent", "compact",
				"err", err,
			)
			return fmt.Errorf("write ledger empty line: %w", err)
		}
		return nil
	}
	for _, e := range entries {
		_, err := fmt.Fprintf(out, "%s  op=%s  target=%s  strips=%s  pre=%d  snap=%s\n",
			e.Timestamp.UTC().Format("2006-01-02 15:04:05Z"),
			e.Op,
			humanInt(e.Target),
			strings.Join(e.Strips, ","),
			e.PreApplyOffset,
			e.SnapshotPath)
		if err != nil {
			slog.Warn("cli.compact.ledger.write_row_failed",
				"component", "cli",
				"subcomponent", "compact",
				"err", err,
			)
			return fmt.Errorf("write ledger row: %w", err)
		}
	}
	return nil
}
