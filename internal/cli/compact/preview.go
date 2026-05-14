package compact

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/cli/output"
	compactengine "goodkind.io/clyde/internal/compact"
	"goodkind.io/clyde/internal/contextusage"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/util"
)

// runMetricsDashboard renders the read-only metrics dashboard by
// issuing a CompactPreview RPC with target=0 and empty strippers.
// The daemon owns the transcript load, calibration lookup, and
// /context probe; the CLI only consumes the streamed upfront event
// and renders it.
func runMetricsDashboard(cmd *cobra.Command, out io.Writer, sess *session.Session, path string, refresh bool) error {
	ctx := cmd.Context()
	cliCompactLog.Logger().Info("cli.compact.preview.metrics.started",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"transcript", path,
		"refresh", refresh,
	)

	events, done, cancel, err := daemon.CompactPreviewViaDaemon(ctx, daemon.CompactRunOptions{
		SessionName:    sess.Name,
		TargetTokens:   0,
		ReservedTokens: DefaultReservedBuffer,
		Model:          "",
		ModelExplicit:  false,
		Thinking:       false,
		Images:         false,
		Tools:          false,
		Chat:           false,
		Summarize:      false,
		SummarizeMode:  "",
		Force:          false,
		Refresh:        refresh,
	})
	if err != nil {
		cliCompactLog.Logger().Error("cli.compact.preview.metrics.failed",
			"session", sess.Name,
			"transcript", path,
			"err", err,
		)
		return fmt.Errorf("compact preview via daemon: %w", err)
	}
	defer cancel()

	var upfront *clydev1.CompactUpfront
	for ev := range events {
		if ev.GetKind() == clydev1.CompactEvent_KIND_UPFRONT {
			upfront = ev.GetUpfront()
		}
	}
	if done != nil {
		if streamErr := <-done; streamErr != nil {
			cliCompactLog.Logger().Error("cli.compact.preview.metrics.failed",
				"session", sess.Name,
				"transcript", path,
				"err", streamErr,
			)
			return fmt.Errorf("compact preview stream: %w", streamErr)
		}
	}
	if upfront == nil {
		return fmt.Errorf("compact preview stream ended without upfront event")
	}

	payload := buildMetricsDashboardPayload(upfront)

	enc, err := output.From(cmd, out)
	if err != nil {
		slog.WarnContext(ctx, "cli.compact.preview.metrics.encoder_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", sess.Name,
			"err", err.Error(),
		)
		return fmt.Errorf("resolve output encoder: %w", err)
	}
	emitErr := enc.Emit(payload, func(w io.Writer) error {
		return writeMetricsDashboardText(w, upfront)
	})
	if emitErr != nil {
		slog.WarnContext(ctx, "cli.compact.preview.metrics.emit_failed",
			"component", "cli",
			"subcomponent", "compact",
			"session", sess.Name,
			"err", emitErr.Error(),
		)
		return fmt.Errorf("emit metrics dashboard: %w", emitErr)
	}

	cliCompactLog.Logger().Info("cli.compact.preview.metrics.completed",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
		"transcript", path,
		"usage_available", upfront.GetUsageAvailable(),
	)
	return nil
}

// buildMetricsDashboardPayload converts a daemon CompactUpfront event
// into the typed JSON payload the dashboard emits in JSON mode.
func buildMetricsDashboardPayload(upfront *clydev1.CompactUpfront) MetricsDashboardJSON {
	payload := MetricsDashboardJSON{
		Session:               upfront.GetSessionName(),
		SessionID:             upfront.GetSessionId(),
		Transcript:            upfront.GetTranscriptPath(),
		FileSizeBytes:         upfront.GetFileSizeBytes(),
		Calibrated:            upfront.GetCalibrated(),
		CalibrationOverhead:   0,
		CalibrationCapturedAt: nil,
		Reserved:              int(upfront.GetReservedTokens()),
		BoundaryLine:          -1,
		BoundaryUUID:          "",
		BoundaryTime:          nil,
		PostBoundaryEntries:   int(upfront.GetPostBoundaryEntries()),
		ThinkingBlocks:        int(upfront.GetThinkingBlocks()),
		ImageBlocks:           int(upfront.GetImageBlocks()),
		ToolPairs:             int(upfront.GetToolPairs()),
		ChatTurns:             int(upfront.GetChatTurns()),
		Usage:                 nil,
		UsageError:            "",
	}
	if upfront.GetCalibrated() {
		payload.CalibrationOverhead = int(upfront.GetCalibrationOverhead())
		if captured, parseErr := time.Parse("2006-01-02", upfront.GetCalibrationDate()); parseErr == nil {
			capturedUTC := captured.UTC()
			payload.CalibrationCapturedAt = &capturedUTC
		}
	}
	if upfront.GetHasBoundary() {
		payload.BoundaryLine = int(upfront.GetBoundaryLine())
		payload.BoundaryUUID = upfront.GetBoundaryUuid()
		if boundary, parseErr := time.Parse(time.RFC3339, upfront.GetBoundaryTime()); parseErr == nil {
			boundaryUTC := boundary.UTC()
			payload.BoundaryTime = &boundaryUTC
		}
	}
	if upfront.GetUsageAvailable() {
		snapshot := contextusage.Snapshot{
			Model:       upfront.GetModel(),
			TotalTokens: int(upfront.GetCurrentTotal()),
			MaxTokens:   int(upfront.GetMaxTokens()),
			Percentage:  int(upfront.GetUsagePercentage()),
			Categories:  nil,
		}
		for _, cat := range upfront.GetUsageCategories() {
			snapshot.Categories = append(snapshot.Categories, contextusage.Category{
				Name:       cat.GetName(),
				Tokens:     int(cat.GetTokens()),
				IsDeferred: cat.GetIsDeferred(),
			})
		}
		payload.Usage = &snapshot
	} else {
		payload.UsageError = upfront.GetUsageError()
	}
	return payload
}

