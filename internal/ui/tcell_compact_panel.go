package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
)

type CompactRunRequest struct {
	SessionName    string
	TargetTokens   int
	ReservedTokens int
	Model          string
	ModelExplicit  bool
	Thinking       bool
	Images         bool
	Tools          bool
	Chat           bool
	Summarize      bool
	SummarizeMode  string
	Force          bool
}

type CompactEvent struct {
	Kind          string
	Message       string
	Upfront       *CompactUpfront
	Iteration     *CompactIteration
	Final         *CompactFinal
	ApplyMutation *CompactApplyMutation
}

type CompactUpfront struct {
	SessionName    string
	SessionID      string
	Model          string
	CurrentTotal   int
	MaxTokens      int
	MessagesTokens int
	BufferTokens   int
	OverheadTokens int
	FreeTokens     int
	TargetTokens   int
	ReservedTokens int
	ThinkingBlocks int
	ImageBlocks    int
	ToolPairs      int
	ChatTurns      int
}

type CompactIteration struct {
	Iteration int
	Step      string
	CtxTotal  int
	Delta     int
}

type CompactFinal struct {
	FinalTail      int
	TargetTokens   int
	StaticFloor    int
	ReservedTokens int
}

type CompactApplyMutation struct {
	BoundaryUUID  string
	SyntheticUUID string
	SnapshotPath  string
	LedgerPath    string
}

type CompactUndoResult struct {
	AppliedAt     string
	BoundaryUUID  string
	SyntheticUUID string
}

type CompactPanel struct {
	Rect Rect

	sessionName string
	sessionID   string
	model       string

	targetTokens  int
	maxTokens     int
	currentTotal  int
	messagesTok   int
	bufferTok     int
	overheadTok   int
	freeTok       int
	contextStatus string
	reserved      int
	targetText    string

	thinking       bool
	images         bool
	tools          bool
	chat           bool
	summary        string
	thinkingBlocks int
	imageBlocks    int
	toolPairs      int
	chatTurns      int
	countsStatus   string

	focusGroup   int
	checkboxIdx  int
	actionIdx    int
	confirmApply bool

	busy       bool
	busyAction string
	status     string
	logRect    Rect
	logScroll  int
	logLines   []string

	iterationHistory []CompactIteration
	latestIteration  *CompactIteration
	latestFinal      *CompactFinal
	latestUndo       *CompactUndoResult

	OnPreview func(CompactRunRequest)
	OnApply   func(CompactRunRequest)
	OnUndo    func()
	OnClose   func()
}

func NewCompactPanel(sessionName string) *CompactPanel {
	return &CompactPanel{
		Rect:             Rect{},
		sessionName:      sessionName,
		sessionID:        "",
		model:            "",
		targetTokens:     200000,
		maxTokens:        1000000,
		currentTotal:     0,
		messagesTok:      0,
		bufferTok:        0,
		overheadTok:      0,
		freeTok:          0,
		contextStatus:    "",
		reserved:         13000,
		targetText:       "200000",
		thinking:         true,
		images:           true,
		tools:            true,
		chat:             true,
		summary:          "auto",
		thinkingBlocks:   0,
		imageBlocks:      0,
		toolPairs:        0,
		chatTurns:        0,
		countsStatus:     "preview required",
		focusGroup:       0,
		checkboxIdx:      0,
		actionIdx:        0,
		confirmApply:     false,
		busy:             false,
		busyAction:       "",
		status:           "adjust controls and run preview",
		logRect:          Rect{},
		logScroll:        0,
		logLines:         nil,
		iterationHistory: nil,
		latestIteration:  nil,
		latestFinal:      nil,
		latestUndo:       nil,
		OnPreview:        nil,
		OnApply:          nil,
		OnUndo:           nil,
		OnClose:          nil,
	}
}

