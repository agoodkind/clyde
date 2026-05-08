package ui

import (
	"strings"

	"github.com/gdamore/tcell/v2"
)

// TextBox is a scrollable, read-only text area.
// It wraps on whitespace when width is fixed, and supports keyboard/mouse scroll.
type TextBox struct {
	// Lines hold raw (unwrapped) logical lines. Markup tags like [gray]..[-]
	// are stripped on draw; colored spans are rendered by the caller via
	// Segments if richer formatting is needed.
	Lines []string

	// Segments are an optional richer form. When non-nil they take precedence
	// over Lines: each inner slice is one logical line composed of styled runs.
	Segments [][]TextSegment

	// Title shown on the top row. Empty means no title.
	Title string

	// TitleStyle for the title line.
	TitleStyle tcell.Style

	// Scroll offset (in wrapped rows).
	Offset int

	// Rect last drawn.
	Rect Rect

	// ScrollbarRect is the last drawn scrollbar column (empty when no bar).
	// Used by the parent App for click-to-jump and drag-to-scroll.
	ScrollbarRect Rect

	// totalLines records the full wrapped line count after the most recent
	// draw. JumpToScrollbarY uses it to translate a track Y into an offset.
	totalLines int

	// Word wrap. If true, lines are wrapped on spaces to fit rect width.
	Wrap bool

	// Focused controls whether keyboard events move the scroll.
	Focused bool

	contentVersion      int
	wrappedCacheVersion int
	wrappedCacheWidth   int
	wrappedCache        [][]TextSegment
}

// TextSegment is a styled run of text.
//
// When Spinner is true, the renderer prepends the live spinner glyph
// plus a single space at draw time. Use this for inline "loading…"
// cells so the glyph keeps ticking without rebuilding segments on
// every interrupt.
type TextSegment struct {
	Text    string
	Style   tcell.Style
	Spinner bool
}

// renderedText returns the on-screen string for this segment, applying
// the live spinner glyph when Spinner is true.
func (s TextSegment) renderedText() string {
	if !s.Spinner {
		return s.Text
	}
	return LoadingSpinnerGlyph(currentLoadingFrame()) + " " + s.Text
}

// SetLines replaces content (plain).
func (tb *TextBox) SetLines(lines []string) {
	tb.Lines = lines
	tb.Segments = nil
	tb.Offset = 0
	tb.invalidateWrappedCache()
}

// SetSegments replaces content (styled).
func (tb *TextBox) SetSegments(segs [][]TextSegment) {
	tb.Segments = segs
	tb.Lines = nil
	tb.Offset = 0
	tb.invalidateWrappedCache()
}

func (tb *TextBox) invalidateWrappedCache() {
	tb.contentVersion++
	tb.wrappedCache = nil
	tb.wrappedCacheWidth = 0
	tb.wrappedCacheVersion = 0
}

// wrappedLines expands Lines/Segments into display lines fitting width w.
func (tb *TextBox) wrappedLines(w int) [][]TextSegment {
	if w <= 0 {
		return nil
	}
	if tb.wrappedCache != nil &&
		tb.wrappedCacheVersion == tb.contentVersion &&
		tb.wrappedCacheWidth == w {
		return tb.wrappedCache
	}
	var src [][]TextSegment
	if tb.Segments != nil {
		src = tb.Segments
	} else {
		src = make([][]TextSegment, len(tb.Lines))
		for i, ln := range tb.Lines {
			src[i] = []TextSegment{seg(stripMarkup(ln), StyleDefault)}
		}
	}
	if !tb.Wrap {
		tb.wrappedCache = src
		tb.wrappedCacheWidth = w
		tb.wrappedCacheVersion = tb.contentVersion
		return src
	}
	// Simple greedy word wrap per logical line.
	var out [][]TextSegment
	for _, line := range src {
		// Flatten to a string plus style per rune for easy splitting.
		// Spinner segments contribute their glyph-prefixed rendered
		// text so the wrap calculation reflects on-screen width.
		var plain strings.Builder
		for _, lineSeg := range line {
			plain.WriteString(lineSeg.renderedText())
		}
		if runeCount(plain.String()) <= w {
			out = append(out, line)
			continue
		}
		// Wrap by words; simple approach, splits on spaces.
		words := strings.Fields(plain.String())
		var cur string
		for _, word := range words {
			candidate := cur
			if candidate != "" {
				candidate += " "
			}
			candidate += word
			if runeCount(candidate) > w && cur != "" {
				out = append(out, []TextSegment{seg(cur, StyleDefault)})
				cur = word
			} else {
				cur = candidate
			}
		}
		if cur != "" {
			out = append(out, []TextSegment{seg(cur, StyleDefault)})
		}
	}
	tb.wrappedCache = out
	tb.wrappedCacheWidth = w
	tb.wrappedCacheVersion = tb.contentVersion
	return out
}

