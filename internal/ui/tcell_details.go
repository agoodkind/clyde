package ui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"

	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/util"
)

// DetailsFocus identifies which sub-pane of the details view owns keyboard
// focus. Only matters for scrolling: keys routed to the focused pane advance
// that pane's scroll offset.
type DetailsFocus int

const (
	DetailsFocusNone DetailsFocus = iota
	DetailsFocusLeft
	DetailsFocusRight
)

// DetailsView renders a two column details pane. The left column holds
// session stats; the right column holds the full conversation. Both are
// independently scrollable. A parent (App) shifts focus between them.
type DetailsView struct {
	Left  *TextBox
	Right *TextBox
	Focus DetailsFocus

	Rect      Rect
	LeftRect  Rect // last-drawn rect for the left pane, used for mouse hit-testing
	RightRect Rect // last-drawn rect for the right pane

	// LookupLiveURL returns the active daemon live URL for sess, if any.
	// Claude currently sources that URL from its bridge backend; the
	// details pane keeps the display provider-neutral.
	LookupLiveURL func(sess *session.Session) (LiveURL, bool)

	sessionKey string
}

// formatLiveURL renders the live URL row in the details left column.
func (d *DetailsView) formatLiveURL(sess *session.Session) string {
	if d.LookupLiveURL == nil {
		return "(daemon offline)"
	}
	b, ok := d.LookupLiveURL(sess)
	if !ok {
		return "off"
	}
	return "active  " + b.URL
}

// NewDetailsView constructs a details pane.
func NewDetailsView() *DetailsView {
	return &DetailsView{
		Left:  &TextBox{Wrap: false, TitleStyle: StyleMuted},
		Right: &TextBox{Wrap: true, TitleStyle: StyleMuted},
	}
}

// Set populates both columns from a session and detail payload.
func (d *DetailsView) Set(sess *session.Session, detail SessionDetail) {
	key := detailsSessionKey(sess)
	sameSession := key != "" && key == d.sessionKey
	leftOffset := d.Left.Offset
	rightOffset := d.Right.Offset

	d.Left.Title = " STATS "
	d.Right.Title = " MESSAGES "
	d.Left.SetSegments(d.buildLeft(sess, detail))
	d.Right.SetSegments(d.buildRight(sess, detail))
	if sameSession {
		d.Left.Offset = leftOffset
		d.Right.Offset = rightOffset
	}
	d.sessionKey = key
}

func detailsSessionKey(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	return sess.Name + "\x00" + sess.Metadata.ProviderSessionID()
}

// SetFocus moves keyboard focus between Left, Right, or None.
func (d *DetailsView) SetFocus(f DetailsFocus) {
	d.Focus = f
	d.Left.Focused = f == DetailsFocusLeft
	d.Right.Focused = f == DetailsFocusRight
}

