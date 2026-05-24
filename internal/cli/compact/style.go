package compact

import (
	"fmt"
	"io"
	"strconv"
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

// RenderUpfrontPanel draws the phase-1 information box. Every number
// a user could want BEFORE the long calculation is on screen. The box
// border and ribbon both reflect Mode so there is no ambiguity.
func RenderUpfrontPanel(w io.Writer, s UpfrontStats) {
	box := boxFor(s.Mode)

	shrinkBy := s.CurrentTotal - s.Target
	floorSum := s.StaticFloor + s.Reserved
	budget := max(0, s.Target-floorSum)

	rows := []string{
		styleTitle.Render("compact") + "   " + ribbon(s.Mode),
		"",
		kv("session", s.SessionName),
		kv("uuid", styleMuted.Render(s.SessionID)),
		kv("model", s.Model),
	}

	if s.CurrentTotal > 0 {
		pct := ""
		if s.MaxTokens > 0 {
			pct = styleMuted.Render(fmt.Sprintf("   (%d%% of %s, what your chat shows)",
				int(float64(s.CurrentTotal)/float64(s.MaxTokens)*100),
				humanInt(s.MaxTokens)))
		}
		rows = append(rows,
			"",
			kv("current ctx", humanInt(s.CurrentTotal)+pct),
		)
	}

	if s.Target > 0 {
		shrinkNote := ""
		if shrinkBy > 0 {
			shrinkNote = styleMuted.Render("   → must shrink by " + humanInt(shrinkBy))
		}
		rows = append(rows, kv("target", humanInt(s.Target)+shrinkNote))
	}

	rows = append(rows,
		"",
		kv("static floor", humanInt(s.StaticFloor)+styleMuted.Render("   (system + tools + memory + skills)")),
		kv("reserved", humanInt(s.Reserved)+styleMuted.Render("   (autocompact buffer)")),
	)

	if s.TargetDate != "" {
		rows = append(rows, kv("calibrated", styleMuted.Render(s.TargetDate)))
	}

	if s.Target > 0 {
		rows = append(rows,
			"",
			kv("tail budget", humanInt(budget)+styleMuted.Render(fmt.Sprintf("   (target - static - reserved = %s)", humanInt(budget)))),
		)
	}

	rows = append(rows,
		"",
		kv("tail blocks", fmt.Sprintf("thinking %d · images %d · tools %d · chat %d",
			s.Thinking, s.Images, s.ToolPairs, s.ChatTurns)),
	)
	if s.StrippersText != "" {
		rows = append(rows, kv("strippers", s.StrippersText))
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

	lines := p.composePanelLines(spin, elapsed, iter, rec, step, ctxStr)

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
	iter int,
	rec compactengine.IterationRecord,
	step string,
	ctxStr string,
) []string {
	status := "running planner"
	if iter == 0 {
		status = "starting"
	}
	if p.completed {
		status = "finished"
	}
	phase := phaseFromStep(rec.Step)
	if phase == "" {
		phase = "n/a"
	}
	alwaysKept := p.upfront.StaticFloor + p.upfront.Reserved
	messageBudget := max(p.target-alwaysKept, 0)
	currentTotal := rec.CtxTotal
	messageTokensNowCount := rec.TailTokens
	delta := rec.Delta
	if p.completed && p.finalRes != nil {
		currentTotal = p.finalStatic + p.finalRes.FinalTail + p.finalReserved
		messageTokensNowCount = p.finalRes.FinalTail
		delta = currentTotal - p.target
	}
	if currentTotal > 0 {
		ctxStr = humanInt(currentTotal)
	}
	messageTokensNow := "?"
	if messageTokensNowCount > 0 {
		messageTokensNow = humanInt(messageTokensNowCount)
	}
	deltaText := deltaTextFriendly(delta)
	header := fmt.Sprintf("%s %s  %s", spin, p.modeLabel(), styleMuted.Render(fmt.Sprintf("time %s", elapsed)))
	if p.completed {
		header = fmt.Sprintf("%s %s  %s", styleGood.Render("✓"), p.modeLabel(), styleMuted.Render(fmt.Sprintf("finished in %s", elapsed)))
	}
	lines := []string{
		header,
		"  " + styleTitle.Render("run"),
		renderPaneRow("status", styleVal.Render(status)),
		renderPaneRow("step", styleVal.Render(strconv.Itoa(iter))),
		renderPaneRow("now doing", styleVal.Render(phase)),
		"  " + styleMuted.Render(strings.Repeat("─", 62)),
		"  " + styleTitle.Render("target"),
		renderPaneRow("current total", styleNum.Render(ctxStr)),
		renderPaneRow("target limit", styleNum.Render(humanInt(p.target))),
		renderPaneRow("over/under target", deltaText),
		"  " + styleMuted.Render(strings.Repeat("─", 62)),
		"  " + styleTitle.Render("token math"),
		renderPaneRow("equation", styleMuted.Render("current total = always-kept + message tokens")),
		renderPaneRow("check", styleNum.Render(ctxStr)+styleMuted.Render(fmt.Sprintf(" = %s + %s", humanInt(alwaysKept), messageTokensNow))),
		renderPaneRow("static tokens", styleNum.Render(humanInt(p.upfront.StaticFloor))),
		renderPaneRow("safety buffer", styleNum.Render(humanInt(p.upfront.Reserved))),
		renderPaneRow("always-kept", styleNum.Render(humanInt(alwaysKept))+styleMuted.Render(" (= static + safety)")),
		renderPaneRow("message tokens", styleNum.Render(messageTokensNow)),
		renderPaneRow("message budget", styleNum.Render(humanInt(messageBudget))+styleMuted.Render(" (= target - always-kept)")),
		"  " + styleMuted.Render(strings.Repeat("─", 62)),
		"  " + styleTitle.Render("what changed"),
		renderBreakdownRow("thinking", categoryStateThinking(rec)),
		renderBreakdownRow("images", categoryStateImages(rec)),
		renderBreakdownRow("tools", categoryStateTools(rec)),
		renderBreakdownRow("chat", categoryStateChat(rec)),
	}
	if p.completed && p.finalRes != nil {
		after := p.finalStatic + p.finalRes.FinalTail + p.finalReserved
		reduction := 0
		if p.finalRes.BaselineTail > 0 {
			reduction = int(float64(p.finalRes.BaselineTail-p.finalRes.FinalTail) / float64(p.finalRes.BaselineTail) * 100)
		}
		lines = append(lines,
			"  "+styleMuted.Render(strings.Repeat("─", 62)),
			"  "+styleTitle.Render("result"),
			renderPaneRow("message tokens", styleVal.Render(fmt.Sprintf("%s -> %s", humanInt(p.finalRes.BaselineTail), humanInt(p.finalRes.FinalTail)))),
			renderPaneRow("total reduction", styleVal.Render(fmt.Sprintf("%d%%", reduction))),
			renderPaneRow("final total", styleNum.Render(humanInt(after))),
			renderPaneRow("target limit", styleNum.Render(humanInt(p.target))),
			renderPaneRow("over/under target", deltaTextFriendly(after-p.target)),
		)
		if p.finalApplied {
			lines = append(lines, renderPaneRow("transcript", styleMuted.Render(p.finalPath)))
		} else {
			lines = append(lines, "  "+styleMuted.Render("note: nothing was written. pass --apply to mutate."))
		}
	}
	if len(step) > 0 && !p.completed {
		lines = append(lines, "  "+styleMuted.Render("detail: "+step))
	}
	return lines
}

// deltaText formats a ctx-to-target delta for inline display. The
// target is a floor: a result at or above target is valid, and a
// result under target means the planner trimmed more than asked, so
// only the under-target case is colored bad.
func deltaText(d int) string {
	if d < 0 {
		return styleBad.Render(fmt.Sprintf("-%s under target", humanInt(-d)))
	}
	return styleGood.Render(fmt.Sprintf("+%s above target", humanInt(d)))
}

func deltaTextFriendly(d int) string {
	if d < 0 {
		return styleBad.Render(humanInt(-d) + " under target (over-trimmed)")
	}
	if d > 0 {
		return styleGood.Render("above target by " + humanInt(d))
	}
	return styleGood.Render("on target")
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
		return styleGood.Render("dropped")
	}
	return styleMuted.Render("kept")
}

func categoryStateImages(r compactengine.IterationRecord) string {
	if r.ImagesPlaceholder {
		return styleGood.Render("placeholdered")
	}
	return styleMuted.Render("kept")
}

func categoryStateTools(r compactengine.IterationRecord) string {
	total := r.ToolsFull + r.ToolsLineOnly + r.ToolsDropped
	if total == 0 {
		return styleMuted.Render("none")
	}
	// Stacked bar: full = muted, line-only = warn, dropped = bad.
	bar := stackedBar(20, []segment{
		{count: r.ToolsFull, style: styleMuted, char: "█"},
		{count: r.ToolsLineOnly, style: styleWarn, char: "█"},
		{count: r.ToolsDropped, style: styleBad, char: "█"},
	}, total)
	return fmt.Sprintf("%s  %d full · %d line-only · %d dropped",
		bar, r.ToolsFull, r.ToolsLineOnly, r.ToolsDropped)
}

func categoryStateChat(r compactengine.IterationRecord) string {
	if r.ChatTurnsTotal == 0 {
		return styleMuted.Render("none")
	}
	kept := r.ChatTurnsTotal - r.ChatTurnsDropped
	bar := stackedBar(20, []segment{
		{count: kept, style: styleMuted, char: "█"},
		{count: r.ChatTurnsDropped, style: styleBad, char: "█"},
	}, r.ChatTurnsTotal)
	return fmt.Sprintf("%s  %d kept · %d dropped  %s",
		bar, kept, r.ChatTurnsDropped,
		styleMuted.Render(fmt.Sprintf("of %d", r.ChatTurnsTotal)))
}

// renderBreakdownRow formats a label + value row with consistent
// indentation so the live dashboard aligns cleanly.
func renderBreakdownRow(label, value string) string {
	return renderPaneRow(label, value)
}

func renderPaneRow(label, value string) string {
	return "  " + stylePaneKey.Render(label) + " " + value
}

// segment is one colored slice of a stacked bar.
type segment struct {
	count int
	style lipgloss.Style
	char  string
}

// stackedBar renders a width-cell bar divided among segments in
// proportion to their count of total. Cell widths round so the total
// never exceeds width. Zero-count segments contribute no cells.
func stackedBar(width int, segs []segment, total int) string {
	if total <= 0 || width <= 0 {
		return strings.Repeat(" ", width)
	}
	var cells []int
	remaining := width
	for i, s := range segs {
		if i == len(segs)-1 {
			cells = append(cells, remaining)
			break
		}
		c := s.count * width / total
		c = min(c, remaining)
		cells = append(cells, c)
		remaining -= c
	}
	var b strings.Builder
	for i, s := range segs {
		if cells[i] <= 0 {
			continue
		}
		b.WriteString(s.style.Render(strings.Repeat(s.char, cells[i])))
	}
	return b.String()
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

// RenderFinalPreview draws the phase-3 result box for a preview run
// with the full what-was-lopped-off breakdown.
func RenderFinalPreview(w io.Writer, res *compactengine.PlanResult, target, static, reserved int) {
	before := static + res.BaselineTail + reserved
	after := static + res.FinalTail + reserved
	reduction := 0
	if res.BaselineTail > 0 {
		reduction = int(float64(res.BaselineTail-res.FinalTail) / float64(res.BaselineTail) * 100)
	}

	var verdict string
	if res.HitTarget {
		verdict = styleGood.Render("✓ reached target")
	} else {
		verdict = styleWarn.Render("floor above target (kept minimum)")
	}

	rows := []string{
		styleTitle.Render("result") + "   " + ribbon(ModePreview) + "   " + verdict,
		"",
		kv("tail", fmt.Sprintf("%s → %s   %s",
			humanInt(res.BaselineTail),
			humanInt(res.FinalTail),
			styleMuted.Render(fmt.Sprintf("(%d%% reduction)", reduction)))),
		kv("ctx total", fmt.Sprintf("%s → %s", humanInt(before), humanInt(after))),
		kv("target", fmt.Sprintf("%s   %s",
			humanInt(target),
			styleMuted.Render(fmt.Sprintf("(%s above target)", humanInt(after-target))))),
		kv("iterations", strconv.Itoa(len(res.Iterations))),
		"",
		styleTitle.Render("what was stripped"),
	}
	rows = append(rows, finalBreakdownRows(finalRecord(res))...)
	rows = append(rows,
		"",
		styleMuted.Render("nothing was written. pass --apply to mutate."),
	)
	fmt.Fprintln(w, stylePreviewBox.Render(strings.Join(rows, "\n")))
}

// RenderFinalApply draws the phase-3 result box for an applied run.
// The box is red-bordered and explicit about the transcript mutation.
func RenderFinalApply(w io.Writer, res *compactengine.PlanResult, target, static, reserved int, transcriptPath string) {
	before := static + res.BaselineTail + reserved
	after := static + res.FinalTail + reserved
	reduction := 0
	if res.BaselineTail > 0 {
		reduction = int(float64(res.BaselineTail-res.FinalTail) / float64(res.BaselineTail) * 100)
	}

	verdict := styleWarn.Render("✓ APPLIED · transcript mutated")
	if !res.HitTarget {
		verdict = styleWarn.Render("✓ APPLIED · transcript mutated · floor above target")
	}

	rows := []string{
		styleTitle.Render("result") + "   " + ribbon(ModeApply) + "   " + verdict,
		"",
		kv("tail", fmt.Sprintf("%s → %s   %s",
			humanInt(res.BaselineTail),
			humanInt(res.FinalTail),
			styleMuted.Render(fmt.Sprintf("(%d%% reduction)", reduction)))),
		kv("ctx total", fmt.Sprintf("%s → %s", humanInt(before), humanInt(after))),
		kv("target", humanInt(target)),
		kv("transcript", styleMuted.Render(transcriptPath)),
		"",
		styleTitle.Render("what was stripped"),
	}
	rows = append(rows, finalBreakdownRows(finalRecord(res))...)
	rows = append(rows,
		"",
		styleMuted.Render("to revert: clyde compact <session> --undo"),
	)
	fmt.Fprintln(w, styleApplyBox.Render(strings.Join(rows, "\n")))
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
