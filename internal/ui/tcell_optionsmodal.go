package ui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

const optionsModalHintMinGap = 3

// OptionsModalEntry is one selectable row in the options popup.
// Hint is the short description rendered in dim text on the right.
// Disabled entries draw muted and ignore activation.
type OptionsModalEntry struct {
	Label    string
	Hint     string
	Action   func()
	Disabled bool
}

type OptionsModalContext string

const (
	OptionsModalContextSession OptionsModalContext = "session"
	OptionsModalContextReturn  OptionsModalContext = "return"
)

// OptionsModal is the popup that appears when the user presses Enter on
// a session row. It replaces the implicit "Enter resumes" behavior with
// an explicit menu so destructive actions like delete are not one
// keystroke away from the most common navigation key.
type OptionsModal struct {
	Title      string
	Context    OptionsModalContext
	TopEntries []OptionsModalEntry
	Entries    []OptionsModalEntry
	// StatsSegments, when provided, renders a reusable session stats
	// pane on the left and keeps the options list on the right.
	StatsSegments    [][]TextSegment
	StatsLoading     bool
	StatsSessionName string

	cursor int
	rect   Rect
	// entryRects mirrors the currently visible row hit areas, in logical
	// index order across TopEntries followed by Entries.
	entryRects  []Rect
	statsRect   Rect
	optionsRect Rect
	// optionsOffset scrolls logical rows in the options pane.
	optionsOffset int
	// optionsScrollbarRect is the last drawn scrollbar region for options.
	optionsScrollbarRect Rect
	// optionsTotalRows tracks logical option rows including the separator gap.
	optionsTotalRows int
	// optionsVisibleRows tracks visible rows in the options pane.
	optionsVisibleRows int
	statsBox           *TextBox
	grab               optionsModalGrab

	// OnCancel fires when the user dismisses with Esc or q. The dialog removes
	// itself on activation already.
	OnCancel func()
	// OnQuit is optional. When set, q triggers OnQuit instead of OnCancel.
	OnQuit func()
}

func (m *OptionsModal) StatusLegendActions() []LegendAction {
	actions := []LegendAction{LegendMove, LegendClose}
	if m.Context == OptionsModalContextReturn {
		actions = append(actions, LegendQuit)
	}
	return actions
}

type optionsModalGrab int

const (
	optionsModalGrabNone optionsModalGrab = iota
	optionsModalGrabStats
	optionsModalGrabOptions
)

// NewOptionsModal builds a modal with the cursor on the first enabled
// entry. Entries with no action set are treated as disabled.
func NewOptionsModal(title string, entries []OptionsModalEntry) *OptionsModal {
	m := &OptionsModal{Title: title, Entries: entries, Context: OptionsModalContextSession}
	m.resetCursor()
	return m
}

func (m *OptionsModal) Draw(scr tcell.Screen, r Rect) {
	if r.W <= 2 || r.H <= 2 {
		return
	}
	w := min(r.W-4, 110)
	if w < 28 {
		w = r.W - 2
	}
	if w < 1 {
		w = 1
	}
	contentRows := m.totalEntries() + 4
	h := max(min(max(contentRows, 10), r.H-2), 1)
	box := Rect{X: r.X + (r.W-w)/2, Y: r.Y + (r.H-h)/2, W: w, H: h}
	m.rect = box

	clearRect(scr, box)
	drawBoxBorder(scr, box, ColorAccent)

	titleStyle := StyleDefault.Foreground(ColorAccent).Bold(true)
	if m.Title != "" {
		drawString(scr, box.X+2, box.Y, titleStyle, " "+m.Title+" ", box.W-4)
	}

	m.entryRects = make([]Rect, m.totalEntries())
	for i := range m.entryRects {
		m.entryRects[i] = Rect{}
	}
	m.statsRect = Rect{}
	m.optionsRect = Rect{}
	m.optionsScrollbarRect = Rect{}

	inner := Rect{X: box.X + 1, Y: box.Y + 1, W: box.W - 2, H: box.H - 2}
	if inner.W <= 0 || inner.H <= 0 {
		return
	}
	contentTop := inner.Y + 1
	contentH := max(inner.H-2, 1)

	showStats := len(m.StatsSegments) > 0
	layout := "options_only"
	if showStats && inner.W >= 78 && contentH >= 8 {
		layout = "split"
	} else if showStats && inner.W >= 44 && contentH >= 10 {
		layout = "stacked"
	}

	switch layout {
	case "split":
		statsW := max((inner.W-1)*55/100, 24)
		statsW = max(min(statsW, inner.W-20), 1)
		optionsX := inner.X + statsW + 1
		optionsW := max(inner.W-statsW-1, 1)
		for y := inner.Y; y < inner.Y+inner.H; y++ {
			scr.SetContent(optionsX-1, y, '│', nil, StyleMuted)
		}
		m.drawStatsPane(scr, Rect{X: inner.X, Y: contentTop, W: statsW, H: contentH})
		m.drawOptionsPane(scr, Rect{X: optionsX, Y: contentTop, W: optionsW, H: contentH})
	case "stacked":
		statsH := max(contentH*40/100, 4)
		statsH = max(min(statsH, contentH-4), 1)
		optionsH := max(contentH-statsH-1, 1)
		m.drawStatsPane(scr, Rect{X: inner.X, Y: contentTop, W: inner.W, H: statsH})
		dividerY := contentTop + statsH
		for x := inner.X; x < inner.X+inner.W; x++ {
			scr.SetContent(x, dividerY, '─', nil, StyleMuted)
		}
		m.drawOptionsPane(scr, Rect{X: inner.X, Y: dividerY + 1, W: inner.W, H: optionsH})
	default:
		m.drawOptionsPane(scr, Rect{X: inner.X, Y: contentTop, W: inner.W, H: contentH})
	}

	hint := "↑↓ navigate · enter select · esc cancel"
	if m.Context == OptionsModalContextReturn {
		hint = "↑↓ navigate · enter select · esc to list · q quit"
	}
	drawString(scr, inner.X, box.Y+box.H-1, StyleMuted, hint, inner.W)
}