// TotalLines reports the number of wrapped display lines for width w.
func (tb *TextBox) TotalLines(w int) int {
	return len(tb.wrappedLines(w))
}

// Draw renders into r. A cute one column scrollbar appears on the right
// edge whenever the wrapped content is taller than the viewport.
func (tb *TextBox) Draw(scr tcell.Screen, r Rect) {
	tb.Rect = r
	clearRect(scr, r)

	y := r.Y
	contentY := y

	// Compute whether we will need a scrollbar before we wrap text, so
	// we can shrink the wrap width to match the content column. Missing
	// this step makes the last rune of every line collide with the bar.
	// We wrap provisionally using the full width to count rows, then
	// rewrap at the narrower width if overflow is detected.
	rawLines := tb.wrappedLines(r.W)
	rows := r.H
	if tb.Title != "" {
		rows = r.H - 1
	}
	needsBar := len(rawLines) > rows && r.W > 2
	contentW := r.W
	if needsBar {
		contentW = r.W - 1
	}

	if tb.Title != "" {
		style := tb.TitleStyle
		if style == (tcell.Style{}) {
			style = StyleMuted
		}
		drawString(scr, r.X, y, style, tb.Title, contentW)
		contentY = y + 1
	}

	if rows <= 0 {
		return
	}

	lines := rawLines
	if needsBar {
		lines = tb.wrappedLines(contentW)
	}
	maxOff := imax(0, len(lines)-rows)
	tb.Offset = clamp(tb.Offset, 0, maxOff)

	for i := range rows {
		idx := tb.Offset + i
		if idx >= len(lines) {
			break
		}
		segs := lines[idx]
		x := r.X
		remaining := contentW
		for _, lineSeg := range segs {
			used := drawString(scr, x, contentY+i, lineSeg.Style, lineSeg.renderedText(), remaining)
			x += used
			remaining -= used
			if remaining <= 0 {
				break
			}
		}
	}

	tb.totalLines = len(lines)
	if needsBar {
		tb.ScrollbarRect = Rect{X: r.X + r.W - 1, Y: contentY, W: 1, H: rows}
		drawScrollbar(scr, tb.ScrollbarRect.X, tb.ScrollbarRect.Y,
			tb.ScrollbarRect.H, rows, len(lines), tb.Offset)
	} else {
		tb.ScrollbarRect = Rect{}
	}
}

// JumpToScrollbarY maps an absolute screen Y inside the scrollbar track to
// an offset. The parent App calls this on click or drag over the bar.
func (tb *TextBox) JumpToScrollbarY(y int) {
	if tb.ScrollbarRect.H <= 0 {
		return
	}
	rel := max(y-tb.ScrollbarRect.Y, 0)
	if rel >= tb.ScrollbarRect.H {
		rel = tb.ScrollbarRect.H - 1
	}
	maxOff := imax(0, tb.totalLines-tb.ScrollbarRect.H)
	newOff := rel * maxOff / imax(1, tb.ScrollbarRect.H-1)
	tb.Offset = clamp(newOff, 0, maxOff)
}

// HandleEvent handles scroll keys when focused.
func (tb *TextBox) HandleEvent(ev tcell.Event) bool {
	if !tb.Focused {
		return false
	}
	ek, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}
	rows := imax(1, tb.Rect.H)
	switch ek.Key() {
	case tcell.KeyUp:
		tb.Offset = imax(0, tb.Offset-1)
		return true
	case tcell.KeyDown:
		tb.Offset++
		return true
	case tcell.KeyPgUp:
		tb.Offset = imax(0, tb.Offset-rows)
		return true
	case tcell.KeyPgDn:
		tb.Offset += rows
		return true
	case tcell.KeyHome:
		tb.Offset = 0
		return true
	case tcell.KeyEnd:
		tb.Offset = 1 << 30 // clamp on next draw
		return true
	case tcell.KeyRune:
		switch ek.Rune() {
		case 'j':
			tb.Offset++
			return true
		case 'k':
			tb.Offset = imax(0, tb.Offset-1)
			return true
		case 'g':
			tb.Offset = 0
			return true
		case 'G':
			tb.Offset = 1 << 30
			return true
		}
	}
	return false
}

// ScrollPercent returns visible position as 0..100, or -1 if fully fits.
func (tb *TextBox) ScrollPercent(width int) int {
	total := tb.TotalLines(width)
	rows := imax(1, tb.Rect.H)
	if total <= rows {
		return -1
	}
	maxOff := total - rows
	if maxOff <= 0 {
		return 100
	}
	return clamp(tb.Offset*100/maxOff, 0, 100)
}
