package ui

import (
	"github.com/gdamore/tcell/v2"
)

// StatusMode identifies which mode badge and legend to show.
type StatusMode int

const (
	StatusBrowse StatusMode = iota
	StatusDetail
	StatusFilter
	StatusSearch
	StatusCompact
	StatusExport
	StatusView
	StatusConfirm
)

// StatusBarWidget draws the single line at the bottom of the screen.
// It has a full width background, a colored mode badge on the left,
// contextual keybinding hints in the middle, and a scroll/position
// indicator on the right.
type StatusBarWidget struct {
	Mode     StatusMode
	Position string // e.g. "Top", "Bot", "45%". Empty means nothing to show.
	Clock    string // e.g. "15:04:05"
	// LegendOverride lets overlays/panels supply a context-specific
	// legend while reusing the same fixed status bar location.
	LegendOverride []LegendAction
	// LiveURLCount surfaces the number of active live URLs. Claude
	// currently reports these through its bridge backend.
	LiveURLCount int
	// DaemonOnline indicates whether the daemon subscription stream is
	// currently healthy. When false the status bar shows an offline
	// badge, but the rest of the TUI remains interactive.
	DaemonOnline bool
	// DaemonStatus carries a short human-readable error summary when the
	// daemon is offline so the user can see why live features degraded.
	DaemonStatus string
	// DaemonConnecting suppresses the offline badge during startup while
	// the dashboard is still waiting for the first daemon-backed snapshot.
	DaemonConnecting bool
	// DaemonSpinner is the single-glyph frame to render inside the
	// connecting badge.
	DaemonSpinner string
}

// LegendProvider is implemented by overlays that want to customize
// the bottom status bar legend while they are the topmost pane.
type LegendProvider interface {
	StatusLegendActions() []LegendAction
}

type legendHint struct {
	key   string
	label string
}

// LegendAction is the allowed catalog of legend entries. All status-bar
// legends map from these enums so wording stays consistent across modes
// and overlays.
type LegendAction int

const (
	LegendMove LegendAction = iota
	LegendTopBottom
	LegendSelectOption
	LegendSelectDetail
	LegendFilter
	LegendNew
	LegendRefresh
	LegendRestartDaemon
	LegendHelp
	LegendQuit
	LegendSearch
	LegendView
	LegendCompact
	LegendFork
	LegendDelete
	LegendEditBasedir
	LegendClose
	LegendTypeFilter
	LegendConfirm
	LegendClear
	LegendNext
	LegendFocus
	LegendAdjust
	LegendSelect
	LegendScroll
	LegendYesConfirm
	LegendNoCancel
	LegendPreview
	LegendApply
	LegendUndo
)

// badgeFor returns the label and background color for a mode.
func badgeFor(m StatusMode) (string, tcell.Color) {
	switch m {
	case StatusBrowse:
		return " BROWSE ", ColorModeBrowse
	case StatusDetail:
		return " DETAIL ", ColorModeDetail
	case StatusFilter:
		return " FILTER ", ColorModeFilter
	case StatusSearch:
		return " SEARCH ", ColorModeSearch
	case StatusCompact:
		return " COMPACT ", ColorModeCompact
	case StatusExport:
		return " EXPORT ", ColorModeCompact
	case StatusView:
		return " VIEW ", ColorModeView
	case StatusConfirm:
		return " CONFIRM ", ColorWarning
	}
	return " ? ", tcell.ColorWhite
}

func badgeStyle(bg tcell.Color) tcell.Style {
	return tcell.StyleDefault.Background(bg).Foreground(badgeTextColor(bg)).Bold(true)
}

func badgeTextColor(bg tcell.Color) tcell.Color {
	switch bg {
	case ColorAccent, ColorModeDetail, ColorModeView, ColorModeFilter:
		if detectedTerminalTheme == terminalThemeLight {
			return tcell.ColorWhite
		}
	}
	return tcell.ColorBlack
}

// legendForStatus returns styled segments for the keybinding hints.
func legendForStatus(s *StatusBarWidget) []TextSegment {
	actions := legendActionsForStatus(s)
	return legendSegmentsFromActions(actions)
}