// HandleEvent applies keyboard and mouse input to the modal.
func (m *OptionsModal) HandleEvent(ev tcell.Event) bool {
	switch e := ev.(type) {
	case *tcell.EventKey:
		return m.handleKey(e)
	case *tcell.EventMouse:
		return m.handleMouse(e)
	}
	return false
}

func (m *OptionsModal) handleMouse(e *tcell.EventMouse) bool {
	x, y := e.Position()
	btns := e.Buttons()
	if btns == 0 {
		m.grab = optionsModalGrabNone
	}
	if !m.rect.Contains(x, y) {
		m.grab = optionsModalGrabNone
		return true
	}
	if m.handleMouseGrab(y, btns) {
		return true
	}
	if m.handleMouseScrollbarStart(x, y, btns) {
		return true
	}
	if m.handleStatsPaneMouse(x, y, btns) {
		return true
	}
	if m.handleOptionsPaneMouse(x, y, btns) {
		return true
	}
	if btns&tcell.ButtonPrimary != 0 {
		m.activateMouseEntry(x, y)
	}
	return true
}

func (m *OptionsModal) handleMouseGrab(y int, btns tcell.ButtonMask) bool {
	if m.grab == optionsModalGrabNone || btns&tcell.Button1 == 0 {
		return false
	}
	switch m.grab {
	case optionsModalGrabStats:
		if m.statsBox != nil {
			m.statsBox.JumpToScrollbarY(y)
		}
	case optionsModalGrabOptions:
		m.jumpToOptionsScrollbarY(y)
	}
	return true
}

func (m *OptionsModal) handleMouseScrollbarStart(x, y int, btns tcell.ButtonMask) bool {
	if btns&tcell.Button1 == 0 {
		return false
	}
	if m.statsBox != nil && m.statsBox.ScrollbarRect.Contains(x, y) {
		m.grab = optionsModalGrabStats
		m.statsBox.JumpToScrollbarY(y)
		return true
	}
	if m.optionsScrollbarRect.Contains(x, y) {
		m.grab = optionsModalGrabOptions
		m.jumpToOptionsScrollbarY(y)
		return true
	}
	return false
}

func (m *OptionsModal) handleStatsPaneMouse(x, y int, btns tcell.ButtonMask) bool {
	if !m.statsRect.Contains(x, y) {
		return false
	}
	if btns&tcell.WheelUp != 0 && m.statsBox != nil {
		m.statsBox.Offset = imax(0, m.statsBox.Offset-3)
		return true
	}
	if btns&tcell.WheelDown != 0 && m.statsBox != nil {
		m.statsBox.Offset += 3
		return true
	}
	if btns&tcell.Button1 != 0 {
		return true
	}
	return false
}