func (p *CompactPanel) ApplyCompactEvent(ev CompactEvent) {
	switch ev.Kind {
	case "upfront":
		if ev.Upfront != nil {
			p.iterationHistory = nil
			p.logLines = nil
			p.logScroll = 0
			p.latestIteration = nil
			p.latestFinal = nil
			p.latestUndo = nil
			p.sessionName = ev.Upfront.SessionName
			p.sessionID = ev.Upfront.SessionID
			p.model = ev.Upfront.Model
			p.currentTotal = ev.Upfront.CurrentTotal
			p.messagesTok = ev.Upfront.MessagesTokens
			p.bufferTok = ev.Upfront.BufferTokens
			p.overheadTok = ev.Upfront.OverheadTokens
			p.freeTok = ev.Upfront.FreeTokens
			p.contextStatus = ""
			p.thinkingBlocks = ev.Upfront.ThinkingBlocks
			p.imageBlocks = ev.Upfront.ImageBlocks
			p.toolPairs = ev.Upfront.ToolPairs
			p.chatTurns = ev.Upfront.ChatTurns
			p.countsStatus = ""
			if ev.Upfront.MaxTokens > 0 {
				p.maxTokens = ev.Upfront.MaxTokens
			}
			if ev.Upfront.TargetTokens > 0 {
				p.targetTokens = ev.Upfront.TargetTokens
				p.targetText = strconv.Itoa(ev.Upfront.TargetTokens)
			}
		}
	case "iteration":
		if ev.Iteration != nil {
			p.latestIteration = ev.Iteration
			p.iterationHistory = append(p.iterationHistory, *ev.Iteration)
			p.logLines = append(p.logLines, p.renderIterationLine(*ev.Iteration))
			p.clampLogScroll()
		}
	case "final":
		p.latestFinal = ev.Final
		if ev.Final != nil {
			p.logLines = append(p.logLines, p.renderFinalLine(*ev.Final))
		}
		p.clampLogScroll()
	case "status":
		if ev.Message != "" {
			p.status = ev.Message
		}
	}
}

func (p *CompactPanel) setInitialContextUsage(usage SessionContextUsage, loaded bool, status string) {
	if usage.Model != "" {
		p.model = usage.Model
	}
	if usage.MaxTokens > 0 {
		p.maxTokens = usage.MaxTokens
	}
	if usage.TotalTokens > 0 {
		p.currentTotal = usage.TotalTokens
	}
	if usage.MessagesTokens > 0 {
		p.messagesTok = usage.MessagesTokens
	}
	if loaded {
		p.contextStatus = ""
		return
	}
	if status != "" {
		p.contextStatus = status
	}
}

func (p *CompactPanel) SetBusy(action string, busy bool) {
	p.busy = busy
	p.busyAction = action
	if busy {
		p.status = action + " in progress..."
	}
}

func (p *CompactPanel) SetUndoResult(res *CompactUndoResult, err error) {
	if err != nil {
		p.status = fmt.Sprintf("undo failed: %v", err)
		return
	}
	p.latestUndo = res
	p.status = "undo completed"
	if res != nil {
		p.logLines = append(p.logLines, "last undo at "+res.AppliedAt)
		p.clampLogScroll()
	}
}