func legendActionsForStatus(s *StatusBarWidget) []LegendAction {
	switch s.Mode {
	case StatusBrowse:
		actions := []LegendAction{
			LegendMove, LegendFilter, LegendNew,
			LegendRefresh, LegendHelp, LegendQuit,
		}
		if !s.DaemonOnline {
			actions[3] = LegendRestartDaemon
		}
		return actions
	case StatusDetail:
		return []LegendAction{
			LegendSearch, LegendView,
			LegendCompact, LegendFork, LegendDelete,
			LegendEditBasedir, LegendClose,
		}
	case StatusFilter:
		return []LegendAction{LegendTypeFilter, LegendClear}
	case StatusSearch:
		return []LegendAction{LegendNext, LegendSearch, LegendClose}
	case StatusCompact, StatusExport:
		return []LegendAction{LegendFocus, LegendAdjust, LegendSelect, LegendClose}
	case StatusView:
		return []LegendAction{LegendScroll, LegendClose}
	case StatusConfirm:
		return []LegendAction{LegendYesConfirm, LegendNoCancel}
	}
	return nil
}

func legendActionAt(s *StatusBarWidget, r Rect, x int) (LegendAction, bool) {
	actions := s.LegendOverride
	if len(actions) == 0 {
		actions = legendActionsForStatus(s)
	}
	curX := r.X + 1 + runeCount(badgeLabelForStatus(s)) + 2
	for i, action := range actions {
		hint, ok := legendHintForAction(action)
		if !ok {
			continue
		}
		if i > 0 {
			curX += 2
		}
		start := curX
		width := runeCount(hint.key) + runeCount(" "+hint.label)
		end := start + width
		if x >= start && x < end {
			return action, true
		}
		curX = end
	}
	return 0, false
}

func badgeLabelForStatus(s *StatusBarWidget) string {
	label, _ := badgeFor(s.Mode)
	return label
}

func legendSegmentsFromActions(actions []LegendAction) []TextSegment {
	barBg := StyleStatusBar
	keyStyle := barBg.Foreground(ColorText).Bold(true)
	labelStyle := barBg.Foreground(ColorMuted)

	var segs []TextSegment
	for i, action := range actions {
		hint, ok := legendHintForAction(action)
		if !ok {
			continue
		}
		if i > 0 {
			segs = append(segs, TextSegment{Text: "  ", Style: barBg, Spinner: false})
		}
		segs = append(segs, TextSegment{Text: hint.key, Style: keyStyle, Spinner: false})
		segs = append(segs, TextSegment{Text: " " + hint.label, Style: labelStyle, Spinner: false})
	}
	return segs
}

// legendHintTable maps each LegendAction to its static key and label.
// Keeping the catalog as data instead of a long switch keeps the
// dispatch helper trivial for cyclomatic-complexity gates.
var legendHintTable = map[LegendAction]legendHint{
	LegendMove:          {key: "j/k", label: "move"},
	LegendTopBottom:     {key: "g/G", label: "top/bot"},
	LegendSelectOption:  {key: "enter/O", label: "select option"},
	LegendSelectDetail:  {key: "space", label: "select detail"},
	LegendFilter:        {key: "/", label: "filter"},
	LegendNew:           {key: "N", label: "new"},
	LegendRefresh:       {key: "R", label: "refresh"},
	LegendRestartDaemon: {key: "R", label: "restart daemon"},
	LegendHelp:          {key: "?", label: "help"},
	LegendQuit:          {key: "q", label: "quit"},
	LegendSearch:        {key: "/", label: "search"},
	LegendView:          {key: "v", label: "view"},
	LegendCompact:       {key: "c", label: "compact"},
	LegendFork:          {key: "f", label: "fork"},
	LegendDelete:        {key: "d", label: "delete"},
	LegendEditBasedir:   {key: "B", label: "edit basedir"},
	LegendClose:         {key: "esc", label: "close"},
	LegendTypeFilter:    {key: "type", label: "filter"},
	LegendConfirm:       {key: "enter", label: "confirm"},
	LegendClear:         {key: "esc", label: "clear"},
	LegendNext:          {key: "tab", label: "next"},
	LegendFocus:         {key: "↑↓", label: "focus"},
	LegendAdjust:        {key: "←→", label: "adjust"},
	LegendSelect:        {key: "enter/spc", label: "select"},
	LegendScroll:        {key: "↑↓", label: "scroll"},
	LegendYesConfirm:    {key: "y", label: "confirm"},
	LegendNoCancel:      {key: "n/esc", label: "cancel"},
	LegendPreview:       {key: "p", label: "preview"},
	LegendApply:         {key: "a", label: "apply"},
	LegendUndo:          {key: "u", label: "undo"},
}