func (m *OptionsModal) handleOptionsPaneMouse(x, y int, btns tcell.ButtonMask) bool {
	if !m.optionsRect.Contains(x, y) {
		return false
	}
	if btns&tcell.WheelUp != 0 {
		m.optionsOffset = imax(0, m.optionsOffset-3)
		return true
	}
	if btns&tcell.WheelDown != 0 {
		maxOff := imax(0, m.optionsTotalRows-m.optionsVisibleRows)
		m.optionsOffset = clamp(m.optionsOffset+3, 0, maxOff)
		return true
	}
	return false
}

func (m *OptionsModal) activateMouseEntry(x, y int) {
	for i, rowRect := range m.entryRects {
		if rowRect.Contains(x, y) {
			m.cursor = i
			m.activate()
			return
		}
	}
}

// handleKey routes navigation keys. Cursor movement stays local so
// the skip-disabled-entry loop in moveCursor is preserved; the
// remaining gestures (Enter / LF / Esc / q) delegate to the shared
// HandleMenuKey helper so the Enter-vs-LF terminal-mode bug cannot
// resurface here.
func (m *OptionsModal) handleKey(e *tcell.EventKey) bool {
	// Local navigation: respects the skip-disabled-entry walk.
	switch e.Key() {
	case tcell.KeyUp:
		m.moveCursor(-1)
		m.ensureCursorVisible()
		return true
	case tcell.KeyDown:
		m.moveCursor(+1)
		m.ensureCursorVisible()
		return true
	case tcell.KeyRune:
		switch e.Rune() {
		case 'j':
			m.moveCursor(+1)
			m.ensureCursorVisible()
			return true
		case 'k':
			m.moveCursor(-1)
			m.ensureCursorVisible()
			return true
		}
	}
	// Shared handler for Enter / LF / Esc / q. Pass a throwaway
	// cursor pointer because the helper would otherwise increment
	// our cursor on Up / Down (which we already handled above).
	dummy := m.cursor
	quitAction := m.OnCancel
	if m.OnQuit != nil {
		quitAction = m.OnQuit
	}
	return HandleMenuKey(e, &dummy, m.totalEntries(), MenuKeyOptions{
		OnActivate: func(int) { m.activate() },
		OnCancel:   m.OnCancel,
		OnQuit:     quitAction,
		EnableJK:   true,
	})
}

func (m *OptionsModal) moveCursor(delta int) {
	if m.totalEntries() == 0 {
		return
	}
	m.clampCursor()
	start := m.cursor
	for {
		m.cursor += delta
		if m.cursor < 0 {
			m.cursor = m.totalEntries() - 1
		}
		if m.cursor >= m.totalEntries() {
			m.cursor = 0
		}
		e := m.entryAt(m.cursor)
		if e != nil && !e.Disabled && e.Action != nil {
			return
		}
		if m.cursor == start {
			return
		}
	}
}

func (m *OptionsModal) activate() {
	m.clampCursor()
	if m.cursor < 0 || m.cursor >= m.totalEntries() {
		return
	}
	e := m.entryAt(m.cursor)
	if e == nil || e.Disabled || e.Action == nil {
		return
	}
	e.Action()
}

func (m *OptionsModal) totalEntries() int {
	return len(m.TopEntries) + len(m.Entries)
}

func (m *OptionsModal) entryAt(index int) *OptionsModalEntry {
	if index < 0 {
		return nil
	}
	if index < len(m.TopEntries) {
		return &m.TopEntries[index]
	}
	base := index - len(m.TopEntries)
	if base >= 0 && base < len(m.Entries) {
		return &m.Entries[base]
	}
	return nil
}

func (m *OptionsModal) clampCursor() {
	total := m.totalEntries()
	if total == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= total {
		m.cursor = total - 1
	}
}

func (m *OptionsModal) resetCursor() {
	m.cursor = 0
	total := m.totalEntries()
	for i := range total {
		entry := m.entryAt(i)
		if entry != nil && !entry.Disabled && entry.Action != nil {
			m.cursor = i
			return
		}
	}
}

func (m *OptionsModal) drawStatsPane(scr tcell.Screen, r Rect) {
	if r.W <= 0 || r.H <= 0 || len(m.StatsSegments) == 0 {
		return
	}
	m.statsRect = r
	if m.statsBox == nil {
		m.statsBox = &TextBox{Wrap: false, TitleStyle: StyleMuted, Title: " STATS "}
	}
	m.statsBox.Title = " STATS "
	m.statsBox.Segments = m.StatsSegments
	m.statsBox.Lines = nil
	m.statsBox.Draw(scr, r)
	if m.StatsLoading && r.H >= 2 {
		// drawStatsPane runs every frame and the spinner draws directly to the screen, so the live glyph animates without Spinner segments.
		NewLoadingSpinner("loading stats...", currentLoadingFrame()).Draw(scr, r.X+1, r.Y+r.H-1, imax(0, r.W-2))
	}
}