func (p *CompactPanel) Draw(scr tcell.Screen, r Rect) {
	p.Rect = r
	dimBackground(scr)
	box := Rect{X: r.X + 2, Y: r.Y + 1, W: r.W - 4, H: r.H - 2}
	clearRect(scr, box)
	drawBoxBorder(scr, box, ColorBorder)

	inner := Rect{X: box.X + 2, Y: box.Y + 1, W: box.W - 4, H: box.H - 2}
	compactLayout := inner.H < 24
	y := inner.Y
	drawString(scr, inner.X, y, StyleHeader, "Compact (Interactive)", inner.W)
	y++
	drawString(scr, inner.X, y, StyleMuted, fmt.Sprintf("session %s  model %s", p.valueOrDash(p.sessionName), p.valueOrDash(p.model)), inner.W)
	if compactLayout {
		y++
	} else {
		y += 2
	}
	if !compactLayout {
		y = p.drawContextSummary(scr, inner, y)
		y++
	}

	drawString(scr, inner.X, y, StyleHeader, "Target", inner.W)
	y++
	drawString(scr, inner.X, y, p.focusStyle(0), "slider "+p.renderSlider(20)+" "+p.percentLabel(), inner.W)
	y++
	drawString(scr, inner.X, y, p.focusStyle(1), "target tokens ["+p.targetText+"]", inner.W)
	y++
	drawString(scr, inner.X, y, p.focusStyle(2), p.renderChecks(), inner.W)
	y++
	if !compactLayout {
		drawString(scr, inner.X, y, StyleMuted, p.summaryHelpText(inner.W), inner.W)
		y += 2
	}

	if compactLayout {
		drawString(scr, inner.X, y, StyleMuted, "status: "+p.status, inner.W)
		y++
	} else {
		drawString(scr, inner.X, y, StyleHeader, "Live Status", inner.W)
		y++
		drawString(scr, inner.X, y, StyleMuted, "status: "+p.status, inner.W)
		y += 2
	}

	availableProgressH := imax(0, inner.Y+inner.H-y-4)
	progressH := imin(availableProgressH, 9)
	p.drawProgressLog(scr, Rect{X: inner.X, Y: y, W: inner.W, H: progressH})

	y += progressH + 1
	drawString(scr, inner.X, y, StyleHeader, "Actions", inner.W)
	y++
	p.drawActionButtons(scr, inner.X, y, inner.W)
}

func (p *CompactPanel) StatusLegendActions() []LegendAction {
	return []LegendAction{
		LegendFocus,
		LegendAdjust,
		LegendSelect,
		LegendClose,
	}
}

func (p *CompactPanel) HandleEvent(ev tcell.Event) bool {
	switch e := ev.(type) {
	case *tcell.EventMouse:
		x, y := e.Position()
		if !p.Rect.Contains(x, y) {
			return false
		}
		if p.logRect.Contains(x, y) {
			if e.Buttons()&tcell.WheelUp != 0 {
				p.scrollLog(-3)
				return true
			}
			if e.Buttons()&tcell.WheelDown != 0 {
				p.scrollLog(3)
				return true
			}
		}
		return p.Rect.Contains(x, y)
	case *tcell.EventKey:
		if e.Key() == tcell.KeyEscape || (e.Key() == tcell.KeyRune && (e.Rune() == 'q' || e.Rune() == 'Q')) {
			if p.OnClose != nil {
				p.OnClose()
			}
			return true
		}
		if p.busy {
			return true
		}
		if e.Key() == tcell.KeyRune {
			switch e.Rune() {
			case 'p', 'P':
				p.triggerAction(0)
				return true
			case 'a', 'A':
				p.focusGroup = 3
				p.actionIdx = 1
				p.armApplyConfirmation()
				return true
			case 'u', 'U':
				p.triggerAction(2)
				return true
			}
		}
		switch e.Key() {
		case tcell.KeyUp:
			p.focusGroup = (p.focusGroup + 3) % 4
			p.clearApplyConfirmation()
			return true
		case tcell.KeyDown:
			p.focusGroup = (p.focusGroup + 1) % 4
			p.clearApplyConfirmation()
			return true
		}
		switch p.focusGroup {
		case 0:
			return p.handleSliderKeys(e)
		case 1:
			return p.handleTargetInputKeys(e)
		case 2:
			return p.handleCheckKeys(e)
		case 3:
			return p.handleActionKeys(e)
		}
	}
	return false
}

