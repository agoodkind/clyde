package compact

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli/output"
	compactengine "goodkind.io/clyde/internal/compact"
	contextusage "goodkind.io/clyde/internal/providers/claude/contextusage"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/util"
)

func runMetricsDashboard(cmd *cobra.Command, out io.Writer, sess *session.Session, path string, refresh bool) error {
	ctx := cmd.Context()
	cliCompactLog.Logger().Info("cli.compact.preview.metrics.started",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"transcript", path,
		"refresh", refresh,
	)
	slice, err := compactengine.LoadSlice(path)
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.preview.metrics.failed",
			"session", sess.Name,
			"transcript", path,
			"err", err,
		)
		return err
	}
	stat, err := os.Stat(path)
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.preview.metrics.failed",
			"session", sess.Name,
			"transcript", path,
			"err", err,
		)
		return err
	}

	thinking, images, toolPairs, chatTurns := categoryCounts(slice)
	cal, calibrated, _ := compactengine.LoadCalibration(sess.Metadata.ProviderSessionID())
	layer := contextusage.NewDefault(sess, "", "")
	usage, usageErr := layer.Usage(ctx, contextusage.UsageOptions{
		Refresh: refresh,
		MaxAge:  5 * time.Minute,
	})
	if usageErr != nil {
		cliCompactLog.Logger().Warn("cli.compact.preview.metrics.usage_failed",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			slog.Any("err", usageErr),
		)
	}

	payload := buildMetricsDashboardPayload(sess, path, slice, stat.Size(), cal, calibrated, usage, usageErr, thinking, images, toolPairs, chatTurns)

	enc, err := output.From(cmd, out)
	if err != nil {
		return fmt.Errorf("resolve output encoder: %w", err)
	}
	emitErr := enc.Emit(payload, func(w io.Writer) error {
		return writeMetricsDashboardText(w, sess, path, slice, stat.Size(), cal, calibrated, usage, usageErr, thinking, images, toolPairs, chatTurns)
	})
	if emitErr != nil {
		slog.Warn("cli.compact.preview.metrics.emit_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", sess.Name,
			"err", emitErr,
		)
		return fmt.Errorf("emit metrics dashboard: %w", emitErr)
	}

	cliCompactLog.Logger().Info("cli.compact.preview.metrics.completed",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"transcript", path,
		"usage_available", usageErr == nil,
	)
	return nil
}

func buildMetricsDashboardPayload(
	sess *session.Session,
	path string,
	slice *compactengine.Slice,
	fileSize int64,
	cal compactengine.Calibration,
	calibrated bool,
	usage contextusage.Usage,
	usageErr error,
	thinking, images, toolPairs, chatTurns int,
) MetricsDashboardJSON {
	payload := MetricsDashboardJSON{
		Session:               sess.Name,
		SessionID:             sess.Metadata.ProviderSessionID(),
		Transcript:            path,
		FileSizeBytes:         fileSize,
		Calibrated:            calibrated,
		CalibrationOverhead:   0,
		CalibrationCapturedAt: nil,
		Reserved:              DefaultReservedBuffer,
		BoundaryLine:          slice.BoundaryLine,
		BoundaryUUID:          "",
		BoundaryTime:          nil,
		PostBoundaryEntries:   len(slice.PostBoundary),
		ThinkingBlocks:        thinking,
		ImageBlocks:           images,
		ToolPairs:             toolPairs,
		ChatTurns:             chatTurns,
		Usage:                 nil,
		UsageError:            "",
	}
	if calibrated {
		payload.CalibrationOverhead = cal.StaticOverhead
		captured := cal.CapturedAt.UTC()
		payload.CalibrationCapturedAt = &captured
	}
	if slice.BoundaryLine >= 0 {
		payload.BoundaryUUID = slice.BoundaryUUID
		boundary := slice.BoundaryTime.UTC()
		payload.BoundaryTime = &boundary
	}
	if usageErr != nil {
		payload.UsageError = usageErr.Error()
	} else {
		snapshot := usage.Snapshot
		payload.Usage = &snapshot
	}
	return payload
}

