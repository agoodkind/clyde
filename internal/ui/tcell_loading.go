package ui

import (
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
)

const loadingFrameInterval = 100 * time.Millisecond

type loadingStatus string

const (
	loadingStatusEmpty       loadingStatus = ""
	loadingStatusProbing     loadingStatus = "probing"
	loadingStatusLoadingDots loadingStatus = "loading..."
	loadingStatusLoading     loadingStatus = "loading"
	loadingStatusCooldown    loadingStatus = "cooldown"
	loadingStatusRefreshing  loadingStatus = "refreshing"
	loadingStatusUnsupported loadingStatus = "unsupported"
	loadingStatusCancelled   loadingStatus = "cancelled"
	loadingStatusCanceled    loadingStatus = "canceled"
	loadingStatusProbeFailed loadingStatus = "probe_failed"
)

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
	return newTextSegment(s.Text(), style)
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

// loadingSegment returns a TextSegment whose glyph is substituted at draw
// time so the spinner ticks live without rebuilding the parent segment list.
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
		return newTextSegment(trimmed, StyleMuted)
	}
	return TextSegment{Text: trimmed, Style: StyleMuted, Spinner: true}
}

// isGenericLoadingStatus reports whether status is a "still working"
// sentinel that adds no information beyond the spinner itself. The
// details pane hides redundant Diagnostics rows for these so the
// user does not see the same word in two places at once.
func isGenericLoadingStatus(status string) bool {
	switch loadingStatus(strings.TrimSpace(status)) {
	case loadingStatusEmpty,
		loadingStatusProbing,
		loadingStatusLoadingDots,
		loadingStatusLoading,
		loadingStatusCooldown,
		loadingStatusRefreshing:
		return true
	case loadingStatusUnsupported,
		loadingStatusCancelled,
		loadingStatusCanceled,
		loadingStatusProbeFailed:
		return false
	}
	return false
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
	switch loadingStatus(trimmed) {
	case loadingStatusUnsupported,
		loadingStatusCancelled,
		loadingStatusCanceled,
		loadingStatusProbeFailed:
		return true
	case loadingStatusEmpty,
		loadingStatusProbing,
		loadingStatusLoadingDots,
		loadingStatusLoading,
		loadingStatusCooldown,
		loadingStatusRefreshing:
		return false
	}
	return false
}