func (p *CompactPanel) handleSliderKeys(e *tcell.EventKey) bool {
	p.clearApplyConfirmation()
	switch e.Key() {
	case tcell.KeyLeft:
		p.adjustTargetByPercent(1)
		return true
	case tcell.KeyRight:
		p.adjustTargetByPercent(-1)
		return true
	case tcell.KeyPgUp:
		p.adjustTargetByPercent(5)
		return true
	case tcell.KeyPgDn:
		p.adjustTargetByPercent(-5)
		return true
	case tcell.KeyEnter:
		p.adjustTargetByPercent(1)
		return true
	}
	if e.Key() == tcell.KeyRune && e.Rune() == ' ' {
		p.adjustTargetByPercent(-1)
		return true
	}
	return false
}

func (p *CompactPanel) handleTargetInputKeys(e *tcell.EventKey) bool {
	p.clearApplyConfirmation()
	switch e.Key() {
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(p.targetText) > 0 {
			p.targetText = p.targetText[:len(p.targetText)-1]
		}
		p.updateTargetFromText()
		return true
	case tcell.KeyEnter:
		p.updateTargetFromText()
		return true
	case tcell.KeyRune:
		if e.Rune() >= '0' && e.Rune() <= '9' {
			p.targetText += string(e.Rune())
			p.updateTargetFromText()
			return true
		}
	}
	if e.Key() == tcell.KeyRune && e.Rune() == ' ' {
		p.updateTargetFromText()
		return true
	}
	return false
}

func (p *CompactPanel) handleCheckKeys(e *tcell.EventKey) bool {
	p.clearApplyConfirmation()
	switch e.Key() {
	case tcell.KeyLeft:
		p.checkboxIdx = (p.checkboxIdx + 4) % 5
		return true
	case tcell.KeyRight:
		p.checkboxIdx = (p.checkboxIdx + 1) % 5
		return true
	case tcell.KeyEnter:
		p.toggleCheckbox(p.checkboxIdx)
		return true
	case tcell.KeyRune:
		if e.Rune() == ' ' {
			p.toggleCheckbox(p.checkboxIdx)
			return true
		}
	}
	return false
}

func (p *CompactPanel) handleActionKeys(e *tcell.EventKey) bool {
	switch e.Key() {
	case tcell.KeyLeft:
		p.actionIdx = (p.actionIdx + 3) % 4
		if p.actionIdx != 1 {
			p.clearApplyConfirmation()
		}
		return true
	case tcell.KeyRight:
		p.actionIdx = (p.actionIdx + 1) % 4
		if p.actionIdx != 1 {
			p.clearApplyConfirmation()
		}
		return true
	case tcell.KeyEnter:
		p.triggerAction(p.actionIdx)
		return true
	}
	if e.Key() == tcell.KeyRune && e.Rune() == ' ' {
		p.triggerAction(p.actionIdx)
		return true
	}
	return false
}

func (p *CompactPanel) toggleCheckbox(idx int) {
	switch idx {
	case 0:
		p.thinking = !p.thinking
	case 1:
		p.images = !p.images
	case 2:
		p.tools = !p.tools
	case 3:
		p.chat = !p.chat
	case 4:
		p.cycleSummaryMode()
	}
}

func (p *CompactPanel) cycleSummaryMode() {
	switch p.summary {
	case "auto":
		p.summary = "on"
	case "on":
		p.summary = "off"
	default:
		p.summary = "auto"
	}
}

func (p *CompactPanel) triggerAction(idx int) {
	switch idx {
	case 0:
		p.clearApplyConfirmation()
		if p.OnPreview != nil {
			p.OnPreview(p.buildRequest())
		}
	case 1:
		if !p.confirmApply {
			p.armApplyConfirmation()
			return
		}
		p.clearApplyConfirmation()
		if p.OnApply != nil {
			p.OnApply(p.buildRequest())
		}
	case 2:
		p.clearApplyConfirmation()
		if p.OnUndo != nil {
			p.OnUndo()
		}
	case 3:
		p.clearApplyConfirmation()
		if p.OnClose != nil {
			p.OnClose()
		}
	}
}

func (p *CompactPanel) armApplyConfirmation() {
	p.confirmApply = true
	p.status = "confirm apply: press Enter or Space on [Apply] again to mutate transcript"
}

