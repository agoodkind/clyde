package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

const (
	// copySharedProgressInterval bounds how often one phase reports while it
	// runs. A phase always reports when it starts and when it finishes.
	copySharedProgressInterval = 30 * time.Second
	percentScale               = 100
)

// copySharedProgress prints and logs the progress of each copy and
// verification phase: rows done, total rows, percent, rate, and remaining time.
type copySharedProgress struct {
	out        io.Writer
	now        func() time.Time
	round      int
	phase      vectorsearch.SharedCopyProgress
	phaseStart time.Time
	lastReport time.Time
}

func newCopySharedProgress(out io.Writer) *copySharedProgress {
	return &copySharedProgress{
		out:        out,
		now:        time.Now,
		round:      0,
		phase:      vectorsearch.SharedCopyProgress{Phase: "", Done: 0, Total: 0},
		phaseStart: time.Time{},
		lastReport: time.Time{},
	}
}

// setRound records the verification round that later reports belong to. Copy
// phases report round 0.
func (p *copySharedProgress) setRound(round int) {
	p.round = round
}

// callback returns the progress function that the copy and verification call.
func (p *copySharedProgress) callback(ctx context.Context) vectorsearch.SharedCopyProgressFunc {
	return func(progress vectorsearch.SharedCopyProgress) {
		p.observe(ctx, progress)
	}
}

// observe receives one progress report from the copy or verification.
func (p *copySharedProgress) observe(ctx context.Context, progress vectorsearch.SharedCopyProgress) {
	now := p.now()
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
	p.write(ctx, now)
}

func (p *copySharedProgress) write(ctx context.Context, now time.Time) {
	elapsed := now.Sub(p.phaseStart)
	percent := 0.0
	if p.phase.Total > 0 {
		percent = float64(p.phase.Done) * percentScale / float64(p.phase.Total)
	}
	rate := 0.0
	if elapsed > 0 {
		rate = float64(p.phase.Done) / elapsed.Seconds()
	}
	remaining := time.Duration(0)
	if rate > 0 && p.phase.Total > p.phase.Done {
		remaining = time.Duration(float64(p.phase.Total-p.phase.Done) / rate * float64(time.Second))
	}
	slog.InfoContext(ctx, "cli.daemon.copy_shared.progress", "concern", "cli.daemon", "component", "cli",
		"round", p.round,
		"phase", string(p.phase.Phase),
		"done", p.phase.Done,
		"total", p.phase.Total,
		"percent", percent,
		"rows_per_second", rate,
		"elapsed_seconds", elapsed.Seconds(),
		"remaining_seconds", remaining.Seconds(),
	)
	line := fmt.Sprintf("%s round %d: %d of %d rows (%.1f%%), %.0f rows/s, %s elapsed, about %s left\n",
		p.phase.Phase, p.round, p.phase.Done, p.phase.Total, percent, rate,
		elapsed.Round(time.Second), remaining.Round(time.Second))
	if _, err := io.WriteString(p.out, line); err != nil {
		slog.WarnContext(ctx, "cli.daemon.copy_shared.progress_write_failed", "concern", "cli.daemon", "component", "cli", "err", err)
	}
}