// writeMetricsDashboardText emits the human-readable dashboard. The
// individual [fmt.Fprint] calls are intentionally fire-and-forget
// because a partial-write error on stdout has no meaningful recovery
// path here.
func writeMetricsDashboardText(
	out io.Writer,
	sess *session.Session,
	path string,
	slice *compactengine.Slice,
	fileSize int64,
	cal compactengine.Calibration,
	calibrated bool,
	usage contextusage.Usage,
	usageErr error,
	thinking, images, toolPairs, chatTurns int,
) error {
	_, _ = fmt.Fprintln(out, "session     "+sess.Name)
	_, _ = fmt.Fprintln(out, "uuid        "+sess.Metadata.ProviderSessionID())
	_, _ = fmt.Fprintf(out, "file        %s   (%d lines, %s)\n", path, len(slice.AllEntries), util.FormatSize(fileSize))

	if calibrated {
		_, _ = fmt.Fprintf(out, "calibration static_overhead = %s  (captured %s)\n",
			humanInt(cal.StaticOverhead), cal.CapturedAt.UTC().Format("2006-01-02"))
	} else {
		_, _ = fmt.Fprintf(out, "calibration NOT CALIBRATED  (run: clyde compact %s --auto-calibrate)\n", sess.Name)
	}
	_, _ = fmt.Fprintf(out, "reserved    %s   (default, assumes autocompact on)\n", humanInt(DefaultReservedBuffer))
	_, _ = fmt.Fprintln(out)

	if slice.BoundaryLine >= 0 {
		_, _ = fmt.Fprintf(out, "boundary    line %d  (uuid %s, %s)\n",
			slice.BoundaryLine+1, shortUUID(slice.BoundaryUUID), slice.BoundaryTime.UTC().Format(time.RFC3339))
	} else {
		_, _ = fmt.Fprintln(out, "boundary    none")
	}
	_, _ = fmt.Fprintf(out, "            %d entries since boundary\n", len(slice.PostBoundary))

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "tail blocks")
	_, _ = fmt.Fprintf(out, "  thinking blocks   %d\n", thinking)
	_, _ = fmt.Fprintf(out, "  image blocks      %d\n", images)
	_, _ = fmt.Fprintf(out, "  tool_use/result   %d pairs\n", toolPairs)
	_, _ = fmt.Fprintf(out, "  chat turns        %d\n", chatTurns)

	_, _ = fmt.Fprintln(out)
	if usageErr != nil {
		_, _ = fmt.Fprintf(out, "context     unavailable (%v)\n", usageErr)
		_, _ = fmt.Fprintf(out, "            rerun with --refresh to probe claude /context\n")
		return nil
	}
	_, _ = fmt.Fprintf(out, "context (from claude /context, source=%s, captured=%s)\n",
		usage.Source, usage.CapturedAt.UTC().Format(time.RFC3339))
	for _, cat := range usage.Categories {
		name := cat.Name
		if cat.IsDeferred && !strings.Contains(name, "(deferred)") {
			name += " (deferred)"
		}
		_, _ = fmt.Fprintf(out, "  %-24s %s tok\n", name, humanInt(cat.Tokens))
	}
	_, _ = fmt.Fprintf(out, "  %-24s %s / %s  (%d%%)\n",
		"total", humanInt(usage.TotalTokens), humanInt(usage.MaxTokens), usage.Percentage)
	_, _ = fmt.Fprintf(out, "  %-24s %s tok   (derived: everything except Messages, Compact buffer, Free space)\n",
		"static overhead", humanInt(usage.StaticOverhead()))
	return nil
}

func categoryCounts(slice *compactengine.Slice) (thinking, images, toolPairs, chatTurns int) {
	for _, e := range slice.PostBoundary {
		for _, b := range e.Content {
			switch b.Type {
			case "thinking", "redacted_thinking":
				thinking++
			case "image":
				images++
			}
		}
		if e.Type == "user" || e.Type == "assistant" {
			chatTurns++
		}
	}
	toolPairs = len(slice.PairIndex)
	return
}

func strippersDescribe(s compactengine.Strippers) string {
	var parts []string
	if s.Thinking {
		parts = append(parts, "thinking")
	}
	if s.Images {
		parts = append(parts, "images")
	}
	if s.Tools {
		parts = append(parts, "tools")
	}
	if s.Chat {
		parts = append(parts, "chat")
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, ",")
}