func (p *CompactPanel) clearApplyConfirmation() {
	p.confirmApply = false
}

func (p *CompactPanel) buildRequest() CompactRunRequest {
	return CompactRunRequest{
		SessionName:    p.sessionName,
		TargetTokens:   p.targetTokens,
		ReservedTokens: p.reserved,
		Model:          p.model,
		ModelExplicit:  false,
		Thinking:       p.thinking,
		Images:         p.images,
		Tools:          p.tools,
		Chat:           p.chat,
		Summarize:      p.summary == "on",
		SummarizeMode:  p.summary,
		Force:          false,
	}
}

func (p *CompactPanel) focusStyle(group int) tcell.Style {
	if p.focusGroup == group {
		return StyleSelected
	}
	return StyleDefault
}

func (p *CompactPanel) renderChecks() string {
	check := func(name string, on bool, idx int) string {
		return renderCheckItem(name, on, p.focusGroup == 2 && p.checkboxIdx == idx)
	}
	return check("thinking", p.thinking, 0) + "  " +
		check("images", p.images, 1) + "  " +
		check("tools", p.tools, 2) + "  " +
		check("chat", p.chat, 3) + "  " +
		p.renderSummaryControl()
}

func (p *CompactPanel) renderSummaryControl() string {
	text := "summary [" + p.summary + "]"
	if p.focusGroup == 2 && p.checkboxIdx == 4 {
		return "[" + text + "]"
	}
	return " " + text + " "
}

func (p *CompactPanel) drawContextSummary(scr tcell.Screen, inner Rect, y int) int {
	drawString(scr, inner.X, y, StyleHeader, "Context (/context)", inner.W)
	y++
	if inner.W >= 82 {
		return p.drawWideContextSummary(scr, inner, y)
	}
	return p.drawNarrowContextSummary(scr, inner, y)
}

func (p *CompactPanel) drawWideContextSummary(scr tcell.Screen, inner Rect, y int) int {
	leftW := imax(36, inner.W/2-2)
	rightX := inner.X + leftW + 4
	rightW := imax(0, inner.W-(rightX-inner.X))
	leftRows := []string{
		p.contextCurrentLine(false),
		p.contextTokenLine("messages", p.messagesTok),
		p.contextTokenLine("buffer", p.bufferTok),
		p.contextTokenLine("overhead", p.overheadTok),
		p.contextTokenLine("free", p.freeTok),
	}
	rightRows := []string{
		p.contextCountLine("chat turns", p.chatTurns),
		p.contextCountLine("tool pairs", p.toolPairs),
		p.contextCountLine("images", p.imageBlocks),
		p.contextCountLine("thinking blocks", p.thinkingBlocks),
	}
	for i, row := range leftRows {
		drawString(scr, inner.X, y+i, StyleDefault, row, leftW)
		if i < len(rightRows) {
			drawString(scr, rightX, y+i, StyleDefault, rightRows[i], rightW)
		}
	}
	return y + len(leftRows)
}

func (p *CompactPanel) drawNarrowContextSummary(scr tcell.Screen, inner Rect, y int) int {
	rows := []string{
		p.contextCurrentLine(true),
		p.contextTokenLine("messages", p.messagesTok),
		p.contextTokenLine("buffer", p.bufferTok),
		p.contextTokenLine("overhead", p.overheadTok),
		p.contextTokenLine("free", p.freeTok),
		p.contextCountLine("chat turns", p.chatTurns),
		p.contextCountLine("tool pairs", p.toolPairs),
		p.contextCountLine("images", p.imageBlocks),
		p.contextCountLine("thinking blocks", p.thinkingBlocks),
	}
	for i, row := range rows {
		drawString(scr, inner.X, y+i, StyleDefault, row, inner.W)
	}
	return y + len(rows)
}

