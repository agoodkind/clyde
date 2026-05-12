package compact

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/session"
)

// runAutoCalibrate asks the daemon to probe the live session, derive
// static_overhead, persist the calibration, and return the stored
// record. The CLI no longer reaches into the contextusage layer or the
// on-disk calibration store directly; the daemon owns both.
func runAutoCalibrate(ctx context.Context, out io.Writer, sess *session.Session, model string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cliCompactLog.Logger().Info("cli.compact.auto_calibrate.started",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"work_dir", sess.Metadata.WorkDir,
		"model", model,
	)
	_, _ = fmt.Fprintf(out, "probing %s via claude /context (may take 30-60 seconds)...\n", sess.Name)

	resp, err := daemon.CalibrateSessionViaDaemon(ctx, sess.Name)
	if err != nil {
		slog.ErrorContext(ctx, "cli.compact.auto_calibrate.probe_failed",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			"err", err,
		)
		return fmt.Errorf("auto-calibrate probe: %w", err)
	}

	cal := resp.GetCalibration()
	overhead := int(cal.GetStaticOverhead())
	totalTokens := int(resp.GetTotalTokens())
	categoriesCount := int(resp.GetCategoriesCount())

	cliCompactLog.Logger().Info("cli.compact.auto_calibrate.completed",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"static_overhead", overhead,
		"total_tokens", totalTokens,
		"model", cal.GetModel(),
	)
	_, _ = fmt.Fprintf(out, "auto-calibrated %s: static_overhead = %s (total=%s, derived from %d categories)\n",
		sess.Name, humanInt(overhead), humanInt(totalTokens), categoriesCount)
	return nil
}

// runCalibrate asks the daemon to persist an operator-supplied
// static_overhead value for the session. The daemon owns the write.
func runCalibrate(ctx context.Context, out io.Writer, sess *session.Session, n int, model string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cliCompactLog.Logger().Info("cli.compact.calibrate.started",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"static_overhead", n,
		"model", model,
	)
	resp, err := daemon.SetCalibrationViaDaemon(ctx, sess.Name, int64(n))
	if err != nil {
		slog.ErrorContext(ctx, "cli.compact.calibrate.failed",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			"err", err,
		)
		return fmt.Errorf("set calibration: %w", err)
	}
	overhead := int(resp.GetCalibration().GetStaticOverhead())
	_, _ = fmt.Fprintf(out, "calibrated session %s: static_overhead = %s\n", sess.Name, humanInt(overhead))
	cliCompactLog.Logger().Info("cli.compact.calibrate.completed",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"static_overhead", overhead,
	)
	return nil
}