// buildLeft composes the stats column as a slice of styled logical lines.
// The parent TextBox handles wrapping and scrolling.
func (d *DetailsView) buildLeft(sess *session.Session, detail SessionDetail) [][]TextSegment {
	var out [][]TextSegment

	// Header: name + optional context (no label, just prominent).
	out = append(out, []TextSegment{{Text: sess.Name, Style: StyleDefault.Bold(true)}})
	if sess.Metadata.Context != "" {
		ctx := sess.Metadata.Context
		if n := runeCount(ctx); n > 120 {
			ctx = string([]rune(ctx)[:117]) + "..."
		}
		out = append(out, []TextSegment{{Text: ctx, Style: StyleMuted}})
	}
	out = append(out, []TextSegment{})

	// section writes a bold heading line.
	section := func(title string) {
		out = append(out, []TextSegment{{Text: title, Style: StyleDefault.Foreground(ColorMuted).Bold(true).Underline(true)}})
	}
	kv := func(k, v string) {
		out = append(out, []TextSegment{
			{Text: fmt.Sprintf("  %-14s", k), Style: StyleSubtext},
			{Text: v, Style: StyleDefault},
		})
	}
	kvLoading := func(k, status string) {
		out = append(out, []TextSegment{
			{Text: fmt.Sprintf("  %-14s", k), Style: StyleSubtext},
			loadingSegment(status),
		})
	}
	kvStacked := func(k, v string, note string) {
		kv(k, v)
		if strings.TrimSpace(note) == "" {
			return
		}
		out = append(out, []TextSegment{
			{Text: "                ", Style: StyleSubtext},
			{Text: note, Style: StyleMuted},
		})
	}

	section("Overview")
	if detail.ContextUsageLoaded {
		kv("Context", formatExactContextUsage(detail.ContextUsage))
		kv("Messages", formatDetailTokens(detail.ContextUsage.MessagesTokens))
	} else {
		kvLoading("Context", detail.ContextUsageStatus)
		kvLoading("Messages", detail.ContextUsageStatus)
	}
	lastActivityAt, lastActivityAgo := formatDetailLastActivity(sess)
	kvStacked("Last activity", lastActivityAt, lastActivityAgo)
	out = append(out, []TextSegment{})

	// Diagnostics is only worth a section heading when the status
	// carries information beyond the spinner (a failure, a cooldown
	// reason, "unsupported"). Generic in-flight sentinels would just
	// repeat the Overview spinner row, which looks cluttered.
	if detail.ContextUsageStatus != "" && !isGenericLoadingStatus(detail.ContextUsageStatus) {
		section("Diagnostics")
		kv("Context probe", detail.ContextUsageStatus)
		out = append(out, []TextSegment{})
	}

	section("Identity")
	kv("Model", detail.Model)
	kv("Live URL", d.formatLiveURL(sess))
	kv("Basedir", shortPath(sess.Metadata.WorkspaceRoot))
	if sess.Metadata.WorkDir != "" && sess.Metadata.WorkDir != sess.Metadata.WorkspaceRoot {
		kv("Work dir", shortPath(sess.Metadata.WorkDir))
	}
	if sess.Metadata.IsForkedSession {
		kv("Type", "fork of "+sess.Metadata.ParentSession)
	}
	if sess.Metadata.IsIncognito {
		kv("Type", "incognito (auto-delete on exit)")
	}
	out = append(out, []TextSegment{})

	section("Timing")
	kv("Created", sess.Metadata.Created.Format("2006-01-02 15:04"))
	kv("Last used", sess.Metadata.LastAccessed.Local().Format("2006-01-02 15:04"))
	kv("Used ago", util.FormatRelativeTime(sess.Metadata.LastAccessed))
	age := util.FormatRelativeTime(sess.Metadata.Created)
	kv("Age", age)
	out = append(out, []TextSegment{})

	section("Transcript")
	if detail.TranscriptStatsLoaded {
		kv("Visible msgs", formatDetailMessageCount(detail))
		kv("Last msg est", formatDetailTokens(detail.LastMessageTokens))
		if detail.CompactionCount > 0 {
			kv("Compactions", formatDetailCompactions(detail))
		}
		if detail.TranscriptSizeBytes > 0 {
			mb := float64(detail.TranscriptSizeBytes) / (1024 * 1024)
			kv("Size", fmt.Sprintf("%.2f MB", mb))
		}
	} else {
		status := detail.TranscriptStatsStatus
		kvLoading("Visible msgs", status)
		kvLoading("Last msg est", status)
		kvLoading("Compactions", status)
		kvLoading("Size", status)
	}
	out = append(out, []TextSegment{})

	if len(detail.AllMessages) > 0 {
		section("Conversation")
		users, assistants := 0, 0
		var firstTS, lastTS string
		for i, m := range detail.AllMessages {
			switch m.Role {
			case "user":
				users++
			case "assistant":
				assistants++
			}
			if i == 0 && !m.Timestamp.IsZero() {
				firstTS = m.Timestamp.Local().Format("2006-01-02 15:04")
			}
			if i == len(detail.AllMessages)-1 && !m.Timestamp.IsZero() {
				lastTS = m.Timestamp.Local().Format("2006-01-02 15:04")
			}
		}
		kv("Total msgs", fmt.Sprintf("%d  (%d user, %d assistant)", users+assistants, users, assistants))
		if firstTS != "" {
			kv("First message", firstTS)
		}
		if lastTS != "" {
			kv("Last message", lastTS)
		}
		out = append(out, []TextSegment{})
	}

	if len(detail.Tools) > 0 {
		section("Top tools")
		for _, t := range detail.Tools {
			out = append(out, []TextSegment{
				{Text: fmt.Sprintf("  %-14s", t.Name), Style: StyleSubtext},
				{Text: fmt.Sprintf("%d", t.Count), Style: StyleDefault},
			})
		}
		out = append(out, []TextSegment{})
	}

	section("Identifiers")
	kv("UUID", sess.Metadata.ProviderSessionID())
	if len(sess.Metadata.PreviousProviderSessionIDStrings()) > 0 {
		kv("Previous", fmt.Sprintf("%d prior UUID(s)", len(sess.Metadata.PreviousProviderSessionIDStrings())))
	}
	out = append(out, []TextSegment{})

	section("Resume")
	out = append(out, []TextSegment{{Text: "  clyde resume " + strconv.Quote(session.SessionDisplayName(sess)), Style: StyleMuted}})
	out = append(out, []TextSegment{{Text: "  claude --resume " + sess.Metadata.ProviderSessionID(), Style: StyleMuted}})

	return out
}

