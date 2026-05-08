// Package ui implements the clyde TUI using raw tcell.
//
// Architecture:
//   - One tcell.Screen owns the terminal.
//   - One event loop reads screen.PollEvent and dispatches events.
//   - Widgets (table, textbox, statusbar, modal, input) implement a small
//     Widget interface. The App computes a Rect for each and calls Draw.
//   - Mouse hit testing is direct: the App checks which widget's Rect
//     contains the click coordinates. No InRect delegation chains.
//   - Terminal tab switches are handled via tcell.EventFocus. A full redraw
//     on refocus keeps the TUI responsive after switching terminal tabs.
package ui

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash/fnv"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"

	"goodkind.io/clyde/internal/binaryhandoff"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/terminalcontrol"
	"goodkind.io/clyde/internal/util"
	gklogversion "goodkind.io/gklog/version"
)

// ---------------- Public API ----------------

// AppOptions tweaks the startup behavior of the main TUI. Every field is
// optional. Zero values preserve the normal dashboard flow.
type AppOptions struct {
	// Context carries command-level request, trace, and span identifiers into
	// TUI lifecycle logs. Callback-specific daemon work should create child
	// spans before making RPC calls.
	Context context.Context
	// ReturnTo, when non-nil, pre-selects the given session in the table.
	// The header banner prompts the user to resume or pick something else.
	ReturnTo *session.Session
	// DashboardLaunchCWD is the process working directory when the dashboard
	// started. It is the default for "new session" without opening the picker.
	DashboardLaunchCWD string
	// LaunchBasedir biases the startup dashboard toward sessions rooted at
	// this basedir while keeping global sessions visible underneath.
	LaunchBasedir string
}

// AppCallbacks provides hooks the TUI calls to perform actions.
// Kept identical to the previous tview API so cmd/root.go needs no changes.
type AppCallbacks struct {
	ResumeSession func(sess *session.Session) error
	DeleteSession func(sess *session.Session) error
	ForkSession   func(sess *session.Session) error
	RenameSession func(sess *session.Session) (string, error)
	StartSession  func() error
	// StartSessionWithBasedir creates a new session pinned to the given
	// workspace root. Empty string falls back to the caller's cwd.
	StartSessionWithBasedir func(basedir string) error
	// StartIncognitoWithBasedir launches an incognito session pinned
	// to basedir. The session auto deletes on exit unless persisted
	// later.
	StartIncognitoWithBasedir func(basedir string, liveURL bool) error
	// ListLiveSessions returns provider-neutral live sessions from the daemon.
	ListLiveSessions func() ([]LiveSession, error)
	// StartLiveSession launches a provider-neutral daemon-owned live session.
	StartLiveSession func(req LiveSessionStartRequest) (LiveSession, error)
	// SendLiveSession sends text to a provider-neutral live session.
	SendLiveSession func(sessionID, text string) error
	// StreamLiveSession subscribes to provider-neutral live session events.
	StreamLiveSession func(sessionID string) (<-chan LiveSessionEvent, func(), error)
	// StopLiveSession stops a provider-neutral live session.
	StopLiveSession func(sessionID string) error
	// SetBasedir rewrites the session's workspaceRoot field in metadata.
	// newPath is already resolved by the caller; "" clears the field.
	SetBasedir func(sess *session.Session, newPath string) error
	// LoadConfigControls returns daemon-owned config controls rendered by
	// the Settings tab.
	LoadConfigControls func() ([]ConfigControl, error)
	// UpdateConfigControl persists one config control change via the daemon.
	UpdateConfigControl func(key, value string) error
	// UpdateSessionContextWindow persists one Claude Code context-window
	// choice for one session. Empty means provider default behavior.
	UpdateSessionContextWindow func(sess *session.Session, contextWindow string) error
	// ListSessions returns the daemon-owned dashboard snapshot.
	ListSessions func() (SessionSnapshot, error)
	// LoadStats returns asynchronously rendered dashboard-wide cache stats.
	LoadStats func() (DashboardStats, error)
	// SubscribeProviderStats streams provider-level stats updates from the daemon.
	SubscribeProviderStats func() (events <-chan ProviderStats, cancel func(), err error)
	// RestartDaemon asks the local daemon supervisor to self-heal and restart.
	RestartDaemon func() error
	// RefreshSummary triggers a background regeneration of the session's
	// Context field via the daemon. It should return quickly once the
	// request is queued. The returned sessions callback (may be nil)
	// fires once the updated metadata is persisted; the TUI uses it to
	// redraw the affected row.
	RefreshSummary   func(sess *session.Session, onDone func(*session.Session)) error
	GetSessionDetail func(sess *session.Session) (SessionDetail, error)
	ViewContent      func(sess *session.Session) string
	ExportSession    func(sess *session.Session, req SessionExportRequest) ([]byte, error)
	LoadExportStats  func(sess *session.Session) (SessionExportStats, error)
	// SubscribeRegistry, when set, opens a long-lived subscription to
	// the daemon's registry-event stream. Each event nudges the TUI to
	// reload sessions from disk so adoptions land immediately instead
	// of waiting for the polling watcher. The returned cancel function
	// runs when the TUI exits. Errors are silently tolerated: the
	// fallback poller still runs.
	SubscribeRegistry func() (events <-chan SessionEvent, cancel func(), err error)
	// CompactPreview streams compact progress events in preview mode.
	CompactPreview func(req CompactRunRequest) (events <-chan CompactEvent, done <-chan error, cancel func(), err error)
	// CompactApply streams compact progress events in apply mode.
	CompactApply func(req CompactRunRequest) (events <-chan CompactEvent, done <-chan error, cancel func(), err error)
	// CompactUndo rolls back the latest compact apply for a session.
	CompactUndo func(sessionName string) (*CompactUndoResult, error)
}

type SessionExportFormat string

const (
	SessionExportPlainText SessionExportFormat = "plain_text"
	SessionExportMarkdown  SessionExportFormat = "markdown"
	SessionExportHTML      SessionExportFormat = "html"
	SessionExportJSON      SessionExportFormat = "json"
)

type SessionExportWhitespaceCompression string

const (
	SessionExportWhitespacePreserve SessionExportWhitespaceCompression = "preserve"
	SessionExportWhitespaceTidy     SessionExportWhitespaceCompression = "tidy"
	SessionExportWhitespaceCompact  SessionExportWhitespaceCompression = "compact"
	SessionExportWhitespaceDense    SessionExportWhitespaceCompression = "dense"
)

type SessionExportRequest struct {
	SessionName            string
	Format                 SessionExportFormat
	HistoryStart           int
	WhitespaceCompression  SessionExportWhitespaceCompression
	IncludeChat            bool
	IncludeThinking        bool
	IncludeSystemPrompts   bool
	IncludeToolCalls       bool
	IncludeToolOutputs     bool
	IncludeRawJSONMetadata bool
	CopyToClipboard        bool
	SaveToFile             bool
	Directory              string
	Filename               string
	Overwrite              bool
}

type SessionExportStats struct {
	VisibleTokensEstimate int
	VisibleMessages       int
	UserMessages          int
	AssistantMessages     int
	ToolResultMessages    int
	ToolCalls             int
	SystemPrompts         int
	Compactions           int
	TranscriptSizeBytes   int64
}

type SessionSnapshot struct {
	Sessions      []*session.Session
	Models        map[string]string
	MessageCounts map[string]int
	ContextStates map[string]SessionContextState
	LiveURLs      []LiveURL
}

// SessionEvent is the UI-facing copy of the daemon SubscribeRegistryResponse. The
// ui package keeps its own type so the daemon's protobuf does not leak
// into widget code.
type SessionEvent struct {
	Kind          string
	SessionName   string
	SessionID     string
	OldName       string
	LiveURL       string
	Session       *session.Session
	Model         string
	MessageCount  int
	ContextState  *SessionContextState
	LiveURLRecord *LiveURL
	BinaryPath    string
	BinaryReason  string
	BinaryHash    string
}

type SessionContextState struct {
	Usage  SessionContextUsage
	Loaded bool
	Status string
}

type ConfigControl struct {
	Key          string
	Section      string
	Label        string
	Description  string
	Type         string
	Value        string
	DefaultValue string
	Options      []ConfigControlOption
	Sensitive    bool
	ReadOnly     bool
}

type ConfigControlOption struct {
	Value       string
	Label       string
	Description string
}

// LiveURL is the UI-facing provider-neutral URL for a live session.
type LiveURL struct {
	SessionID string
	URL       string
}

// TranscriptEntry carries one parsed line of a streamed transcript
// from the daemon. The widget renders Role and Text directly and uses
// Timestamp for the leading clock column.
type TranscriptEntry struct {
	ByteOffset int64
	RawJSONL   string
	Role       string
	Text       string
	Timestamp  time.Time
}

// LiveSession is the TUI-facing provider-neutral live session summary.
type LiveSession struct {
	Provider       string
	SessionName    string
	SessionID      string
	Status         string
	Basedir        string
	URL            string
	StartedAt      time.Time
	SupportsSend   bool
	SupportsStream bool
	SupportsStop   bool
}

// LiveSessionStartRequest carries provider-neutral launch intent from
// the sidecar UI to the daemon.
type LiveSessionStartRequest struct {
	Provider  string
	Name      string
	Basedir   string
	Model     string
	Effort    string
	Incognito bool
}

// LiveSessionEvent is one streamed provider-neutral sidecar event.
type LiveSessionEvent struct {
	SessionID string
	Kind      string
	Role      string
	Text      string
	Timestamp time.Time
}

// SessionDetail holds pre-extracted data for the details pane.
// Messages is the most-recent short list used in the original design;
// AllMessages carries the full transcript for the scrollable right pane.
// Tools ranks the top assistant tool uses for the stats pane.
type SessionDetail struct {
	Model                 string
	Messages              []DetailMessage // last N for quick peek (kept for backwards compat)
	AllMessages           []DetailMessage // full transcript, ordered oldest -> newest
	Tools                 []ToolUse       // descending by Count
	TotalMessages         int             // full visible user/assistant message count
	VisibleTokensEstimate int             // rough estimate of visible message tokens
	LastMessageTokens     int             // rough estimate of latest visible message tokens
	CompactionCount       int             // compact_boundary entries in this transcript
	LastPreCompactTokens  int             // pre-compact token count from the latest clyde compact boundary
	TranscriptSizeBytes   int64
	ConversationLoading   bool
	ContextUsage          SessionContextUsage
	ContextUsageLoaded    bool
	ContextUsageStatus    string
	TranscriptStatsLoaded bool
	TranscriptStatsStatus string
}

type ProviderStats struct {
	Provider                   string
	Requests                   int
	Inflight                   int
	Streaming                  int
	InputTokens                int64
	OutputTokens               int64
	CacheReadTokens            int64
	CacheCreationTokens        int64
	DerivedCacheCreationTokens int64
	HitRatio                   float64
	EstimatedCostMicrocents    int64
	LastSeen                   time.Time
	Error                      string
}

type DashboardStats struct {
	Providers []ProviderStats
	LoadedAt  time.Time
	StreamErr string
}

// SessionContextUsage is the exact no-persist /context payload used by the
// details pane once the async probe completes.
type SessionContextUsage struct {
	Model          string
	TotalTokens    int
	MaxTokens      int
	Percentage     int
	MessagesTokens int
}

// DetailMessage is a simplified message for display.
type DetailMessage struct {
	Role      string // "user" or "assistant"
	Text      string
	Timestamp time.Time // zero when unknown
}

// ToolUse is a tool name and usage count inside a session.
type ToolUse struct {
	Name  string
	Count int
}

// scrollGrab identifies which scrollbar the user is currently dragging.
type scrollGrab int

const (
	grabNone scrollGrab = iota
	grabTable
	grabDetailsLeft
	grabDetailsRight
)

const (
	tabSessions = iota
	tabStats
	tabSettings
	tabSidecar
)

// SortColumn identifies which column the table is sorted by.
type SortColumn int

const (
	SortColName SortColumn = iota
	SortColWorkspace
	SortColModel
	SortColMessages
	SortColSummary
	SortColUsed
	SortColCreated
)

// ---------------- App ----------------

// App is the main tcell TUI.
type App struct {
	screenMu sync.RWMutex
	screen   tcell.Screen
	cb       AppCallbacks
	clock    uiClock
	ctx      context.Context

	// Widgets
	tabs    *TabBarWidget
	table   *TableWidget
	details *DetailsView
	status  *StatusBarWidget

	// activeTab indexes into tabs.Tabs. 0 is the sessions dashboard. 1
	// is the stats snapshot. 2 is the settings editor stub. 3 is the
	// sidecar live view. The
	// dashboard renders the table view when activeTab is 0; other
	// indices replace the body with a tab specific panel.
	activeTab int

	// sidecar holds the live remote control panel. nil until the user
	// pins a session by pressing S on a row. Recreated when the user
	// pivots to a different session.
	sidecar             *SidecarPanel
	sidecarSessionID    string
	sidecarSessionName  string
	sidecarTailPending  bool
	sidecarCancel       func() // cancels the daemon live-session stream
	compactCancel       func() // cancels in-flight compact preview/apply stream
	reloadExecPath      string // set when the on-disk executable changed and Run should exec after teardown
	pendingReloadPath   string // deferred self-reload while an overlay is open
	pendingReloadReason string
	terminalRestorer    *terminalcontrol.ProcessRestorer

	// Overlays (one at a time)
	overlay Widget
	// overlayStack remembers overlays under the current one. Pushing a
	// new overlay on top preserves the bottom layer so dismissing the
	// top returns the user where they came from instead of dropping
	// back to the dashboard.
	overlayStack []overlayFrame

	// Rects (recomputed on resize)
	headerRect Rect
	tableRect  Rect
	detailRect Rect
	statusRect Rect

	// Mode and state
	mode          StatusMode
	selected      *session.Session
	sessions      []*session.Session
	visibleIdx    []int // indexes into sessions after filter
	tableRowIdx   []int // maps rendered table rows to sessions; -1 is a section separator
	filter        string
	sortCol       SortColumn
	sortAsc       bool
	showEphemeral bool // when false (default), hide sessions from test/tmp workspaces
	hiddenCount   int  // number of sessions hidden by the ephemeral filter

	// Caches
	modelCache        map[string]string
	messageCountCache map[string]int
	contextStateCache map[string]SessionContextState
	configControls    []ConfigControl
	configSelected    int
	configLoading     bool
	configErr         string
	// liveURLs holds daemon-emitted live URLs keyed by provider session ID.
	// Claude currently sources these from its bridge runtime; the TUI treats
	// them as provider-neutral live session URLs.
	liveURLMu sync.RWMutex
	liveURLs  map[string]LiveURL

	// daemonOnline tracks whether the daemon-backed registry stream is
	// currently healthy. The dashboard keeps running when false and the
	// status bar surfaces "daemon offline" so users know live bridge and
	// registry updates are degraded.
	daemonMu       sync.RWMutex
	daemonOnline   bool
	daemonLastErr  string
	daemonLastSeen time.Time

	// detailCache stores the fully-extracted SessionDetail keyed by session
	// name. Populated off the UI goroutine by loadDetailAsync so repeat
	// selections render instantly. detailLoading tracks sessions whose
	// load is in flight, guarding against duplicate goroutines.
	detailCache   map[string]SessionDetail
	detailLoading map[string]bool
	// detailRepollAfter[name] holds the earliest wall-clock time at
	// which the session may be re-polled while its cached detail
	// still has a pending status (probing/cooldown). Throttles the
	// background poll so a slow daemon probe does not turn into a
	// 100ms RPC storm.
	detailRepollAfter map[string]time.Time
	detailMu          sync.Mutex

	// exportStatsCache stores daemon-derived export aggregates keyed by
	// session name. Like detail loading, requests are coalesced so the
	// export panel can open immediately and hydrate asynchronously.
	exportStatsCache   map[string]SessionExportStats
	exportStatsLoading map[string]bool

	// spinnerFrame increments on each redraw that is waiting for async
	// data so the user sees motion in the details header.
	spinnerFrame int

	// drawCount/lastDrawAt track presentation progress as seen by draw().
	// health ticker interrupts use these to detect "event loop alive but
	// not producing fresh frames" situations.
	drawCount            uint64
	lastDrawAt           time.Time
	lastDrawSpinner      int
	lastEventType        string
	lastEventAt          time.Time
	lastNonInterruptType string
	lastNonInterruptAt   time.Time
	healthLastDraw       uint64
	healthLastSpin       int
	heartbeatStartedAt   time.Time
	lastHeartbeatAt      time.Time

	// summaryRefreshing tracks session names whose summary refresh is
	// in flight, so repeated highlights do not spawn duplicate requests.
	summaryRefreshing map[string]bool

	// lastUsedTickerSeen records the last observed transcript mtime
	// per session so the live ticker only updates rows whose mtime
	// actually advanced. recentlyUpdatedAt records the wall clock
	// time of the most recent observed change so the table can
	// briefly highlight that row without a full re-sort.
	lastUsedMu         sync.Mutex
	lastUsedTickerSeen map[string]time.Time
	recentlyUpdatedAt  map[string]time.Time

	// lastInteraction records the last time a keyboard or mouse event
	// was handled. The background session watcher consults it and skips
	// a soft refresh when the user is mid-scroll or mid-click. The
	// value is read and written only from the event-loop goroutine
	// except for the watcher, which treats it as best-effort.
	lastInteraction time.Time
	interactionMu   sync.Mutex

	// sessionsLoading gates in-flight daemon snapshot requests so rapid
	// refresh triggers coalesce without spawning redundant RPC calls.
	sessionsLoadingMu sync.Mutex
	sessionsLoading   bool
	startupLoading    bool

	// statsLoading gates the async Stats-tab snapshot so the render path never
	// blocks on log or transcript scans.
	statsMu      sync.Mutex
	stats        DashboardStats
	statsLoaded  bool
	statsLoading bool
	statsErr     string

	// Double click tracking
	lastClickTime time.Time
	lastClickRow  int

	// Scrollbar grab: which bar the user is currently dragging. Cleared
	// when any button release happens (buttons == 0).
	grab scrollGrab

	// Event loop control
	running bool
	// appFocused tracks the terminal focus state from EventFocus so we can
	// avoid high-frequency spinner redraws while unfocused.
	appFocused bool

	// suspendImpl is the test seam for suspendAndRun. Production
	// builds set this to a wrapper around the real method that
	// tears down the tcell screen, runs fn (which may exec claude),
	// and reinitializes. Tests override it with a no-op or a
	// `fn()`-only callback so the resume cycle can be exercised
	// without touching a real terminal.
	suspendImpl func(fn func())

	// dashboardLaunchCWD is cwd when clyde started the TUI; used for New chat default.
	dashboardLaunchCWD string
	// launchBasedir is set for `clyde <dir>` and ranks matching sessions
	// before the rest of the global list.
	launchBasedir string

	// Return flow trace state is used to stitch resume/prompt freeze
	// paths across event-loop boundaries and make state transitions
	// observable in slog JSON traces.
	returnPathState   returnPathState
	returnPathSession *session.Session

	// Last layout dimensions logged for resize diagnostics.
	lastLayoutW int
	lastLayoutH int

	// buildHash identifies stamped build metadata, executableHash
	// fingerprints the actual running file, and runHash identifies this
	// process instance so operators can tell which wrapper is painting.
	buildHash      string
	executableHash string
	runHash        string
	executableMu   sync.Mutex
	executableBase executableSnapshot

	// pendingResizeDisplaySync is set on every EventResize so the next draw
	// in the run loop is followed by Screen.Sync. A debounced only approach
	// let many Show-only frames through during an active drag and the host
	// could desync from the tcell buffer.
	pendingResizeDisplaySync bool
	inResizeEvent            bool
}

// returnPathState captures the resumability lifecycle during pause/return.
type returnPathState string

const (
	returnPathStateDashboardActive     returnPathState = "dashboard_active"
	returnPathStateSuspendedForResume  returnPathState = "suspended_for_resume"
	returnPathStateResumingTerminal    returnPathState = "resuming_terminal"
	returnPathStateReturnPromptPending returnPathState = "return_prompt_pending"
	returnPathStateReturnPromptVisible returnPathState = "return_prompt_visible"
)

// NewApp creates and returns the clyde TUI.
func NewApp(sessions []*session.Session, cb AppCallbacks, opts ...AppOptions) *App {
	applyTUITheme(detectTerminalTheme())
	var opt AppOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	clock := defaultUIClock
	now := clock.Now()
	a := new(App)
	a.cb = cb
	a.clock = clock
	a.ctx = opt.Context
	a.sessions = sessions
	a.mode = StatusBrowse
	a.initCaches()
	a.sortCol = SortColUsed
	a.sortAsc = false
	a.returnPathState = returnPathStateDashboardActive
	a.daemonOnline = false
	a.startupLoading = len(sessions) == 0
	a.configLoading = cb.LoadConfigControls != nil
	a.appFocused = true
	a.lastNonInterruptAt = now
	a.heartbeatStartedAt = now
	a.lastHeartbeatAt = now
	a.initRuntimeIdentity(now)
	if a.ctx == nil {
		a.ctx = context.Background()
	}
	// Default suspendImpl: real teardown / exec / reinit. Tests
	// replace this before driving events so the resume cycle can be
	// exercised without touching a real terminal.
	a.suspendImpl = a.suspendAndRun
	a.dashboardLaunchCWD = strings.TrimSpace(opt.DashboardLaunchCWD)
	a.launchBasedir = session.CanonicalWorkspaceRoot(opt.LaunchBasedir)

	// Seed visible indexes with all sessions, unsorted for now.
	a.rebuildVisible()
	a.sortSessions()

	// Build widgets
	a.tabs = NewTabBar([]string{"Sessions", "Stats", "Settings", "Sidecar"})
	a.tabs.OnActivate = func(idx int) { a.activeTab = idx }
	a.table = NewTableWidget([]string{"NAME", "BASEDIR", "LAST ACTIVE", "MODEL", "MSGS", "SUMMARY", "CREATED"})
	a.table.SortCol = int(a.sortCol)
	a.table.SortAsc = a.sortAsc
	a.table.OnActivate = func(row int) { a.openSessionOptions(row) }
	a.table.OnSelect = func(row int) {
		a.trackSelection(row)
	}
	a.details = NewDetailsView()
	a.details.LookupLiveURL = a.liveURLRecordFor
	a.status = &StatusBarWidget{Mode: StatusBrowse}

	a.populateTable()
	if cb.LoadConfigControls != nil {
		a.refreshConfigControls()
	}

	// If a ReturnTo session is provided, pre-select its row and set the
	// banner. The row is located after any sorting or filtering so that
	// the activation highlights the correct index.
	if opt.ReturnTo != nil {
		a.returnPathSession = opt.ReturnTo
		for vi, idx := range a.visibleIdx {
			if sessionMatchesLookup(a.sessions[idx], opt.ReturnTo.Name, opt.ReturnTo.Metadata.ProviderSessionID()) {
				a.table.Active = true
				a.table.SelectedRow = vi
				a.table.Offset = vi
				break
			}
		}
		a.openReturnPrompt(opt.ReturnTo)
	}
	return a
}

func (a *App) initCaches() {
	a.modelCache = make(map[string]string)
	a.messageCountCache = make(map[string]int)
	a.contextStateCache = make(map[string]SessionContextState)
	a.liveURLs = make(map[string]LiveURL)
	a.detailCache = make(map[string]SessionDetail)
	a.detailLoading = make(map[string]bool)
	a.detailRepollAfter = make(map[string]time.Time)
	a.exportStatsCache = make(map[string]SessionExportStats)
	a.exportStatsLoading = make(map[string]bool)
	a.summaryRefreshing = make(map[string]bool)
	a.lastUsedTickerSeen = make(map[string]time.Time)
	a.recentlyUpdatedAt = make(map[string]time.Time)
}

func (a *App) initRuntimeIdentity(now time.Time) {
	a.buildHash = shortStableHash(gklogversion.String())
	a.executableHash = currentExecutableHash()
	a.runHash = shortStableHash(fmt.Sprintf("%d:%d", os.Getpid(), now.UnixNano()))
}

func (a *App) isCurrentReturnPathSession(sess *session.Session) bool {
	if a.returnPathSession == nil || sess == nil {
		return false
	}
	return a.returnPathSession.Metadata.ProviderSessionID() == sess.Metadata.ProviderSessionID()
}

// resolveSession deterministically maps a session reference onto the current
// in-memory view. UUID takes precedence, then name, then the original pointer.
func (a *App) resolveSession(sess *session.Session) *session.Session {
	if sess == nil {
		return nil
	}
	if sess.Metadata.ProviderSessionID() != "" {
		for _, candidate := range a.sessions {
			if candidate.Metadata.ProviderSessionID() == sess.Metadata.ProviderSessionID() {
				return candidate
			}
		}
	}
	if resolved := a.findSessionByName(sess.Name); resolved != nil {
		return resolved
	}
	return sess
}

func (a *App) trackReturnPathState(state returnPathState, source string, sess *session.Session) {
	prev := a.returnPathState
	if prev == "" {
		prev = returnPathStateDashboardActive
	}
	if prev == state {
		return
	}
	a.returnPathState = state
	fields := []any{
		"component", "tui",
		"subcomponent", "return_path",
		"source", source,
		"from_state", string(prev),
		"to_state", string(state),
	}
	if sess != nil {
		fields = append(fields,
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
		)
	}
	tuiLog.Logger().Info("tui.return_path.transition", fields...)
}

func (a *App) closeReturnPrompt(sess *session.Session, source string) {
	a.overlay = nil
	if a.returnPathState == returnPathStateReturnPromptVisible || a.returnPathState == returnPathStateReturnPromptPending {
		a.trackReturnPathState(returnPathStateDashboardActive, "returnprompt."+source, sess)
	}
}

// openReturnPrompt shows the post-session modal with the same session
// actions as the normal options popup, plus return-path actions.
func (a *App) openReturnPrompt(sess *session.Session) {
	if sess == nil {
		tuiLog.Logger().Warn("returnprompt.open skipped", "reason", "nil_session")
		return
	}
	close := func() {
		a.closeReturnPrompt(sess, "closed")
	}
	if a.isCurrentReturnPathSession(sess) {
		a.trackReturnPathState(returnPathStateReturnPromptPending, "returnprompt.enter", sess)
	}
	topEntries := []OptionsModalEntry{
		{
			Label: "Quit clyde",
			Hint:  "q",
			Action: func() {
				a.closeReturnPrompt(sess, "quit")
				a.running = false
			},
		},
		{
			Label: "Return back to chat",
			Hint:  "enter",
			Action: func() {
				a.closeReturnPrompt(sess, "return_to_chat")
				a.resumeSession(sess)
			},
		},
	}
	bodyEntries := a.sessionOptionsEntriesWithoutResume(sess, close)
	statsSegments, statsLoading := a.buildSessionStatsSegments(sess)
	modal := NewOptionsModal("Session exited: "+sess.Name, bodyEntries)
	modal.TopEntries = topEntries
	// Re-run resetCursor now that TopEntries is populated. The
	// constructor walked an empty TopEntries and may have parked the
	// cursor on the first enabled body entry; without this call the
	// initial Enter press would activate that body entry instead of
	// the intended top-row "Quit clyde".
	modal.resetCursor()
	modal.Context = OptionsModalContextReturn
	modal.OnCancel = close
	modal.OnQuit = func() {
		a.closeReturnPrompt(sess, "quit")
		a.running = false
	}
	modal.StatsSegments = statsSegments
	modal.StatsLoading = statsLoading
	modal.StatsSessionName = sess.Name
	a.overlay = modal
	if a.isCurrentReturnPathSession(sess) {
		a.trackReturnPathState(returnPathStateReturnPromptVisible, "returnprompt.visible", sess)
	}
	tuiLog.Logger().Info("returnprompt.opened",
		"session", sess.Name,
		"overlay", fmt.Sprintf("%T", a.overlay),
		"screen", fmt.Sprintf("%p", a.screen))
}

