package ui

import (
	"slices"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

// genericLoadingStatuses are the sentinel status strings the daemon
// (and a few legacy TUI fabrication paths) emit while a probe is in
// flight. They convey "still working" with no extra information; the
// details pane hides redundant Diagnostics rows for these. The set is
// a slice rather than a switch so the bare-string-switch lint stays
// quiet and the eventual typed enum migration in the cat-3 plan has
// one place to swap.
var genericLoadingStatuses = []string{"", "probing", "loading...", "loading", "cooldown", "refreshing"}

// terminalLoadingStatuses are the settled outcomes the daemon emits
// when a probe finished without success. Terminal statuses do not
// animate; they replace the spinner with stable copy. "failed" is
// handled separately because daemon error strings include the cause.
var terminalLoadingStatuses = []string{"unsupported", "cancelled", "canceled", "probe_failed"}

const loadingFrameInterval = 100 * time.Millisecond

// LoadingSpinner is the shared TUI loading affordance. Keep loading copy
// routed through this type so panes, overlays, and the status bar animate
// consistently.
type LoadingSpinner struct {
	Label string
	Frame int
	Style tcell.Style
}

func NewLoadingSpinner(label string, frame int) LoadingSpinner {
	return LoadingSpinner{Label: label, Frame: frame, Style: StyleMuted}
}

func ClockLoadingSpinner(label string) LoadingSpinner {
	return NewLoadingSpinner(label, currentLoadingFrame())
}

func (s LoadingSpinner) Text() string {
	label := strings.TrimSpace(s.Label)
	if label == "" {
		label = "loading..."
	}
	return LoadingSpinnerGlyph(s.Frame) + " " + label
}

func (s LoadingSpinner) Segment() TextSegment {
	style := s.Style
	if style == (tcell.Style{}) {
		style = StyleMuted
	}
	return seg(s.Text(), style)
}

func (s LoadingSpinner) Draw(scr tcell.Screen, x, y int, width int) {
	drawString(scr, x, y, s.Segment().Style, s.Text(), width)
}

func LoadingSpinnerGlyph(frame int) string {
	frames := []string{"|", "/", "-", "\\"}
	if frame < 0 {
		frame = 0
	}
	return frames[frame%len(frames)]
}

func currentLoadingFrame() int {
	return int(currentUITime().UnixNano() / int64(loadingFrameInterval))
}

// loadingSegment is the inline-cell counterpart of LoadingSpinner. It
// returns a TextSegment whose glyph is substituted at draw time so
// the spinner ticks live without rebuilding the parent segment list.
// Use this whenever the loading copy lives inside a [][]TextSegment
// column (stats panes, kv rows) where a baked-in glyph would freeze.
//
// Terminal statuses (failed:, unsupported, cancelled, probe_failed)
// return a non-spinning segment so the user sees a stable label.
func loadingSegment(status string) TextSegment {
	trimmed := strings.TrimSpace(status)
	if trimmed == "" {
		trimmed = "loading..."
	}
	if isTerminalLoadingStatus(trimmed) {
		return seg(trimmed, StyleMuted)
	}
	return TextSegment{Text: trimmed, Style: StyleMuted, Spinner: true}
}

// seg returns a non-spinner styled text segment. Pair with
// loadingSegment so every TextSegment literal in the codebase goes
// through one of two factories and the exhaustruct linter has a
// stable Spinner field to look at.
func seg(text string, style tcell.Style) TextSegment {
	return TextSegment{Text: text, Style: style, Spinner: false}
}

// isGenericLoadingStatus reports whether status is a "still working"
// sentinel that adds no information beyond the spinner itself. The
// details pane hides redundant Diagnostics rows for these so the
// user does not see the same word in two places at once.
func isGenericLoadingStatus(status string) bool {
	return slices.Contains(genericLoadingStatuses, strings.TrimSpace(status))
}

// isTerminalLoadingStatus reports whether status describes a settled
// outcome (failure, unsupported, cancelled). Terminal statuses do not
// animate; they replace the spinner entirely with stable copy.
func isTerminalLoadingStatus(status string) bool {
	trimmed := strings.TrimSpace(status)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "failed") {
		return true
	}
	return slices.Contains(terminalLoadingStatuses, trimmed)
}