func legendHintForAction(action LegendAction) (legendHint, bool) {
	hint, ok := legendHintTable[action]
	return hint, ok
}

// Draw renders the status bar into r (r.H should be 1).
func (s *StatusBarWidget) Draw(scr tcell.Screen, r Rect) {
	fillRow(scr, r.X, r.Y, r.W, StyleStatusBar)

	label, bg := badgeFor(s.Mode)
	modeBadgeStyle := badgeStyle(bg)

	x := r.X + 1
	used := drawString(scr, x, r.Y, modeBadgeStyle, label, r.W-1)
	x += used
	// Small gap after badge
	drawString(scr, x, r.Y, StyleStatusBar, "  ", 2)
	x += 2

	// Legend
	var segs []TextSegment
	if len(s.LegendOverride) == 0 {
		segs = legendForStatus(s)
	} else {
		segs = legendSegmentsFromActions(s.LegendOverride)
	}
	for _, seg := range segs {
		if x >= r.X+r.W {
			break
		}
		u := drawString(scr, x, r.Y, seg.Style, seg.Text, r.X+r.W-x)
		x += u
	}

	// Right aligned clock.
	rightX := r.X + r.W
	if s.Clock != "" {
		clockStyle := StyleStatusBar.Foreground(ColorMuted)
		txt := " " + s.Clock + " "
		rx := rightX - runeCount(txt)
		if rx > x {
			drawString(scr, rx, r.Y, clockStyle, txt, rightX-rx)
			rightX = rx
		}
	}

	// Right aligned position.
	if s.Position != "" {
		posStyle := StyleStatusBar.Foreground(ColorText).Bold(true)
		txt := " " + s.Position + " "
		rx := rightX - runeCount(txt)
		if rx > x {
			drawString(scr, rx, r.Y, posStyle, txt, rightX-rx)
			rightX = rx
		}
	}

	// Right aligned daemon health badge. Sits to the left of position.
	if s.DaemonConnecting {
		txt := " " + s.DaemonSpinner + " connecting "
		bx := rightX - runeCount(txt)
		if bx > x {
			connectingStyle := badgeStyle(ColorAccent)
			drawString(scr, bx, r.Y, connectingStyle, txt, rightX-bx)
			rightX = bx
		}
	} else if !s.DaemonOnline {
		txt := " DAEMON OFFLINE "
		bx := rightX - runeCount(txt)
		if bx > x {
			offlineStyle := badgeStyle(ColorWarning)
			drawString(scr, bx, r.Y, offlineStyle, txt, rightX-bx)
			rightX = bx
		}
		if s.DaemonStatus != "" {
			msg := " " + s.DaemonStatus + " "
			msgX := rightX - runeCount(msg)
			if msgX > x {
				msgStyle := tcell.StyleDefault.Background(ColorStatusBg).Foreground(ColorWarning)
				drawString(scr, msgX, r.Y, msgStyle, msg, rightX-msgX)
				rightX = msgX
			}
		}
	}

	// Right aligned live-session indicator. Sits to the left of daemon/position.
	if s.LiveURLCount > 0 {
		txt := " LIVE×" + itoa(s.LiveURLCount) + " "
		bx := rightX - runeCount(txt)
		if bx > x {
			drawString(scr, bx, r.Y, badgeStyle(ColorSuccess), txt, rightX-bx)
		}
	}
}

// itoa is a small wrapper over strconv to keep imports tidy.
func itoa(n int) string {
	if n < 0 {
		return "?"
	}
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

// HandleEvent does nothing (status bar is decorative).
func (s *StatusBarWidget) HandleEvent(ev tcell.Event) bool { return false }