func (a *App) ensureReturnPrompt(sess *session.Session, source string) {
	if sess == nil {
		tuiLog.Logger().Warn("returnprompt.ensure skipped", "source", source, "reason", "nil_session")
		return
	}
	if a.overlay != nil {
		if modal, ok := a.overlay.(*OptionsModal); !ok || modal.Context != OptionsModalContextReturn {
			a.trackReturnPathState(returnPathStateReturnPromptPending, "ensureReturnPrompt.deferred", sess)
			tuiLog.Logger().Info("returnprompt.ensure_deferred",
				"source", source,
				"session", sess.Name,
				"overlay", fmt.Sprintf("%T", a.overlay))
			return
		}
	}
	if modal, ok := a.overlay.(*OptionsModal); ok {
		if modal.Context == OptionsModalContextReturn {
			if a.isCurrentReturnPathSession(sess) {
				a.trackReturnPathState(returnPathStateReturnPromptVisible, "ensureReturnPrompt.while_visible", sess)
				return
			}
			tuiLog.Logger().Warn("returnprompt.ensure_reopen_mismatched_session",
				"source", source,
				"requested_session", sess.Name,
				"requested_session_id", sess.Metadata.ProviderSessionID(),
				"return_path_session", func() string {
					if a.returnPathSession == nil {
						return ""
					}
					return a.returnPathSession.Name
				}(),
				"return_path_session_id", func() string {
					if a.returnPathSession == nil {
						return ""
					}
					return a.returnPathSession.Metadata.ProviderSessionID()
				}(),
				"overlay", fmt.Sprintf("%T", a.overlay))
		}
	}
	tuiLog.Logger().Warn("returnprompt.ensure_reopen", "source", source, "session", sess.Name, "overlay", fmt.Sprintf("%T", a.overlay))
	a.trackReturnPathState(returnPathStateReturnPromptPending, "ensureReturnPrompt.reopen", sess)
	a.openReturnPrompt(sess)
}

func (a *App) runResumeLifecycle(sess *session.Session, source string) {
	if sess == nil || a.cb.ResumeSession == nil {
		return
	}
	resolved := a.resolveSession(sess)
	if resolved == nil {
		return
	}
	tuiLog.Logger().Info("resume.start", "session", resolved.Name, "uuid", resolved.Metadata.ProviderSessionID(), "path", source)
	a.returnPathSession = resolved
	a.trackReturnPathState(returnPathStateSuspendedForResume, source+".before_suspend", resolved)
	a.suspendImpl(func() {
		a.trackReturnPathState(returnPathStateResumingTerminal, source+".callback", resolved)
		_ = a.cb.ResumeSession(resolved)
	})
	tuiLog.Logger().Info("resume.exit", "session", resolved.Name, "path", source)
	a.trackReturnPathState(returnPathStateReturnPromptPending, source+".after_suspend", resolved)
	sessionForPrompt := a.resolveSession(resolved)
	if sessionForPrompt == nil {
		sessionForPrompt = resolved
	}
	a.returnPathSession = sessionForPrompt
	a.ensureReturnPrompt(sessionForPrompt, source)
	a.requestSessionsAsync(source + ".after_resume")
	if row := a.findVisibleRowByName(sessionForPrompt.Name); row >= 0 {
		a.table.Active = true
		a.table.SelectedRow = row
	}
}

// resumeSession is the row-agnostic resume path used when the prompt's
// row lookup fails (filter excludes the session, table not yet rebuilt,
// etc.).
func (a *App) resumeSession(sess *session.Session) {
	a.runResumeLifecycle(sess, "resumeSession")
}

func (a *App) cachedDetailForSession(sess *session.Session) (SessionDetail, bool) {
	if sess == nil {
		return SessionDetail{}, false
	}
	if !sessionCapabilities(sess).TranscriptExport {
		return SessionDetail{
			Model:                 valueOr(a.modelCache[sess.Name], "-"),
			ContextUsageStatus:    "unsupported",
			TranscriptStatsStatus: "unsupported",
		}, false
	}
	a.detailMu.Lock()
	cached, ok := a.detailCache[sess.Name]
	a.detailMu.Unlock()
	if ok {
		return cached, false
	}
	// Never block overlay opening on transcript extraction; schedule a
	// background load and render with a lightweight placeholder until
	// cache is warm.
	a.loadDetailAsync(sess)
	return SessionDetail{
		Model:                 valueOr(a.modelCache[sess.Name], "-"),
		ContextUsageStatus:    "loading...",
		TranscriptStatsStatus: "loading...",
	}, true
}

func (a *App) buildSessionStatsSegments(sess *session.Session) ([][]TextSegment, bool) {
	if sess == nil {
		return nil, false
	}
	detail, loading := a.cachedDetailForSession(sess)
	view := NewDetailsView()
	view.LookupLiveURL = a.liveURLRecordFor
	return view.buildLeft(sess, detail), loading
}

// valueOr returns v if it is non-empty and not a placeholder, else fallback.
func valueOr(v, fallback string) string {
	if v == "" || v == "-" {
		return fallback
	}
	return v
}

// Run starts the event loop.
func (a *App) Run() (err error) {
	if err := a.initScreen(); err != nil {
		return err
	}
	tuiLog.Logger().InfoContext(a.ctx, "tui.run.started", "screen", fmt.Sprintf("%p", a.screen))
	stopSIGQUITDump := installSIGQUITDumpHandler()
	defer stopSIGQUITDump()
	defer a.finishRun(&err)

	a.running = true
	a.draw()
	a.requestSessionsAsync("startup")

	if err := a.refreshExecutableBaseline(); err != nil {
		tuiLog.Logger().WarnContext(a.ctx, "tui.self_reload.snapshot_failed",
			"component", "tui",
			"err", err)
	}

	// Ticker that posts a spinner tick every 100ms. The handler only
	// triggers a redraw when something is actually loading, so an idle
	// dashboard does not waste CPU.
	stopTicker := a.startRunSupervisor("spinner_ticker", a.runSpinnerTicker)
	defer close(stopTicker)

	// Health ticker posts a low-rate interrupt used to emit liveness
	// summaries and detect frame stalls even when only spinner interrupts
	// are flowing.
	stopHealthTicker := a.startRunSupervisor("health_ticker", a.runHealthTicker)
	defer close(stopHealthTicker)

	stopReloadWatcher := a.startRunSupervisor("self_reload_watcher", a.runSelfReloadWatcher)
	defer close(stopReloadWatcher)

	stopSessionRefresh := a.startRunSupervisor("session_refresh_ticker", a.runSessionRefreshTicker)
	defer close(stopSessionRefresh)

	// Registry stream supervisor keeps daemon subscriptions healthy even
	// when the daemon restarts. The dashboard remains usable in offline
	// mode and the polling watcher above still refreshes snapshots.
	stopRegistry := a.startRunSupervisor("registry_supervisor", a.runRegistrySupervisor)
	defer close(stopRegistry)

	stopProviderStats := a.startRunSupervisor("provider_stats_supervisor", a.runProviderStatsSupervisor)
	defer close(stopProviderStats)

	// Idle sweeper that regenerates stale session summaries one at a
	// time while the user is inactive. Rate limited so it never floods
	// the daemon or the upstream LLM.
	stopSweep := a.startRunSupervisor("idle_summary_sweeper", a.runIdleSummarySweeper)
	defer close(stopSweep)

	a.runEventLoop(stopTicker)
	return nil
}

type runSupervisorFunc func(<-chan struct{})

type runEventLoopState struct {
	nilEventStreak          int
	reinitAttemptedAfterNil bool
}

type runEventProfile struct {
	eventType          string
	isSpinnerInterrupt bool
	isHealthInterrupt  bool
	isIdleMouseMotion  bool
	stateChanged       bool
	handleDuration     time.Duration
	drawDuration       time.Duration
}

func (a *App) finishRun(err *error) {
	if a.sidecarCancel != nil {
		a.sidecarCancel()
		a.sidecarCancel = nil
	}
	if r := recover(); r != nil {
		a.teardownScreen()
		*err = fmt.Errorf("tui panic: %v", r)
		tuiLog.Logger().ErrorContext(a.ctx, "tui.run.panic",
			"component", "tui",
			"recover", fmt.Sprint(r),
			"err", *err)
		return
	}
	a.teardownScreen()
	a.execReloadAfterRun()
}

func (a *App) execReloadAfterRun() {
	if a.reloadExecPath == "" {
		return
	}
	if err := execCurrentProcess(a.reloadExecPath); err != nil {
		tuiLog.Logger().ErrorContext(a.ctx, "tui.self_reload.exec_failed",
			"component", "tui",
			"path", a.reloadExecPath,
			"err", err)
		_, _ = fmt.Fprintf(os.Stderr, "clyde self-reload failed: %v\n", err)
	}
}

func (a *App) startRunSupervisor(name string, run runSupervisorFunc) chan struct{} {
	stop := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic(name, fmt.Sprint(r))
			}
		}()
		run(stop)
	}()
	return stop
}

func (a *App) runEventLoop(stopTicker <-chan struct{}) {
	state := runEventLoopState{}
	for a.running {
		if a.screen == nil {
			tuiLog.Logger().ErrorContext(a.ctx, "tui.loop screen is nil, exiting", "err", "screen_nil")
			return
		}
		pollStartedAt := a.now()
		ev := a.pollEvent()
		pollDuration := time.Since(pollStartedAt)
		if ev == nil {
			if !a.handleNilRunEvent(&state, stopTicker) {
				return
			}
			continue
		}
		state.nilEventStreak = 0
		state.reinitAttemptedAfterNil = false
		profile := a.prepareRunEventDispatch(ev)
		if a.screen == nil {
			tuiLog.Logger().Debug("tui.loop screen became nil before dispatch")
			continue
		}
		profile = a.dispatchRunEvent(ev, profile)
		a.logRunEventTiming(profile, pollDuration)
	}
}

func (a *App) handleNilRunEvent(state *runEventLoopState, stopTicker <-chan struct{}) bool {
	if !a.running {
		return false
	}
	state.nilEventStreak++
	tuiLog.Logger().WarnContext(a.ctx, "tui.loop nil event with running=true",
		"nil_event_streak", state.nilEventStreak,
		"screen", fmt.Sprintf("%p", a.screen),
		"reinit_attempted", state.reinitAttemptedAfterNil)
	if state.nilEventStreak < 2 {
		tuiLog.Logger().Debug("tui.loop nil event temporary; sleeping briefly")
		return waitForTimer(stopTicker, 20*time.Millisecond)
	}
	if !state.reinitAttemptedAfterNil {
		return a.reinitAfterNilRunEvent(state)
	}
	tuiLog.Logger().ErrorContext(a.ctx, "tui.loop repeated nil events with running=true; terminating",
		"screen", fmt.Sprintf("%p", a.screen),
		"nil_event_streak", state.nilEventStreak,
		"err", "repeated_nil_events")
	a.running = false
	return true
}

func (a *App) reinitAfterNilRunEvent(state *runEventLoopState) bool {
	state.reinitAttemptedAfterNil = true
	tuiLog.Logger().WarnContext(a.ctx, "tui.loop attempting screen reinit after repeated nil events")
	a.teardownScreen()
	if err := a.initScreen(); err != nil {
		tuiLog.Logger().ErrorContext(a.ctx, "tui.loop reinit failed after repeated nil events", "error", err, "err", err)
		a.running = false
		return true
	}
	state.nilEventStreak = 0
	tuiLog.Logger().Debug("tui.loop reinit succeeded after repeated nil events", "screen", fmt.Sprintf("%p", a.screen))
	a.draw()
	return true
}

func (a *App) prepareRunEventDispatch(ev tcell.Event) runEventProfile {
	profile := runEventProfile{
		eventType:          fmt.Sprintf("%T", ev),
		isSpinnerInterrupt: false,
		isHealthInterrupt:  false,
		isIdleMouseMotion:  false,
		stateChanged:       false,
		handleDuration:     0,
		drawDuration:       0,
	}
	if interrupt, ok := ev.(*tcell.EventInterrupt); ok {
		switch interrupt.Data().(type) {
		case spinnerTick:
			profile.isSpinnerInterrupt = true
		case healthTick:
			profile.isHealthInterrupt = true
		}
	}
	profile.isIdleMouseMotion = a.isIdleMouseMotion(ev)
	a.lastEventType = profile.eventType
	a.lastEventAt = a.now()
	if profile.eventType != "*tcell.EventInterrupt" {
		a.lastNonInterruptType = profile.eventType
		a.lastNonInterruptAt = a.lastEventAt
	}
	if !profile.isSpinnerInterrupt && !profile.isHealthInterrupt && !profile.isIdleMouseMotion {
		tuiLog.Logger().Debug("tui.loop dispatching event", "event", profile.eventType)
	}
	return profile
}

func (a *App) dispatchRunEvent(ev tcell.Event, profile runEventProfile) runEventProfile {
	handleStartedAt := a.now()
	profile.stateChanged = a.handleEvent(ev)
	profile.handleDuration = time.Since(handleStartedAt)
	profile.drawDuration = a.drawAfterRunEvent(profile)
	return profile
}

func (a *App) drawAfterRunEvent(profile runEventProfile) time.Duration {
	if !a.shouldDrawAfterRunEvent(profile) {
		return 0
	}
	drawStartedAt := a.now()
	a.draw()
	drawDuration := time.Since(drawStartedAt)
	a.syncAfterResizeDraw()
	return drawDuration
}

func (a *App) shouldDrawAfterRunEvent(profile runEventProfile) bool {
	if !a.running {
		return false
	}
	if !profile.stateChanged {
		return false
	}
	if profile.isSpinnerInterrupt && !a.hasPendingVisualActivity() {
		return false
	}
	_, compactOverlay := a.overlay.(*CompactPanel)
	if profile.isSpinnerInterrupt && !a.appFocused && !compactOverlay {
		return false
	}
	return true
}

func (a *App) syncAfterResizeDraw() {
	if !a.pendingResizeDisplaySync || a.screen == nil {
		return
	}
	a.runTerminalCall("sync", func() {
		a.screen.Sync()
	})
	a.pendingResizeDisplaySync = false
	tuiLog.Logger().Debug("tui.display.synced_after_resize", "component", "tui")
}

func (a *App) logRunEventTiming(profile runEventProfile, pollDuration time.Duration) {
	if !profile.stateChanged && profile.handleDuration <= 20*time.Millisecond && profile.drawDuration == 0 {
		return
	}
	if profile.eventType == "*tcell.EventInterrupt" && profile.handleDuration <= 20*time.Millisecond && profile.drawDuration <= 35*time.Millisecond {
		return
	}
	logLevel := slog.LevelDebug
	if profile.handleDuration > 40*time.Millisecond || profile.drawDuration > 80*time.Millisecond || pollDuration > 1500*time.Millisecond {
		logLevel = slog.LevelWarn
	}
	tuiLog.Logger().Log(a.ctx, logLevel, "tui.loop.event_timing",
		"event", profile.eventType,
		"poll_ms", pollDuration.Milliseconds(),
		"handle_ms", profile.handleDuration.Milliseconds(),
		"draw_ms", profile.drawDuration.Milliseconds(),
		"active_tab", a.activeTab,
		"has_overlay", a.overlay != nil,
		"overlay_type", fmt.Sprintf("%T", a.overlay),
		"mode", int(a.mode))
}

// spinnerTick is posted periodically while something is loading so the
// UI can advance the spinner glyph.
type spinnerTick struct{}

// healthTick is posted periodically to emit watchdog liveness snapshots.
type healthTick struct{}

// runSpinnerTicker posts a spinnerTick every 100ms until stop is closed.
// The tick is cheap; handleEvent only repaints when data is actually
// pending so this does not burn CPU on an idle dashboard.
func (a *App) runSpinnerTicker(stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer func() { t.Stop() }()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			a.postInterrupt(spinnerTick{})
		}
	}
}

// runHealthTicker posts a healthTick every 25 seconds. The handler logs frame
// progress and warns when draw() has not advanced recently.
func (a *App) runHealthTicker(stop <-chan struct{}) {
	t := time.NewTicker(25 * time.Second)
	defer func() { t.Stop() }()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			a.postInterrupt(healthTick{})
		}
	}
}

func (a *App) runSelfReloadWatcher(stop <-chan struct{}) {
	if err := a.refreshExecutableBaseline(); err != nil {
		tuiLog.Logger().Warn("tui.self_reload.snapshot_failed",
			"component", "tui",
			"err", err)
		return
	}
	tuiLog.Logger().Debug("tui.self_reload.watcher_started",
		"component", "tui",
		"path", a.executableBaselinePath(),
		"mtime", a.executableBaselineModTime(),
		"size", a.executableBaselineSize())

	t := time.NewTicker(2 * time.Second)
	defer func() { t.Stop() }()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			changed, reason, err := a.executableChangedSinceBaseline()
			if err != nil {
				tuiLog.Logger().Debug("tui.self_reload.check_failed",
					"component", "tui",
					"path", a.executableBaselinePath(),
					"err", err)
				continue
			}
			if !changed {
				continue
			}
			a.postInterrupt(selfReloadAvailable{path: a.executableBaselinePath(), reason: reason})
			return
		}
	}
}

func (a *App) refreshExecutableBaseline() error {
	base, err := currentExecutableSnapshot()
	if err != nil {
		return err
	}
	a.executableMu.Lock()
	defer a.executableMu.Unlock()
	a.executableBase = base
	return nil
}

func (a *App) executableChangedSinceBaseline() (bool, string, error) {
	a.executableMu.Lock()
	base := a.executableBase
	a.executableMu.Unlock()
	if base.path == "" || base.info == nil {
		if err := a.refreshExecutableBaseline(); err != nil {
			return false, "", err
		}
		return false, "", nil
	}
	return executableChanged(base)
}

func (a *App) executableBaselinePath() string {
	a.executableMu.Lock()
	defer a.executableMu.Unlock()
	return a.executableBase.path
}

func (a *App) executableBaselineModTime() string {
	a.executableMu.Lock()
	defer a.executableMu.Unlock()
	if a.executableBase.info == nil {
		return ""
	}
	return a.executableBase.info.ModTime().Format(time.RFC3339Nano)
}

func (a *App) executableBaselineSize() int64 {
	a.executableMu.Lock()
	defer a.executableMu.Unlock()
	if a.executableBase.info == nil {
		return 0
	}
	return a.executableBase.info.Size()
}

func currentExecutableSnapshot() (executableSnapshot, error) {
	path, err := os.Executable()
	if err != nil {
		slog.Warn("tui.self_reload.executable_resolve_failed",
			"component", "tui",
			"err", err)
		return executableSnapshot{}, fmt.Errorf("resolve executable: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		slog.Warn("tui.self_reload.executable_stat_failed",
			"component", "tui",
			"path", path,
			"err", err)
		return executableSnapshot{}, fmt.Errorf("stat executable: %w", err)
	}
	if err := validateExecutableCandidate(path, info); err != nil {
		return executableSnapshot{}, err
	}
	return executableSnapshot{path: path, info: info}, nil
}

func executableChanged(base executableSnapshot) (bool, string, error) {
	info, err := os.Stat(base.path)
	if err != nil {
		return false, "", err
	}
	if err := validateExecutableCandidate(base.path, info); err != nil {
		return false, "", err
	}
	if !os.SameFile(base.info, info) {
		return true, "file_replaced", nil
	}
	if !info.ModTime().Equal(base.info.ModTime()) {
		return true, "mtime_changed", nil
	}
	if info.Size() != base.info.Size() {
		return true, "size_changed", nil
	}
	return false, "", nil
}

func validateExecutableCandidate(path string, info os.FileInfo) error {
	if info == nil {
		return fmt.Errorf("self-reload candidate has no file info: %s", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("self-reload candidate is not a regular file: %s", path)
	}
	if info.Size() <= 0 {
		return fmt.Errorf("self-reload candidate is empty: %s", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("self-reload candidate is not executable: %s", path)
	}
	return nil
}

func validateSelfReloadCandidate(path string) error {
	if err := binaryhandoff.ValidateClydeExecutableWithProbe(path, probeSelfReloadCandidate); err != nil {
		return fmt.Errorf("validate self-reload candidate: %w", err)
	}
	return nil
}

var probeSelfReloadCandidate = binaryhandoff.ProbeClydeExecutable

var execCurrentProcess = func(path string) error {
	tuiLog.Logger().Info("tui.self_reload.exec",
		"component", "tui",
		"path", path,
		"arg_count", len(os.Args))
	return syscall.Exec(path, os.Args, os.Environ())
}

func (a *App) execNewBinaryBeforeReinit(source string) bool {
	changed, reason, err := a.executableChangedSinceBaseline()
	if err != nil {
		tuiLog.Logger().Debug("tui.self_reload.reentry_check_failed",
			"component", "tui",
			"source", source,
			"path", a.executableBaselinePath(),
			"err", err)
		return false
	}
	if !changed {
		return false
	}
	path := a.executableBaselinePath()
	if err := validateSelfReloadCandidate(path); err != nil {
		tuiLog.Logger().Warn("tui.self_reload.reentry_rejected",
			"component", "tui",
			"source", source,
			"path", path,
			"reason", reason,
			"err", err)
		return false
	}
	a.exportReturnPromptForExec(source)
	tuiLog.Logger().Info("tui.self_reload.reentry_exec",
		"component", "tui",
		"source", source,
		"path", path,
		"reason", reason)
	if err := execCurrentProcess(path); err != nil {
		tuiLog.Logger().Warn("tui.self_reload.reentry_exec_failed",
			"component", "tui",
			"source", source,
			"path", path,
			"reason", reason,
			"err", err)
		return false
	}
	return true
}

func (a *App) exportReturnPromptForExec(source string) {
	if source != "suspend_return" || a.returnPathSession == nil {
		return
	}
	_ = os.Setenv(EnvTUIReturnSessionID, a.returnPathSession.Metadata.ProviderSessionID())
	_ = os.Setenv(EnvTUIReturnSessionName, a.returnPathSession.Name)
	tuiLog.Logger().Info("tui.self_reload.return_prompt_exported",
		"component", "tui",
		"session", a.returnPathSession.Name,
		"session_id", a.returnPathSession.Metadata.ProviderSessionID())
}

func (a *App) requestSelfReload(path, reason, source, hash string) {
	if path == "" {
		path = a.executableBaselinePath()
	}
	if source == "daemon_registry" && hash != "" && strings.TrimSpace(a.executableHash) != "" {
		pathHash := executableHashForPath(path)
		if pathHash != "" && pathHash != "unknown" && pathHash != hash {
			tuiLog.Logger().Warn("tui.self_reload.stale_daemon_event_ignored",
				"component", "tui",
				"source", source,
				"path", path,
				"reason", reason,
				"event_hash", hash,
				"path_hash", pathHash)
			return
		}
	}
	if err := validateSelfReloadCandidate(path); err != nil {
		tuiLog.Logger().Warn("tui.self_reload.rejected",
			"component", "tui",
			"source", source,
			"path", path,
			"reason", reason,
			"err", err)
		return
	}
	tuiLog.Logger().Info("tui.self_reload.requested",
		"component", "tui",
		"source", source,
		"path", path,
		"reason", reason,
		"hash", hash)
	if a.overlay != nil {
		a.pendingReloadPath = path
		a.pendingReloadReason = reason
		if panel, ok := a.overlay.(*CompactPanel); ok {
			panel.ApplyCompactEvent(CompactEvent{
				Kind:    "status",
				Message: "clyde update available; close compact panel to reload",
			})
		}
		return
	}
	a.reloadExecPath = path
	a.running = false
}

func (a *App) daemonBinaryUpdateAlreadyRunning(hash string) bool {
	hash = strings.TrimSpace(hash)
	if hash == "" || hash == "unknown" {
		return false
	}
	runningHash := strings.TrimSpace(a.executableHash)
	if runningHash == "" || runningHash == "unknown" {
		runningHash = currentExecutableHash()
	}
	if runningHash != hash {
		return false
	}
	tuiLog.Logger().Debug("tui.self_reload.daemon_event_ignored",
		"component", "tui",
		"reason", "already_running_binary",
		"hash", hash)
	return true
}

type compactStreamEvent struct {
	event CompactEvent
}

type compactStreamOpened struct {
	action string
	events <-chan CompactEvent
	done   <-chan error
	cancel func()
	err    error
}

type compactStreamDone struct {
	action string
	err    error
}

type compactUndoDone struct {
	result *CompactUndoResult
	err    error
}

type sidecarTailOpened struct {
	events <-chan TranscriptEntry
	cancel func()
	err    error
}

type sidecarSendDone struct {
	err error
}

type sidecarLaunchDone struct {
	sessionName string
	sessionID   string
	liveURL     string
	err         error
}

type sidecarStatusUpdate struct {
	status string
}

type viewContentLoaded struct {
	sessionName string
	content     string
}

type exportFinished struct {
	title string
	body  string
	stack bool
}

type exportStatsLoaded struct {
	name  string
	stats SessionExportStats
	err   error
}

type modelsPrewarmed struct {
	models map[string]string
}

type sessionsLoaded struct {
	snapshot SessionSnapshot
	err      error
	reason   string
}

type configControlsLoaded struct {
	controls []ConfigControl
	err      error
}

type detailsLoaded struct {
	name   string
	detail SessionDetail
	err    error
}

type summaryRefreshDone struct {
	name    string
	updated *session.Session
}

type registryEvent struct {
	event SessionEvent
}

type providerStatsEvent struct {
	stats ProviderStats
}

type tableSelectionAnchor struct {
	name        string
	sessionID   string
	row         int
	offset      int
	active      bool
	detailsOpen bool
}

type overlayFrame struct {
	widget Widget
	mode   StatusMode
}

type lastUsedTick struct{}

type idleSummarySweepTick struct{}

type selfReloadAvailable struct {
	path   string
	reason string
}

type executableSnapshot struct {
	path string
	info os.FileInfo
}

const terminalModeResetSequence = terminalcontrol.ModeResetSequence

const (
	EnvTUIReturnSessionID   = "CLYDE_TUI_RETURN_SESSION_ID"
	EnvTUIReturnSessionName = "CLYDE_TUI_RETURN_SESSION_NAME"
)

func enableAppMouse(scr tcell.Screen) {
	if scr == nil {
		return
	}
	scr.EnableMouse(tcell.MouseButtonEvents | tcell.MouseDragEvents | tcell.MouseMotionEvents)
}

func (a *App) runSessionRefreshTicker(stop <-chan struct{}) {
	<-stop
}

func (a *App) requestSessionsAsync(reason string) {
	if a.cb.ListSessions == nil {
		return
	}
	a.sessionsLoadingMu.Lock()
	if a.sessionsLoading {
		a.sessionsLoadingMu.Unlock()
		return
	}
	a.sessionsLoading = true
	a.sessionsLoadingMu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("sessions_load", fmt.Sprint(r))
			}
		}()
		started := a.now()
		snapshot, err := a.cb.ListSessions()
		tuiLog.Logger().Debug("tui.sessions.load.finished",
			"component", "tui",
			"reason", reason,
			"duration_ms", time.Since(started).Milliseconds(),
			"err", err)
		a.postInterrupt(sessionsLoaded{snapshot: snapshot, err: err, reason: reason})
	}()
}

