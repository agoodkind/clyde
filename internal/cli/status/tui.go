package status

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"goodkind.io/clyde/internal/cli"
)

// The terminal view follows the lm-semantic-search status TUI: an alt-screen
// bubbletea program that refreshes on a tick, keeps the previous snapshot when
// a refresh fails, diffs integer counters against the prior read, and pins a
// header and key line around a scrollable body.

const (
	statusNameGap  = 2
	statusUnitWide = 14
	defaultWidth   = 120
	chromeRows     = 3
)

var (
	faintStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	headerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Bold(true)
	errorStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
)

type statusRefreshMsg struct {
	snapshot statusSnapshot
}

type statusTickMsg struct{}

type statusModel struct {
	// gather reads one fresh snapshot; the model holds the closure rather
	// than a context so refresh cancellation stays owned by the caller.
	gather     func() statusSnapshot
	build      string
	interval   time.Duration
	snapshot   statusSnapshot
	previous   map[string]int64
	comparable bool
	paused     bool
	refreshing bool
	quitting   bool
	offset     int
	width      int
	height     int
	refreshErr error
}

func newStatusModel(gather func() statusSnapshot, build string, interval time.Duration) statusModel {
	return statusModel{
		gather:     gather,
		build:      build,
		interval:   interval,
		snapshot:   gather(),
		previous:   nil,
		comparable: false,
		paused:     false,
		refreshing: false,
		quitting:   false,
		offset:     0,
		width:      defaultWidth,
		height:     0,
		refreshErr: nil,
	}
}

// runLive drives the bubbletea program until q, esc, or Ctrl-C.
func runLive(ctx context.Context, f *cli.Factory, build string, interval time.Duration) error {
	gather := func() statusSnapshot { return gatherSnapshot(ctx) }
	program := tea.NewProgram(
		newStatusModel(gather, build, interval),
		tea.WithAltScreen(),
		tea.WithContext(ctx),
		tea.WithInput(f.IOStreams.In),
		tea.WithOutput(f.IOStreams.Out),
	)
	if _, err := program.Run(); err != nil {
		slog.ErrorContext(ctx, "cli.status.view_failed", "concern", "cmd.dispatch", "component", "cli", "err", err)
		return fmt.Errorf("run the status view: %w", err)
	}
	return nil
}

func (m statusModel) Init() tea.Cmd {
	return m.tick()
}

func (m statusModel) tick() tea.Cmd {
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return statusTickMsg{} })
}

func (m statusModel) refresh() tea.Cmd {
	gather := m.gather
	return func() tea.Msg {
		return statusRefreshMsg{snapshot: gather()}
	}
}

func (m statusModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch typed := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = typed.Width
		m.height = typed.Height
		return m, nil
	case statusTickMsg:
		if m.paused || m.refreshing {
			return m, m.tick()
		}
		m.refreshing = true
		return m, tea.Batch(m.refresh(), m.tick())
	case statusRefreshMsg:
		m = m.applyRefresh(typed.snapshot)
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(typed)
	}
	return m, nil
}

// applyRefresh installs the new snapshot and moves the delta baseline. A
// supervisor restart drops the baseline so counters resetting to zero do not
// render as large negative deltas.
func (m statusModel) applyRefresh(snapshot statusSnapshot) statusModel {
	m.refreshing = false
	m.refreshErr = nil
	sameRun := m.snapshot.report.SupervisorPID == snapshot.report.SupervisorPID
	m.previous = intValuesByName(buildMetrics(m.snapshot))
	m.comparable = sameRun
	m.snapshot = snapshot
	return m
}

func (m statusModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch {
	case slices.Contains([]string{"ctrl+c", "q", "esc"}, key):
		m.quitting = true
		return m, tea.Quit
	case key == "p":
		m.paused = !m.paused
		if !m.paused {
			// The next read follows a gap, so its deltas would span the pause.
			m.comparable = false
		}
		return m, nil
	case key == "r":
		if m.refreshing {
			return m, nil
		}
		m.refreshing = true
		return m, m.refresh()
	case slices.Contains([]string{"up", "k"}, key):
		if m.offset > 0 {
			m.offset--
		}
		return m, nil
	case slices.Contains([]string{"down", "j"}, key):
		if m.offset < m.maxOffset() {
			m.offset++
		}
		return m, nil
	}
	return m, nil
}

