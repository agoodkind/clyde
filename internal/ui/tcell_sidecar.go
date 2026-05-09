// Package ui implements the Clyde terminal user interface.
//
// The panel renders three regions stacked top to bottom: a header
// strip with the visible session title and optional live URL, a scrolling
// message buffer fed by a daemon live-session stream, and a single
// line input that posts user messages back via the daemon. Bottom
// stick scroll keeps the latest message visible while the user is at
// the end; manual scrolling pauses the auto follow until the user hits
// End again.
package ui

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
)

// SidecarPanel is the live view widget for the third "Sidecar" tab.
type SidecarPanel struct {
	SessionName string
	SessionID   string
	LiveURL     string
	Buffer      []SidecarLine
	BufferLimit int
	Input       *TextInput
	rect        Rect
	bodyRect    Rect
	inputRect   Rect
	auto        bool // bottom-stick scroll
	offset      int  // index of first visible line
	focus       sidecarFocus
	status      string // last delivery status ("sent", "no listener", "error: ...")
	OnSend      func(text string) error
}

// SidecarLine is one rendered row in the sidecar buffer.
type SidecarLine struct {
	Role string
	Text string
	When string // pre formatted clock string for the leading column
}

type sidecarFocus int

const (
	sidecarFocusBody sidecarFocus = iota
	sidecarFocusInput
)

// NewSidecarPanel constructs a panel with the input ready for typing.
// The buffer is empty until the streaming subscriber pushes lines in.
func NewSidecarPanel(name, sessionID, bridgeURL string) *SidecarPanel {
	in := NewTextInput("> ")
	return &SidecarPanel{
		SessionName: name,
		SessionID:   sessionID,
		LiveURL:     bridgeURL,
		BufferLimit: 500,
		Input:       in,
		auto:        true,
		focus:       sidecarFocusInput,
	}
}

// Append adds one line to the buffer and trims to the limit. The
// auto follow flag controls whether the cursor jumps to the new line.
func (p *SidecarPanel) Append(line SidecarLine) {
	p.Buffer = append(p.Buffer, line)
	if p.BufferLimit > 0 && len(p.Buffer) > p.BufferLimit {
		p.Buffer = p.Buffer[len(p.Buffer)-p.BufferLimit:]
	}
}

func (p *SidecarPanel) Draw(scr tcell.Screen, r Rect) {
	p.rect = r
	if r.W < 10 || r.H < 5 {
		return
	}
	headerH := 2
	inputH := 2
	bodyH := max(r.H-headerH-inputH, 1)
	headerRect := Rect{X: r.X, Y: r.Y, W: r.W, H: headerH}
	bodyRect := Rect{X: r.X, Y: r.Y + headerH, W: r.W, H: bodyH}
	inputRect := Rect{X: r.X, Y: r.Y + headerH + bodyH, W: r.W, H: inputH}
	p.bodyRect = bodyRect
	p.inputRect = inputRect

	p.drawHeader(scr, headerRect)
	p.drawBody(scr, bodyRect)
	p.drawInput(scr, inputRect)
}

func (p *SidecarPanel) drawHeader(scr tcell.Screen, r Rect) {
	fillRow(scr, r.X, r.Y, r.W, StyleHeaderBar)
	left := fmt.Sprintf(" sidecar: %s ", p.SessionName)
	drawString(scr, r.X, r.Y, StyleHeaderBar.Bold(true), left, r.W)
	if p.LiveURL != "" {
		bridge := "  url: " + p.LiveURL
		x := r.X + runeCount(left)
		drawString(scr, x, r.Y, StyleHeaderBar, bridge, r.W-(x-r.X))
	}
	hint := "tab: focus  esc: dashboard  enter: send"
	if p.status != "" {
		hint = "[" + p.status + "]  " + hint
	}
	drawString(scr, r.X, r.Y+1, StyleMuted, "  "+hint, r.W)
}

