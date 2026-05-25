package compact

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	compactengine "goodkind.io/clyde/internal/compact"
)

// Styled output for the compact CLI. The existing plain-text fallback
// stays around for non-TTY sinks (pipes, hook captures, test harnesses);
// the styled variants kick in when the output is an interactive TTY.
// Styles follow Charm's convention: foreground colors, bold for headers,
// faint for secondary info, borders for summary boxes.

// Mode signals whether the run will mutate the transcript. Rendered
// prominently in every panel so destructive vs non-destructive runs
// are obvious at a glance.
type Mode int

const (
	// ModePreview renders a non-mutating compact run.
	ModePreview Mode = iota
	// ModeApply renders a compact run that mutates the transcript.
	ModeApply
)

// Label returns the uppercase display label for a compact mode.
func (m Mode) Label() string {
	if m == ModeApply {
		return "APPLY"
	}
	return "PREVIEW"
}

var (
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	styleKey     = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Width(14)
	stylePaneKey = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).Width(17)
	styleVal     = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	styleNum     = lipgloss.NewStyle().Foreground(lipgloss.Color("48")).Bold(true)
	styleMuted   = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	styleBad     = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	styleGood    = lipgloss.NewStyle().Foreground(lipgloss.Color("48")).Bold(true)
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)

	// Mode ribbons. Preview uses calm cyan; apply uses hot red with a
	// bang so destructive runs scream visually.
	styleRibbonPreview = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("15")).
				Background(lipgloss.Color("39")).
				Padding(0, 1)
	styleRibbonApply = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("15")).
				Background(lipgloss.Color("160")).
				Padding(0, 1)
	styleRibbonUndo = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(lipgloss.Color("33")).
			Padding(0, 1)

	stylePreviewBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("39")).
			Padding(0, 1)
	styleApplyBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("160")).
			Padding(0, 1)
	styleUndoBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("33")).
			Padding(0, 1)
)

func ribbon(m Mode) string {
	switch m {
	case ModeApply:
		return styleRibbonApply.Render(" " + m.Label() + " · will mutate transcript ")
	case ModePreview:
		return styleRibbonPreview.Render(" " + m.Label() + " · no disk writes ")
	default:
		return styleRibbonPreview.Render(" " + m.Label() + " · no disk writes ")
	}
}

func boxFor(m Mode) lipgloss.Style {
	switch m {
	case ModeApply:
		return styleApplyBox
	case ModePreview:
		return stylePreviewBox
	default:
		return stylePreviewBox
	}
}

// UpfrontStats captures everything the planner can show instantly,
// before any Prober call. All fields are filled from the slice,
// the calibration file, and (when cached) the session context probe.
type UpfrontStats struct {
	SessionName   string
	SessionID     string
	Model         string
	Mode          Mode
	CurrentTotal  int // live context total when available (cached or fresh)
	MaxTokens     int // 1,000,000 or session-specific
	Target        int
	StaticFloor   int
	Reserved      int
	Thinking      int
	Images        int
	ToolPairs     int
	ChatTurns     int
	BaselineTail  int // zero when not yet counted
	StrippersText string
	TargetDate    string // calibration capture date (empty when not calibrated)
}

// RenderUpfrontPanel draws the information box shown before the
// planner starts. It carries the core run identity (session, model)
// plus the current and target token counts, in label-first form, with
// no debug arithmetic rows.
func RenderUpfrontPanel(w io.Writer, s UpfrontStats) {
	box := boxFor(s.Mode)

	rows := []string{
		styleTitle.Render("Compact") + "   " + ribbon(s.Mode),
		"",
		kv("Session", s.SessionName),
		kv("Model", s.Model),
	}

	if s.CurrentTotal > 0 {
		current := humanInt(s.CurrentTotal)
		if s.MaxTokens > 0 {
			current += styleMuted.Render(fmt.Sprintf("   (%d%% of %s)",
				int(float64(s.CurrentTotal)/float64(s.MaxTokens)*100),
				humanInt(s.MaxTokens)))
		}
		rows = append(rows, kv("Current", current))
	}

	if s.Target > 0 {
		rows = append(rows, kv("Target", humanInt(s.Target)))
	}

	if s.StrippersText != "" {
		rows = append(rows, kv("Trim", s.StrippersText))
	}

	fmt.Fprintln(w, box.Render(strings.Join(rows, "\n")))
}