func (a *App) refreshConfigControls() {
	if a.cb.LoadConfigControls == nil {
		return
	}
	a.configLoading = true
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("config_controls_load", fmt.Sprint(r))
			}
		}()
		controls, err := a.cb.LoadConfigControls()
		a.postInterrupt(configControlsLoaded{controls: controls, err: err})
	}()
}

func (a *App) applySessionSnapshot(snapshot SessionSnapshot) {
	selection := a.captureTableSelection()

	a.sessions = dedupeSessionList(snapshot.Sessions)
	a.modelCache = copyStringMap(snapshot.Models)
	a.messageCountCache = copyIntMap(snapshot.MessageCounts)
	a.contextStateCache = copyContextStateMap(snapshot.ContextStates)
	a.liveURLMu.Lock()
	clear(a.liveURLs)
	for _, b := range snapshot.LiveURLs {
		a.liveURLs[b.SessionID] = b
	}
	a.liveURLMu.Unlock()
	a.sortSessions()
	a.populateTable()
	a.restoreTableSelection(selection)
	if a.selected != nil {
		a.populateDetails()
	}
}

func (a *App) applySessionEvent(ev SessionEvent) {
	if ev.Kind == "CLYDE_BINARY_UPDATED" {
		if a.daemonBinaryUpdateAlreadyRunning(ev.BinaryHash) {
			return
		}
		a.requestSelfReload(ev.BinaryPath, ev.BinaryReason, "daemon_registry", ev.BinaryHash)
		return
	}

	selection := a.captureTableSelection()
	if ev.Kind == "SESSION_RENAMED" && selection.name == ev.OldName && ev.SessionName != "" {
		selection.name = ev.SessionName
	}

	if ev.LiveURLRecord != nil {
		a.liveURLMu.Lock()
		a.liveURLs[ev.LiveURLRecord.SessionID] = *ev.LiveURLRecord
		a.liveURLMu.Unlock()
	}

	switch ev.Kind {
	case "SESSION_ADOPTED", "SESSION_UPDATED":
		a.upsertSessionEvent(ev)
		if ev.Session != nil && a.sidecar != nil && (ev.Session.Name == a.sidecarSessionName || ev.Session.Metadata.ProviderSessionID() == a.sidecarSessionID) {
			a.maybeOpenSidecarTail()
		}
	case "SESSION_RENAMED":
		a.renameSessionEvent(ev)
	case "SESSION_DELETED":
		a.deleteSessionEvent(ev)
	case "BRIDGE_OPENED":
		a.liveURLMu.Lock()
		a.liveURLs[ev.SessionID] = LiveURL{
			SessionID: ev.SessionID,
			URL:       ev.LiveURL,
		}
		a.liveURLMu.Unlock()
		if a.sidecar != nil && a.sidecarSessionID == ev.SessionID {
			a.sidecar.LiveURL = ev.LiveURL
			a.maybeOpenSidecarTail()
		}
	case "BRIDGE_CLOSED":
		a.liveURLMu.Lock()
		delete(a.liveURLs, ev.SessionID)
		a.liveURLMu.Unlock()
		if a.sidecar != nil && a.sidecarSessionID == ev.SessionID {
			a.sidecar.LiveURL = ""
		}
	case "GLOBAL_SETTINGS_UPDATED":
		// Global defaults already applied above. Refresh the generic
		// descriptor view so Settings stays daemon-authoritative.
		a.refreshConfigControls()
	}

	a.sortSessions()
	a.populateTable()
	a.restoreTableSelection(selection)
	if a.selected != nil {
		a.populateDetails()
	}
}

func (a *App) upsertSessionEvent(ev SessionEvent) {
	if ev.Session == nil {
		return
	}
	filtered := a.sessions[:0]
	replaced := false
	for _, sess := range a.sessions {
		if sess.Name == ev.Session.Name || (ev.Session.Metadata.ProviderSessionID() != "" && sess.Metadata.ProviderSessionID() == ev.Session.Metadata.ProviderSessionID()) {
			if !replaced {
				filtered = append(filtered, ev.Session)
				replaced = true
			}
			continue
		}
		filtered = append(filtered, sess)
	}
	if !replaced {
		filtered = append(filtered, ev.Session)
	}
	a.sessions = dedupeSessionList(filtered)
	a.modelCache[ev.Session.Name] = ev.Model
	a.messageCountCache[ev.Session.Name] = ev.MessageCount
	if ev.ContextState != nil {
		a.contextStateCache[ev.Session.Name] = *ev.ContextState
	}
}

func (a *App) renameSessionEvent(ev SessionEvent) {
	if ev.Session != nil {
		filtered := a.sessions[:0]
		renamed := false
		for _, sess := range a.sessions {
			if sess.Name == ev.OldName || (ev.Session.Metadata.ProviderSessionID() != "" && sess.Metadata.ProviderSessionID() == ev.Session.Metadata.ProviderSessionID()) {
				if !renamed {
					filtered = append(filtered, ev.Session)
					renamed = true
				}
				continue
			}
			filtered = append(filtered, sess)
		}
		if !renamed {
			filtered = append(filtered, ev.Session)
		}
		a.sessions = dedupeSessionList(filtered)
		a.modelCache[ev.Session.Name] = ev.Model
		a.messageCountCache[ev.Session.Name] = ev.MessageCount
		if ev.ContextState != nil {
			a.contextStateCache[ev.Session.Name] = *ev.ContextState
		}
	}
	if ev.OldName != "" && ev.OldName != ev.SessionName {
		delete(a.modelCache, ev.OldName)
		delete(a.messageCountCache, ev.OldName)
		delete(a.contextStateCache, ev.OldName)
	}
}

func (a *App) deleteSessionEvent(ev SessionEvent) {
	filtered := a.sessions[:0]
	for _, sess := range a.sessions {
		if ev.SessionName != "" && sess.Name == ev.SessionName {
			continue
		}
		if ev.SessionID != "" && sess.Metadata.ProviderSessionID() == ev.SessionID {
			continue
		}
		filtered = append(filtered, sess)
	}
	a.sessions = filtered
	if ev.SessionName != "" {
		delete(a.modelCache, ev.SessionName)
		delete(a.messageCountCache, ev.SessionName)
		delete(a.contextStateCache, ev.SessionName)
	}
}

func dedupeSessionList(in []*session.Session) []*session.Session {
	if len(in) <= 1 {
		return in
	}
	bestByKey := make(map[string]*session.Session, len(in))
	order := make([]string, 0, len(in))
	for _, sess := range in {
		if sess == nil {
			continue
		}
		key := session.IdentityKey(sess)
		if _, ok := bestByKey[key]; !ok {
			order = append(order, key)
		}
		if current := bestByKey[key]; current == nil || session.PreferIdentityWinner(sess, current) {
			bestByKey[key] = sess
		}
	}
	out := make([]*session.Session, 0, len(bestByKey))
	for _, key := range order {
		if sess := bestByKey[key]; sess != nil {
			out = append(out, sess)
		}
	}
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func copyIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	maps.Copy(out, in)
	return out
}

func copyContextStateMap(in map[string]SessionContextState) map[string]SessionContextState {
	out := make(map[string]SessionContextState, len(in))
	maps.Copy(out, in)
	return out
}

// noteInteraction records the current time as the last user interaction.
// Called from handleKey and handleMouse so the watcher can see activity.
func (a *App) noteInteraction() {
	a.interactionMu.Lock()
	a.lastInteraction = a.now()
	a.interactionMu.Unlock()
}

// runRegistrySubscriber drains the daemon event stream and pokes the
// store watcher's interrupt path on every event. Live URL events also
// patch the local map so the dashboard reflects opened and closed
// live URLs within milliseconds. The reload runs on the same
// code path as the polling watcher so concurrency stays simple.
func (a *App) runRegistrySubscriber(events <-chan SessionEvent) {
	tuiLog.Logger().Info("tui.registry_subscriber.started", "component", "tui")
	for ev := range events {
		a.postInterrupt(registryEvent{event: ev})
	}
	tuiLog.Logger().Warn("tui.registry_subscriber.exited", "component", "tui")
}

func (a *App) runProviderStatsSubscriber(events <-chan ProviderStats) {
	tuiLog.Logger().Info("tui.provider_stats_subscriber.started", "component", "tui")
	for ev := range events {
		a.postInterrupt(providerStatsEvent{stats: ev})
	}
	tuiLog.Logger().Warn("tui.provider_stats_subscriber.exited", "component", "tui")
}

func (a *App) runRegistrySupervisor(stop <-chan struct{}) {
	if a.cb.SubscribeRegistry == nil {
		a.setDaemonOffline("registry_unavailable", fmt.Errorf("subscribe callback not configured"))
		return
	}

	retryDelay := 250 * time.Millisecond

	for {
		select {
		case <-stop:
			tuiLog.Logger().Debug("tui.registry_supervisor.stopped", "component", "tui")
			return
		default:
		}

		subscribeStartedAt := a.now()
		tuiLog.Logger().Debug("tui.registry_supervisor.subscribe_begin",
			"component", "tui",
			"started_at", subscribeStartedAt.Format(time.RFC3339Nano))
		events, cancel, err := a.cb.SubscribeRegistry()
		subscribeDuration := time.Since(subscribeStartedAt)
		if err != nil {
			a.setDaemonOffline("subscribe_failed", err)
			tuiLog.Logger().Warn("tui.registry_supervisor.subscribe_failed",
				"component", "tui",
				"subscribe_ms", subscribeDuration.Milliseconds(),
				"retry_ms", retryDelay.Milliseconds(),
				"err", err)
			if !waitForRegistryRetry(stop, retryDelay) {
				return
			}
			retryDelay = nextRegistryRetryDelay(retryDelay)
			continue
		}

		a.setDaemonOnline("subscribe_ok")
		tuiLog.Logger().Debug("tui.registry_supervisor.subscribe_ok",
			"component", "tui",
			"subscribe_ms", subscribeDuration.Milliseconds())
		a.requestSessionsAsync("registry.resubscribe")
		retryDelay = 250 * time.Millisecond

		done := make(chan struct{})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logUIGoroutinePanic("registry_subscriber", fmt.Sprint(r))
				}
			}()
			defer close(done)
			a.runRegistrySubscriber(events)
		}()

		select {
		case <-stop:
			cancel()
			<-done
			tuiLog.Logger().Debug("tui.registry_supervisor.cancelled", "component", "tui")
			return
		case <-done:
			cancel()
			a.setDaemonOffline("stream_closed", fmt.Errorf("registry stream closed"))
			tuiLog.Logger().Warn("tui.registry_supervisor.stream_closed",
				"component", "tui",
				"retry_ms", retryDelay.Milliseconds())
			if !waitForRegistryRetry(stop, retryDelay) {
				return
			}
			retryDelay = nextRegistryRetryDelay(retryDelay)
		}
	}
}

func (a *App) runProviderStatsSupervisor(stop <-chan struct{}) {
	if a.cb.SubscribeProviderStats == nil {
		a.statsMu.Lock()
		a.stats.StreamErr = "stats stream unavailable"
		a.statsMu.Unlock()
		return
	}

	retryDelay := 250 * time.Millisecond

	for {
		select {
		case <-stop:
			tuiLog.Logger().Debug("tui.provider_stats_supervisor.stopped", "component", "tui")
			return
		default:
		}

		events, cancel, err := a.cb.SubscribeProviderStats()
		if err != nil {
			a.statsMu.Lock()
			a.stats.StreamErr = err.Error()
			a.statsMu.Unlock()
			a.postInterrupt(a)
			if !waitForRegistryRetry(stop, retryDelay) {
				return
			}
			retryDelay = nextRegistryRetryDelay(retryDelay)
			continue
		}

		a.statsMu.Lock()
		a.stats.StreamErr = ""
		a.statsMu.Unlock()
		a.postInterrupt(a)
		retryDelay = 250 * time.Millisecond

		done := make(chan struct{})
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logUIGoroutinePanic("provider_stats_subscriber", fmt.Sprint(r))
				}
			}()
			defer close(done)
			a.runProviderStatsSubscriber(events)
		}()

		select {
		case <-stop:
			cancel()
			<-done
			return
		case <-done:
			cancel()
			a.statsMu.Lock()
			if a.stats.StreamErr == "" {
				a.stats.StreamErr = "stats stream closed"
			}
			a.statsMu.Unlock()
			a.postInterrupt(a)
			if !waitForRegistryRetry(stop, retryDelay) {
				return
			}
			retryDelay = nextRegistryRetryDelay(retryDelay)
		}
	}
}

func (a *App) applyProviderStats(update ProviderStats) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	found := false
	for i := range a.stats.Providers {
		if a.stats.Providers[i].Provider == update.Provider {
			a.stats.Providers[i] = update
			found = true
			break
		}
	}
	if !found {
		a.stats.Providers = append(a.stats.Providers, update)
	}
	sort.Slice(a.stats.Providers, func(i, j int) bool {
		if a.stats.Providers[i].Inflight != a.stats.Providers[j].Inflight {
			return a.stats.Providers[i].Inflight > a.stats.Providers[j].Inflight
		}
		if !a.stats.Providers[i].LastSeen.Equal(a.stats.Providers[j].LastSeen) {
			return a.stats.Providers[i].LastSeen.After(a.stats.Providers[j].LastSeen)
		}
		return a.stats.Providers[i].Provider < a.stats.Providers[j].Provider
	})
	a.stats.LoadedAt = a.now()
	a.statsLoaded = true
}

func waitForRegistryRetry(stop <-chan struct{}, delay time.Duration) bool {
	return waitForTimer(stop, delay)
}

func waitForTimer(stop <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer func() { timer.Stop() }()
	select {
	case <-stop:
		return false
	case <-timer.C:
		return true
	}
}

func nextRegistryRetryDelay(current time.Duration) time.Duration {
	const maxRetryDelay = 5 * time.Second
	next := current * 2
	if next > maxRetryDelay {
		return maxRetryDelay
	}
	return next
}

func (a *App) setDaemonOnline(source string) {
	a.daemonMu.Lock()
	wasOnline := a.daemonOnline
	previousErr := a.daemonLastErr
	a.daemonOnline = true
	a.daemonLastErr = ""
	a.daemonLastSeen = a.now()
	a.daemonMu.Unlock()

	if !wasOnline {
		tuiLog.Logger().Info("tui.daemon.online",
			"component", "tui",
			"source", source,
			"previous_error", previousErr)
	}
	a.postInterrupt(a)
}

func (a *App) setDaemonOffline(source string, err error) {
	errorMessage := ""
	if err != nil {
		errorMessage = err.Error()
	}

	a.daemonMu.Lock()
	wasOnline := a.daemonOnline
	errorChanged := a.daemonLastErr != errorMessage
	a.daemonOnline = false
	a.daemonLastErr = errorMessage
	a.daemonMu.Unlock()

	if wasOnline || errorChanged {
		tuiLog.Logger().Warn("tui.daemon.offline",
			"component", "tui",
			"source", source,
			"err", errorMessage)
	}
	a.postInterrupt(a)
}

func shortDaemonStatus(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	msg = strings.ReplaceAll(msg, "\n", " ")
	replacements := []struct{ old, new string }{
		{"rpc error: code = ", ""},
		{"desc = ", ""},
		{"dial daemon at ", "dial "},
		{"daemon not responding:", "not responding:"},
		{"unknown method ListSessions for service clyde.v1.ClydeService", "daemon binary too old"},
	}
	for _, rep := range replacements {
		msg = strings.ReplaceAll(msg, rep.old, rep.new)
	}
	runes := []rune(msg)
	if len(runes) > 56 {
		msg = string(runes[:55]) + "..."
	}
	return msg
}

func (a *App) isDaemonOnline() bool {
	a.daemonMu.RLock()
	defer a.daemonMu.RUnlock()
	return a.daemonOnline
}

func (a *App) isSessionsLoading() bool {
	a.sessionsLoadingMu.Lock()
	defer a.sessionsLoadingMu.Unlock()
	return a.sessionsLoading
}

func (a *App) hasPendingVisualActivity() bool {
	if a.isSessionsLoading() || a.configLoading {
		return true
	}
	if panel, ok := a.overlay.(*CompactPanel); ok && panel.busy {
		return true
	}
	a.statsMu.Lock()
	statsLoading := a.statsLoading
	a.statsMu.Unlock()
	if statsLoading {
		return true
	}
	a.detailMu.Lock()
	detailsLoading := len(a.detailLoading) > 0
	if !detailsLoading {
		// Spinner segments inside an open modal must keep ticking
		// while any cached detail still reports a pending status,
		// even after the most recent in-flight load completed. The
		// re-poll loop below will replace the placeholder once the
		// daemon-side cache warms.
		for _, cached := range a.detailCache {
			if detailHasPendingStatus(cached) {
				detailsLoading = true
				break
			}
		}
	}
	a.detailMu.Unlock()
	if detailsLoading {
		return true
	}
	return len(a.exportStatsLoading) > 0
}

// detailHasPendingStatus reports whether the cached SessionDetail is
// still settling (context probe in flight, transcript stats not yet
// extracted) and therefore should keep an animated placeholder on
// screen.
func detailHasPendingStatus(d SessionDetail) bool {
	if !d.ContextUsageLoaded && !isTerminalLoadingStatus(d.ContextUsageStatus) {
		return true
	}
	if !d.TranscriptStatsLoaded && !isTerminalLoadingStatus(d.TranscriptStatsStatus) {
		return true
	}
	return false
}

func (a *App) showStartupLoadingState() bool {
	return a.startupLoading && a.isSessionsLoading() && len(a.sessions) == 0
}

func (a *App) showSessionsEmptyState() bool {
	return !a.showStartupLoadingState() && len(a.visibleIdx) == 0
}

func (a *App) logHealthTick() {
	now := a.now()
	drawDelta := a.drawCount - a.healthLastDraw
	spinDelta := a.spinnerFrame - a.healthLastSpin
	sinceLastDraw := time.Duration(0)
	if !a.lastDrawAt.IsZero() {
		sinceLastDraw = now.Sub(a.lastDrawAt)
	}
	sinceLastEvent := time.Duration(0)
	if !a.lastEventAt.IsZero() {
		sinceLastEvent = now.Sub(a.lastEventAt)
	}
	sinceLastNonInterrupt := time.Duration(0)
	if !a.lastNonInterruptAt.IsZero() {
		sinceLastNonInterrupt = now.Sub(a.lastNonInterruptAt)
	}

	tuiLog.Logger().Debug("tui.health.tick",
		"component", "tui",
		"draw_count", a.drawCount,
		"draw_delta", drawDelta,
		"spinner_frame", a.spinnerFrame,
		"spinner_delta", spinDelta,
		"last_draw_ms", sinceLastDraw.Milliseconds(),
		"last_event_ms", sinceLastEvent.Milliseconds(),
		"last_event_type", a.lastEventType,
		"last_non_interrupt_ms", sinceLastNonInterrupt.Milliseconds(),
		"last_non_interrupt_type", a.lastNonInterruptType,
		"app_focused", a.appFocused,
		"active_tab", a.activeTab,
		"selected", a.selectedSessionName(),
		"has_overlay", a.overlay != nil,
		"overlay_type", fmt.Sprintf("%T", a.overlay),
		"daemon_online", a.isDaemonOnline())

	if sinceLastDraw > 1500*time.Millisecond || drawDelta == 0 {
		tuiLog.Logger().Warn("tui.health.draw_stall_suspected",
			"component", "tui",
			"draw_count", a.drawCount,
			"draw_delta", drawDelta,
			"spinner_frame", a.spinnerFrame,
			"spinner_delta", spinDelta,
			"last_draw_ms", sinceLastDraw.Milliseconds(),
			"last_event_ms", sinceLastEvent.Milliseconds(),
			"last_event_type", a.lastEventType,
			"last_non_interrupt_ms", sinceLastNonInterrupt.Milliseconds(),
			"last_non_interrupt_type", a.lastNonInterruptType,
			"app_focused", a.appFocused,
			"active_tab", a.activeTab,
			"has_overlay", a.overlay != nil,
			"overlay_type", fmt.Sprintf("%T", a.overlay))
	}
	if a.appFocused && sinceLastNonInterrupt > 4*time.Second {
		tuiLog.Logger().Warn("tui.health.input_starvation_suspected",
			"component", "tui",
			"draw_count", a.drawCount,
			"draw_delta", drawDelta,
			"spinner_frame", a.spinnerFrame,
			"spinner_delta", spinDelta,
			"last_non_interrupt_ms", sinceLastNonInterrupt.Milliseconds(),
			"last_non_interrupt_type", a.lastNonInterruptType,
			"last_event_type", a.lastEventType,
			"active_tab", a.activeTab,
			"has_overlay", a.overlay != nil,
			"overlay_type", fmt.Sprintf("%T", a.overlay))
	}

	a.healthLastDraw = a.drawCount
	a.healthLastSpin = a.spinnerFrame
}

func (a *App) selectedSessionName() string {
	if a.selected == nil {
		return ""
	}
	return a.selected.Name
}

// runIdleSummarySweeper regenerates stale or missing session summaries
// while the user is idle. It wakes periodically, checks for a long idle
// window, picks one session whose Context looks outdated, and kicks off
// the same RefreshSummary path used on highlight. The rate is one
// candidate per sweep tick so the LLM workload stays low.
func (a *App) runIdleSummarySweeper(stop <-chan struct{}) {
	if a.cb.RefreshSummary == nil {
		return
	}
	const tick = 15 * time.Second
	const idleFor = 30 * time.Second
	t := time.NewTicker(tick)
	defer func() { t.Stop() }()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if !a.hasScreen() {
				continue
			}
			a.interactionMu.Lock()
			lastAct := a.lastInteraction
			a.interactionMu.Unlock()
			if !lastAct.IsZero() && time.Since(lastAct) < idleFor {
				continue
			}
			a.postInterrupt(idleSummarySweepTick{})
		}
	}
}

// pickStaleForSweep returns the first session whose Context looks stale
// and whose refresh is not already in flight. Sessions without a
// transcript, incognito sessions, and ephemeral test sessions are
// skipped. Called only from the idle sweeper goroutine; the App fields
// it touches are either read-mostly or guarded.
func (a *App) pickStaleForSweep() *session.Session {
	for _, s := range a.sessions {
		if s == nil || s.Metadata.IsIncognito {
			continue
		}
		if s.Metadata.ProviderTranscriptPath() == "" {
			continue
		}
		if isEphemeralSession(s) {
			continue
		}
		if a.summaryRefreshing[s.Name] {
			continue
		}
		ctx := strings.TrimSpace(s.Metadata.Context)
		words := 0
		if ctx != "" {
			words = len(strings.Fields(ctx))
		}
		stale := ctx == "" || words > 6
		if stale {
			return s
		}
	}
	return nil
}

// detailsLoadingNow reports whether the named session's details are being
// fetched in a goroutine. Used to gate spinner repaints.
func (a *App) detailsLoadingNow(name string) bool {
	a.detailMu.Lock()
	defer a.detailMu.Unlock()
	return a.detailLoading[name]
}

func (a *App) cachedExportStatsForSession(sess *session.Session) (SessionExportStats, bool) {
	if sess == nil {
		return SessionExportStats{}, false
	}
	stats, ok := a.exportStatsCache[sess.Name]
	return stats, ok
}

func (a *App) requestExportStatsAsync(sess *session.Session) {
	if sess == nil || a.cb.LoadExportStats == nil {
		return
	}
	if !sessionCapabilities(sess).TranscriptExport {
		return
	}
	name := sess.Name
	if a.exportStatsLoading[name] {
		return
	}
	a.exportStatsLoading[name] = true
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("export_stats_load", fmt.Sprint(r))
			}
		}()
		stats, err := a.cb.LoadExportStats(sess)
		a.postInterrupt(exportStatsLoaded{name: name, stats: stats, err: err})
	}()
}

// syncTableSelectionWithOffset moves the selected row to stay in the visible
// window after a click-to-jump or drag on the table scrollbar. Without this
// the highlight would vanish off screen as the viewport scrolled.
func (a *App) syncTableSelectionWithOffset() {
	if !a.table.Active {
		return
	}
	vis := imax(1, a.table.Rect.H-1)
	if a.table.SelectedRow < a.table.Offset {
		a.table.SelectedRow = a.table.Offset
	} else if a.table.SelectedRow >= a.table.Offset+vis {
		a.table.SelectedRow = a.table.Offset + vis - 1
	}
}

// teardownScreen returns the terminal to a sensible state. Mouse and
// focus tracking are explicitly disabled before Fini so the host
// terminal does not keep emitting tracking sequences after exit.
// Calling this twice is safe.
//
// The post-Fini reset sequence covers every private mode that tcell
// or our initScreen turned on, plus a DECSTR soft reset. Earlier
// revisions only reset the alt screen, mouse, and scroll region,
// which left focus reporting and bracketed paste mode active on
// macOS Terminal and iTerm2. That caused stray "^[[I"/"^[[O" tokens
// to land in the shell after exit, and pasted text showed up wrapped
// in "^[[200~...^[[201~". The full sequence below matches what tput
// reset emits minus the screen clear.
func (a *App) teardownScreen() {
	a.screenMu.Lock()
	scr := a.screen
	restorer := a.terminalRestorer
	a.terminalRestorer = nil
	if scr != nil {
		tuiLog.Logger().Debug("tui.teardownScreen.start", "screen", fmt.Sprintf("%p", scr))
		a.pendingResizeDisplaySync = false
		scr.DisableMouse()
		scr.DisableFocus()
		scr.ShowCursor(0, 0)
		scr.Fini()
		tuiLog.Logger().Debug("tui.teardownScreen.fini", "screen", fmt.Sprintf("%p", scr))
		a.screen = nil
	} else {
		tuiLog.Logger().Debug("tui.teardownScreen.no_screen")
	}
	a.screenMu.Unlock()

	if restorer != nil {
		restorer.Restore()
		return
	}
	terminalcontrol.WriteResetToTerminal()
}