func formatDetailTokens(tokens int) string {
	if tokens <= 0 {
		return "-"
	}
	return "~" + formatCompactTokens(tokens) + " tok"
}

func formatExactContextUsage(usage SessionContextUsage) string {
	if usage.TotalTokens <= 0 {
		return "-"
	}
	if usage.MaxTokens > 0 {
		percent := usage.Percentage
		if percent <= 0 {
			percent = usage.TotalTokens * 100 / usage.MaxTokens
		}
		return fmt.Sprintf("%s/%s tok  %d%%",
			formatCompactTokens(usage.TotalTokens),
			formatCompactTokens(usage.MaxTokens),
			percent)
	}
	return formatCompactTokens(usage.TotalTokens) + " tok"
}

func formatCompactTokens(tokens int) string {
	if tokens < 0 {
		return "0"
	}
	switch {
	case tokens >= 1_000_000:
		value := float64(tokens) / 1_000_000
		if tokens%1_000_000 == 0 {
			return fmt.Sprintf("%.0fM", value)
		}
		return fmt.Sprintf("%.1fM", value)
	case tokens >= 100_000:
		return fmt.Sprintf("%dk", tokens/1000)
	case tokens >= 1_000:
		value := float64(tokens) / 1_000
		if tokens%1_000 == 0 {
			return fmt.Sprintf("%.0fk", value)
		}
		return fmt.Sprintf("%.1fk", value)
	default:
		return formatWithCommas(tokens)
	}
}

func formatDetailMessageCount(detail SessionDetail) string {
	total := detail.TotalMessages
	if total == 0 {
		total = len(detail.AllMessages)
	}
	if total == 0 {
		return "-"
	}
	if detail.CompactionCount > 0 {
		return fmt.Sprintf("%s incl. compacted history", formatWithCommas(total))
	}
	return formatWithCommas(total)
}

func formatDetailLastActivity(sess *session.Session) (string, string) {
	if sess == nil {
		return "-", ""
	}
	t := lastUsedTime(sess)
	if t.IsZero() {
		return "-", ""
	}
	return t.Local().Format("2006-01-02 15:04"), util.FormatRelativeTime(t)
}

func formatDetailCompactions(detail SessionDetail) string {
	value := formatWithCommas(detail.CompactionCount)
	if detail.LastPreCompactTokens > 0 {
		value += "  last pre " + formatDetailTokens(detail.LastPreCompactTokens)
	}
	return value
}