// progressView renders a rolling one liner during the target loop.
// Spinner frames animate on a time ticker so the UI stays smooth even
// when a single Prober call takes 500ms to 2s. Numbers update
// on each iteration. Mode stays visible on every frame so destructive
// runs are always marked.
type progressView struct {
	w         io.Writer
	target    int
	mode      Mode
	isTTY     bool
	startedAt time.Time
	upfront   UpfrontStats

	// Shared state written by Update and read by the ticker goroutine.
	// The mutex protects reads and writes. Contention is negligible.
	// The ticker fires every 60ms or so. Update fires on iteration
	// completion.
	mu            sync.Mutex
	iterCount     int
	lastRec       compactengine.IterationRecord
	frame         int
	lastLineCount int
	completed     bool
	finalRes      *compactengine.PlanResult
	finalStatic   int
	finalReserved int
	finalPath     string
	finalApplied  bool

	// Ticker goroutine lifecycle channels.
	stop chan progressSignal
	done chan progressSignal
}

type progressSignal struct {
	Triggered bool
}

// spinnerFPS is the animation rate. 16 frames per second keeps the
// braille spinner visibly smooth without burning CPU.
const spinnerFPS = 16

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func newProgressView(w io.Writer, target int, mode Mode, isTTY bool, upfront UpfrontStats) *progressView {
	p := &progressView{
		w:         w,
		target:    target,
		mode:      mode,
		isTTY:     isTTY,
		startedAt: cliCompactClock.Now(),
		upfront:   upfront,
		mu:        sync.Mutex{},
		iterCount: 0,
		lastRec: compactengine.IterationRecord{
			Step:              "",
			TailTokens:        0,
			CtxTotal:          0,
			Delta:             0,
			ThinkingDropped:   false,
			ImagesPlaceholder: false,
			ToolsFull:         0,
			ToolsLineOnly:     0,
			ToolsDropped:      0,
			ChatTurnsTotal:    0,
			ChatTurnsDropped:  0,
			Probe:             false,
		},
		frame:         0,
		lastLineCount: 0,
		completed:     false,
		finalRes:      nil,
		finalStatic:   0,
		finalReserved: 0,
		finalPath:     "",
		finalApplied:  false,
		stop:          make(chan progressSignal),
		done:          make(chan progressSignal),
	}
	if isTTY {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					cliCompactLog.Logger().Error("cli.compact.progress_panic", "panic", r, "err", fmt.Errorf("panic: %v", r))
				}
			}()
			p.animate()
		}()
		p.draw()
	} else {
		close(p.done)
	}
	return p
}

// animate is the ticker goroutine. It runs at spinnerFPS until stop is
// closed. The goroutine calls draw concurrently with Update. Both
// paths hold the same mutex so the shared frame counter stays
// consistent.
func (p *progressView) animate() {
	interval := time.Second / spinnerFPS
	t := time.NewTicker(interval)
	defer func() { t.Stop() }()
	defer close(p.done)
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.mu.Lock()
			p.frame++
			p.mu.Unlock()
			p.draw()
		}
	}
}

func (p *progressView) modeLabel() string {
	if p.mode == ModeApply {
		return styleBad.Render("APPLY")
	}
	return styleGood.Render("PREVIEW")
}

// lastLineCount tracks how many lines the previous draw emitted so
// the next draw can rewind the cursor to the top of the dashboard and
// redraw in place.
func (p *progressView) draw() {
	p.mu.Lock()
	iter := p.iterCount
	rec := p.lastRec
	frame := p.frame
	prevLines := p.lastLineCount
	p.mu.Unlock()

	if iter == 0 && rec.Step == "" {
		rec.Step = "warming up"
	}

	spin := spinnerFrames[frame%len(spinnerFrames)]
	elapsed := time.Since(p.startedAt).Round(time.Second)

	step := strings.TrimSpace(rec.Step)
	if len(step) > 44 {
		step = step[:41] + "..."
	}

	ctxStr := humanInt(rec.CtxTotal)
	if rec.CtxTotal == 0 {
		ctxStr = "?"
	}

	lines := p.composePanelLines(spin, elapsed, rec, step, ctxStr)

	var buf strings.Builder
	// Move cursor up prevLines (if any) and clear each line so we can
	// redraw without leaving stale content.
	if prevLines > 0 {
		fmt.Fprintf(&buf, "\x1b[%dF", prevLines)
	} else {
		buf.WriteString("\r")
	}
	for _, line := range lines {
		buf.WriteString("\x1b[2K") // clear entire line
		buf.WriteString(line)
		buf.WriteString("\n")
	}

	p.mu.Lock()
	p.lastLineCount = len(lines)
	p.mu.Unlock()
	fmt.Fprint(p.w, buf.String())
}