// initScreen allocates a tcell screen and enables mouse + focus.
func (a *App) initScreen() error {
	applyTUITheme(detectTerminalTheme())
	tuiLog.Logger().Info("tui.initScreen.start", "current_screen", fmt.Sprintf("%p", a.screen))
	restorer := terminalcontrol.CaptureProcessRestorer(tuiLog.Logger())
	scr, err := tcell.NewScreen()
	if err != nil {
		if restorer != nil {
			restorer.Restore()
		}
		slog.Error("tui.initScreen.new_screen_failed",
			"component", "tui",
			"err", err)
		return fmt.Errorf("tcell NewScreen: %w", err)
	}
	if err := scr.Init(); err != nil {
		if restorer != nil {
			restorer.Restore()
		}
		slog.Error("tui.initScreen.init_failed",
			"component", "tui",
			"err", err)
		return fmt.Errorf("tcell Init: %w", err)
	}
	enableAppMouse(scr)
	scr.EnableFocus()
	scr.Clear()
	a.screenMu.Lock()
	a.screen = scr
	a.terminalRestorer = restorer
	a.screenMu.Unlock()
	tuiLog.Logger().Info("tui.initScreen.success", "screen", fmt.Sprintf("%p", a.screen))
	return nil
}

func (a *App) postInterrupt(data any) {
	a.screenMu.RLock()
	scr := a.screen
	a.screenMu.RUnlock()
	if scr == nil {
		return
	}
	_ = scr.PostEvent(tcell.NewEventInterrupt(data))
}

func (a *App) hasScreen() bool {
	a.screenMu.RLock()
	defer a.screenMu.RUnlock()
	return a.screen != nil
}

func (a *App) screenSizeSnapshot() (int, int) {
	a.screenMu.RLock()
	defer a.screenMu.RUnlock()
	if a.screen == nil {
		return 0, 0
	}
	return a.screen.Size()
}

func (a *App) runTerminalCall(call string, fn func()) {
	startedAt := a.now()
	if call == "poll_event" {
		fn()
		duration := time.Since(startedAt)
		if duration <= terminalCallWarnThreshold(call) {
			return
		}
		tuiLog.Logger().WarnContext(a.ctx, "tui.terminal_call.done",
			"component", "tui",
			"call", call,
			"started_at", startedAt.Format(time.RFC3339Nano),
			"duration_ms", duration.Milliseconds())
		return
	}
	width, height := a.screenSizeSnapshot()
	overlayType := fmt.Sprintf("%T", a.overlay)
	tuiLog.Logger().Debug("tui.terminal_call.begin",
		"component", "tui",
		"call", call,
		"started_at", startedAt.Format(time.RFC3339Nano),
		"width", width,
		"height", height,
		"active_tab", a.activeTab,
		"selected", a.selectedSessionName(),
		"overlay_type", overlayType,
		"during_resize", a.inResizeEvent)
	fn()
	duration := time.Since(startedAt)
	logLevel := slog.LevelDebug
	if duration > terminalCallWarnThreshold(call) {
		logLevel = slog.LevelWarn
	}
	tuiLog.Logger().Log(a.ctx, logLevel, "tui.terminal_call.done",
		"component", "tui",
		"call", call,
		"started_at", startedAt.Format(time.RFC3339Nano),
		"duration_ms", duration.Milliseconds(),
		"width", width,
		"height", height,
		"active_tab", a.activeTab,
		"selected", a.selectedSessionName(),
		"overlay_type", overlayType,
		"during_resize", a.inResizeEvent)
}

func (a *App) pollEvent() tcell.Event {
	var ev tcell.Event
	a.runTerminalCall("poll_event", func() {
		ev = a.screen.PollEvent()
	})
	return ev
}

func terminalCallWarnThreshold(call string) time.Duration {
	switch call {
	case "poll_event":
		return 1500 * time.Millisecond
	case "show", "sync":
		return 250 * time.Millisecond
	default:
		return 500 * time.Millisecond
	}
}

// PreWarmStats kicks off background model computation. Results land in
// modelCache and a redraw is triggered via PostEvent.
func (a *App) PreWarmStats() {
	a.requestSessionsAsync("prewarm")
}

// ---------------- Event dispatch ----------------

func (a *App) handleEvent(ev tcell.Event) bool {
	switch e := ev.(type) {
	case *tcell.EventResize:
		a.handleResizeEvent(e)
		return true
	case *tcell.EventFocus:
		return a.handleFocusEvent(e)
	case *tcell.EventInterrupt:
		return a.handleInterruptEvent(e)
	case *tcell.EventKey:
		a.handleKey(e)
		return true
	case *tcell.EventMouse:
		return a.handleMouse(e)
	}
	return false
}

func (a *App) handleResizeEvent(e *tcell.EventResize) {
	a.inResizeEvent = true
	defer func() { a.inResizeEvent = false }()
	w, h := e.Size()
	sw, sh := a.screen.Size()
	if w != sw || h != sh {
		tuiLog.Logger().Debug("tui.event.resize.size_mismatch",
			"component", "tui",
			"event_w", w, "event_h", h, "screen_w", sw, "screen_h", sh,
			"overlay_type", fmt.Sprintf("%T", a.overlay))
	}
	tuiLog.Logger().Debug("tui.event.resize",
		"component", "tui",
		"width", w,
		"height", h,
		"overlay_type", fmt.Sprintf("%T", a.overlay),
		"return_path_state", string(a.returnPathState))
	// One full Sync after the draw for this event (see main loop), not
	// on a debounce, so the terminal does not smear during drag resize.
	a.pendingResizeDisplaySync = true
	a.ensureReturnPromptForEvent("event.resize")
}

func (a *App) handleFocusEvent(e *tcell.EventFocus) bool {
	wasFocused := a.appFocused
	tuiLog.Logger().Debug("tui.event.focus",
		"component", "tui",
		"focused", e.Focused,
		"overlay_type", fmt.Sprintf("%T", a.overlay),
		"return_path_state", string(a.returnPathState))
	a.appFocused = e.Focused
	if !e.Focused {
		return wasFocused != e.Focused
	}
	a.runTerminalCall("sync", func() {
		a.screen.Sync()
	})
	overlayBefore := fmt.Sprintf("%T", a.overlay)
	a.ensureReturnPromptForEvent("event.focus")
	return wasFocused != e.Focused || overlayBefore != fmt.Sprintf("%T", a.overlay)
}

func (a *App) ensureReturnPromptForEvent(reason string) {
	if a.returnPathSession == nil {
		return
	}
	switch a.returnPathState {
	case returnPathStateReturnPromptPending, returnPathStateReturnPromptVisible:
		a.ensureReturnPrompt(a.returnPathSession, reason)
	}
}

func (a *App) handleInterruptEvent(e *tcell.EventInterrupt) bool {
	a.logInterruptEvent(e)
	if changed, handled := a.handleRefreshInterruptEvent(e); handled {
		return changed
	}
	switch d := e.Data().(type) {
	case summaryRefreshDone:
		a.handleSummaryRefreshDone(d)
		return true
	case registryEvent:
		a.applySessionEvent(d.event)
		return true
	case providerStatsEvent:
		a.applyProviderStats(d.stats)
		return true
	case selfReloadAvailable:
		a.requestSelfReload(d.path, d.reason, "local_watcher", "")
		return true
	case compactStreamEvent:
		a.handleCompactStreamEvent(d)
		return true
	case compactStreamOpened:
		a.handleCompactStreamOpened(d)
		return true
	case compactStreamDone:
		a.handleCompactStreamDone(d)
		return true
	case compactUndoDone:
		a.handleCompactUndoDone(d)
		return true
	case sidecarTailOpened:
		a.handleSidecarTailOpened(d)
		return true
	case sidecarSendDone:
		a.handleSidecarSendDone(d)
		return true
	case sidecarLaunchDone:
		a.handleSidecarLaunchDone(d)
		return true
	case sidecarStatusUpdate:
		a.handleSidecarStatusUpdate(d)
		return true
	case viewContentLoaded:
		a.handleViewContentLoaded(d)
		return true
	case exportFinished:
		a.handleExportFinished(d)
		return true
	default:
		// Several call sites post the *App itself as the payload to
		// request a generic table repaint (see PostEvent calls in
		// runDiscoveryScanner, the bridge watcher, and the input
		// handlers). Treat anything we don't recognise as that.
		a.populateTable()
		return true
	}
}

func (a *App) handleRefreshInterruptEvent(e *tcell.EventInterrupt) (bool, bool) {
	// Interrupts are posted from background goroutines. The Data
	// payload tells us which cache to refresh.
	switch d := e.Data().(type) {
	case sessionsLoaded:
		a.handleSessionsLoaded(d)
		return true, true
	case configControlsLoaded:
		a.handleConfigControlsLoaded(d)
		return true, true
	case detailsLoaded:
		a.handleDetailsLoaded(d)
		return true, true
	case exportStatsLoaded:
		a.handleExportStatsLoaded(d)
		return true, true
	case spinnerTick:
		a.handleSpinnerTick()
		return true, true
	case healthTick:
		a.lastHeartbeatAt = a.now()
		a.logHealthTick()
		return false, true
	case modelsPrewarmed:
		maps.Copy(a.modelCache, d.models)
		a.populateTable()
		return true, true
	case summaryRefreshed:
		// Table was already repopulated by maybeRefreshSummary. The
		// interrupt exists to trigger a draw cycle from the event loop.
		_ = d
		return true, true
	case idleSummarySweepTick:
		return a.handleIdleSummarySweepTick(), true
	default:
		return false, false
	}
}

func (a *App) logInterruptEvent(e *tcell.EventInterrupt) {
	interruptType := fmt.Sprintf("%T", e.Data())
	switch e.Data().(type) {
	case spinnerTick, healthTick:
		return
	default:
		tuiLog.Logger().Debug("tui.event.interrupt",
			"component", "tui",
			"payload_type", interruptType,
			"selected", a.selectedSessionName(),
			"active_tab", a.activeTab,
			"overlay_type", fmt.Sprintf("%T", a.overlay))
	}
}

func (a *App) handleSessionsLoaded(d sessionsLoaded) {
	a.sessionsLoadingMu.Lock()
	a.sessionsLoading = false
	a.sessionsLoadingMu.Unlock()
	if d.err != nil {
		if d.reason == "startup" {
			a.startupLoading = false
		}
		tuiLog.Logger().Warn("tui.sessions.load.failed", "component", "tui", "reason", d.reason, "err", d.err)
		return
	}
	a.startupLoading = false
	a.applySessionSnapshot(d.snapshot)
}

func (a *App) handleConfigControlsLoaded(d configControlsLoaded) {
	a.configLoading = false
	if d.err != nil {
		a.configErr = d.err.Error()
		return
	}
	a.configErr = ""
	a.configControls = d.controls
	if a.configSelected >= len(a.configControls) {
		a.configSelected = max(0, len(a.configControls)-1)
	}
}

func (a *App) handleDetailsLoaded(d detailsLoaded) {
	a.detailMu.Lock()
	if d.err != nil {
		if cached, ok := a.detailCache[d.name]; ok {
			d.detail = cached
		}
		if d.detail.TranscriptStatsStatus == "" {
			d.detail.TranscriptStatsStatus = fmt.Sprintf("failed: %v", d.err)
		}
		d.detail.TranscriptStatsLoaded = false
	}
	a.detailCache[d.name] = d.detail
	delete(a.detailLoading, d.name)
	a.detailMu.Unlock()
	a.refreshOpenOptionsModalStats(d.name)
	if a.selected != nil && a.selected.Name == d.name {
		a.populateDetails()
	}
}

func (a *App) handleExportStatsLoaded(d exportStatsLoaded) {
	delete(a.exportStatsLoading, d.name)
	if d.err == nil {
		a.exportStatsCache[d.name] = d.stats
	}
	a.applyExportStatsResult(d.name, d.stats, d.err)
}

func (a *App) handleSpinnerTick() {
	a.spinnerFrame++
	if a.selected != nil && a.detailsLoadingNow(a.selected.Name) {
		a.populateDetails()
	}
	a.maybeRepollPendingDetails()
}

// detailRepollInterval bounds how often the TUI re-fetches a session's
// detail while its cached status reports a pending probe. The daemon's
// cache is single-flight and short-circuits once warm, so the cost of
// each poll is dominated by an RPC round trip; 1.5s keeps the user-
// visible delay short without producing a poll storm.
const detailRepollInterval = 1500 * time.Millisecond

// maybeRepollPendingDetails re-issues GetSessionDetail for any session
// whose cached detail is still pending. The daemon kicks off a probe on
// the first cold-cache call, returns immediately with Status="probing",
// and the TUI must come back later to pick up the warm result. This
// loop is the "come back later" piece, throttled per session via
// detailRepollAfter so a slow probe cannot turn into an RPC storm.
func (a *App) maybeRepollPendingDetails() {
	if a.cb.GetSessionDetail == nil {
		return
	}
	now := a.now()
	a.detailMu.Lock()
	pending := make([]string, 0, len(a.detailCache))
	for name, cached := range a.detailCache {
		if !detailHasPendingStatus(cached) {
			continue
		}
		if a.detailLoading[name] {
			continue
		}
		if next, ok := a.detailRepollAfter[name]; ok && now.Before(next) {
			continue
		}
		pending = append(pending, name)
		a.detailRepollAfter[name] = now.Add(detailRepollInterval)
	}
	a.detailMu.Unlock()
	if len(pending) == 0 {
		return
	}
	for _, name := range pending {
		sess := a.sessionByName(name)
		if sess == nil {
			continue
		}
		a.loadDetailAsync(sess)
	}
}

// sessionByName returns the in-memory session matching name, or nil.
// Used by the detail re-poll to translate cache keys back into the
// pointer the load helpers want.
func (a *App) sessionByName(name string) *session.Session {
	for _, sess := range a.sessions {
		if sess != nil && sess.Name == name {
			return sess
		}
	}
	return nil
}

func (a *App) handleIdleSummarySweepTick() bool {
	sess := a.pickStaleForSweep()
	if sess != nil {
		a.maybeRefreshSummary(sess)
		return true
	}
	return false
}

func (a *App) handleSummaryRefreshDone(d summaryRefreshDone) {
	a.summaryRefreshing[d.name] = false
	if d.updated == nil {
		return
	}
	for i := range a.sessions {
		if a.sessions[i].Name == d.name {
			a.sessions[i] = d.updated
			break
		}
	}
	a.populateTable()
}

func (a *App) handleCompactStreamEvent(d compactStreamEvent) {
	if panel, ok := a.overlay.(*CompactPanel); ok {
		panel.ApplyCompactEvent(d.event)
	}
}

func (a *App) handleCompactStreamOpened(d compactStreamOpened) {
	panel, ok := a.overlay.(*CompactPanel)
	if !ok {
		return
	}
	if d.err != nil {
		panel.SetBusy(d.action, false)
		panel.ApplyCompactEvent(CompactEvent{
			Kind:    "status",
			Message: fmt.Sprintf("%s start failed: %v", d.action, d.err),
		})
		return
	}
	a.compactCancel = d.cancel
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("compact_stream", fmt.Sprint(r))
			}
		}()
		a.runCompactStream(d.events, d.done, d.action)
	}()
}

func (a *App) handleCompactStreamDone(d compactStreamDone) {
	a.compactCancel = nil
	panel, ok := a.overlay.(*CompactPanel)
	if !ok {
		return
	}
	panel.SetBusy(d.action, false)
	if d.err != nil {
		panel.ApplyCompactEvent(CompactEvent{
			Kind:    "status",
			Message: fmt.Sprintf("%s failed: %v", d.action, d.err),
		})
		return
	}
	panel.ApplyCompactEvent(CompactEvent{
		Kind:    "status",
		Message: d.action + " completed",
	})
}

func (a *App) handleCompactUndoDone(d compactUndoDone) {
	if panel, ok := a.overlay.(*CompactPanel); ok {
		panel.SetBusy("undo", false)
		panel.SetUndoResult(d.result, d.err)
	}
}

func (a *App) handleSidecarTailOpened(d sidecarTailOpened) {
	if a.sidecar == nil {
		return
	}
	if d.err != nil {
		a.sidecar.status = "tail failed: " + d.err.Error()
		return
	}
	a.sidecarTailPending = false
	a.sidecarCancel = d.cancel
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("sidecar_tail", fmt.Sprint(r))
			}
		}()
		a.runSidecarTail(d.events, a.sidecar)
	}()
}

func (a *App) handleSidecarSendDone(d sidecarSendDone) {
	if a.sidecar == nil {
		return
	}
	if d.err != nil {
		a.sidecar.status = "error: " + d.err.Error()
		return
	}
	a.sidecar.status = "sent"
}

func (a *App) handleSidecarLaunchDone(d sidecarLaunchDone) {
	if d.err != nil {
		if a.sidecar != nil {
			a.sidecar.status = "launch failed: " + d.err.Error()
		}
		return
	}
	a.sidecarSessionName = d.sessionName
	a.sidecarSessionID = d.sessionID
	a.sidecarTailPending = true
	panel := NewSidecarPanel(d.sessionName, d.sessionID, d.liveURL)
	panel.status = "waiting for transcript..."
	panel.OnSend = func(text string) error {
		send := a.sidecarSendFunc()
		if send == nil {
			return fmt.Errorf("daemon offline")
		}
		panel.status = "sending..."
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logUIGoroutinePanic("sidecar_send", fmt.Sprint(r))
				}
			}()
			err := send(d.sessionID, text)
			a.postInterrupt(sidecarSendDone{err: err})
		}()
		return nil
	}
	a.sidecar = panel
	a.refreshSessions()
	a.maybeOpenSidecarTail()
}

func (a *App) handleSidecarStatusUpdate(d sidecarStatusUpdate) {
	if a.sidecar != nil {
		a.sidecar.status = d.status
	}
}

func (a *App) handleViewContentLoaded(d viewContentLoaded) {
	if d.content == "" {
		return
	}
	tb := &TextBox{
		Title:      "Conversation: " + d.sessionName,
		TitleStyle: StyleHeader,
		Wrap:       true,
		Focused:    true,
	}
	tb.SetLines(strings.Split(d.content, "\n"))
	a.overlay = &ViewerOverlay{Box: tb, OnClose: a.closeOverlay}
	a.mode = StatusView
}

func (a *App) handleExportFinished(d exportFinished) {
	if d.stack {
		a.pushNoticeModal(d.title, d.body)
		return
	}
	a.openNoticeModal(d.title, d.body)
}

// handleKey dispatches keyboard events.
// Global shortcuts (Ctrl+C) always apply. Overlays take priority over widgets.
func (a *App) handleKey(e *tcell.EventKey) {
	a.noteInteraction()
	a.logKeyEvent(e)
	// Global: Ctrl+C always quits, regardless of focus.
	if e.Key() == tcell.KeyCtrlC {
		a.running = false
		return
	}
	if a.overlay != nil {
		a.overlay.HandleEvent(e)
		return
	}
	if a.handleSidecarKey(e) {
		return
	}
	if a.handleFocusedDetailsKey(e) {
		return
	}
	if a.handleSettingsKey(e) {
		return
	}
	if a.handleAppKey(e) {
		return
	}

	// Fall through: table handles navigation.
	a.table.HandleEvent(e)
}

func (a *App) logKeyEvent(e *tcell.EventKey) {
	tuiLog.Logger().Debug("tui.input.key",
		"component", "tui",
		"key", int(e.Key()),
		"rune", string(e.Rune()),
		"modifiers", int(e.Modifiers()),
		"selected", a.selectedSessionName(),
		"active_tab", a.activeTab,
		"has_overlay", a.overlay != nil,
		"overlay_type", fmt.Sprintf("%T", a.overlay),
		"mode", int(a.mode))
}

func (a *App) handleSidecarKey(e *tcell.EventKey) bool {
	// When the Sidecar tab is active, the panel owns most key events
	// so the user can type into the inject input without the global
	// shortcuts (e.g. "d" for delete) firing while typing a message.
	// Esc returns to the Sessions tab.
	if a.activeTab != tabSidecar || a.sidecar == nil {
		return false
	}
	if e.Key() == tcell.KeyEscape {
		a.activeTab = tabSessions
		a.tabs.SetActive(tabSessions)
		return true
	}
	return a.sidecar.HandleEvent(e)
}

func (a *App) handleFocusedDetailsKey(e *tcell.EventKey) bool {
	// When a details sub-pane is focused, scroll keys go to that pane.
	// Escape/Tab are handled globally below; action keys (r/v/s/d/c/etc.)
	// still work from details focus to avoid mode confusion.
	if !a.detailsHasFocus() {
		return false
	}
	switch e.Key() {
	case tcell.KeyUp, tcell.KeyDown, tcell.KeyPgUp, tcell.KeyPgDn,
		tcell.KeyHome, tcell.KeyEnd:
		a.details.HandleEvent(e)
		return true
	case tcell.KeyRune:
		r := e.Rune()
		if r == 'j' || r == 'k' || r == 'g' || r == 'G' {
			a.details.HandleEvent(e)
			return true
		}
	}
	return false
}

func (a *App) handleSettingsKey(e *tcell.EventKey) bool {
	// Mode-specific shortcuts that must fire before the table consumes keys.
	if a.activeTab != tabSettings || a.overlay != nil || a.detailsHasFocus() {
		return false
	}
	switch e.Key() {
	case tcell.KeyUp:
		a.stepConfigSelection(-1)
		return true
	case tcell.KeyDown:
		a.stepConfigSelection(1)
		return true
	case tcell.KeyEnter:
		a.activateSelectedConfigControl()
		return true
	case tcell.KeyRight:
		a.cycleSelectedConfigControl(1)
		return true
	case tcell.KeyLeft:
		a.cycleSelectedConfigControl(-1)
		return true
	case tcell.KeyRune:
		return a.handleSettingsRuneKey(e.Rune())
	}
	return false
}

func (a *App) handleSettingsRuneKey(r rune) bool {
	switch r {
	case 'j':
		a.stepConfigSelection(1)
		return true
	case 'k':
		a.stepConfigSelection(-1)
		return true
	case 'l':
		a.cycleSelectedConfigControl(1)
		return true
	case 'h':
		a.cycleSelectedConfigControl(-1)
		return true
	}
	return false
}

func (a *App) handleAppKey(e *tcell.EventKey) bool {
	switch e.Key() {
	case tcell.KeyTab, tcell.KeyBacktab:
		return a.handleTabFocusKey(e.Key() == tcell.KeyBacktab)
	case tcell.KeyEscape:
		a.handleEscapeKey()
		return true
	case tcell.KeyRune:
		return a.handleAppRuneKey(e.Rune())
	}
	return false
}

func (a *App) handleTabFocusKey(reverse bool) bool {
	// When the details pane is open, Tab cycles focus:
	//   table -> details.left -> details.right -> table
	// BackTab goes the other way.
	if a.selected == nil {
		return false
	}
	a.cycleDetailsFocus(reverse)
	return true
}

func (a *App) handleEscapeKey() {
	if a.detailsHasFocus() {
		a.details.SetFocus(DetailsFocusNone)
		return
	}
	if a.selected != nil {
		a.deselect()
		return
	}
	// Esc on a bare dashboard (no selection, no overlay, no
	// details focus) used to flip a.running = false here, which
	// surprised users who pressed Esc expecting "go back" and
	// got an unexpected quit instead. Quit is on q (or Ctrl-C);
	// Esc on bare dashboard is now a safe no-op.
}

func (a *App) handleAppRuneKey(r rune) bool {
	if a.handlePrimaryRuneKey(r) {
		return true
	}
	if a.handleTabRuneKey(r) {
		return true
	}
	if a.handleSortRuneKey(r) {
		return true
	}
	return a.handleActionRuneKey(r)
}

func (a *App) handlePrimaryRuneKey(r rune) bool {
	switch r {
	case ' ':
		if a.activeTab == tabSettings {
			a.activateSelectedConfigControl()
			return true
		}
		if len(a.visibleIdx) > 0 {
			a.table.Active = true
			a.openDetails(a.currentSession())
		}
		return true
	case 'q':
		if a.selected != nil {
			a.deselect()
			return true
		}
		a.running = false
		return true
	case '/':
		// nvim-style: when a session is highlighted, "/" searches
		// inside that session's transcript. With no selection it
		// opens the table filter so the same key is always "find".
		if sess := a.rowSession(); sess != nil {
			a.openSearchForm()
			return true
		}
		a.openFilter()
		return true
	}
	return false
}

func (a *App) handleTabRuneKey(r rune) bool {
	switch r {
	case '1':
		a.activeTab = tabSessions
		a.tabs.SetActive(tabSessions)
		return true
	case '2':
		a.activeTab = tabStats
		a.tabs.SetActive(tabStats)
		a.requestStatsAsync("tab.switch")
		return true
	case '3':
		a.activeTab = tabSettings
		a.tabs.SetActive(tabSettings)
		return true
	case '4':
		a.activeTab = tabSidecar
		a.tabs.SetActive(tabSidecar)
		return true
	case 'S':
		// Pin the highlighted row in the sidecar tab and switch
		// to it. Useful when the user wants the live transcript
		// view of a daemon live session one keystroke away.
		if sess := a.rowSession(); sess != nil && a.sidecarCanDrive(sess) {
			a.pinSidecar(sess)
			a.activeTab = tabSidecar
			a.tabs.SetActive(tabSidecar)
		}
		return true
	}
	return false
}

func (a *App) handleSortRuneKey(r rune) bool {
	switch r {
	case '!':
		a.toggleSort(SortColName)
		return true
	case '@':
		a.toggleSort(SortColWorkspace)
		return true
	case '#':
		a.toggleSort(SortColModel)
		return true
	case '$':
		a.toggleSort(SortColUsed)
		return true
	case '%':
		a.toggleSort(SortColCreated)
		return true
	}
	return false
}