// buildRight renders the full conversation. Each message gets a role tag
// and a timestamp. Long bodies are wrapped by the parent TextBox because
// its Wrap flag is on.
func (d *DetailsView) buildRight(_ *session.Session, detail SessionDetail) [][]TextSegment {
	src := detail.AllMessages
	if len(src) == 0 && len(detail.Messages) > 0 {
		src = detail.Messages
	}

	if len(src) == 0 {
		if detail.ConversationLoading {
			return [][]TextSegment{{
				{Text: "  ", Style: StyleMuted},
				loadingSegment("loading conversation..."),
			}}
		}
		return [][]TextSegment{{{Text: "  (no visible messages)", Style: StyleMuted}}}
	}

	// Latest message first so the user reads the most recent turn at the
	// top without scrolling. We copy rather than reverse in place because
	// the source slice is shared with the cache.
	msgs := make([]DetailMessage, len(src))
	for i, m := range src {
		msgs[len(src)-1-i] = m
	}

	var out [][]TextSegment

	userTag := StyleDefault.Foreground(ColorSuccess).Bold(true)
	userBody := StyleDefault.Foreground(ColorText)
	assistantTag := StyleDefault.Foreground(ColorAccent).Bold(true)
	assistantBody := StyleDefault.Foreground(ColorSubtext)
	tsStyle := StyleDefault.Foreground(ColorMuted)

	for i, m := range msgs {
		tag := "You"
		tagStyle := userTag
		bodyStyle := userBody
		if m.Role == "assistant" {
			tag = "Claude"
			tagStyle = assistantTag
			bodyStyle = assistantBody
		}

		ts := ""
		if !m.Timestamp.IsZero() {
			ts = m.Timestamp.Local().Format("01-02 15:04")
		}

		header := []TextSegment{
			{Text: fmt.Sprintf("▎%-6s", tag), Style: tagStyle},
		}
		if ts != "" {
			header = append(header, TextSegment{Text: " " + ts, Style: tsStyle})
		}
		out = append(out, header)

		// Body lines: split on existing newlines so the TextBox wrapping does
		// not accidentally glue them together. Blank line after each message.
		body := strings.ReplaceAll(m.Text, "\r", "")
		for line := range strings.SplitSeq(body, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			out = append(out, []TextSegment{{Text: "  " + line, Style: bodyStyle}})
		}
		if i != len(msgs)-1 {
			out = append(out, []TextSegment{})
		}
	}
	return out
}