func (p *progressView) composePanelLines(
	spin string,
	elapsed time.Duration,
	rec compactengine.IterationRecord,
	step string,
	ctxStr string,
) []string {
	currentTotal := rec.CtxTotal
	if p.completed && p.finalRes != nil {
		currentTotal = p.finalStatic + p.finalRes.FinalTail + p.finalReserved
	}
	if currentTotal > 0 {
		ctxStr = humanInt(currentTotal)
	}

	var header string
	if p.completed {
		header = fmt.Sprintf("%s %s  %s",
			styleGood.Render("✓"),
			p.modeLabel(),
			styleMuted.Render(fmt.Sprintf("finished in %s", elapsed)))
	} else {
		header = fmt.Sprintf("%s %s  %s",
			spin,
			p.modeLabel(),
			styleMuted.Render(fmt.Sprintf("running %s", elapsed)))
	}

	lines := []string{
		header,
		"",
		renderPaneRow("Current", styleNum.Render(ctxStr)),
		renderPaneRow("Target", styleNum.Render(humanInt(p.target))),
		"",
		"  " + styleTitle.Render("Trimmed"),
		renderBreakdownRow("thinking", categoryStateThinking(rec)),
		renderBreakdownRow("images", categoryStateImages(rec)),
		renderBreakdownRow("chat", categoryStateChat(rec)),
		renderBreakdownRow("tools", categoryStateTools(rec)),
	}

	if !p.completed && len(step) > 0 {
		phase := phaseFromStep(rec.Step)
		if phase == "" {
			phase = step
		}
		lines = append(lines, "", "  "+styleMuted.Render("Step: "+phase))
	}

	return lines
}

// deltaText formats a ctx-to-target delta for inline display in the
// per-iteration log table. The target is a floor, so a result at or
// above target is valid and only the under-target case is colored bad.
func deltaText(d int) string {
	if d < 0 {
		return styleBad.Render(fmt.Sprintf("-%s under target", humanInt(-d)))
	}
	return styleGood.Render(fmt.Sprintf("+%s above target", humanInt(d)))
}

func phaseFromStep(step string) string {
	switch {
	case strings.HasPrefix(step, "drop oldest chat turns"):
		return "chat drop"
	case strings.HasPrefix(step, "refine: restore"):
		return "chat refine (restore)"
	case strings.HasPrefix(step, "tools full -> line-only"):
		return "tools pass 1/2 (full -> line-only)"
	case strings.HasPrefix(step, "tools line-only -> drop"):
		return "tools pass 2/2 (line-only -> drop)"
	case strings.HasPrefix(step, "drop thinking"):
		return "drop thinking"
	case strings.HasPrefix(step, "replace images with placeholders"):
		return "images placeholder"
	case strings.HasPrefix(step, "baseline (no transforms)"):
		return "baseline"
	}
	return ""
}

// RenderIterationLog prints the compact iteration rows for non-live output.
func RenderIterationLog(w io.Writer, iterations []compactengine.IterationRecord) {
	if len(iterations) == 0 {
		return
	}
	lines := []string{
		styleTitle.Render("passes"),
		"  step                                              ctx       gap       delta",
	}
	for _, rec := range iterations {
		step := strings.TrimSpace(rec.Step)
		if len(step) > 40 {
			step = step[:37] + "..."
		}
		lines = append(lines, fmt.Sprintf("  %-40s  %-8s  %-8s  %s",
			step,
			styleNum.Render(humanInt(rec.CtxTotal)),
			styleNum.Render(humanInt(rec.TailTokens)),
			deltaText(rec.Delta)))
	}
	fmt.Fprintln(w, strings.Join(lines, "\n"))
}

// categoryStateThinking / categoryStateImages / categoryStateTools /
// categoryStateChat each render the right-hand cell of a breakdown
// row. They know how to translate an IterationRecord into a one-line
// summary with optional progress bar.
func categoryStateThinking(r compactengine.IterationRecord) string {
	if r.ThinkingDropped {
		return styleGood.Render("all dropped")
	}
	return styleMuted.Render("none changed")
}

func categoryStateImages(r compactengine.IterationRecord) string {
	if r.ImagesPlaceholder {
		return styleGood.Render("replaced with text")
	}
	return styleMuted.Render("none changed")
}