func (a *App) handleActionRuneKey(r rune) bool {
	// App-level shortcuts. We avoid binding lowercase letters that
	// the table uses for nvim-style movement (h/j/k/l/g/G) so the
	// movement keys fall through to the table widget below.
	switch r {
	case 'N':
		if a.activeTab == tabSidecar {
			a.openSidecarLaunchStarterModal()
		} else {
			a.newSession()
		}
		return true
	case 'R':
		return a.handleRefreshRuneKey()
	case 'B':
		if sess := a.rowSession(); sess != nil {
			a.openBasedirEditor(sess)
		}
		return true
	case 'H':
		a.showEphemeral = !a.showEphemeral
		a.rebuildVisible()
		a.populateTable()
		return true
	case 'O':
		if sess := a.rowSession(); sess != nil {
			a.openSessionOptionsFor(sess)
		}
		return true
	case 'e':
		if a.activeTab != tabSettings {
			return false
		}
		a.editConfigFile(false)
		return true
	case 'E':
		if a.activeTab != tabSettings {
			return false
		}
		a.editConfigFile(true)
		return true
	case 'G':
		if a.activeTab != tabSettings {
			return false
		}
		a.activateSelectedConfigControl()
		return true
	case 'v':
		if a.selected != nil {
			a.viewSelected()
		}
		return true
	case 's':
		if a.selected != nil {
			a.openSearchForm()
		}
		return true
	case 'd':
		if a.selected != nil {
			a.openDeleteConfirm()
		}
		return true
	case 'f':
		if a.selected != nil {
			a.doFork()
		}
		return true
	case 'c':
		if a.selected != nil {
			a.openCompactForm()
		}
		return true
	case '?':
		// Help screen with the full keymap. Uses the options modal
		// styling so it looks consistent with other overlays.
		a.openHelpModal()
		return true
	}
	return false
}

func (a *App) handleRefreshRuneKey() bool {
	if !a.isDaemonOnline() && a.cb.RestartDaemon != nil {
		a.restartDaemonAsync()
		return true
	}
	if a.activeTab == tabStats {
		a.refreshStats()
		return true
	}
	a.refreshSessions()
	if a.activeTab == tabSettings {
		a.refreshConfigControls()
	}
	return true
}

// handleMouse dispatches mouse events via direct rect hit tests.
// Overlays take priority. No InRect chain.
func (a *App) handleMouse(e *tcell.EventMouse) bool {
	if a.isIdleMouseMotion(e) {
		return false
	}
	// Only count buttoned events as interaction. Bare motion (no button)
	// should not block the watcher because macOS delivers a lot of idle
	// mouse-move events over the terminal window.
	if e.Buttons() != 0 {
		a.noteInteraction()
	}
	x, y := e.Position()
	btns := e.Buttons()
	a.logMouseEvent(x, y, btns)
	if a.handleOverlayMouse(e) {
		return true
	}
	if a.handleStatusMouse(x, y, btns) {
		return true
	}
	// Tab strip click takes priority over the rest of the body.
	if a.tabs != nil && a.tabs.HandleEvent(e) {
		tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "tabs")
		return true
	}
	if btns == 0 {
		a.grab = grabNone
		return true
	}
	if a.handleActiveGrabMouse(y, btns) {
		return true
	}
	if a.handleTableScrollbarMouse(x, y, btns) {
		return true
	}
	if a.handleDetailsMouse(x, y, btns) {
		return true
	}
	return a.handleTableMouse(e, x, y, btns)
}

func (a *App) isIdleMouseMotion(ev tcell.Event) bool {
	e, ok := ev.(*tcell.EventMouse)
	return ok && e.Buttons() == 0 && a.grab == grabNone
}

func (a *App) logMouseEvent(x int, y int, btns tcell.ButtonMask) {
	tuiLog.Logger().Debug("tui.input.mouse",
		"component", "tui",
		"x", x,
		"y", y,
		"buttons", int(btns),
		"selected", a.selectedSessionName(),
		"active_tab", a.activeTab,
		"has_overlay", a.overlay != nil,
		"overlay_type", fmt.Sprintf("%T", a.overlay),
		"mode", int(a.mode))
}

func (a *App) handleOverlayMouse(e *tcell.EventMouse) bool {
	if a.overlay == nil {
		return false
	}
	tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "overlay")
	a.overlay.HandleEvent(e)
	return true
}

func (a *App) handleStatusMouse(x int, y int, btns tcell.ButtonMask) bool {
	if btns&tcell.Button1 == 0 || !a.statusRect.Contains(x, y) {
		return false
	}
	action, ok := legendActionAt(a.status, a.statusRect, x)
	if !ok {
		return false
	}
	tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "status_legend", "action", int(action))
	a.invokeLegendAction(action)
	return true
}

func (a *App) handleActiveGrabMouse(y int, btns tcell.ButtonMask) bool {
	// If the user is currently dragging a scrollbar, keep routing the
	// mouse position to that widget until the button is released.
	if a.grab == grabNone || btns&tcell.Button1 == 0 {
		return false
	}
	tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "grab", "grab", int(a.grab))
	switch a.grab {
	case grabTable:
		a.table.JumpToScrollbarY(y)
		a.syncTableSelectionWithOffset()
	case grabDetailsLeft:
		if a.details != nil {
			a.details.Left.JumpToScrollbarY(y)
		}
	case grabDetailsRight:
		if a.details != nil {
			a.details.Right.JumpToScrollbarY(y)
		}
	}
	return true
}

func (a *App) handleTableScrollbarMouse(x int, y int, btns tcell.ButtonMask) bool {
	// Click on the table scrollbar starts a grab and jumps.
	if btns&tcell.Button1 == 0 || !a.table.ScrollbarRect.Contains(x, y) {
		return false
	}
	tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "table_scrollbar")
	a.grab = grabTable
	a.table.JumpToScrollbarY(y)
	a.syncTableSelectionWithOffset()
	return true
}

func (a *App) handleDetailsMouse(x int, y int, btns tcell.ButtonMask) bool {
	// Detail panes consume wheel events when the cursor is over them.
	// Each sub-pane scrolls independently, so hit-test both rects.
	if a.selected == nil || a.details == nil {
		return false
	}
	if a.handleDetailsScrollbarMouse(x, y, btns) {
		return true
	}
	if a.handleDetailsPaneMouse(a.details.LeftRect, a.details.Left, DetailsFocusLeft, "details_left_focus", x, y, btns) {
		return true
	}
	return a.handleDetailsPaneMouse(a.details.RightRect, a.details.Right, DetailsFocusRight, "details_right_focus", x, y, btns)
}

func (a *App) handleDetailsScrollbarMouse(x int, y int, btns tcell.ButtonMask) bool {
	// Scrollbar click or drag start on either sub-pane.
	if btns&tcell.Button1 != 0 && a.details.Left.ScrollbarRect.Contains(x, y) {
		tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "details_left_scrollbar")
		a.grab = grabDetailsLeft
		a.details.SetFocus(DetailsFocusLeft)
		a.details.Left.JumpToScrollbarY(y)
		return true
	}
	if btns&tcell.Button1 != 0 && a.details.Right.ScrollbarRect.Contains(x, y) {
		tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "details_right_scrollbar")
		a.grab = grabDetailsRight
		a.details.SetFocus(DetailsFocusRight)
		a.details.Right.JumpToScrollbarY(y)
		return true
	}
	return false
}

func (a *App) handleDetailsPaneMouse(rect Rect, box *TextBox, focus DetailsFocus, route string, x int, y int, btns tcell.ButtonMask) bool {
	if !rect.Contains(x, y) {
		return false
	}
	if btns&tcell.WheelUp != 0 {
		box.Offset = imax(0, box.Offset-3)
		return true
	}
	if btns&tcell.WheelDown != 0 {
		box.Offset += 3
		return true
	}
	if btns&tcell.Button1 != 0 {
		tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", route)
		a.details.SetFocus(focus)
		return true
	}
	return false
}

func (a *App) handleTableMouse(e *tcell.EventMouse, x int, y int, btns tcell.ButtonMask) bool {
	if !a.tableRect.Contains(x, y) {
		return false
	}
	tuiLog.Logger().Debug("tui.input.mouse.route", "component", "tui", "route", "table")
	if a.handleTableWheelMouse(btns) {
		return true
	}
	if btns&tcell.Button1 != 0 {
		a.handleTableClickMouse(e, x, y)
		return true
	}
	return false
}

func (a *App) handleTableWheelMouse(btns tcell.ButtonMask) bool {
	if btns&tcell.WheelUp != 0 {
		a.table.ScrollUp(3)
		return true
	}
	if btns&tcell.WheelDown != 0 {
		a.table.ScrollDown(3)
		return true
	}
	if btns&tcell.WheelLeft != 0 {
		a.table.ScrollLeft(6)
		return true
	}
	if btns&tcell.WheelRight != 0 {
		a.table.ScrollRight(6)
		return true
	}
	return false
}

func (a *App) handleTableClickMouse(e *tcell.EventMouse, x int, y int) {
	// Header click sorts. The column index matches the display
	// order (NAME, BASEDIR, LAST ACTIVE, MODEL, MSGS, SUMMARY,
	// CREATED).
	if y == a.tableRect.Y {
		a.toggleSortColumnAtX(x)
		return
	}
	row := a.table.RowAtY(y)
	if row < 0 {
		return
	}
	now := e.When()
	isDouble := !a.lastClickTime.IsZero() &&
		now.Sub(a.lastClickTime) < 400*time.Millisecond &&
		a.lastClickRow == row
	a.lastClickTime = now
	a.lastClickRow = row
	if isDouble {
		tuiLog.Logger().Debug("tui.input.mouse.double_click_resume", "component", "tui", "row", row)
		a.resumeRow(row)
		return
	}
	a.table.SelectAt(row)
	a.openDetails(a.currentSession())
}

func (a *App) toggleSortColumnAtX(x int) {
	switch a.table.ColAtX(x) {
	case 0:
		a.toggleSort(SortColName)
	case 1:
		a.toggleSort(SortColWorkspace)
	case 2:
		a.toggleSort(SortColUsed)
	case 3:
		a.toggleSort(SortColModel)
	case 4:
		a.toggleSort(SortColMessages)
	case 5:
		a.toggleSort(SortColSummary)
	case 6:
		a.toggleSort(SortColCreated)
	}
}

func (a *App) invokeLegendAction(action LegendAction) {
	switch action {
	case LegendNew:
		a.newSession()
	case LegendRefresh:
		if a.activeTab == tabStats {
			a.refreshStats()
			return
		}
		a.refreshSessions()
	case LegendRestartDaemon:
		if a.cb.RestartDaemon != nil {
			a.restartDaemonAsync()
		}
	case LegendHelp:
		a.openHelpModal()
	case LegendQuit:
		if a.selected != nil {
			a.deselect()
			return
		}
		a.running = false
	case LegendSearch:
		if a.selected != nil {
			a.openSearchForm()
		}
	case LegendView:
		if a.selected != nil {
			a.viewSelected()
		}
	case LegendCompact:
		if a.selected != nil {
			a.openCompactForm()
		}
	case LegendFork:
		if a.selected != nil {
			a.doFork()
		}
	case LegendDelete:
		if a.selected != nil {
			a.openDeleteConfirm()
		}
	case LegendEditBasedir:
		if sess := a.rowSession(); sess != nil {
			a.openBasedirEditor(sess)
		}
	case LegendClose:
		if a.overlay != nil {
			a.closeOverlay()
			return
		}
		if a.selected != nil {
			a.deselect()
		}
	case LegendSelectOption:
		if sess := a.rowSession(); sess != nil {
			a.openSessionOptionsFor(sess)
		}
	case LegendSelectDetail:
		if sess := a.currentSession(); sess != nil {
			a.openDetails(sess)
		}
	}
}

// ---------------- Drawing ----------------

func (a *App) draw() {
	drawStartedAt := a.now()
	a.layout()
	layoutDuration := time.Since(drawStartedAt)

	// Clear the whole screen to the default style before redrawing. Each
	// widget also clears its own rect, but the margins between rects (the
	// two-column left and right gutters around the table, the row between
	// the top of the details pane and the table, and so on) are no
	// widget's responsibility. Without this wipe, stale runes from a
	// previous frame linger in those gutters and the UI visibly corrupts
	// the longer the user interacts with it.
	w, h := a.screen.Size()
	clearStartedAt := a.now()
	for y := range h {
		for x := range w {
			a.screen.SetContent(x, y, ' ', nil, tcell.StyleDefault)
		}
	}
	clearDuration := time.Since(clearStartedAt)

	// Tab strip (purple). Always visible across tabs so the user can
	// click between Sessions and Settings. Runtime diagnostics and
	// dashboard summary render right-aligned on the same row.
	a.tabs.Active = a.activeTab
	a.tabs.Draw(a.screen, Rect{X: 0, Y: 0, W: w, H: 1})

	right := "clyde"
	if !a.startupLoading {
		right += fmt.Sprintf("  %d sessions", len(a.visibleIdx))
		if a.hiddenCount > 0 {
			right += fmt.Sprintf("  (%d hidden, H to show)", a.hiddenCount)
		} else if a.showEphemeral {
			right += "  (showing test/tmp)"
		}
		if a.filter != "" {
			right += fmt.Sprintf("  (filter: %q)", a.filter)
		}
	}
	// Header runtime stamp keeps build/process identity and health age.
	lastHeartbeatText := "--"
	if !a.lastHeartbeatAt.IsZero() {
		lastHeartbeatText = formatHeartbeatAge(time.Since(a.lastHeartbeatAt))
	}
	runtimeStamp := fmt.Sprintf("b:%s x:%s r:%s lh:%s",
		a.buildHash,
		a.executableHash,
		a.runHash,
		lastHeartbeatText,
	)
	rightX := w
	rightText := " " + right + " "
	rx := rightX - runeCount(rightText)
	if rx >= 0 {
		drawString(a.screen, rx, 0, StyleTabBar.Bold(true), rightText, rightX-rx)
		rightX = rx
	}
	stampText := " " + runtimeStamp + " "
	sx := rightX - runeCount(stampText)
	if sx > 0 {
		drawString(a.screen, sx, 0, StyleTabBar, stampText, rightX-sx)
	}

	_, compactOverlay := a.overlay.(*CompactPanel)
	if !compactOverlay {
		switch a.activeTab {
		case tabSessions:
			switch {
			case a.showStartupLoadingState():
				a.drawSessionsLoadingState(a.tableRect)
			case a.showSessionsEmptyState():
				a.drawSessionsEmptyState(a.tableRect)
			default:
				a.table.Draw(a.screen, a.tableRect)
				if a.selected != nil {
					a.details.Draw(a.screen, a.detailRect)
				}
			}
		case tabStats:
			a.requestStatsAsync("draw")
			a.drawStatsTab(a.tableRect)
		case tabSidecar:
			a.drawSidecarTab(a.tableRect)
		default:
			a.drawSettingsTab(a.tableRect)
		}
	}
	bodyDuration := time.Since(clearStartedAt) - clearDuration

	// Status bar
	a.status.Mode = a.mode
	a.status.Position = a.positionTextFor()
	a.status.Clock = a.now().Format("15:04:05")
	a.status.LegendOverride = nil
	if provider, ok := a.overlay.(LegendProvider); ok {
		a.status.LegendOverride = provider.StatusLegendActions()
	}
	a.liveURLMu.RLock()
	a.status.LiveURLCount = len(a.liveURLs)
	a.liveURLMu.RUnlock()
	a.status.DaemonOnline = a.isDaemonOnline()
	a.status.DaemonConnecting = a.showStartupLoadingState() && !a.isDaemonOnline()
	a.status.DaemonSpinner = LoadingSpinnerGlyph(a.spinnerFrame)
	a.daemonMu.RLock()
	a.status.DaemonStatus = shortDaemonStatus(a.daemonLastErr)
	a.daemonMu.RUnlock()
	a.status.Draw(a.screen, a.statusRect)
	statusDuration := time.Since(clearStartedAt) - clearDuration - bodyDuration

	// Overlay on top. Dim the existing frame first so the pane reads
	// as a lifted panel; the overlay then clears its own box back to
	// the terminal default, which visually stands out against the
	// darkened backdrop.
	if a.overlay != nil {
		if !compactOverlay {
			dimBackground(a.screen)
		}
		ww, hh := a.screen.Size()
		a.overlay.Draw(a.screen, Rect{X: 0, Y: 0, W: ww, H: hh})
	}

	showStartedAt := a.now()
	a.runTerminalCall("show", func() {
		a.screen.Show()
	})
	showDuration := time.Since(showStartedAt)
	totalDuration := time.Since(drawStartedAt)
	a.drawCount++
	a.lastDrawAt = a.now()
	a.lastDrawSpinner = a.spinnerFrame
	overlayDuration := time.Duration(0)
	if a.overlay != nil {
		overlayDuration = max(totalDuration-layoutDuration-clearDuration-bodyDuration-statusDuration-showDuration, 0)
	}
	if totalDuration > 20*time.Millisecond {
		logLevel := slog.LevelDebug
		if totalDuration > 75*time.Millisecond || clearDuration > 50*time.Millisecond || showDuration > 30*time.Millisecond {
			logLevel = slog.LevelWarn
		}
		tuiLog.Logger().Log(a.ctx, logLevel, "tui.draw.timing",
			"component", "tui",
			"total_ms", totalDuration.Milliseconds(),
			"layout_ms", layoutDuration.Milliseconds(),
			"clear_ms", clearDuration.Milliseconds(),
			"body_ms", bodyDuration.Milliseconds(),
			"status_ms", statusDuration.Milliseconds(),
			"overlay_ms", overlayDuration.Milliseconds(),
			"show_ms", showDuration.Milliseconds(),
			"width", w,
			"height", h,
			"active_tab", a.activeTab,
			"selected", a.selectedSessionName(),
			"has_overlay", a.overlay != nil,
			"overlay_type", fmt.Sprintf("%T", a.overlay),
			"mode", int(a.mode))
	}
}

func (a *App) layout() {
	w, h := a.screen.Size()
	if w != a.lastLayoutW || h != a.lastLayoutH {
		tuiLog.Logger().Debug("tui.layout.size_change",
			"component", "tui",
			"old_w", a.lastLayoutW,
			"old_h", a.lastLayoutH,
			"new_w", w,
			"new_h", h,
			"selected", a.selectedSessionName(),
			"active_tab", a.activeTab,
			"has_overlay", a.overlay != nil,
			"overlay_type", fmt.Sprintf("%T", a.overlay))
		a.lastLayoutW = w
		a.lastLayoutH = h
	}
	if w <= 0 || h <= 0 {
		a.headerRect = Rect{}
		a.tableRect = Rect{}
		a.detailRect = Rect{}
		a.statusRect = Rect{}
		return
	}

	headerH := 1
	if h < 2 {
		headerH = 0
	}
	statusH := 1
	if h-headerH < 1 {
		statusH = 0
	}
	a.headerRect = Rect{X: 0, Y: 0, W: w, H: headerH}
	statusY := h - statusH
	a.statusRect = Rect{X: 0, Y: statusY, W: w, H: statusH}

	contentTop := headerH
	availableH := max(h-headerH-statusH, 0)

	detailH := 0
	if a.selected != nil && availableH >= 5 {
		desired := 12
		maxDetail := max(availableH-3, 0)
		detailH = imin(desired, maxDetail)
		if detailH < 4 {
			detailH = 0
		}
	}

	tableH := availableH - detailH
	if tableH < 1 {
		if detailH > 0 {
			detailH = imax(0, availableH-1)
			tableH = availableH - detailH
		}
	}
	if tableH < 0 {
		tableH = 0
	}

	// Keep a slim side gutter so the table scrollbar does not look detached
	// from the terminal edge on narrow windows.
	tableX := 1
	if w < 8 {
		tableX = 0
	}
	tableW := max(w-(tableX*2), 0)
	a.tableRect = Rect{X: tableX, Y: contentTop, W: tableW, H: tableH}

	a.detailRect = Rect{}
	if detailH > 0 {
		a.detailRect = Rect{X: 0, Y: contentTop + tableH, W: w, H: detailH}
	}
}

func (a *App) positionTextFor() string {
	n := len(a.tableRowIdx)
	if n == 0 || !a.table.Active {
		return ""
	}
	row := a.table.SelectedRow
	if row <= 0 {
		return "Top"
	}
	if row >= n-1 {
		return "Bot"
	}
	return fmt.Sprintf("%d%%", (row*100)/imax(1, n-1))
}

// ---------------- Table population ----------------

// populateTable rebuilds the table rows from the current visible sessions.
func (a *App) populateTable() {
	startedAt := a.now()
	rows := make([][]TableCell, 0, len(a.visibleIdx)+1)
	a.tableRowIdx = a.tableRowIdx[:0]
	insertedGlobalSeparator := false
	for _, idx := range a.visibleIdx {
		sess := a.sessions[idx]
		if a.launchBasedir != "" && !insertedGlobalSeparator && a.launchBasedirRank(sess) > 0 {
			rows = append(rows, a.globalSessionListSeparatorRow())
			a.tableRowIdx = append(a.tableRowIdx, -1)
			insertedGlobalSeparator = true
		}
		rows = append(rows, a.rowFor(sess))
		a.tableRowIdx = append(a.tableRowIdx, idx)
	}
	a.table.Rows = rows
	a.table.SortCol = int(a.sortCol)
	a.table.SortAsc = a.sortAsc
	// Clamp selection after any change.
	if a.table.SelectedRow >= len(rows) {
		a.table.SelectedRow = imax(0, len(rows)-1)
	}
	duration := time.Since(startedAt)
	if duration > 15*time.Millisecond {
		logLevel := slog.LevelDebug
		if duration > 40*time.Millisecond {
			logLevel = slog.LevelWarn
		}
		tuiLog.Logger().Log(a.ctx, logLevel, "tui.table.populate_timing",
			"component", "tui",
			"duration_ms", duration.Milliseconds(),
			"sessions_total", len(a.sessions),
			"visible_total", len(a.visibleIdx),
			"rows_total", len(rows),
			"selected_row", a.table.SelectedRow,
			"sort_col", int(a.sortCol),
			"sort_asc", a.sortAsc,
			"filter", a.filter)
	}
}

func (a *App) globalSessionListSeparatorRow() []TableCell {
	style := StyleDefault.Foreground(ColorAccent).Bold(true)
	fillerStyle := StyleDefault.Foreground(ColorMuted).Dim(true)
	return []TableCell{
		{Text: "[global session list]", Style: style},
		{Text: strings.Repeat("-", 10), Style: fillerStyle},
		{Text: strings.Repeat("-", 8), Style: fillerStyle},
		{Text: "", Style: fillerStyle},
		{Text: "", Style: fillerStyle},
		{Text: "", Style: fillerStyle},
		{Text: "", Style: fillerStyle},
	}
}

func (a *App) rowFor(sess *session.Session) []TableCell {
	a.lastUsedMu.Lock()
	defer a.lastUsedMu.Unlock()
	return a.rowForLockedLastUsed(sess)
}

func (a *App) rowForLockedLastUsed(sess *session.Session) []TableCell {
	nameStyle := StyleDefault.Foreground(ColorText)
	if sess.Metadata.IsForkedSession {
		nameStyle = StyleDefault.Foreground(ColorFork)
	}
	model := a.modelCache[sess.Name]
	if model == "-" {
		model = ""
	}
	if model == "<synthetic>" {
		model = "synthetic"
	}
	modelStyle := StyleDefault.Foreground(ColorMuted)
	switch {
	case strings.Contains(model, "opus"):
		modelStyle = StyleDefault.Foreground(ColorModelOpus)
	case strings.Contains(model, "sonnet"):
		modelStyle = StyleDefault.Foreground(ColorModelSonnet)
	case strings.Contains(model, "haiku"):
		modelStyle = StyleDefault.Foreground(ColorModelHaiku)
	}
	subStyle := StyleSubtext
	// Dim ephemeral rows when they are being shown, so they are easy to ignore.
	if isEphemeralSession(sess) {
		dim := StyleDefault.Foreground(ColorMuted).Dim(true)
		nameStyle = dim
		modelStyle = dim
		subStyle = dim
	}
	summary := shortSummary(sess.Metadata.Context, 32)
	summaryStyle := StyleSubtext
	if summary == "" {
		summary = compactStyleEmpty
		summaryStyle = StyleDefault.Foreground(ColorMuted).Dim(true)
	}
	if isEphemeralSession(sess) {
		summaryStyle = StyleDefault.Foreground(ColorMuted).Dim(true)
	}
	msgCount := sessionMessageCount(a, sess)
	msgs := "-"
	msgStyle := StyleDefault.Foreground(ColorMuted).Dim(true)
	if msgCount > 0 {
		msgs = formatWithCommas(msgCount)
		msgStyle = subStyle
	}
	// If this session was just touched, paint the LAST ACTIVE cell in
	// the accent color so the eye catches the live update. The tint
	// fades after a few seconds so a steady stream of updates does
	// not turn the whole column accent.
	lastUsedStyle := subStyle
	if t, ok := a.recentlyUpdatedAt[sess.Name]; ok {
		if time.Since(t) < 4*time.Second {
			lastUsedStyle = StyleDefault.Foreground(ColorAccent).Bold(true)
		} else {
			delete(a.recentlyUpdatedAt, sess.Name)
		}
	}
	return []TableCell{
		{Text: sessionDisplayTitle(sess), Style: nameStyle},
		{Text: shortPath(sess.Metadata.WorkspaceRoot), Style: subStyle},
		{Text: util.FormatRelativeTime(lastUsedTime(sess)), Style: lastUsedStyle},
		{Text: model, Style: modelStyle},
		{Text: msgs, Style: msgStyle},
		{Text: summary, Style: summaryStyle},
		{Text: sess.Metadata.Created.Format("Jan 02"), Style: subStyle},
	}
}

// compactStyleEmpty is the placeholder shown in the SUMMARY column when
// a session has not yet had its context generated.
const compactStyleEmpty = "(no summary yet)"

// shortSummary truncates a multi-line free-form context string into one
// line bounded by maxRunes so it fits a table cell. It picks the first
// non-blank line to avoid leading blank or decoration lines.
func shortSummary(ctx string, maxRunes int) string {
	s := strings.TrimSpace(ctx)
	if s == "" {
		return ""
	}
	// Collapse newlines to spaces and squash runs of whitespace.
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if maxRunes > 0 {
		if rs := []rune(s); len(rs) > maxRunes {
			s = string(rs[:maxRunes-1]) + "…"
		}
	}
	return s
}

// rebuildVisible computes a.visibleIdx from a.sessions + a.filter.
// Also updates a.hiddenCount based on the ephemeral filter.
func (a *App) rebuildVisible() {
	a.visibleIdx = a.visibleIdx[:0]
	a.hiddenCount = 0
	f := strings.ToLower(a.filter)
	for i, sess := range a.sessions {
		if !a.showEphemeral && isEphemeralSession(sess) {
			a.hiddenCount++
			continue
		}
		if f != "" {
			hay := strings.ToLower(sessionDisplayTitle(sess) + " " + sess.Name + " " + sess.Metadata.WorkspaceRoot + " " + sess.Metadata.Context)
			if !strings.Contains(hay, f) {
				continue
			}
		}
		a.visibleIdx = append(a.visibleIdx, i)
	}
}