func (p *CompactPanel) contextCurrentLine(narrow bool) string {
	if p.currentTotal <= 0 {
		return fmt.Sprintf("%-10s %s", "current", p.placeholder(p.contextStatus))
	}
	value := formatWithCommas(p.currentTotal)
	if p.maxTokens > 0 {
		value += " / " + formatWithCommas(p.maxTokens)
	}
	if p.contextPercent() > 0 && !narrow {
		value += fmt.Sprintf("   %d%%", p.contextPercent())
	}
	return fmt.Sprintf("%-10s %s", "current", value)
}

func (p *CompactPanel) contextTokenLine(label string, tokens int) string {
	if tokens <= 0 {
		return fmt.Sprintf("%-10s %s", label, p.placeholder(p.contextStatus))
	}
	return fmt.Sprintf("%-10s %s", label, formatWithCommas(tokens))
}

func (p *CompactPanel) contextCountLine(label string, count int) string {
	if count <= 0 {
		return fmt.Sprintf("%-15s %s", label, p.placeholder(p.countsStatus))
	}
	return fmt.Sprintf("%-15s %s", label, formatWithCommas(count))
}

func (p *CompactPanel) placeholder(status string) string {
	if status == "" {
		return "-"
	}
	return status
}

func (p *CompactPanel) contextPercent() int {
	if p.currentTotal <= 0 || p.maxTokens <= 0 {
		return 0
	}
	return p.currentTotal * 100 / p.maxTokens
}

func (p *CompactPanel) summaryHelpText(width int) string {
	switch p.summary {
	case "on":
		if width < 72 {
			return "summary on: recap any dropped content"
		}
		return "summary on: recap any dropped content before applying"
	case "off":
		return "summary off: apply without generating a recap"
	default:
		if width < 72 {
			return "summary auto: recap when chat/prior summaries drop"
		}
		return "summary auto: recap only when chat turns or prior summaries are dropped"
	}
}

func (p *CompactPanel) drawActionButtons(scr tcell.Screen, x, y, maxW int) {
	labels := []string{"Preview", "Apply", "Undo", "Close"}
	cursor := x
	for i, label := range labels {
		if i == 1 && p.confirmApply {
			label = "Apply (confirm)"
		}
		text := "[ " + label + " ]"
		width := cellCount(text)
		if cursor > x {
			if cursor-x+1 >= maxW {
				break
			}
			drawString(scr, cursor, y, StyleDefault, " ", maxW-(cursor-x))
			cursor++
		}
		if cursor-x+width > maxW {
			break
		}
		style := StyleDefault
		if p.busy {
			style = StyleMuted.Dim(true)
		} else if p.focusGroup == 3 && p.actionIdx == i {
			style = StyleSelected
			fillRow(scr, cursor, y, width, style)
		}
		drawString(scr, cursor, y, style, text, maxW-(cursor-x))
		cursor += width
	}
}

func (p *CompactPanel) drawProgressLog(scr tcell.Screen, r Rect) {
	p.logRect = Rect{}
	if r.W <= 2 || r.H <= 2 {
		return
	}
	clearRect(scr, r)
	drawBoxBorder(scr, r, ColorBorder)
	drawString(scr, r.X+2, r.Y, StyleHeader, " Progress ", imax(0, r.W-4))

	content := Rect{X: r.X + 2, Y: r.Y + 1, W: r.W - 4, H: r.H - 2}
	p.logRect = content
	if content.W <= 0 || content.H <= 0 {
		return
	}
	lines := p.logLines
	busyLineCount := 0
	if p.busy {
		busyLineCount = 1
	}
	scrollLineCount := len(lines) + busyLineCount
	p.logScroll = clamp(p.logScroll, 0, imax(0, scrollLineCount-content.H))
	if len(lines) == 0 {
		if p.busy {
			ClockLoadingSpinner(p.busyAction+" in progress...").Draw(scr, content.X, content.Y, content.W)
			return
		}
		drawString(scr, content.X, content.Y, StyleMuted, "No run yet. Press Preview to inspect or Apply to mutate.", content.W)
		return
	}
	start := imax(0, scrollLineCount-content.H-p.logScroll)
	end := imin(scrollLineCount, start+content.H)
	contentW := content.W
	if scrollLineCount > content.H {
		contentW = imax(0, content.W-1)
		drawScrollbar(scr, content.X+content.W-1, content.Y, content.H, content.H, scrollLineCount, start)
	}
	y := content.Y
	for i := start; i < end; i++ {
		if i < len(lines) {
			drawString(scr, content.X, y, StyleDefault, lines[i], contentW)
			y++
			continue
		}
		ClockLoadingSpinner(p.busyAction+" in progress...").Draw(scr, content.X, y, contentW)
		y++
	}
}