func categoryStateTools(r compactengine.IterationRecord) string {
	total := r.ToolsFull + r.ToolsLineOnly + r.ToolsDropped
	if total == 0 {
		return styleMuted.Render("none in transcript")
	}
	if r.ToolsLineOnly == 0 && r.ToolsDropped == 0 {
		return styleMuted.Render("none changed")
	}
	parts := []string{}
	if r.ToolsLineOnly > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d demoted to line-only", r.ToolsLineOnly, total))
	}
	if r.ToolsDropped > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d dropped", r.ToolsDropped, total))
	}
	return styleGood.Render(strings.Join(parts, ", "))
}

func categoryStateChat(r compactengine.IterationRecord) string {
	if r.ChatTurnsTotal == 0 {
		return styleMuted.Render("none in transcript")
	}
	if r.ChatTurnsDropped == 0 {
		return styleMuted.Render("none changed")
	}
	return styleGood.Render(fmt.Sprintf("%d of %d dropped", r.ChatTurnsDropped, r.ChatTurnsTotal))
}

// renderBreakdownRow formats a label + value row with consistent
// indentation so the live dashboard aligns cleanly.
func renderBreakdownRow(label, value string) string {
	return renderPaneRow(label, value)
}

func renderPaneRow(label, value string) string {
	return "  " + stylePaneKey.Render(label) + " " + value
}

// Update refreshes the iteration numbers. For non TTY sinks, like
// pipes or test harnesses, it also emits one line per iteration so
// the log stays readable without terminal control codes.
func (p *progressView) Update(r compactengine.IterationRecord) {
	p.mu.Lock()
	p.iterCount++
	p.lastRec = r
	iter := p.iterCount
	p.mu.Unlock()

	if p.isTTY {
		// Force an immediate redraw so numbers refresh without waiting
		// for the next ticker beat.
		p.draw()
		return
	}

	// Non TTY fallback. One line per iteration, no ANSI.
	step := strings.TrimSpace(r.Step)
	var tag string
	if r.Delta > 0 {
		tag = fmt.Sprintf("+%d over", r.Delta)
	} else {
		tag = fmt.Sprintf("-%d under", -r.Delta)
	}
	fmt.Fprintf(p.w, "  [%s] iter %02d %-44s ctx=%s %s\n",
		p.mode.Label(), iter, step, humanInt(r.CtxTotal), tag)
}

// Finish stops the ticker. The function blocks until the ticker
// goroutine has exited so the next print does not race with the
// ticker. The final live frame remains visible on TTY.
func (p *progressView) Finish() {
	if !p.isTTY {
		return
	}
	close(p.stop)
	<-p.done
}

func (p *progressView) Complete(res *compactengine.PlanResult, static, reserved int, applied bool, transcriptPath string) {
	p.mu.Lock()
	p.completed = true
	p.finalRes = res
	p.finalStatic = static
	p.finalReserved = reserved
	p.finalApplied = applied
	p.finalPath = transcriptPath
	p.mu.Unlock()
	if p.isTTY {
		p.draw()
	}
}

// finalBreakdownRows renders the same four-category breakdown the
// live dashboard uses, but from the final IterationRecord. Shared by
// preview and apply result boxes so the end-state matches the last
// live frame the user saw.
func finalBreakdownRows(final compactengine.IterationRecord) []string {
	return []string{
		renderBreakdownRow("thinking", categoryStateThinking(final)),
		renderBreakdownRow("images", categoryStateImages(final)),
		renderBreakdownRow("tools", categoryStateTools(final)),
		renderBreakdownRow("chat", categoryStateChat(final)),
	}
}

// finalRecord extracts the last IterationRecord from PlanResult so
// both Render functions can show the same end-state breakdown.
func finalRecord(res *compactengine.PlanResult) compactengine.IterationRecord {
	if len(res.Iterations) == 0 {
		return compactengine.IterationRecord{
			Step:              "",
			TailTokens:        0,
			CtxTotal:          0,
			Delta:             0,
			ThinkingDropped:   false,
			ImagesPlaceholder: false,
			ToolsFull:         0,
			ToolsLineOnly:     0,
			ToolsDropped:      0,
			ChatTurnsTotal:    0,
			ChatTurnsDropped:  0,
			Probe:             false,
		}
	}
	return res.Iterations[len(res.Iterations)-1]
}