func (p *SidecarPanel) drawBody(scr tcell.Screen, r Rect) {
	clearRect(scr, r)
	if len(p.Buffer) == 0 {
		ClockLoadingSpinner("waiting for transcript lines...").Draw(scr, r.X+2, r.Y+1, r.W-4)
		return
	}
	if p.auto {
		p.offset = imax(0, len(p.Buffer)-r.H)
	}
	end := min(p.offset+r.H, len(p.Buffer))
	y := r.Y
	for i := p.offset; i < end; i++ {
		line := p.Buffer[i]
		roleStyle := StyleSubtext
		switch line.Role {
		case "user":
			roleStyle = StyleDefault.Foreground(ColorAccent).Bold(true)
		case "assistant":
			roleStyle = StyleDefault.Foreground(ColorSuccess).Bold(true)
		case "system":
			roleStyle = StyleMuted
		}
		head := fmt.Sprintf("%s  %-9s ", line.When, line.Role)
		used := drawString(scr, r.X+1, y, roleStyle, head, r.W-2)
		body := strings.ReplaceAll(line.Text, "\n", "  ")
		drawString(scr, r.X+1+used, y, StyleDefault, body, r.W-2-used)
		y++
	}
}

func (p *SidecarPanel) drawInput(scr tcell.Screen, r Rect) {
	fillRow(scr, r.X, r.Y, r.W, StyleStatusBar)
	prefix := " send: "
	style := StyleStatusBar.Bold(true)
	if p.focus == sidecarFocusInput {
		style = style.Foreground(ColorAccent)
	}
	drawString(scr, r.X, r.Y, style, prefix, r.W)
	box := Rect{X: r.X + runeCount(prefix), Y: r.Y, W: r.W - runeCount(prefix), H: 1}
	p.Input.Draw(scr, box)
	hint := "enter to send  ctrl-c clears"
	drawString(scr, r.X+2, r.Y+1, StyleMuted, hint, r.W-4)
}

func (p *SidecarPanel) HandleEvent(ev tcell.Event) bool {
	switch e := ev.(type) {
	case *tcell.EventKey:
		return p.handleKey(e)
	case *tcell.EventMouse:
		x, y := e.Position()
		switch {
		case p.bodyRect.Contains(x, y):
			p.focus = sidecarFocusBody
		case p.inputRect.Contains(x, y):
			p.focus = sidecarFocusInput
		}
	}
	return false
}

func (p *SidecarPanel) handleKey(e *tcell.EventKey) bool {
	if e.Key() == tcell.KeyTab {
		if p.focus == sidecarFocusInput {
			p.focus = sidecarFocusBody
		} else {
			p.focus = sidecarFocusInput
		}
		return true
	}
	if p.focus == sidecarFocusInput {
		if e.Key() == tcell.KeyEnter || e.Key() == tcell.KeyLF {
			text := strings.TrimSpace(p.Input.Text)
			if text == "" {
				return true
			}
			if p.OnSend != nil {
				if err := p.OnSend(text); err != nil {
					p.status = "error: " + err.Error()
				} else {
					p.status = "sent"
				}
			}
			p.Input.Text = ""
			p.Input.CursorX = 0
			return true
		}
		if e.Key() == tcell.KeyCtrlC {
			p.Input.Text = ""
			p.Input.CursorX = 0
			return true
		}
		return p.Input.HandleEvent(e)
	}
	// Body navigation.
	switch e.Key() {
	case tcell.KeyUp:
		p.scroll(-1)
		return true
	case tcell.KeyDown:
		p.scroll(+1)
		return true
	case tcell.KeyPgUp:
		p.scroll(-imax(1, p.bodyRect.H-1))
		return true
	case tcell.KeyPgDn:
		p.scroll(+imax(1, p.bodyRect.H-1))
		return true
	case tcell.KeyHome:
		p.offset = 0
		p.auto = false
		return true
	case tcell.KeyEnd:
		p.auto = true
		return true
	case tcell.KeyRune:
		switch e.Rune() {
		case 'k':
			p.scroll(-1)
		case 'j':
			p.scroll(+1)
		case 'g':
			p.offset = 0
			p.auto = false
		case 'G':
			p.auto = true
		}
		return true
	}
	return false
}

func (p *SidecarPanel) scroll(delta int) {
	p.offset += delta
	if p.offset < 0 {
		p.offset = 0
	}
	maxOffset := imax(0, len(p.Buffer)-p.bodyRect.H)
	if p.offset > maxOffset {
		p.offset = maxOffset
	}
	p.auto = (p.offset >= maxOffset)
}