// sortSessions sorts a.sessions in place by current sort column.
func (a *App) sortSessions() {
	sort.SliceStable(a.sessions, func(i, j int) bool {
		x, y := a.sessions[i], a.sessions[j]
		if a.launchBasedir != "" {
			xRank := a.launchBasedirRank(x)
			yRank := a.launchBasedirRank(y)
			if xRank != yRank {
				return xRank < yRank
			}
		}
		cmp := 0
		switch a.sortCol {
		case SortColName:
			cmp = strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name))
		case SortColWorkspace:
			cmp = strings.Compare(x.Metadata.WorkspaceRoot, y.Metadata.WorkspaceRoot)
		case SortColModel:
			cmp = strings.Compare(a.modelCache[x.Name], a.modelCache[y.Name])
		case SortColMessages:
			cmp = compareInts(sessionMessageCount(a, x), sessionMessageCount(a, y))
		case SortColSummary:
			cmp = strings.Compare(strings.ToLower(x.Metadata.Context), strings.ToLower(y.Metadata.Context))
		case SortColCreated:
			cmp = compareTimes(x.Metadata.Created, y.Metadata.Created)
		case SortColUsed:
			cmp = compareTimes(lastUsedTime(x), lastUsedTime(y))
		}
		if cmp == 0 {
			return strings.ToLower(x.Name) < strings.ToLower(y.Name)
		}
		if !a.sortAsc {
			cmp = -cmp
		}
		return cmp < 0
	})
	a.rebuildVisible()
}

func (a *App) launchBasedirRank(sess *session.Session) int {
	if sess != nil && session.CanonicalWorkspaceRoot(sess.Metadata.WorkspaceRoot) == a.launchBasedir {
		return 0
	}
	return 1
}

func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func compareTimes(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	default:
		return 0
	}
}

func (a *App) toggleSort(col SortColumn) {
	if a.sortCol == col {
		a.sortAsc = !a.sortAsc
	} else {
		a.sortCol = col
		a.sortAsc = col == SortColName
	}
	a.sortSessions()
	a.populateTable()
	if a.table != nil {
		a.table.SortCol = sortColTableIndex(col)
		a.table.SortAsc = a.sortAsc
	}
}

// sortColTableIndex maps a SortColumn to the table column index used
// by the table widget for header indicators.
func sortColTableIndex(c SortColumn) int {
	switch c {
	case SortColName:
		return 0
	case SortColWorkspace:
		return 1
	case SortColUsed:
		return 2
	case SortColModel:
		return 3
	case SortColMessages:
		return 4
	case SortColSummary:
		return 5
	case SortColCreated:
		return 6
	}
	return -1
}

func (a *App) captureTableSelection() tableSelectionAnchor {
	anchor := tableSelectionAnchor{
		row:    a.table.SelectedRow,
		offset: a.table.Offset,
		active: a.table.Active,
	}
	if a.selected != nil {
		anchor.name = a.selected.Name
		anchor.sessionID = a.selected.Metadata.ProviderSessionID()
		anchor.detailsOpen = true
		return anchor
	}
	if a.table.Active {
		sess := a.sessionForTableRow(a.table.SelectedRow)
		if sess != nil {
			anchor.name = sess.Name
			anchor.sessionID = sess.Metadata.ProviderSessionID()
		}
	}
	return anchor
}

func (a *App) restoreTableSelection(anchor tableSelectionAnchor) {
	a.table.Active = anchor.active
	a.table.SelectedRow = anchor.row
	a.table.Offset = clamp(anchor.offset, 0, imax(0, len(a.tableRowIdx)-1))

	if anchor.name == "" && anchor.sessionID == "" {
		if len(a.tableRowIdx) == 0 {
			a.table.SelectedRow = 0
			a.table.Offset = 0
		} else if anchor.active {
			a.table.SelectedRow = clamp(anchor.row, 0, len(a.tableRowIdx)-1)
		}
		if anchor.detailsOpen {
			a.deselect()
		}
		return
	}

	updated, row := a.findVisibleSession(anchor.name, anchor.sessionID)
	if updated == nil {
		if anchor.detailsOpen {
			a.deselect()
		}
		if anchor.active && len(a.tableRowIdx) > 0 {
			a.table.Active = true
			a.table.SelectedRow = clamp(anchor.row, 0, len(a.tableRowIdx)-1)
		}
		return
	}

	if anchor.detailsOpen {
		a.selected = updated
	}
	a.table.Active = anchor.active || anchor.detailsOpen
	a.table.SelectedRow = row
	a.table.Offset = clamp(anchor.offset, 0, imax(0, len(a.tableRowIdx)-1))
}

// currentSession returns the session at the currently selected table row.
func (a *App) currentSession() *session.Session {
	return a.sessionForTableRow(a.table.SelectedRow)
}

func (a *App) sessionForTableRow(row int) *session.Session {
	if row < 0 || row >= len(a.tableRowIdx) {
		return nil
	}
	idx := a.tableRowIdx[row]
	if idx < 0 || idx >= len(a.sessions) {
		return nil
	}
	return a.sessions[idx]
}

func (a *App) trackSelection(row int) {
	sess := a.sessionForTableRow(row)
	if sess == nil {
		return
	}
	// Keep details in sync if currently shown. The first arrow key
	// press also auto-opens the pane so the user gets context without
	// hitting Space first.
	if a.selected != nil {
		a.selected = sess
		a.populateDetails()
	} else if a.table.Active {
		a.openDetails(sess)
	}
	// Kick off a background summary refresh if the cached Context is
	// stale. This runs whether or not the details pane is open, so the
	// SUMMARY column stays fresh as the user moves through the list.
	a.maybeRefreshSummary(sess)
}

// maybeRefreshSummary decides whether to request a new Context for sess and
// dispatches the request through the daemon. It guards against duplicates
// via summaryRefreshing, and only fires when the transcript has grown at
// least 5 messages beyond the count recorded when Context was last set.
// A session with no Context is always refreshed.
func (a *App) maybeRefreshSummary(sess *session.Session) {
	if sess == nil || a.cb.RefreshSummary == nil {
		return
	}
	if sess.Metadata.IsIncognito || sess.Metadata.ProviderTranscriptPath() == "" {
		return
	}
	if a.summaryRefreshing[sess.Name] {
		return
	}

	// Use daemon-reported message counts to judge growth without any
	// local transcript scanning on the UI path.
	msgNow := sessionMessageCount(a, sess)

	// Criteria for refresh:
	//   1. No Context yet.
	//   2. Transcript grew by >= 5 entries since last generation.
	//   3. Stored Context is visibly too long. Earlier revisions of the
	//      daemon prompt produced sentence-length summaries that no
	//      longer fit the five-word column contract; treat anything
	//      over six words as stale so a regeneration runs.
	needs := false
	ctx := strings.TrimSpace(sess.Metadata.Context)
	wordCount := 0
	if ctx != "" {
		wordCount = len(strings.Fields(ctx))
	}
	switch {
	case ctx == "":
		needs = true
	case wordCount > 6:
		needs = true
	case msgNow > 0 && msgNow-sess.Metadata.ContextMessageCount >= 5:
		needs = true
	}
	if !needs {
		return
	}

	a.summaryRefreshing[sess.Name] = true
	name := sess.Name
	if err := a.cb.RefreshSummary(sess, func(updated *session.Session) {
		a.postInterrupt(summaryRefreshDone{name: name, updated: updated})
		a.postInterrupt(summaryRefreshed{})
	}); err != nil {
		a.summaryRefreshing[name] = false
		tuiLog.Logger().Warn("tui.summary.refresh.request_failed", "component", "tui", "session", name, "error", err)
	}
}

// summaryRefreshed signals that a background summary update completed.
type summaryRefreshed struct{}

// ---------------- Selection / details ----------------

// detailsHasFocus reports whether the details pane owns keyboard focus.
func (a *App) detailsHasFocus() bool {
	return a.selected != nil && a.details != nil && a.details.Focus != DetailsFocusNone
}

// cycleDetailsFocus advances focus through the three regions of the details
// layout in order: table -> left pane -> right pane -> back to table.
// If back is true, the cycle runs in reverse.
func (a *App) cycleDetailsFocus(back bool) {
	if a.details == nil {
		return
	}
	cur := a.details.Focus
	var next DetailsFocus
	if back {
		switch cur {
		case DetailsFocusNone:
			next = DetailsFocusRight
		case DetailsFocusLeft:
			next = DetailsFocusNone
		case DetailsFocusRight:
			next = DetailsFocusLeft
		}
	} else {
		switch cur {
		case DetailsFocusNone:
			next = DetailsFocusLeft
		case DetailsFocusLeft:
			next = DetailsFocusRight
		case DetailsFocusRight:
			next = DetailsFocusNone
		}
	}
	a.details.SetFocus(next)
}

func (a *App) openDetails(sess *session.Session) {
	if sess == nil {
		return
	}
	a.selected = sess
	a.mode = StatusDetail
	a.populateDetails()
}

func (a *App) deselect() {
	a.selected = nil
	a.mode = StatusBrowse
	if a.details != nil {
		a.details.SetFocus(DetailsFocusNone)
	}
}

// populateDetails renders the details pane for the currently selected
// session. If a full SessionDetail is already cached it paints synchronously.
// Otherwise it paints a lightweight "loading" placeholder and kicks off a
// background goroutine via loadDetailAsync. When the goroutine finishes, it
// posts a tcell.EventInterrupt so the main loop repaints with the result.
func (a *App) populateDetails() {
	if a.selected == nil {
		return
	}
	name := a.selected.Name
	contextState := a.contextStateCache[name]

	a.detailMu.Lock()
	cached, ok := a.detailCache[name]
	a.detailMu.Unlock()

	if ok {
		cached.ContextUsage = contextState.Usage
		cached.ContextUsageLoaded = contextState.Loaded
		cached.ContextUsageStatus = contextState.Status
		a.details.Set(a.selected, cached)
		return
	}

	// Paint a fast placeholder so the UI is never blocked on disk I/O.
	placeholder := SessionDetail{
		Model:                 a.modelCache[name],
		ConversationLoading:   true,
		ContextUsage:          contextState.Usage,
		ContextUsageLoaded:    contextState.Loaded,
		ContextUsageStatus:    contextState.Status,
		TranscriptStatsLoaded: false,
		TranscriptStatsStatus: "loading...",
	}
	a.details.Set(a.selected, placeholder)

	a.loadDetailAsync(a.selected)
}

// loadDetailAsync spawns a goroutine to call cb.GetSessionDetail.
// Duplicate calls for the same session are coalesced via detailLoading.
// The goroutine posts detailsLoaded; the event loop applies cache mutations.
func (a *App) loadDetailAsync(sess *session.Session) {
	if a.cb.GetSessionDetail == nil {
		return
	}
	name := sess.Name

	a.detailMu.Lock()
	if a.detailLoading[name] {
		a.detailMu.Unlock()
		return
	}
	a.detailLoading[name] = true
	a.detailMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("detail_load", fmt.Sprint(r))
			}
		}()
		detail, err := a.cb.GetSessionDetail(sess)
		a.postInterrupt(detailsLoaded{name: name, detail: detail, err: err})
	}()
}

func (a *App) drawSessionsLoadingState(r Rect) {
	clearRect(a.screen, r)
	lines := []struct {
		style tcell.Style
		text  string
	}{
		{style: StyleDefault.Bold(true), text: NewLoadingSpinner("Loading sessions", a.spinnerFrame).Text()},
		{style: StyleMuted, text: "Connecting to daemon and building the initial snapshot"},
	}
	a.drawCenteredLines(r, lines)
}

func (a *App) drawSessionsEmptyState(r Rect) {
	clearRect(a.screen, r)
	lines := []struct {
		style tcell.Style
		text  string
	}{
		{style: StyleDefault.Bold(true), text: "No sessions yet"},
		{style: StyleMuted, text: "Start a chat with claude and it will appear here"},
	}
	a.drawCenteredLines(r, lines)
}

func (a *App) drawCenteredLines(r Rect, lines []struct {
	style tcell.Style
	text  string
},
) {
	if r.W <= 0 || r.H <= 0 || len(lines) == 0 {
		return
	}
	startY := r.Y + (r.H-len(lines))/2
	for i, line := range lines {
		y := startY + i
		if y < r.Y || y >= r.Y+r.H {
			continue
		}
		x := r.X
		if width := runeCount(line.text); width < r.W {
			x = r.X + (r.W-width)/2
		}
		drawString(a.screen, x, y, line.style, line.text, r.X+r.W-x)
	}
}

func formatHeartbeatAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	seconds := int(age.Round(time.Second).Seconds())
	return fmt.Sprintf("%ds", seconds)
}

// ---------------- Actions ----------------

func (a *App) resumeRow(row int) {
	if a.cb.ResumeSession == nil {
		return
	}
	sess := a.sessionForTableRow(row)
	if sess == nil {
		return
	}
	tuiLog.Logger().Debug("resume.row_selected", "session", sess.Name, "row", row)
	a.runResumeLifecycle(sess, "resumeRow")
}

func (a *App) newSession() {
	a.openNewStarterModal()
}

func (a *App) defaultLaunchCWD() string {
	if s := strings.TrimSpace(a.dashboardLaunchCWD); s != "" {
		return s
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// openNewStarterModal offers a fast default (cwd where clyde started) or the file picker.
func (a *App) openNewStarterModal() {
	cwd := a.defaultLaunchCWD()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	entries := []OptionsModalEntry{
		{
			Label: "Start in this directory",
			Hint:  shortPath(cwd),
			Action: func() {
				a.closeOverlay()
				a.openNewSessionTypeModal(cwd)
			},
		},
		{
			Label: "Choose folder",
			Hint:  "browse",
			Action: func() {
				a.closeOverlay()
				a.openNewSessionPrompt()
			},
		},
	}
	modal := NewOptionsModal("New chat", entries)
	modal.OnCancel = func() { a.closeOverlay() }
	a.overlay = modal
	a.mode = StatusFilter
}

func (a *App) openSidecarLaunchStarterModal() {
	cwd := a.defaultLaunchCWD()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	entries := []OptionsModalEntry{
		{
			Label: "Launch remote here",
			Hint:  shortPath(cwd),
			Action: func() {
				a.closeOverlay()
				a.openSidecarLaunchTypeModal(cwd)
			},
		},
		{
			Label: "Choose folder",
			Hint:  "browse",
			Action: func() {
				a.closeOverlay()
				a.openSidecarLaunchPrompt()
			},
		},
	}
	modal := NewOptionsModal("Launch sidecar session", entries)
	modal.OnCancel = func() { a.closeOverlay() }
	a.overlay = modal
	a.mode = StatusFilter
}

// openNewSessionPrompt opens the file picker so the user can navigate
// to the directory the new session should be anchored under. Pressing
// "s" in the picker commits the highlighted directory; Enter steps in.
// The directory is then passed to StartSessionWithBasedir. Esc cancels.
//
// If the user types a path that doesn't exist yet (e.g. /tmp/new-proj),
// the next overlay offers to create that directory before continuing.
// The remote-control + temp/persist choice happens after that.
func (a *App) openNewSessionPrompt() {
	cwd, _ := os.Getwd()
	picker := NewFilePickerOverlay("Pick basedir for new session", cwd)
	picker.OnCancel = func() { a.closeOverlay() }
	picker.OnSelect = func(path string) {
		a.closeOverlay()
		basedir := strings.TrimSpace(path)
		// Probe whether the path exists. When it doesn't, ask first
		// before creating it so the user does not accidentally seed
		// an empty workspace from a typo.
		if info, err := os.Stat(basedir); err != nil || !info.IsDir() {
			a.openCreateFolderConfirm(basedir)
			return
		}
		a.openNewSessionTypeModal(basedir)
	}
	a.overlay = picker
	a.mode = StatusFilter
}

func (a *App) openSidecarLaunchPrompt() {
	cwd, _ := os.Getwd()
	picker := NewFilePickerOverlay("Pick basedir for sidecar session", cwd)
	picker.OnCancel = func() { a.closeOverlay() }
	picker.OnSelect = func(path string) {
		a.closeOverlay()
		basedir := strings.TrimSpace(path)
		if info, err := os.Stat(basedir); err != nil || !info.IsDir() {
			a.openSidecarCreateFolderConfirm(basedir)
			return
		}
		a.openSidecarLaunchTypeModal(basedir)
	}
	a.overlay = picker
	a.mode = StatusFilter
}

// openCreateFolderConfirm asks the user to confirm that a missing
// basedir should be created on disk. Selecting Create makes the
// directory and continues into the type modal. Cancel reopens the
// picker so the user can pick a different path.
func (a *App) openCreateFolderConfirm(basedir string) {
	entries := []OptionsModalEntry{
		{
			Label: "Create folder and continue",
			Hint:  basedir,
			Action: func() {
				a.closeOverlay()
				if err := os.MkdirAll(basedir, 0o755); err != nil {
					tuiLog.Logger().Error("newsession.mkdir failed", "basedir", basedir, "error", err)
					return
				}
				a.openNewSessionTypeModal(basedir)
			},
		},
		{
			Label: "Pick a different folder",
			Hint:  "back to file picker",
			Action: func() {
				a.closeOverlay()
				a.openNewSessionPrompt()
			},
		},
	}
	modal := NewOptionsModal("Folder does not exist: "+shortPath(basedir), entries)
	modal.OnCancel = func() { a.closeOverlay() }
	a.overlay = modal
	a.mode = StatusFilter
}

func (a *App) openSidecarCreateFolderConfirm(basedir string) {
	entries := []OptionsModalEntry{
		{
			Label: "Create folder and continue",
			Hint:  basedir,
			Action: func() {
				a.closeOverlay()
				if err := os.MkdirAll(basedir, 0o755); err != nil {
					tuiLog.Logger().Error("sidecar.mkdir failed", "basedir", basedir, "error", err)
					return
				}
				a.openSidecarLaunchTypeModal(basedir)
			},
		},
		{
			Label: "Pick a different folder",
			Hint:  "back to file picker",
			Action: func() {
				a.closeOverlay()
				a.openSidecarLaunchPrompt()
			},
		},
	}
	modal := NewOptionsModal("Folder does not exist: "+shortPath(basedir), entries)
	modal.OnCancel = func() { a.closeOverlay() }
	a.overlay = modal
	a.mode = StatusFilter
}

// openNewSessionTypeModal asks whether the new session should be a
// regular tracked session or a temporary one (incognito). For the
// temp path the session auto deletes on exit unless the user opts to
// persist it via the post session prompt.
func (a *App) openNewSessionTypeModal(basedir string) {
	entries := []OptionsModalEntry{
		{
			Label: "New tracked session",
			Hint:  "persists in the dashboard",
			Action: func() {
				a.closeOverlay()
				a.launchNewTrackedSession(basedir)
			},
		},
		{
			Label: "Temporary session (incognito)",
			Hint:  "auto-delete on exit unless you keep it",
			Action: func() {
				a.closeOverlay()
				a.launchNewIncognitoSession(basedir)
			},
			Disabled: a.cb.StartIncognitoWithBasedir == nil,
		},
	}
	modal := NewOptionsModal("Start at "+shortPath(basedir), entries)
	modal.OnCancel = func() { a.closeOverlay() }
	a.overlay = modal
	a.mode = StatusFilter
}

func (a *App) openSidecarLaunchTypeModal(basedir string) {
	launch := func(incognito bool) {
		a.closeOverlay()
		a.activeTab = tabSidecar
		if a.tabs != nil {
			a.tabs.SetActive(tabSidecar)
		}
		a.sidecar = NewSidecarPanel("launching…", "", "")
		a.sidecar.status = "launching live session..."
		a.sidecarSessionName = ""
		a.sidecarSessionID = ""
		a.sidecarTailPending = false
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logUIGoroutinePanic("sidecar_launch", fmt.Sprint(r))
				}
			}()
			if a.cb.StartLiveSession == nil {
				a.postInterrupt(sidecarLaunchDone{err: fmt.Errorf("daemon live launch unavailable")})
				return
			}
			live, err := a.cb.StartLiveSession(LiveSessionStartRequest{
				Basedir:   basedir,
				Incognito: incognito,
			})
			a.postInterrupt(sidecarLaunchDone{
				sessionName: live.SessionName,
				sessionID:   live.SessionID,
				liveURL:     live.URL,
				err:         err,
			})
		}()
	}
	entries := []OptionsModalEntry{
		{
			Label: a.sidecarLaunchLabel(false),
			Hint:  "daemon-owned, re-enterable",
			Action: func() {
				launch(false)
			},
		},
		{
			Label: a.sidecarLaunchLabel(true),
			Hint:  "daemon-owned incognito",
			Action: func() {
				launch(true)
			},
		},
	}
	modal := NewOptionsModal("Launch sidecar session at "+shortPath(basedir), entries)
	modal.OnCancel = func() { a.closeOverlay() }
	a.overlay = modal
	a.mode = StatusFilter
}

func (a *App) sidecarLaunchLabel(temporary bool) string {
	if temporary {
		return "Temporary live session"
	}
	return "Tracked live session"
}

func (a *App) launchNewTrackedSession(basedir string) {
	runner := func() {
		switch {
		case a.cb.StartSessionWithBasedir != nil:
			_ = a.cb.StartSessionWithBasedir(basedir)
		case a.cb.StartSession != nil:
			_ = a.cb.StartSession()
		}
	}
	a.suspendImpl(runner)
	a.refreshSessions()
}

func (a *App) launchNewIncognitoSession(basedir string) {
	runner := func() {
		if a.cb.StartIncognitoWithBasedir != nil {
			_ = a.cb.StartIncognitoWithBasedir(basedir, false)
		}
	}
	a.suspendImpl(runner)
	a.refreshSessions()
}

func (a *App) viewSelected() {
	if a.selected == nil || a.cb.ViewContent == nil {
		return
	}
	sess := a.selected
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("view_content", fmt.Sprint(r))
			}
		}()
		content := a.cb.ViewContent(sess)
		a.postInterrupt(viewContentLoaded{sessionName: sess.Name, content: content})
	}()
}

func (a *App) openDeleteConfirm() {
	if a.selected == nil || a.cb.DeleteSession == nil {
		return
	}
	sess := a.selected
	m := &Modal{
		Title: "Delete Session",
		Body:  fmt.Sprintf("Delete session %q?", sess.Name),
		Details: []string{
			"Session folder and metadata will be removed.",
			"Claude transcript will be deleted.",
		},
		Buttons:     []string{"Cancel", "Delete"},
		ActiveIndex: 0,
		Destructive: true,
		Shortcuts:   map[rune]int{'y': 1, 'Y': 1, 'n': 0, 'N': 0},
	}
	m.OnChoice = func(idx int) {
		a.closeOverlay()
		if idx == 1 {
			a.deselect()
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logUIGoroutinePanic("delete_session", fmt.Sprint(r))
					}
				}()
				_ = a.cb.DeleteSession(sess)
				a.requestSessionsAsync("delete")
			}()
		}
	}
	a.overlay = m
	a.mode = StatusConfirm
}

func (a *App) openSearchForm() {
	// Minimal: prompt for query only, depth fixed at "quick".
	// Accept either the explicitly-detail-opened a.selected OR the
	// row that's merely highlighted in the table. The earlier
	// version bailed when a.selected was nil, which made `/` a
	// silent no-op on the freshly-launched dashboard until the user
	// pressed Space first, which was confusing and surfaced by the PTY suite.
	sess := a.rowSession()
	if sess == nil {
		return
	}
	input := NewTextInput("Search " + sess.Name + ": ")
	input.OnCancel = a.closeOverlay
	input.OnSubmit = func(q string) {
		a.closeOverlay()
		// TODO: wire search once cmd-level search is callable through cb.
		_ = q
	}
	a.overlay = &InputOverlay{Input: input, Title: "Search"}
	a.mode = StatusSearch
}

func (a *App) openCompactForm() {
	a.openRichCompactForm(a.selected)
}

func (a *App) openRichCompactForm(sess *session.Session) {
	if sess == nil {
		return
	}
	if !sessionCapabilities(sess).Compaction {
		return
	}
	panel := NewCompactPanel(sess.Name)
	if sess.Metadata.ProviderSessionID() != "" {
		panel.sessionID = sess.Metadata.ProviderSessionID()
	}
	if cachedModel, ok := a.modelCache[sess.Name]; ok && cachedModel != "" {
		panel.model = cachedModel
	}
	if contextState, ok := a.contextStateCache[sess.Name]; ok {
		panel.setInitialContextUsage(contextState.Usage, contextState.Loaded, contextState.Status)
	} else if sessionCapabilities(sess).ContextUsageInspect {
		panel.setInitialContextUsage(SessionContextUsage{
			Model:          "",
			TotalTokens:    0,
			MaxTokens:      0,
			Percentage:     0,
			MessagesTokens: 0,
		}, false, "loading...")
	}
	panel.OnClose = a.closeOverlay
	panel.OnPreview = func(req CompactRunRequest) {
		a.startCompactRun(req, false, panel)
	}
	panel.OnApply = func(req CompactRunRequest) {
		a.startCompactRun(req, true, panel)
	}
	panel.OnUndo = func() {
		a.startCompactUndo(sess.Name, panel)
	}
	if a.overlay != nil {
		a.overlayStack = append(a.overlayStack, overlayFrame{
			widget: a.overlay,
			mode:   a.mode,
		})
	}
	a.overlay = panel
	a.mode = StatusCompact
}

func (a *App) startCompactRun(req CompactRunRequest, apply bool, panel *CompactPanel) {
	var runner func(CompactRunRequest) (<-chan CompactEvent, <-chan error, func(), error)
	action := "preview"
	if apply {
		runner = a.cb.CompactApply
		action = "apply"
	} else {
		runner = a.cb.CompactPreview
	}
	if runner == nil {
		panel.ApplyCompactEvent(CompactEvent{
			Kind:    "status",
			Message: "daemon compact RPC unavailable",
		})
		return
	}
	if a.compactCancel != nil {
		a.compactCancel()
		a.compactCancel = nil
	}
	panel.SetBusy(action, true)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("compact_open", fmt.Sprint(r))
			}
		}()
		events, done, cancel, err := runner(req)
		a.postInterrupt(compactStreamOpened{
			action: action,
			events: events,
			done:   done,
			cancel: cancel,
			err:    err,
		})
	}()
}

func (a *App) runCompactStream(events <-chan CompactEvent, done <-chan error, action string) {
	var streamErr error
	for ev := range events {
		a.postInterrupt(compactStreamEvent{event: ev})
	}
	if done != nil {
		streamErr = <-done
	}
	a.postInterrupt(compactStreamDone{action: action, err: streamErr})
}

func (a *App) startCompactUndo(sessionName string, panel *CompactPanel) {
	if a.cb.CompactUndo == nil {
		panel.ApplyCompactEvent(CompactEvent{
			Kind:    "status",
			Message: "daemon compact undo unavailable",
		})
		return
	}
	panel.SetBusy("undo", true)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("compact_undo", fmt.Sprint(r))
			}
		}()
		result, err := a.cb.CompactUndo(sessionName)
		a.postInterrupt(compactUndoDone{
			result: result,
			err:    err,
		})
	}()
}