// writeMetricsDashboardText emits the human-readable dashboard from
// the daemon's upfront event. The individual [fmt.Fprint] calls are
// intentionally fire-and-forget because a partial-write error on
// stdout has no meaningful recovery path here.
func writeMetricsDashboardText(out io.Writer, upfront *clydev1.CompactUpfront) error {
	_, _ = fmt.Fprintln(out, "session     "+upfront.GetSessionName())
	_, _ = fmt.Fprintln(out, "uuid        "+upfront.GetSessionId())
	_, _ = fmt.Fprintf(out, "file        %s   (%d lines, %s)\n",
		upfront.GetTranscriptPath(), int(upfront.GetFileLineCount()), util.FormatSize(upfront.GetFileSizeBytes()))

	if upfront.GetCalibrated() {
		_, _ = fmt.Fprintf(out, "calibration static_overhead = %s  (captured %s)\n",
			humanInt(int(upfront.GetCalibrationOverhead())), upfront.GetCalibrationDate())
	} else {
		_, _ = fmt.Fprintf(out, "calibration NOT CALIBRATED  (run: clyde compact %s --auto-calibrate)\n", upfront.GetSessionName())
	}
	_, _ = fmt.Fprintf(out, "reserved    %s   (default, assumes autocompact on)\n", humanInt(int(upfront.GetReservedTokens())))
	_, _ = fmt.Fprintln(out)

	if upfront.GetHasBoundary() {
		_, _ = fmt.Fprintf(out, "boundary    line %d  (uuid %s, %s)\n",
			int(upfront.GetBoundaryLine())+1, shortUUID(upfront.GetBoundaryUuid()), upfront.GetBoundaryTime())
	} else {
		_, _ = fmt.Fprintln(out, "boundary    none")
	}
	_, _ = fmt.Fprintf(out, "            %d entries since boundary\n", int(upfront.GetPostBoundaryEntries()))

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "tail blocks")
	_, _ = fmt.Fprintf(out, "  thinking blocks   %d\n", int(upfront.GetThinkingBlocks()))
	_, _ = fmt.Fprintf(out, "  image blocks      %d\n", int(upfront.GetImageBlocks()))
	_, _ = fmt.Fprintf(out, "  tool_use/result   %d pairs\n", int(upfront.GetToolPairs()))
	_, _ = fmt.Fprintf(out, "  chat turns        %d\n", int(upfront.GetChatTurns()))

	_, _ = fmt.Fprintln(out)
	if !upfront.GetUsageAvailable() {
		_, _ = fmt.Fprintf(out, "context     unavailable (%s)\n", upfront.GetUsageError())
		_, _ = fmt.Fprintf(out, "            rerun with --refresh to probe claude /context\n")
		return nil
	}
	_, _ = fmt.Fprintf(out, "context (from claude /context, source=%s, captured=%s)\n",
		upfront.GetUsageSource(), upfront.GetUsageCapturedAt())
	for _, cat := range upfront.GetUsageCategories() {
		name := cat.GetName()
		if cat.GetIsDeferred() && !strings.Contains(name, "(deferred)") {
			name += " (deferred)"
		}
		_, _ = fmt.Fprintf(out, "  %-24s %s tok\n", name, humanInt(int(cat.GetTokens())))
	}
	_, _ = fmt.Fprintf(out, "  %-24s %s / %s  (%d%%)\n",
		"total", humanInt(int(upfront.GetCurrentTotal())), humanInt(int(upfront.GetMaxTokens())), int(upfront.GetUsagePercentage()))
	_, _ = fmt.Fprintf(out, "  %-24s %s tok   (derived: everything except Messages, Compact buffer, Free space)\n",
		"static overhead", humanInt(int(upfront.GetContextOverheadTokens())))
	return nil
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