func (m *OptionsModal) drawOptionsPane(scr tcell.Screen, r Rect) {
	if r.W <= 0 || r.H <= 0 {
		return
	}
	m.optionsRect = r
	entries := m.totalEntries()
	m.clampCursor()
	logicalTotal := entries
	if len(m.TopEntries) > 0 && len(m.Entries) > 0 {
		logicalTotal++
	}
	m.optionsTotalRows = logicalTotal
	m.optionsVisibleRows = r.H
	maxOff := imax(0, logicalTotal-r.H)
	m.optionsOffset = clamp(m.optionsOffset, 0, maxOff)
	contentW := r.W
	if logicalTotal > r.H && r.W > 2 {
		contentW = r.W - 1
	}
	y := r.Y
	for row := range r.H {
		logical := m.optionsOffset + row
		if logical >= logicalTotal {
			break
		}
		entryIndex, gap := m.entryIndexForLogicalRow(logical)
		if gap {
			drawString(scr, r.X, y, StyleMuted, "──", contentW)
			y++
			continue
		}
		entry := m.entryAt(entryIndex)
		if entry == nil {
			y++
			continue
		}
		style := StyleDefault.Foreground(ColorText)
		marker := "  "
		if entry.Disabled || entry.Action == nil {
			style = StyleMuted
		} else if entryIndex == m.cursor {
			style = StyleDefault.Foreground(ColorAccent).Bold(true)
			marker = "▸ "
		}
		label := marker + strings.TrimSpace(entry.Label)
		drawString(scr, r.X, y, style, label, contentW)
		if entry.Hint != "" {
			hintX := r.X + contentW - runeCount(entry.Hint)
			if hintX >= r.X+runeCount(label)+optionsModalHintMinGap {
				drawString(scr, hintX, y, StyleMuted, entry.Hint, contentW)
			}
		}
		if entryIndex < len(m.entryRects) {
			m.entryRects[entryIndex] = Rect{X: r.X, Y: y, W: contentW, H: 1}
		}
		y++
	}
	if logicalTotal > r.H && r.W > 2 {
		m.optionsScrollbarRect = Rect{X: r.X + r.W - 1, Y: r.Y, W: 1, H: r.H}
		drawScrollbar(scr, m.optionsScrollbarRect.X, m.optionsScrollbarRect.Y,
			m.optionsScrollbarRect.H, r.H, logicalTotal, m.optionsOffset)
	} else {
		m.optionsScrollbarRect = Rect{}
	}
}

func (m *OptionsModal) entryIndexForLogicalRow(logical int) (int, bool) {
	if len(m.TopEntries) > 0 && len(m.Entries) > 0 {
		gapRow := len(m.TopEntries)
		if logical == gapRow {
			return -1, true
		}
		if logical > gapRow {
			return logical - 1, false
		}
	}
	return logical, false
}

func (m *OptionsModal) jumpToOptionsScrollbarY(y int) {
	if m.optionsScrollbarRect.H <= 0 {
		return
	}
	rel := max(y-m.optionsScrollbarRect.Y, 0)
	if rel >= m.optionsScrollbarRect.H {
		rel = m.optionsScrollbarRect.H - 1
	}
	maxOff := imax(0, m.optionsTotalRows-m.optionsVisibleRows)
	newOff := rel * maxOff / imax(1, m.optionsScrollbarRect.H-1)
	m.optionsOffset = clamp(newOff, 0, maxOff)
}

func (m *OptionsModal) ensureCursorVisible() {
	if m.optionsVisibleRows <= 0 {
		return
	}
	logical, ok := m.logicalRowForEntryIndex(m.cursor)
	if !ok {
		return
	}
	if logical < m.optionsOffset {
		m.optionsOffset = logical
	} else if logical >= m.optionsOffset+m.optionsVisibleRows {
		m.optionsOffset = logical - m.optionsVisibleRows + 1
	}
	maxOff := imax(0, m.optionsTotalRows-m.optionsVisibleRows)
	m.optionsOffset = clamp(m.optionsOffset, 0, maxOff)
}

func (m *OptionsModal) logicalRowForEntryIndex(index int) (int, bool) {
	if index < 0 || index >= m.totalEntries() {
		return 0, false
	}
	if len(m.TopEntries) > 0 && len(m.Entries) > 0 && index >= len(m.TopEntries) {
		return index + 1, true
	}
	return index, true
}