// Draw splits r into a left and right column and renders each TextBox.
// A thin horizontal rule separates the details view from the table above.
// A single vertical rule splits the two sub-panes.
func (d *DetailsView) Draw(scr tcell.Screen, r Rect) {
	d.Rect = r
	if r.W <= 0 || r.H <= 0 {
		return
	}

	// Wipe the entire details rect first. The sub-panes each clear their
	// own inner rects, but the one-column left and right margins plus the
	// divider column are outside those rects. Without a full clear, stale
	// pixels from the previous draw (for example table content before the
	// user opened details, or old scrollbar glyphs when content length
	// changed) leak into those margin columns.
	clearRect(scr, r)

	borderStyle := StyleDefault.Foreground(ColorBorder)
	for x := r.X; x < r.X+r.W; x++ {
		scr.SetContent(x, r.Y, '─', nil, borderStyle)
	}

	inner := Rect{X: r.X + 1, Y: r.Y + 1, W: r.W - 2, H: r.H - 1}
	if inner.H <= 0 {
		return
	}

	// Responsive layout. Three modes:
	//
	//   wide  (>=80 cols): side-by-side 40/60 split, the original layout.
	//   tall  (50..79 cols): stack vertically so each pane gets the full
	//                        width. Stats sits on top, messages below.
	//   tiny  (<50 cols):    stats only; messages would be unreadable.
	//
	// The vertical split allocates 40% of the available height to stats
	// and 60% to messages so the bias matches the original 40/60 split
	// the user sees on a wide terminal.
	if inner.W < 50 {
		d.LeftRect = inner
		d.RightRect = Rect{}
		d.Left.Draw(scr, inner)
		// Show a hint at the bottom so users know they're in tiny-mode.
		drawString(scr, inner.X, inner.Y+inner.H-1, StyleMuted,
			"  (resize wider to see messages pane)", inner.W)
		return
	}
	if inner.W < 80 {
		// Vertical stack mode.
		topH := inner.H * 40 / 100
		if topH < 6 {
			topH = imin(6, inner.H-1)
		}
		bottomH := inner.H - topH - 1
		if bottomH < 3 {
			d.LeftRect = inner
			d.RightRect = Rect{}
			d.Left.Draw(scr, inner)
			return
		}
		d.LeftRect = Rect{X: inner.X, Y: inner.Y, W: inner.W, H: topH}
		d.RightRect = Rect{X: inner.X, Y: inner.Y + topH + 1, W: inner.W, H: bottomH}
		d.Left.Draw(scr, d.LeftRect)
		// Horizontal divider between the two stacked panes.
		for x := inner.X; x < inner.X+inner.W; x++ {
			scr.SetContent(x, inner.Y+topH, '─', nil, borderStyle)
		}
		d.Right.Draw(scr, d.RightRect)
		// Focus indicators for the stacked layout. Both top rows get the
		// usual title; the focused pane gets the accent fill so the user
		// can still tell where arrow keys land.
		focusStyle := tcell.StyleDefault.Background(ColorAccent).Foreground(tcell.ColorBlack).Bold(true)
		idleStyle := StyleSubtext.Bold(true)
		switch d.Focus {
		case DetailsFocusLeft:
			fillRow(scr, inner.X, inner.Y, inner.W, focusStyle)
			drawString(scr, inner.X+1, inner.Y, focusStyle, "STATS  (focused, ↑↓ scroll, tab to messages)", inner.W-1)
			drawString(scr, inner.X+1, inner.Y+topH+1, idleStyle, "MESSAGES  (tab to focus)", inner.W-1)
		case DetailsFocusRight:
			fillRow(scr, inner.X, inner.Y+topH+1, inner.W, focusStyle)
			drawString(scr, inner.X+1, inner.Y+topH+1, focusStyle, "MESSAGES  (focused, ↑↓ scroll, tab to stats)", inner.W-1)
			drawString(scr, inner.X+1, inner.Y, idleStyle, "STATS  (tab to focus)", inner.W-1)
		default:
			drawString(scr, inner.X+1, inner.Y, idleStyle, "STATS  (tab to focus)", inner.W-1)
			drawString(scr, inner.X+1, inner.Y+topH+1, idleStyle, "MESSAGES  (tab to focus)", inner.W-1)
		}
		return
	}

	// Wide layout: side-by-side 40/60 split.
	leftW := inner.W * 40 / 100
	if leftW < 28 {
		leftW = imin(28, inner.W-1)
	}
	rightX := inner.X + leftW + 1
	rightW := inner.W - leftW - 1
	if rightW < 15 {
		d.LeftRect = inner
		d.RightRect = Rect{}
		d.Left.Draw(scr, inner)
		return
	}

	d.LeftRect = Rect{X: inner.X, Y: inner.Y, W: leftW, H: inner.H}
	d.RightRect = Rect{X: rightX, Y: inner.Y, W: rightW, H: inner.H}
	d.Left.Draw(scr, d.LeftRect)
	for y := inner.Y; y < inner.Y+inner.H; y++ {
		scr.SetContent(inner.X+leftW, y, '│', nil, borderStyle)
	}
	d.Right.Draw(scr, d.RightRect)

	// Focus indicator: paint the entire top row of the focused pane in
	// accent colors so the user always sees which pane consumes arrow
	// keys. The non-focused pane keeps its muted title.
	focusStyle := tcell.StyleDefault.Background(ColorAccent).Foreground(tcell.ColorBlack).Bold(true)
	idleStyle := StyleSubtext.Bold(true)
	switch d.Focus {
	case DetailsFocusLeft:
		fillRow(scr, inner.X, inner.Y, leftW, focusStyle)
		drawString(scr, inner.X+1, inner.Y, focusStyle, "STATS  (focused, ↑↓ to scroll)", leftW-1)
		drawString(scr, rightX+1, inner.Y, idleStyle, "MESSAGES  (tab to focus)", rightW-1)
	case DetailsFocusRight:
		fillRow(scr, rightX, inner.Y, rightW, focusStyle)
		drawString(scr, rightX+1, inner.Y, focusStyle, "MESSAGES  (focused, ↑↓ to scroll)", rightW-1)
		drawString(scr, inner.X+1, inner.Y, idleStyle, "STATS  (tab to focus)", leftW-1)
	default:
		drawString(scr, inner.X+1, inner.Y, idleStyle, "STATS  (tab to focus)", leftW-1)
		drawString(scr, rightX+1, inner.Y, idleStyle, "MESSAGES  (tab to focus)", rightW-1)
	}
}

// HandleEvent forwards keyboard events to the focused sub-pane only.
// The parent App decides which pane is focused.
func (d *DetailsView) HandleEvent(ev tcell.Event) bool {
	switch d.Focus {
	case DetailsFocusLeft:
		return d.Left.HandleEvent(ev)
	case DetailsFocusRight:
		return d.Right.HandleEvent(ev)
	}
	return false
}
