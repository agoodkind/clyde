package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	// copySharedProgressInterval limits intermediate progress lines for each phase to one every 30 seconds.
	// Each phase also prints a line when it starts and when Done equals Total.
	copySharedProgressInterval = 30 * time.Second
	percentScale               = 100
	copySharedRateUnknown      = "rate unknown"
	copySharedEstimateUnknown  = "time remaining unknown"
	copySharedFinished         = "complete"
)

// copySharedProgress prints each progress line to stderr and writes one log event for it.
// Each line includes the phase, verification round, Done, Total, percentage, rate, elapsed time, and remaining time.
// The rate and remaining time are unknown until the phase processes rows over a measured time.
// The log event omits rows_per_second and remaining_seconds while the rate and remaining time are unknown.
type copySharedProgress struct {
	out        io.Writer
	round      int
	phase      vectorsearch.SharedCopyProgress
	phaseStart time.Time
	lastReport time.Time
}

func newCopySharedProgress(out io.Writer) *copySharedProgress {
	return &copySharedProgress{
		out:        out,
		round:      0,
		phase:      vectorsearch.SharedCopyProgress{Phase: "", Done: 0, Total: 0},
		phaseStart: time.Time{},
		lastReport: time.Time{},
	}
}

// setRound sets the verification round number for later progress lines.
// Copy phases print round 0.
func (p *copySharedProgress) setRound(round int) {
	p.round = round
}

// callback returns the function that copy and verification call after each page.
func (p *copySharedProgress) callback(ctx context.Context) vectorsearch.SharedCopyProgressFunc {
	return func(progress vectorsearch.SharedCopyProgress) {
		p.observe(ctx, progress)
	}
}

// observe handles one progress report.
func (p *copySharedProgress) observe(ctx context.Context, progress vectorsearch.SharedCopyProgress) {
	now := clock.Now()
	if progress.Phase != p.phase.Phase {
		p.phaseStart = now
		p.lastReport = time.Time{}
	}
	p.phase = progress
	finished := progress.Total > 0 && progress.Done >= progress.Total
	if !p.lastReport.IsZero() && !finished && now.Sub(p.lastReport) < copySharedProgressInterval {
		return
	}
	p.lastReport = now
	p.write(ctx, now, finished)
}

func (p *copySharedProgress) write(ctx context.Context, now time.Time, finished bool) {
	elapsed := now.Sub(p.phaseStart)
	percent := 0.0
	if p.phase.Total > 0 {
		percent = float64(p.phase.Done) * percentScale / float64(p.phase.Total)
	}
	attributes := []slog.Attr{
		slog.String("concern", "cli.daemon"),
		slog.String("component", "cli"),
		slog.Int("round", p.round),
		slog.String("phase", string(p.phase.Phase)),
		slog.Int64("done", p.phase.Done),
		slog.Int64("total", p.phase.Total),
		slog.Float64("percent", percent),
		slog.Float64("elapsed_seconds", elapsed.Seconds()),
	}
	rateText := copySharedRateUnknown
	remainingText := copySharedEstimateUnknown
	if p.phase.Done > 0 && elapsed > 0 {
		rate := float64(p.phase.Done) / elapsed.Seconds()
		rateText = fmt.Sprintf("%.0f rows/s", rate)
		attributes = append(attributes, slog.Float64("rows_per_second", rate))
		if !finished {
			remaining := time.Duration(float64(p.phase.Total-p.phase.Done) / rate * float64(time.Second))
			remainingText = fmt.Sprintf("about %s left", remaining.Round(time.Second))
			attributes = append(attributes, slog.Float64("remaining_seconds", remaining.Seconds()))
		}
	}
	if finished {
		remainingText = copySharedFinished
	}
	slog.LogAttrs(ctx, slog.LevelInfo, "cli.daemon.copy_shared.progress", attributes...)
	line := fmt.Sprintf("%s round %d: %d of %d rows (%.1f%%), %s, %s elapsed, %s\n",
		p.phase.Phase, p.round, p.phase.Done, p.phase.Total, percent, rateText,
		elapsed.Round(time.Second), remainingText)
	if _, err := io.WriteString(p.out, line); err != nil {
		slog.WarnContext(ctx, "cli.daemon.copy_shared.progress_write_failed", "concern", "cli.daemon", "component", "cli", "err", err)
	}
}