func (a *App) openFilter() {
	input := NewTextInput("Filter: ")
	input.Text = a.filter
	input.CursorX = runeCount(a.filter)
	input.OnChange = func(s string) {
		a.filter = s
		a.rebuildVisible()
		a.populateTable()
	}
	input.OnSubmit = func(s string) {
		a.filter = s
		a.rebuildVisible()
		a.populateTable()
		a.closeOverlay()
	}
	input.OnCancel = func() {
		a.filter = ""
		a.rebuildVisible()
		a.populateTable()
		a.closeOverlay()
	}
	a.overlay = &InputOverlay{Input: input, Title: "Filter"}
	a.mode = StatusFilter
}

// editConfigFile suspends the TUI, opens the chosen config in the
// user's $EDITOR (defaulting to vi), then resumes. The project flag
// picks the per-project config; otherwise the global one. The file is
// created with sensible defaults if it does not exist so the editor
// always has something to open.
func (a *App) editConfigFile(project bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	var path string
	if project {
		cwd, _ := os.Getwd()
		path = filepath.Join(cwd, ".claude", "clyde", "config.json")
	} else {
		path = filepath.Join(home, ".config", "clyde", "config.toml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		seed := "# clyde config\n"
		if filepath.Ext(path) == ".json" {
			seed = "{}\n"
		}
		_ = os.WriteFile(path, []byte(seed), 0o644)
	}

	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	a.suspendImpl(func() {
		cmd := exec.Command(editor, path)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
	})
}

// drawSidecarTab renders the Sidecar tab body. When no session is
// pinned the body shows a hint explaining the S shortcut. When a
// session is pinned the embedded panel handles the rendering.
func (a *App) drawSidecarTab(r Rect) {
	if a.sidecar == nil {
		drawString(a.screen, r.X+2, r.Y+1, StyleDefault.Foreground(ColorAccent).Bold(true), "Sidecar", r.W-4)
		drawString(a.screen, r.X+2, r.Y+3, StyleSubtext, "Press S on a Sessions row to pin its live transcript here.", r.W-4)
		drawString(a.screen, r.X+2, r.Y+4, StyleSubtext, "Press N here to launch a daemon-owned live session directly into Sidecar.", r.W-4)
		drawString(a.screen, r.X+2, r.Y+5, StyleSubtext, "Live sessions accept text from this panel when the daemon exposes send support.", r.W-4)
		return
	}
	a.sidecar.Draw(a.screen, r)
}

// pinSidecar sets the panel target to the given session. Cancels any
// running live stream subscription, then opens a fresh stream and
// fans the events into the panel buffer.
func (a *App) pinSidecar(sess *session.Session) {
	if sess == nil || sess.Metadata.ProviderSessionID() == "" {
		return
	}
	if !a.sidecarCanDrive(sess) {
		return
	}
	if a.sidecarCancel != nil {
		a.sidecarCancel()
		a.sidecarCancel = nil
	}
	bridgeURL := ""
	if b, ok := a.liveURLRecordFor(sess); ok {
		bridgeURL = b.URL
	}
	panel := NewSidecarPanel(sess.Name, sess.Metadata.ProviderSessionID(), bridgeURL)
	panel.OnSend = func(text string) error {
		send := a.sidecarSendFunc()
		if send == nil {
			return fmt.Errorf("daemon offline")
		}
		panel.status = "sending..."
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logUIGoroutinePanic("sidecar_pin_send", fmt.Sprint(r))
				}
			}()
			err := send(sess.Metadata.ProviderSessionID(), text)
			a.postInterrupt(sidecarSendDone{err: err})
		}()
		return nil
	}
	a.sidecar = panel
	a.sidecarSessionName = sess.Name
	a.sidecarSessionID = sess.Metadata.ProviderSessionID()
	a.sidecarTailPending = false

	if !a.sidecarCanStream(sess) {
		return
	}
	panel.status = "opening tail..."
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("sidecar_tail_open", fmt.Sprint(r))
			}
		}()
		events, cancel, err := a.openSidecarStream(sess.Metadata.ProviderSessionID())
		a.postInterrupt(sidecarTailOpened{events: events, cancel: cancel, err: err})
	}()
}

// runSidecarTail drains the daemon stream and appends each line to
// the panel buffer. PostEvent triggers a redraw on each line.
func (a *App) runSidecarTail(events <-chan TranscriptEntry, panel *SidecarPanel) {
	for line := range events {
		when := ""
		if !line.Timestamp.IsZero() {
			when = line.Timestamp.Local().Format("15:04:05")
		}
		panel.Append(SidecarLine{
			Role: line.Role,
			Text: line.Text,
			When: when,
		})
		a.postInterrupt(a)
	}
}

func (a *App) maybeOpenSidecarTail() {
	if a.sidecar == nil || a.sidecarSessionID == "" || !a.sidecarHasStreamCallback() {
		return
	}
	if a.sidecarCancel != nil {
		return
	}
	a.sidecar.status = "opening tail..."
	go func(sessionID string) {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("sidecar_tail_reopen", fmt.Sprint(r))
			}
		}()
		events, cancel, err := a.openSidecarStream(sessionID)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no transcript") {
				a.sidecarTailPending = true
				a.postInterrupt(sidecarStatusUpdate{status: "waiting for transcript..."})
				return
			}
		}
		a.postInterrupt(sidecarTailOpened{events: events, cancel: cancel, err: err})
	}(a.sidecarSessionID)
}

func (a *App) sidecarCanDrive(sess *session.Session) bool {
	if sess == nil || sess.Metadata.ProviderSessionID() == "" {
		return false
	}
	return a.sidecarCanStream(sess) && a.sidecarSendFunc() != nil
}

func (a *App) sidecarCanStream(sess *session.Session) bool {
	if sess == nil || sess.Metadata.ProviderSessionID() == "" {
		return false
	}
	return a.cb.StreamLiveSession != nil
}

func (a *App) sidecarHasStreamCallback() bool {
	return a.cb.StreamLiveSession != nil
}

func (a *App) sidecarSendFunc() func(sessionID, text string) error {
	return a.cb.SendLiveSession
}

func (a *App) openSidecarStream(sessionID string) (<-chan TranscriptEntry, func(), error) {
	if a.cb.StreamLiveSession == nil {
		return nil, nil, fmt.Errorf("daemon live stream unavailable")
	}
	events, cancel, err := a.cb.StreamLiveSession(sessionID)
	if err != nil {
		return nil, cancel, err
	}
	out := make(chan TranscriptEntry)
	done := make(chan struct{})
	var once sync.Once
	cancelAll := func() {
		once.Do(func() {
			if cancel != nil {
				cancel()
			}
			close(done)
		})
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				tuiLog.Logger().Error("ui.sidecar.stream_panic",
					"component", "ui",
					"session_id", sessionID,
					"panic", recovered,
				)
			}
		}()
		defer close(out)
		for {
			select {
			case <-done:
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				role := ev.Role
				if role == "" {
					role = ev.Kind
				}
				text := ev.Text
				if text == "" {
					text = ev.Kind
				}
				entry := TranscriptEntry{
					Role:      role,
					Text:      text,
					Timestamp: ev.Timestamp,
				}
				select {
				case <-done:
					return
				case out <- entry:
				}
			}
		}
	}()
	return out, cancelAll, nil
}

// drawSettingsTab renders the Settings tab body. It surfaces the active
// config file paths, the resolved values, and the actions a user can
// take. The body is read-mostly: editing happens in an external editor
// invoked via the e shortcut so the dashboard does not have to ship a
// full form widget for every config field.
func (a *App) drawStatsTab(r Rect) {
	if r.W <= 0 || r.H <= 0 {
		return
	}

	a.statsMu.Lock()
	stats := a.stats
	loaded := a.statsLoaded
	loading := a.statsLoading
	errMsg := a.statsErr
	a.statsMu.Unlock()

	type row struct {
		label string
		value string
		style tcell.Style
	}
	rows := []row{
		{label: "Stats", style: StyleDefault.Foreground(ColorAccent).Bold(true)},
		{label: "Provider aggregates from the daemon, including live inflight counts.", style: StyleSubtext},
		{},
	}
	if errMsg != "" {
		rows = append(rows, row{label: "Error", value: errMsg, style: StyleSubtext})
	}
	if stats.StreamErr != "" {
		rows = append(rows, row{label: "Stream", value: stats.StreamErr, style: StyleSubtext})
	}
	if !loaded {
		label := "Loading"
		value := "provider stats unavailable"
		if loading || a.cb.LoadStats != nil {
			value = NewLoadingSpinner("collecting provider stats...", a.spinnerFrame).Text()
		}
		rows = append(rows, row{label: label, value: value, style: StyleSubtext})
	} else {
		if len(stats.Providers) == 0 {
			rows = append(rows, row{label: "Providers", value: "no provider traffic observed yet", style: StyleSubtext})
		}
		for idx, provider := range stats.Providers {
			if idx > 0 {
				rows = append(rows, row{})
			}
			rows = append(rows,
				row{label: provider.Provider, style: StyleDefault.Foreground(ColorAccent).Bold(true)},
				row{label: "Requests", value: strconv.Itoa(provider.Requests), style: StyleSubtext},
				row{label: "Inflight", value: strconv.Itoa(provider.Inflight), style: StyleSubtext},
				row{label: "Streaming", value: strconv.Itoa(provider.Streaming), style: StyleSubtext},
				row{label: "Hit rate", value: formatHitRate(provider.HitRatio, provider.Requests), style: StyleSubtext},
				row{label: "Input", value: formatTokenCount64(provider.InputTokens), style: StyleSubtext},
				row{label: "Output", value: formatTokenCount64(provider.OutputTokens), style: StyleSubtext},
				row{label: "Cache read", value: formatTokenCount64(provider.CacheReadTokens), style: StyleSubtext},
				row{label: "Cache create", value: formatTokenCount64(provider.CacheCreationTokens), style: StyleSubtext},
				row{label: "Cache create (derived)", value: formatTokenCount64(provider.DerivedCacheCreationTokens), style: StyleSubtext},
				row{label: "Est. cost", value: formatCostMicrocents(provider.EstimatedCostMicrocents), style: StyleSubtext},
			)
			if !provider.LastSeen.IsZero() {
				rows = append(rows, row{label: "Last seen", value: provider.LastSeen.Format("15:04:05"), style: StyleSubtext})
			}
			if provider.Error != "" {
				rows = append(rows, row{label: "Last err", value: provider.Error, style: StyleSubtext})
			}
		}
		if !stats.LoadedAt.IsZero() {
			rows = append(rows, row{}, row{label: "Updated", value: stats.LoadedAt.Format("15:04:05"), style: StyleSubtext})
		}
	}

	for i, ln := range rows {
		text := ln.label
		if ln.value != "" {
			text = fmt.Sprintf("%-16s %s", ln.label, ln.value)
		}
		style := ln.style
		if style == (tcell.Style{}) {
			style = StyleSubtext
		}
		if i >= r.H-1 {
			break
		}
		drawString(a.screen, r.X+2, r.Y+1+i, style, text, r.W-4)
	}
}

func formatHitRate(hitRatio float64, requests int) string {
	if requests == 0 {
		return "0.0% (no cache telemetry yet)"
	}
	return fmt.Sprintf("%.1f%%", hitRatio*100)
}

func formatTokenCount64(n int64) string {
	if n <= 0 {
		return "0"
	}
	return fmt.Sprintf("%d tok", n)
}

func formatCostMicrocents(n int64) string {
	if n <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("$%.4f", float64(n)/100000000.0)
}

func (a *App) stepConfigSelection(delta int) {
	if len(a.configControls) == 0 {
		return
	}
	a.configSelected += delta
	if a.configSelected < 0 {
		a.configSelected = 0
	}
	if a.configSelected >= len(a.configControls) {
		a.configSelected = len(a.configControls) - 1
	}
}

func (a *App) selectedConfigControl() *ConfigControl {
	if len(a.configControls) == 0 || a.configSelected < 0 || a.configSelected >= len(a.configControls) {
		return nil
	}
	return &a.configControls[a.configSelected]
}

func (a *App) cycleSelectedConfigControl(delta int) {
	control := a.selectedConfigControl()
	if control == nil || control.ReadOnly || a.cb.UpdateConfigControl == nil {
		return
	}
	switch control.Type {
	case "bool":
		next := "true"
		if strings.EqualFold(control.Value, "true") {
			next = "false"
		}
		a.persistConfigControl(control.Key, next)
	case "enum":
		if len(control.Options) == 0 {
			return
		}
		idx := 0
		for i, opt := range control.Options {
			if opt.Value == control.Value {
				idx = i
				break
			}
		}
		idx = (idx + delta + len(control.Options)) % len(control.Options)
		a.persistConfigControl(control.Key, control.Options[idx].Value)
	}
}

func (a *App) activateSelectedConfigControl() {
	control := a.selectedConfigControl()
	if control == nil || control.ReadOnly {
		return
	}
	switch control.Type {
	case "bool", "enum":
		a.cycleSelectedConfigControl(1)
	case "path", "string":
		a.openConfigControlEditor(*control)
	}
}

func (a *App) persistConfigControl(key, value string) {
	if a.cb.UpdateConfigControl == nil {
		return
	}
	a.configLoading = true
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("config_control_update", fmt.Sprint(r))
			}
		}()
		err := a.cb.UpdateConfigControl(key, value)
		if err != nil {
			a.postInterrupt(configControlsLoaded{err: err})
			return
		}
		if key == "defaults.remote_control" {
			a.refreshSessions()
		}
		a.refreshConfigControls()
	}()
}

func (a *App) openConfigControlEditor(control ConfigControl) {
	input := NewTextInput("")
	input.Text = control.Value
	input.CursorX = len([]rune(control.Value))
	input.OnSubmit = func(value string) {
		a.closeOverlay()
		a.persistConfigControl(control.Key, value)
	}
	input.OnCancel = func() { a.closeOverlay() }
	a.overlay = &InputOverlay{
		Title: "Edit " + control.Label,
		Input: input,
	}
}

func (a *App) drawSettingsTab(r Rect) {
	if r.W <= 0 || r.H <= 0 {
		return
	}

	home, _ := os.UserHomeDir()
	globalCfg := filepath.Join(home, ".config", "clyde", "config.toml")
	globalCfgJSON := filepath.Join(home, ".config", "clyde", "config.json")
	cwd, _ := os.Getwd()
	projectCfg := filepath.Join(cwd, ".claude", "clyde", "config.json")

	type row struct {
		label string
		value string
		style tcell.Style
	}
	rows := []row{
		{label: "Settings", style: StyleDefault.Foreground(ColorAccent).Bold(true)},
		{},
		{label: "Global config", value: configRowDescription(globalCfg, globalCfgJSON), style: StyleSubtext},
		{label: "Project config", value: configRowDescription(projectCfg), style: StyleSubtext},
		{label: "Daemon log", value: filepath.Join(home, ".local", "state", "clyde", "clyde.jsonl"), style: StyleSubtext},
		{label: "Sessions root", value: filepath.Join(home, ".local", "share", "clyde", "sessions"), style: StyleSubtext},
		{},
		{label: "Controls", style: StyleDefault.Foreground(ColorAccent).Bold(true)},
	}
	if a.configLoading {
		rows = append(rows, row{label: "Loading", value: NewLoadingSpinner("fetching daemon-backed controls...", a.spinnerFrame).Text(), style: StyleSubtext})
	}
	if a.configErr != "" {
		rows = append(rows, row{label: "Error", value: a.configErr, style: StyleSubtext})
	}
	section := ""
	for i, control := range a.configControls {
		if control.Section != "" && control.Section != section {
			section = control.Section
			rows = append(rows, row{}, row{label: section, style: StyleMuted.Bold(true)})
		}
		prefix := "  "
		style := StyleSubtext
		if i == a.configSelected {
			prefix = "> "
			style = StyleDefault.Foreground(ColorAccent)
		}
		value := control.Value
		if value == "" {
			value = control.DefaultValue
		}
		rows = append(rows, row{
			label: prefix + control.Label,
			value: value,
			style: style,
		})
	}
	rows = append(rows,
		row{},
		row{label: "Actions", style: StyleDefault.Foreground(ColorAccent).Bold(true)},
		row{label: "  e  edit global config in $EDITOR", style: StyleSubtext},
		row{label: "  E  edit project config in $EDITOR", style: StyleSubtext},
		row{label: "  Enter/Space toggle or edit selected control", style: StyleSubtext},
		row{label: "  h/l or ←/→ cycle enum and bool controls", style: StyleSubtext},
		row{label: "  j/k or ↑/↓ move between controls", style: StyleSubtext},
		row{label: "  R  reload config (or restart daemon when offline)", style: StyleSubtext},
		row{label: "  1  Sessions  2  Stats  4  Sidecar", style: StyleSubtext},
		row{},
		row{label: "Tip", style: StyleDefault.Foreground(ColorAccent).Bold(true)},
		row{label: "  TUI controls write ~/.config/clyde/config.toml through the daemon.", style: StyleSubtext},
		row{label: "  Use e/E as escape hatches when you need to inspect or edit the", style: StyleSubtext},
		row{label: "  backing files directly.", style: StyleSubtext},
	)

	for i, ln := range rows {
		text := ln.label
		if ln.value != "" {
			text = fmt.Sprintf("%-16s %s", ln.label, ln.value)
		}
		style := ln.style
		if style == (tcell.Style{}) {
			style = StyleSubtext
		}
		if i >= r.H-1 {
			break
		}
		drawString(a.screen, r.X+2, r.Y+1+i, style, text, r.W-4)
	}
}

// configRowDescription returns a "<path> (status)" string where status
// is one of "exists" or "missing". Useful for surfacing the active
// config files in the Settings tab without scattering os.Stat calls
// across the draw code.
func configRowDescription(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p + "  (exists)"
		}
	}
	if len(paths) == 0 {
		return ""
	}
	return paths[0] + "  (missing)"
}

// openHelpModal shows the full keymap. Triggered by "?" anywhere in
// the dashboard. The modal uses the same widget as the per-session
// options popup, with disabled entries used for the static rows.
func (a *App) openHelpModal() {
	close := func() { a.closeOverlay() }
	rows := []OptionsModalEntry{
		{Label: "Movement (nvim-style)", Disabled: true},
		{Label: "  j / ↓        next row", Disabled: true},
		{Label: "  k / ↑        prev row", Disabled: true},
		{Label: "  h / ←        scroll left", Disabled: true},
		{Label: "  l / to        scroll right", Disabled: true},
		{Label: "  g            top", Disabled: true},
		{Label: "  G            bottom", Disabled: true},
		{Label: "  PgUp/PgDn    page", Disabled: true},
		{Label: "Actions", Disabled: true},
		{Label: "  Enter / O    options for highlighted row", Disabled: true},
		{Label: "  Space        toggle details pane", Disabled: true},
		{Label: "  /            search transcript (or filter list)", Disabled: true},
		{Label: "  v            view transcript", Disabled: true},
		{Label: "  s            search transcript content", Disabled: true},
		{Label: "  c            compact session", Disabled: true},
		{Label: "  d            delete session", Disabled: true},
		{Label: "  f            fork session", Disabled: true},
		{Label: "Globals (Shift)", Disabled: true},
		{Label: "  N            new session", Disabled: true},
		{Label: "  R            refresh (or restart daemon when offline)", Disabled: true},
		{Label: "  B            edit basedir", Disabled: true},
		{Label: "  H            show/hide test sessions", Disabled: true},
		{Label: "  S            pin row in Sidecar tab", Disabled: true},
		{Label: "  q / Esc      quit / dismiss / deselect", Disabled: true},
		{Label: "  ?            this help", Disabled: true},
		{Label: "Tabs", Disabled: true},
		{Label: "  1            Sessions tab", Disabled: true},
		{Label: "  2            Stats tab", Disabled: true},
		{Label: "  3            Settings tab", Disabled: true},
		{Label: "  4            Sidecar tab (live session view)", Disabled: true},
		{Label: "  !@#$%        sort columns 1..5", Disabled: true},
		{Label: "Live sessions", Disabled: true},
		{Label: "  Drive in sidecar from row Options popup", Disabled: true},
		{Label: "  Open/copy live URL from row Options popup", Disabled: true},
		{Label: "Settings tab only", Disabled: true},
		{Label: "  e            edit global config in $EDITOR", Disabled: true},
		{Label: "  E            edit project config in $EDITOR", Disabled: true},
		{Label: "  G            activate selected config control", Disabled: true},
		{Label: "  h/l          cycle enum and bool config controls", Disabled: true},
		{Label: "Compact form", Disabled: true},
		{Label: "  Up/Down      move focus between rows", Disabled: true},
		{Label: "  Space        toggle focused checkbox", Disabled: true},
		{Label: "  Left/Right   adjust slider or focused keep-last", Disabled: true},
		{Label: "  b            open boundary management overlay", Disabled: true},
		{Label: "  Enter/Space  select focused item (Apply asks to confirm)", Disabled: true},
		{Label: "Compact result", Disabled: true},
		{Label: "  u            undo last compact (restore backup)", Disabled: true},
	}
	modal := NewOptionsModal("Keyboard shortcuts", rows)
	modal.OnCancel = close
	a.overlay = modal
}

// findSessionByName returns the in-memory session matching name or visible
// display title, or nil. Used after a refresh to pick up updated metadata.
// findVisibleRowByName returns the visible row index for the session
// with the given name or visible display title, or -1 if it is not currently in the visible
// list. The post session prompt uses this to re locate the row after
// a refresh cycle so repeated Resume clicks keep firing.
func (a *App) findVisibleRowByName(name string) int {
	for vi, idx := range a.tableRowIdx {
		if idx >= 0 && idx < len(a.sessions) && sessionMatchesLookup(a.sessions[idx], name, "") {
			return vi
		}
	}
	return -1
}

func (a *App) findVisibleSession(name, sessionID string) (*session.Session, int) {
	for vi, idx := range a.tableRowIdx {
		if idx < 0 || idx >= len(a.sessions) {
			continue
		}
		sess := a.sessions[idx]
		if sess == nil {
			continue
		}
		if sessionMatchesLookup(sess, name, sessionID) {
			return sess, vi
		}
	}
	return nil, -1
}

func (a *App) findSessionByName(name string) *session.Session {
	for _, s := range a.sessions {
		if s != nil && sessionMatchesLookup(s, name, "") {
			return s
		}
	}
	return nil
}

func sessionDisplayTitle(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	displayTitle := strings.TrimSpace(sess.Metadata.DisplayTitle)
	if displayTitle != "" {
		return displayTitle
	}
	return sess.Name
}

func sessionMatchesLookup(sess *session.Session, name, sessionID string) bool {
	if sess == nil {
		return false
	}
	if sessionID != "" && sess.Metadata.ProviderSessionID() == sessionID {
		return true
	}
	lookupName := strings.TrimSpace(name)
	if lookupName == "" {
		return false
	}
	if sess.Name == lookupName {
		return true
	}
	return strings.EqualFold(sessionDisplayTitle(sess), lookupName)
}

// liveURLRecordFor returns the cached live URL backing record for sess,
// if any.
func (a *App) liveURLRecordFor(sess *session.Session) (LiveURL, bool) {
	if sess == nil || sess.Metadata.ProviderSessionID() == "" {
		return LiveURL{}, false
	}
	a.liveURLMu.RLock()
	defer a.liveURLMu.RUnlock()
	b, ok := a.liveURLs[sess.Metadata.ProviderSessionID()]
	return b, ok
}

func (a *App) liveURLForSession(sess *session.Session) (string, bool) {
	if sess == nil {
		return "", false
	}
	sessionID := sess.Metadata.ProviderSessionID()
	if sessionID == "" {
		return "", false
	}
	if a.sidecar != nil && a.sidecarSessionID == sessionID && strings.TrimSpace(a.sidecar.LiveURL) != "" {
		return strings.TrimSpace(a.sidecar.LiveURL), true
	}
	if b, ok := a.liveURLRecordFor(sess); ok && strings.TrimSpace(b.URL) != "" {
		return strings.TrimSpace(b.URL), true
	}
	return "", false
}

func sessionCapabilities(sess *session.Session) session.ProviderCapabilities {
	if sess == nil {
		return session.ProviderCapabilities{}
	}
	return sess.SessionProviderCapabilities()
}

// rowSession returns the session under the table cursor regardless of
// whether the details pane is currently showing it. Returns nil when no
// row is highlighted.
func (a *App) rowSession() *session.Session {
	if a.selected != nil {
		return a.selected
	}
	return a.sessionForTableRow(a.table.SelectedRow)
}

// openSessionOptions shows the per-session options popup for the row at
// the given visible index. Used for table OnActivate (Enter or
// double-click). A no-op when the row is out of range.
func (a *App) openSessionOptions(row int) {
	sess := a.sessionForTableRow(row)
	if sess == nil {
		tuiLog.Logger().Debug("tui.overlay.options.skipped",
			"component", "tui",
			"reason", "row_not_session",
			"row", row,
			"rows_total", len(a.tableRowIdx))
		return
	}
	tuiLog.Logger().Debug("tui.overlay.options.open_by_row",
		"component", "tui",
		"row", row,
		"session", sess.Name,
		"selected", a.selectedSessionName(),
		"active_tab", a.activeTab)
	a.openSessionOptionsFor(sess)
}

// openSessionOptionsFor builds the options menu for the given session
// and installs it as the active overlay. Resume is the default cursor
// position so a user who just wants the old behavior types Enter twice.
func (a *App) openSessionOptionsFor(sess *session.Session) {
	if sess == nil {
		tuiLog.Logger().Debug("tui.overlay.options.skipped",
			"component", "tui",
			"reason", "session_nil")
		return
	}
	close := func() { a.closeOverlay() }
	modal := NewOptionsModal(sess.Name, a.sessionOptionsEntries(sess, close))
	modal.OnCancel = close
	modal.StatsSegments, modal.StatsLoading = a.buildSessionStatsSegments(sess)
	modal.StatsSessionName = sess.Name
	a.overlay = modal
	tuiLog.Logger().Debug("tui.overlay.options.opened",
		"component", "tui",
		"session", sess.Name,
		"entries_total", len(modal.Entries),
		"stats_loading", modal.StatsLoading,
		"selected", a.selectedSessionName(),
		"active_tab", a.activeTab)
}

func (a *App) refreshOpenOptionsModalStats(name string) {
	modal, ok := a.overlay.(*OptionsModal)
	if !ok || modal.StatsSessionName != name {
		return
	}
	sess := a.findSessionByName(name)
	if sess == nil && a.returnPathSession != nil && a.returnPathSession.Name == name {
		sess = a.resolveSession(a.returnPathSession)
	}
	if sess == nil {
		return
	}
	modal.StatsSegments, modal.StatsLoading = a.buildSessionStatsSegments(sess)
}

func (a *App) applyExportStatsResult(name string, stats SessionExportStats, err error) {
	applyToPanel := func(panel *ExportPanel) {
		if panel == nil || panel.sessionName != name {
			return
		}
		if err != nil {
			panel.status = "failed to load export stats: " + err.Error()
			return
		}
		panel.ApplyStats(stats)
	}
	if panel, ok := a.overlay.(*ExportPanel); ok {
		applyToPanel(panel)
	}
	for i := range a.overlayStack {
		panel, ok := a.overlayStack[i].widget.(*ExportPanel)
		if !ok {
			continue
		}
		applyToPanel(panel)
	}
}