func (p *CompactPanel) renderIterationLine(iter CompactIteration) string {
	return fmt.Sprintf(
		"iter %d  %s  projected %s  %s",
		iter.Iteration,
		iter.Step,
		formatWithCommas(iter.CtxTotal),
		formatSignedWithCommas(iter.Delta),
	)
}

func (p *CompactPanel) renderFinalLine(final CompactFinal) string {
	total := final.StaticFloor + final.ReservedTokens + final.FinalTail
	return fmt.Sprintf(
		"final projected %s  target %s",
		formatWithCommas(total),
		formatWithCommas(final.TargetTokens),
	)
}

func (p *CompactPanel) scrollLog(delta int) {
	maxScroll := imax(0, len(p.logLines)-p.logRect.H)
	p.logScroll = clamp(p.logScroll-delta, 0, maxScroll)
}

func (p *CompactPanel) clampLogScroll() {
	p.logScroll = clamp(p.logScroll, 0, imax(0, len(p.logLines)-p.logRect.H))
}

func (p *CompactPanel) renderSlider(width int) string {
	if width < 4 {
		width = 4
	}
	fill := min(max(p.compactedPercent()*width/100, 0), width)
	var out strings.Builder
	out.WriteString("|")
	for i := range width {
		if i < fill {
			out.WriteString("=")
			continue
		}
		out.WriteString("-")
	}
	return out.String() + "|"
}

func (p *CompactPanel) percent() int {
	if p.maxTokens <= 0 {
		return 0
	}
	return (p.targetTokens * 100) / p.maxTokens
}

func (p *CompactPanel) compactedPercent() int {
	return 100 - p.percent()
}

func (p *CompactPanel) percentLabel() string {
	return fmt.Sprintf(
		"%d%% (%s/%s)",
		p.percent(),
		formatWithCommas(p.targetTokens),
		formatWithCommas(p.maxTokens),
	)
}

func (p *CompactPanel) adjustTargetByPercent(delta int) {
	next := p.percent() + delta
	next = clamp(next, 1, 100)
	p.targetTokens = max(p.maxTokens*next/100, 1)
	p.targetText = strconv.Itoa(p.targetTokens)
}

func (p *CompactPanel) updateTargetFromText() {
	if p.targetText == "" {
		return
	}
	v, err := strconv.Atoi(p.targetText)
	if err != nil {
		return
	}
	if v < 1 {
		v = 1
	}
	if p.maxTokens > 0 && v > p.maxTokens {
		v = p.maxTokens
	}
	p.targetTokens = v
	p.targetText = strconv.Itoa(v)
}

func (p *CompactPanel) valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatWithCommas(value int) string {
	if value == 0 {
		return "0"
	}
	isNegative := value < 0
	if isNegative {
		value = -value
	}
	digits := strconv.Itoa(value)
	var formatted strings.Builder
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			formatted.WriteString(",")
		}
		formatted.WriteString(string(digits[i]))
	}
	if isNegative {
		return "-" + formatted.String()
	}
	return formatted.String()
}

func formatSignedWithCommas(value int) string {
	if value > 0 {
		return "+" + formatWithCommas(value)
	}
	return formatWithCommas(value)
}