// RenderFinalPreview draws the result box for a preview run with the
// full per-category breakdown of what would change. The session name
// is woven into the Apply command shown at the bottom so the user can
// copy and paste it without retyping their session reference.
func RenderFinalPreview(w io.Writer, res *compactengine.PlanResult, sessionName string, target, static, reserved int) {
	before := static + res.BaselineTail + reserved
	after := static + res.FinalTail + reserved

	rows := []string{
		styleTitle.Render("Compact preview") + "   " + ribbon(ModePreview),
		"",
		kv("Before", humanInt(before)),
		kv("After", humanInt(after)),
		kv("Target", humanInt(target)),
		"",
		styleTitle.Render("Trimmed"),
	}
	rows = append(rows, finalBreakdownRows(finalRecord(res))...)
	rows = append(rows,
		"",
		kv("Apply", styleMuted.Render(fmt.Sprintf("clyde compact %s %s --apply", sessionName, humanInt(target)))),
	)
	if !res.HitTarget {
		rows = append(rows, "", styleMuted.Render(floorAboveTargetText(target, after, static, reserved)))
	}
	fmt.Fprintln(w, stylePreviewBox.Render(strings.Join(rows, "\n")))
}

// RenderFinalApply draws the result box for an applied run. The box is
// red-bordered so destructive runs stay visually distinct from previews
// and the body ends with the literal undo command for this session.
func RenderFinalApply(w io.Writer, res *compactengine.PlanResult, sessionName string, target, static, reserved int, transcriptPath string) {
	before := static + res.BaselineTail + reserved
	after := static + res.FinalTail + reserved

	rows := []string{
		styleTitle.Render("Compacted") + "   " + ribbon(ModeApply),
		"",
		kv("Before", humanInt(before)),
		kv("After", humanInt(after)),
		kv("Target", humanInt(target)),
		"",
		styleTitle.Render("Trimmed"),
	}
	rows = append(rows, finalBreakdownRows(finalRecord(res))...)
	rows = append(rows,
		"",
		kv("Transcript", styleMuted.Render(transcriptPath)),
		kv("Undo", styleMuted.Render(fmt.Sprintf("clyde compact %s --undo", sessionName))),
	)
	if !res.HitTarget {
		rows = append(rows, "", styleMuted.Render(floorAboveTargetText(target, after, static, reserved)))
	}
	fmt.Fprintln(w, styleApplyBox.Render(strings.Join(rows, "\n")))
}

// floorAboveTargetText produces the single explanatory paragraph shown
// on the result panel when the planner's minimum reachable result sits
// above the user-set target. The wording names the static buckets that
// the compact engine cannot trim so the user understands why the
// target was unreachable.
func floorAboveTargetText(target, after, static, reserved int) string {
	floor := static + reserved
	return fmt.Sprintf(
		"The %s target could not be met because the system prompt, tool definitions, and memory take %s on their own, which leaves no room to trim further. Result landed at %s.",
		humanInt(target),
		humanInt(floor),
		humanInt(after),
	)
}

// RenderUndoResult draws the successful undo summary.
func RenderUndoResult(
	w io.Writer,
	sessionName string,
	sessionID string,
	transcriptPath string,
	ledgerPath string,
	entry compactengine.LedgerEntry,
	preBytes int64,
	postBytes int64,
) {
	snapshot := styleMuted.Render("none")
	if entry.SnapshotPath != "" {
		snapshot = styleVal.Render(entry.SnapshotPath)
	}

	verdict := styleGood.Render("✓ UNDID · transcript restored")
	if entry.SnapshotPath != "" && postBytes != entry.PreApplyOffset {
		verdict = styleWarn.Render("⚠ UNDID · snapshot fallback")
	}

	rows := []string{
		styleTitle.Render("result") + "   " + styleRibbonUndo.Render(" UNDO · revert last apply ") + "   " + verdict,
		"",
		kv("session", sessionName),
		kv("session uuid", styleMuted.Render(shortUUID(sessionID))),
		kv("applied at", entry.Timestamp.UTC().Format(time.RFC3339)),
		kv("transcript", styleMuted.Render(transcriptPath)),
		kv("ledger", styleMuted.Render(ledgerPath)),
		kv("target", styleMuted.Render(humanInt(entry.Target))),
		kv("boundary uuid", shortUUID(entry.BoundaryUUID)),
		kv("synthetic uuid", shortUUID(entry.SyntheticUUID)),
		kv("snapshot", snapshot),
		"",
		kv("transcript bytes", fmt.Sprintf("%s → %s", humanInt(int(preBytes)), humanInt(int(postBytes)))),
		kv("bytes delta", humanInt(int(postBytes-preBytes))),
	}
	fmt.Fprintln(w, styleUndoBox.Render(strings.Join(rows, "\n")))
}

func kv(label, value string) string {
	return styleKey.Render(label) + styleVal.Render(value)
}