// sessionOptionsEntriesWithoutResume returns the standard session
// action rows minus the leading "Resume" entry. The return prompt uses
// this so the prompt's top-level "Return back to chat" row stays the
// only resume affordance and Resume does not appear twice.
func (a *App) sessionOptionsEntriesWithoutResume(sess *session.Session, close func()) []OptionsModalEntry {
	entries := a.sessionOptionsEntries(sess, close)
	if len(entries) > 0 && entries[0].Label == "Resume" {
		entries = entries[1:]
	}
	return entries
}

func (a *App) sessionOptionsEntries(sess *session.Session, close func()) []OptionsModalEntry {
	caps := sessionCapabilities(sess)
	entries := []OptionsModalEntry{
		{
			Label: "Resume",
			Hint:  "load this session",
			Action: func() {
				close()
				// Funnel through resumeSession so the options popup path,
				// the return prompt path, and the row activation path all
				// share one resume implementation. Earlier the popup path
				// inlined a slightly different resume sequence and silently
				// drifted from the others when bugs were fixed in resumeRow.
				a.resumeSession(sess)
			},
		},
		{
			Label: "View transcript",
			Hint:  "v",
			Action: func() {
				close()
				a.viewSelected()
			},
			Disabled: a.cb.ViewContent == nil || !caps.TranscriptExport,
		},
		{
			Label: "Export transcript",
			Hint:  "x",
			Action: func() {
				close()
				a.openExportOptions(sess)
			},
			Disabled: a.cb.ExportSession == nil || !caps.TranscriptExport,
		},
		{
			Label: "Edit basedir",
			Hint:  "b",
			Action: func() {
				close()
				a.openBasedirEditor(sess)
			},
		},
		{
			Label: "Drive in sidecar",
			Hint:  "live stream",
			Action: func() {
				close()
				if !a.sidecarCanDrive(sess) {
					tuiLog.Logger().Warn("sidecar.drive unavailable", "session", sess.Name)
					return
				}
				a.pinSidecar(sess)
				a.activeTab = tabSidecar
				if a.tabs != nil {
					a.tabs.SetActive(tabSidecar)
				}
			},
			Disabled: !a.sidecarCanDrive(sess),
		},
		a.openLiveURLEntry(sess, close),
		a.copyLiveURLEntry(sess, close),
		{
			Label: "Rename",
			Hint:  "edits the registry name",
			Action: func() {
				close()
				a.openRenamePrompt(sess)
			},
			Disabled: a.cb.RenameSession == nil,
		},
		{
			Label: "Compact",
			Hint:  "c",
			Action: func() {
				a.openRichCompactForm(sess)
			},
			Disabled: !caps.Compaction,
		},
		{
			Label: "Claude context",
			Hint:  "default / 200k / 1M",
			Action: func() {
				a.openSessionContextWindowOptions(sess, close)
			},
			Disabled: a.cb.UpdateSessionContextWindow == nil || !caps.PerSessionSettings,
		},
		{
			Label: "Fork",
			Hint:  "f",
			Action: func() {
				close()
				a.doFork()
			},
			Disabled: a.cb.ForkSession == nil || !caps.ForkByID,
		},
		{
			Label: "Delete",
			Hint:  "d",
			Action: func() {
				close()
				a.openDeleteConfirm()
			},
			Disabled: a.cb.DeleteSession == nil,
		},
	}
	return entries
}

func (a *App) openSessionContextWindowOptions(sess *session.Session, closeParent func()) {
	if sess == nil || a.cb.UpdateSessionContextWindow == nil {
		return
	}
	if closeParent != nil {
		closeParent()
	}
	apply := func(value string) func() {
		return func() {
			err := a.cb.UpdateSessionContextWindow(sess, value)
			if err != nil {
				a.mode = StatusFilter
				return
			}
			a.closeOverlay()
			a.mode = StatusFilter
		}
	}
	entries := []OptionsModalEntry{
		{
			Label:  "Default",
			Hint:   "provider default",
			Action: apply(""),
		},
		{
			Label:  "Force 200k",
			Hint:   "this chat",
			Action: apply("200k"),
		},
		{
			Label:  "Force 1M",
			Hint:   "this chat",
			Action: apply("1m"),
		},
	}
	modal := NewOptionsModal("Claude context for "+sess.Name, entries)
	modal.OnCancel = func() {
		a.closeOverlay()
		a.mode = StatusFilter
	}
	a.overlay = modal
	a.mode = StatusFilter
}

func (a *App) openLiveURLEntry(sess *session.Session, close func()) OptionsModalEntry {
	url, ok := a.liveURLForSession(sess)
	return OptionsModalEntry{
		Label:    "Open live URL",
		Hint:     "browser",
		Disabled: !ok,
		Action: func() {
			close()
			if url == "" {
				return
			}
			if err := openExternalURL(url); err != nil {
				tuiLog.Logger().Warn("live_url.open failed", "session", sess.Name, "error", err)
			}
		},
	}
}

func (a *App) copyLiveURLEntry(sess *session.Session, close func()) OptionsModalEntry {
	url, ok := a.liveURLForSession(sess)
	return OptionsModalEntry{
		Label:    "Copy live URL",
		Hint:     "clipboard",
		Disabled: !ok,
		Action: func() {
			close()
			if url == "" {
				return
			}
			if err := CopyToClipboard(url); err != nil {
				tuiLog.Logger().Warn("live_url.copy failed", "session", sess.Name, "error", err)
			}
		},
	}
}

func openExternalURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// openRenamePrompt asks for the new session name via an inline input
// and routes the rename through the daemon when the callback is set.
// The wired callback hides whether the actual rename happened locally
// or via gRPC; either way the dashboard refreshes from the store on
// completion.
func (a *App) openRenamePrompt(sess *session.Session) {
	if sess == nil {
		return
	}
	input := NewTextInput("New name: ")
	input.Text = sess.Name
	input.CursorX = runeCount(sess.Name)
	input.OnSubmit = func(s string) {
		a.closeOverlay()
		newName := strings.TrimSpace(s)
		if newName == "" || newName == sess.Name {
			return
		}
		// Mutate the session's Name field so the callback wired in
		// cmd/root.go can read both old and new values from the
		// session struct without us widening the callback signature.
		oldName := sess.Name
		sess.Name = newName
		if a.cb.RenameSession != nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logUIGoroutinePanic("rename_session", fmt.Sprint(r))
					}
				}()
				if _, err := a.cb.RenameSession(sess); err != nil {
					sess.Name = oldName
				}
				a.requestSessionsAsync("rename")
			}()
		}
	}
	input.OnCancel = a.closeOverlay
	a.overlay = &InputOverlay{Input: input, Title: "Rename session"}
	a.mode = StatusFilter
}

// openBasedirEditor pops up an inline single-line input pre-filled with
// the session's current workspace root. Submitting writes the new value
// through the SetBasedir callback. An empty result clears the field.
func (a *App) openBasedirEditor(sess *session.Session) {
	if sess == nil {
		return
	}
	current := sess.Metadata.WorkspaceRoot
	input := NewTextInput("Basedir: ")
	input.Text = current
	input.CursorX = runeCount(current)
	input.OnSubmit = func(s string) {
		a.closeOverlay()
		newPath := strings.TrimSpace(s)
		if newPath == current {
			return
		}
		if a.cb.SetBasedir != nil {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						logUIGoroutinePanic("set_basedir", fmt.Sprint(r))
					}
				}()
				_ = a.cb.SetBasedir(sess, newPath)
				a.requestSessionsAsync("basedir")
			}()
		}
	}
	input.OnCancel = a.closeOverlay
	a.overlay = &InputOverlay{Input: input, Title: "Edit basedir for " + sess.Name + " (empty clears)"}
	a.mode = StatusFilter
}

func (a *App) openExportOptions(sess *session.Session) {
	if sess == nil || a.cb.ExportSession == nil {
		return
	}
	if !sessionCapabilities(sess).TranscriptExport {
		return
	}
	stats, loaded := a.cachedExportStatsForSession(sess)
	panel := NewExportPanel(sess.Name, stats, defaultExportFolder())
	if !loaded && a.cb.LoadExportStats != nil {
		panel.StartLoadingStats()
		a.requestExportStatsAsync(sess)
	}
	panel.OnClose = a.closeOverlay
	panel.OnChooseFolder = func(panel *ExportPanel) {
		a.openExportFolderPicker(panel)
	}
	panel.OnPreview = func(req SessionExportRequest) {
		panel.status = "preview: " + panel.estimateLabel()
	}
	panel.OnExport = func(req SessionExportRequest) {
		a.exportSessionWithRequest(sess, req, panel)
	}
	a.overlay = panel
	a.mode = StatusExport
}

func (a *App) openExportFolderPicker(panel *ExportPanel) {
	if panel == nil {
		return
	}
	a.overlayStack = append(a.overlayStack, overlayFrame{widget: a.overlay, mode: a.mode})
	picker := NewFilePickerOverlay("Choose export folder", panel.folder)
	picker.OnCancel = a.closeOverlay
	picker.OnSelect = func(path string) {
		a.closeOverlay()
		panel.SetFolder(path)
	}
	a.overlay = picker
}

func (a *App) exportSessionWithRequest(sess *session.Session, req SessionExportRequest, panel *ExportPanel) {
	if sess == nil || a.cb.ExportSession == nil {
		return
	}
	panel.status = "export in progress..."
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("export_session", fmt.Sprint(r))
			}
		}()
		body, err := a.cb.ExportSession(sess, req)
		if err != nil {
			a.postInterrupt(exportFinished{title: "Export failed", body: err.Error(), stack: true})
			return
		}
		if req.CopyToClipboard {
			if err := CopyToClipboard(string(body)); err != nil {
				a.postInterrupt(exportFinished{title: "Copy failed", body: err.Error(), stack: true})
				return
			}
		}
		var savedPath string
		if req.SaveToFile {
			path := exportOutputPath(req)
			if _, err := os.Stat(path); err == nil && !req.Overwrite {
				a.postInterrupt(exportFinished{title: "Save failed", body: "file exists: " + path, stack: true})
				return
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				a.postInterrupt(exportFinished{title: "Save failed", body: err.Error(), stack: true})
				return
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				a.postInterrupt(exportFinished{title: "Save failed", body: err.Error(), stack: true})
				return
			}
			savedPath = path
		}
		a.postInterrupt(exportFinished{title: "Export complete", body: exportCompleteBody(req, savedPath), stack: true})
	}()
}

func exportCompleteBody(req SessionExportRequest, savedPath string) string {
	parts := make([]string, 0, 2)
	if req.CopyToClipboard {
		parts = append(parts, "Copied to clipboard.")
	}
	if savedPath != "" {
		parts = append(parts, "Saved to "+savedPath)
	}
	if len(parts) == 0 {
		return "Export completed."
	}
	return strings.Join(parts, "\n")
}

func defaultExportFolder() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, "Downloads")
	}
	return "."
}

func exportFormatExt(format SessionExportFormat) string {
	switch format {
	case SessionExportMarkdown:
		return "md"
	case SessionExportHTML:
		return "html"
	case SessionExportJSON:
		return "json"
	default:
		return "txt"
	}
}

func (a *App) doFork() {
	// Stub: fork via callback if provided.
	if a.cb.ForkSession == nil || a.selected == nil {
		return
	}
	sess := a.selected
	a.suspendImpl(func() { _ = a.cb.ForkSession(sess) })
	a.refreshSessions()
}

func (a *App) closeOverlay() {
	oldOverlay := fmt.Sprintf("%T", a.overlay)
	oldReturnPrompt := false
	if modal, ok := a.overlay.(*OptionsModal); ok && modal.Context == OptionsModalContextReturn {
		oldReturnPrompt = true
	}
	if a.compactCancel != nil {
		a.compactCancel()
		a.compactCancel = nil
	}
	if len(a.overlayStack) > 0 {
		idx := len(a.overlayStack) - 1
		frame := a.overlayStack[idx]
		a.overlayStack = a.overlayStack[:idx]
		a.overlay = frame.widget
		a.mode = frame.mode
		tuiLog.Logger().Debug("tui.overlay.closed",
			"component", "tui",
			"old_overlay_type", oldOverlay,
			"restored_overlay_type", fmt.Sprintf("%T", a.overlay),
			"selected", a.selectedSessionName(),
			"active_tab", a.activeTab,
			"mode", int(a.mode))
		return
	}
	a.overlay = nil
	a.restoreModeAfterOverlayClose()
	tuiLog.Logger().Debug("tui.overlay.closed",
		"component", "tui",
		"old_overlay_type", oldOverlay,
		"selected", a.selectedSessionName(),
		"active_tab", a.activeTab,
		"mode", int(a.mode))
	if !oldReturnPrompt {
		a.ensureReturnPromptForEvent("overlay.close")
	}
	if a.pendingReloadPath != "" {
		if err := validateSelfReloadCandidate(a.pendingReloadPath); err != nil {
			tuiLog.Logger().Warn("tui.self_reload.deferred_rejected",
				"component", "tui",
				"path", a.pendingReloadPath,
				"reason", a.pendingReloadReason,
				"err", err)
			a.pendingReloadPath = ""
			a.pendingReloadReason = ""
			return
		}
		tuiLog.Logger().Info("tui.self_reload.deferred_resume",
			"component", "tui",
			"path", a.pendingReloadPath,
			"reason", a.pendingReloadReason)
		a.reloadExecPath = a.pendingReloadPath
		a.pendingReloadPath = ""
		a.pendingReloadReason = ""
		a.running = false
	}
}

func (a *App) restoreModeAfterOverlayClose() {
	if a.selected != nil {
		a.mode = StatusDetail
		return
	}
	a.mode = StatusBrowse
}

func (a *App) openNoticeModal(title, body string) {
	modal := &Modal{
		Title:       title,
		Body:        body,
		Buttons:     []string{"OK"},
		ActiveIndex: 0,
	}
	modal.OnChoice = func(int) {
		a.closeOverlay()
	}
	a.overlay = modal
}

func (a *App) pushNoticeModal(title, body string) {
	if a.overlay != nil {
		a.overlayStack = append(a.overlayStack, overlayFrame{widget: a.overlay, mode: a.mode})
	}
	a.openNoticeModal(title, body)
}

// refreshSessions requests a daemon-owned dashboard snapshot without blocking
// the UI loop. The eventual sessionsLoaded interrupt applies it.
func (a *App) refreshSessions() {
	a.requestSessionsAsync("refresh.full")
}

func (a *App) refreshStats() {
	a.statsMu.Lock()
	a.statsLoaded = false
	a.statsErr = ""
	a.stats.StreamErr = ""
	a.statsMu.Unlock()
	a.requestStatsAsync("refresh")
}

func (a *App) requestStatsAsync(source string) {
	if a.cb.LoadStats == nil {
		return
	}
	a.statsMu.Lock()
	if a.statsLoading {
		a.statsMu.Unlock()
		return
	}
	if a.statsErr != "" && source == "draw" {
		a.statsMu.Unlock()
		return
	}
	if a.statsLoaded && source == "draw" {
		a.statsMu.Unlock()
		return
	}
	a.statsLoading = true
	a.statsMu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("stats_load", fmt.Sprint(r))
			}
		}()
		stats, err := a.cb.LoadStats()
		a.statsMu.Lock()
		a.statsLoading = false
		if err != nil {
			a.statsErr = err.Error()
			a.statsMu.Unlock()
			a.postInterrupt(a)
			return
		}
		a.stats = stats
		a.statsLoaded = true
		a.statsErr = ""
		a.statsMu.Unlock()
		a.postInterrupt(a)
	}()
}

func (a *App) restartDaemonAsync() {
	if a.cb.RestartDaemon == nil {
		return
	}
	a.daemonMu.Lock()
	a.daemonOnline = false
	a.daemonLastErr = "restarting daemon..."
	a.daemonMu.Unlock()
	a.postInterrupt(a)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("daemon_restart", fmt.Sprint(r))
			}
		}()
		if err := a.cb.RestartDaemon(); err != nil {
			a.setDaemonOffline("restart", err)
			return
		}
		a.setDaemonOnline("restart")
		a.refreshSessions()
		a.refreshStats()
	}()
}

// suspendAndRun shuts down the screen, runs fn (which may launch claude),
// then re-initializes the screen and repaints. This replaces tview's
// Suspend. The teardown path mirrors the Run defer so suspend leaves
// the terminal in the same clean state as a clean exit.
//
// Failure modes that previously caused an unresponsive dashboard after
// resume:
//   - initScreen returning an error left a.screen nil and the next draw
//     panicked; the panic was eaten by the Run defer, but the panic also
//     killed the event loop so the dashboard "froze" with no key response.
//   - fn panicking from inside the resume path took down the goroutine
//     before initScreen could put the alt-screen back, leaving the user
//     stuck in a half-reset terminal.
//
// The recover plus the explicit slog of init/draw failures gives the
// next operator a single grep to find the root cause when a freeze
// recurs (look for "tui.suspend" in the JSONL log).
func (a *App) suspendAndRun(fn func()) {
	totalStartedAt := a.now()
	const callbackWarnInitial = 10 * time.Second
	const callbackWarnEvery = 30 * time.Second
	if a.screen == nil {
		tuiLog.Logger().Warn("tui.suspend no_screen running fn directly")
		defer func() {
			if r := recover(); r != nil {
				tuiLog.Logger().Error("tui.suspend fn panic", "recover", fmt.Sprint(r), "err", "panic")
			}
		}()
		fn()
		return
	}
	tuiLog.Logger().Info("tui.suspend.start", "screen", fmt.Sprintf("%p", a.screen))
	tuiLog.Logger().Info("tui.suspend teardown")
	teardownStartedAt := a.now()
	a.teardownScreen()
	teardownDuration := time.Since(teardownStartedAt)
	tuiLog.Logger().Info("tui.suspend teardown complete", "screen", fmt.Sprintf("%p", a.screen))
	terminalcontrol.WriteSuspendPrepToTerminal()
	tuiLog.Logger().Debug("tui.suspend.terminal_prepared", "component", "tui")
	callbackStartedAt := a.now()
	callbackDone := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logUIGoroutinePanic("suspend_callback_watchdog", fmt.Sprint(r))
			}
		}()
		nextWarning := callbackWarnInitial
		timer := time.NewTimer(nextWarning)
		defer func() { timer.Stop() }()
		for {
			select {
			case <-callbackDone:
				return
			case <-timer.C:
				elapsed := time.Since(callbackStartedAt)
				tuiLog.Logger().Warn("tui.suspend.callback.still_running",
					"component", "tui",
					"elapsed_ms", elapsed.Milliseconds(),
					"selected", a.selectedSessionName(),
					"active_tab", a.activeTab,
					"overlay_type", fmt.Sprintf("%T", a.overlay))
				nextWarning = callbackWarnEvery
				timer.Reset(nextWarning)
			}
		}
	}()
	func() {
		defer close(callbackDone)
		defer func() {
			if r := recover(); r != nil {
				tuiLog.Logger().Error("tui.suspend fn panic", "recover", fmt.Sprint(r), "err", "panic")
			}
		}()
		fn()
	}()
	callbackDuration := time.Since(callbackStartedAt)
	tuiLog.Logger().Info("tui.suspend callback complete")
	if a.execNewBinaryBeforeReinit("suspend_return") {
		return
	}
	tuiLog.Logger().Info("tui.suspend reinit")
	reinitStartedAt := a.now()
	if err := a.initScreen(); err != nil {
		tuiLog.Logger().Error("tui.suspend reinit failed", "error", err, "err", err)
		// Mark the loop dead and bail. Without a screen the main loop
		// would panic on the next PollEvent. Better to exit cleanly so
		// the user sees their shell prompt and can relaunch clyde.
		a.running = false
		return
	}
	reinitDuration := time.Since(reinitStartedAt)
	defer func() {
		if r := recover(); r != nil {
			tuiLog.Logger().Error("tui.suspend draw panic", "recover", fmt.Sprint(r), "err", "panic")
			a.running = false
		}
	}()
	drawStartedAt := a.now()
	a.draw()
	drawDuration := time.Since(drawStartedAt)
	tuiLog.Logger().Info("tui.suspend resumed")
	totalDuration := time.Since(totalStartedAt)
	tuiLog.Logger().Info("tui.suspend.timing",
		"component", "tui",
		"teardown_ms", teardownDuration.Milliseconds(),
		"callback_ms", callbackDuration.Milliseconds(),
		"reinit_ms", reinitDuration.Milliseconds(),
		"draw_ms", drawDuration.Milliseconds(),
		"total_ms", totalDuration.Milliseconds(),
		"selected", a.selectedSessionName(),
		"active_tab", a.activeTab,
		"overlay_type", fmt.Sprintf("%T", a.overlay))
	if totalDuration > 200*time.Millisecond || reinitDuration > 120*time.Millisecond || drawDuration > 80*time.Millisecond {
		tuiLog.Logger().Warn("tui.suspend.slow",
			"component", "tui",
			"teardown_ms", teardownDuration.Milliseconds(),
			"callback_ms", callbackDuration.Milliseconds(),
			"reinit_ms", reinitDuration.Milliseconds(),
			"draw_ms", drawDuration.Milliseconds(),
			"total_ms", totalDuration.Milliseconds())
	}
}

// ---------------- Helpers used by overlays ----------------

// ViewerOverlay is a full-screen textbox with an Esc/q close binding.
type ViewerOverlay struct {
	Box     *TextBox
	OnClose func()
}

func (v *ViewerOverlay) Draw(scr tcell.Screen, r Rect) {
	v.Box.Draw(scr, r)
	// Footer hint
	if r.H > 1 {
		hint := " q/esc close   ↑↓ scroll "
		drawString(scr, r.X+r.W-runeCount(hint), r.Y+r.H-1, StyleMuted, hint, r.W)
	}
}

func (v *ViewerOverlay) HandleEvent(ev tcell.Event) bool {
	if ek, ok := ev.(*tcell.EventKey); ok {
		if ek.Key() == tcell.KeyEscape || (ek.Key() == tcell.KeyRune && ek.Rune() == 'q') {
			if v.OnClose != nil {
				v.OnClose()
			}
			return true
		}
	}
	return v.Box.HandleEvent(ev)
}

// InputOverlay centers a single-line input with a title.
type InputOverlay struct {
	Title string
	Input *TextInput
	rect  Rect
}

func (i *InputOverlay) Draw(scr tcell.Screen, r Rect) {
	w := min(60, r.W-4)
	h := 5
	box := Rect{X: r.X + (r.W-w)/2, Y: r.Y + (r.H-h)/2, W: w, H: h}
	i.rect = box

	// Clear behind the overlay
	clearRect(scr, box)
	// Border
	borderStyle := StyleDefault.Foreground(ColorBorder)
	scr.SetContent(box.X, box.Y, '┌', nil, borderStyle)
	scr.SetContent(box.X+box.W-1, box.Y, '┐', nil, borderStyle)
	scr.SetContent(box.X, box.Y+box.H-1, '└', nil, borderStyle)
	scr.SetContent(box.X+box.W-1, box.Y+box.H-1, '┘', nil, borderStyle)
	for x := box.X + 1; x < box.X+box.W-1; x++ {
		scr.SetContent(x, box.Y, '─', nil, borderStyle)
		scr.SetContent(x, box.Y+box.H-1, '─', nil, borderStyle)
	}
	for y := box.Y + 1; y < box.Y+box.H-1; y++ {
		scr.SetContent(box.X, y, '│', nil, borderStyle)
		scr.SetContent(box.X+box.W-1, y, '│', nil, borderStyle)
	}
	// Title
	if i.Title != "" {
		drawString(scr, box.X+2, box.Y+1, StyleMuted, i.Title, box.W-4)
	}
	// Input
	i.Input.Draw(scr, Rect{X: box.X + 2, Y: box.Y + 2, W: box.W - 4, H: 1})
}

func (i *InputOverlay) HandleEvent(ev tcell.Event) bool {
	if em, ok := ev.(*tcell.EventMouse); ok {
		x, y := em.Position()
		if i.rect.Contains(x, y) {
			return true
		}
	}
	return i.Input.HandleEvent(ev)
}

// ---------------- Shared helpers ----------------

// isEphemeralSession reports whether a session looks like a leaked test
// artifact or something rooted in a temp directory. These sessions pollute
// the dashboard and almost always have no transcript or a `synthetic` model.
//
// Signals we look for in the workspace path:
//   - /private/var/folders/... or /var/folders/... (macOS temp)
//   - /tmp/... (Unix temp)
//   - anything containing "/ginkgo" (Go test framework scratch dirs)
//   - anything containing "/clyde-" under a temp dir (our own tests)
func isEphemeralSession(sess *session.Session) bool {
	if sess == nil {
		return false
	}
	ws := sess.Metadata.WorkspaceRoot
	if ws == "" {
		return false
	}
	tempPrefixes := []string{
		"/private/var/folders/",
		"/var/folders/",
		"/tmp/",
		"/private/tmp/",
	}
	for _, p := range tempPrefixes {
		if strings.HasPrefix(ws, p) {
			return true
		}
	}
	return strings.Contains(ws, "/ginkgo")
}

// sessionMessageCount returns the daemon-provided visible message count
// used by the MSGS column and its sort key.
func sessionMessageCount(a *App, sess *session.Session) int {
	if a == nil || sess == nil {
		return 0
	}
	return a.messageCountCache[sess.Name]
}

// lastUsedTime returns the best available "last activity" timestamp for a
// session. Transcript file mtime is preferred because it advances on every
// message Claude appends, which is what the user actually means by "last
// used". When the transcript is missing or unreadable the metadata's
// LastAccessed timestamp serves as a fallback.
func lastUsedTime(sess *session.Session) time.Time {
	if sess == nil {
		return time.Time{}
	}
	return sess.Metadata.LastAccessed
}

// shortPath abbreviates a workspace path for display.
func shortPath(root string) string {
	if root == "" {
		return "-"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Base(root)
	}
	if root == home {
		return "~"
	}
	if strings.HasPrefix(root, home+"/") {
		return "~/" + root[len(home)+1:]
	}
	return root
}

func shortStableHash(value string) string {
	if value == "" {
		return "000000"
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(value))
	hex := strconv.FormatUint(uint64(hasher.Sum32()), 16)
	if len(hex) < 6 {
		return strings.Repeat("0", 6-len(hex)) + hex
	}
	return hex[:6]
}

func currentExecutableHash() string {
	path, err := os.Executable()
	if err != nil {
		tuiLog.Logger().Debug("tui.executable_hash.path_failed",
			"component", "tui",
			"err", err)
		return "unknown"
	}
	return executableHashForPath(path)
}

func executableHashForPath(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		tuiLog.Logger().Debug("tui.executable_hash.read_failed",
			"component", "tui",
			"path", path,
			"err", err)
		return "unknown"
	}
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])[:6]
}