func (m statusModel) bodyLines() []string {
	metrics := buildMetrics(m.snapshot)
	nameWidth := 0
	valueWidth := 0
	for _, metric := range metrics {
		nameWidth = max(nameWidth, len(metric.name))
		valueWidth = max(valueWidth, len(metric.value))
	}
	lines := make([]string, 0, len(metrics)+4)
	previousGroup := ""
	for _, metric := range metrics {
		if previousGroup != "" && metric.group != previousGroup {
			lines = append(lines, "")
		}
		previousGroup = metric.group
		lines = append(lines, m.metricLine(metric, nameWidth, valueWidth))
	}
	return lines
}

func (m statusModel) metricLine(metric statusMetric, nameWidth, valueWidth int) string {
	name := padTo(metric.name, nameWidth)
	value := padLeftTo(metric.value, valueWidth)
	unit := padTo(metric.unit, statusUnitWide)
	delta := ""
	if metric.isInt && m.comparable && m.previous != nil {
		if before, seen := m.previous[metric.name]; seen {
			delta = deltaText(metric.intValue - before)
		}
	}
	line := strings.TrimRight(name+strings.Repeat(" ", statusNameGap)+value+strings.Repeat(" ", statusNameGap)+unit+delta, " ")
	if m.width > 0 && len(line) > m.width {
		line = line[:m.width]
	}
	if strings.HasSuffix(metric.name, ".error") {
		return errorStyle.Render(line)
	}
	if delta == "" || delta == "+0" {
		return faintStyle.Render(line)
	}
	return line
}

func deltaText(delta int64) string {
	if delta > 0 {
		return "+" + strconv.FormatInt(delta, 10)
	}
	if delta < 0 {
		return strconv.FormatInt(delta, 10)
	}
	return "+0"
}

func intValuesByName(metrics []statusMetric) map[string]int64 {
	values := make(map[string]int64, len(metrics))
	for _, metric := range metrics {
		if metric.isInt {
			values[metric.name] = metric.intValue
		}
	}
	return values
}

func (m statusModel) headerLines() []string {
	header := "clyde  version=" + m.build
	if m.snapshot.report.SupervisorPID > 0 {
		header += "  pid=" + strconv.Itoa(m.snapshot.report.SupervisorPID)
	}
	stateLine := "read_at=" + m.snapshot.readAt.Format("15:04:05") + "  interval=" + m.interval.String()
	if m.paused {
		stateLine += "  paused"
	}
	if m.refreshErr != nil {
		stateLine += "  refresh_error=" + strconv.Quote(m.refreshErr.Error())
	}
	return []string{headerStyle.Render(header), faintStyle.Render(stateLine), ""}
}

func (m statusModel) visibleBodyRows() int {
	if m.height <= 0 {
		return 1 << 30
	}
	rows := m.height - len(m.headerLines()) - chromeRows
	if rows < 1 {
		return 1
	}
	return rows
}

func (m statusModel) maxOffset() int {
	overflow := len(m.bodyLines()) - m.visibleBodyRows()
	if overflow < 0 {
		return 0
	}
	return overflow
}

func (m statusModel) View() string {
	if m.quitting {
		return ""
	}
	body := m.bodyLines()
	offset := min(m.offset, m.maxOffset())
	visible := m.visibleBodyRows()
	end := min(offset+visible, len(body))
	frame := make([]string, 0, len(body)+6)
	frame = append(frame, m.headerLines()...)
	frame = append(frame, body[offset:end]...)
	if remaining := len(body) - end; remaining > 0 {
		frame = append(frame, faintStyle.Render("  "+strconv.Itoa(remaining)+" more lines below"))
	} else {
		frame = append(frame, "")
	}
	frame = append(frame, faintStyle.Render("up/down scroll   p pause   r refresh   q quit"))
	return strings.Join(frame, "\n")
}

func padTo(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(value))
}

func padLeftTo(value string, width int) string {
	if len(value) >= width {
		return value
	}
	return strings.Repeat(" ", width-len(value)) + value
}
