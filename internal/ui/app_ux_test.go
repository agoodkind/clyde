package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/terminalcontrol"
)

func stubSelfReloadProbe(t *testing.T, err error) {
	t.Helper()
	oldProbe := probeSelfReloadCandidate
	probeSelfReloadCandidate = func(string) error {
		return err
	}
	t.Cleanup(func() {
		probeSelfReloadCandidate = oldProbe
	})
}

func TestUX_OpenReturnPromptDoesNotBlockOnDetailExtraction(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()
	block := make(chan struct{})
	a.cb.GetSessionDetail = func(*session.Session) (SessionDetail, error) {
		<-block
		return SessionDetail{Model: "opus"}, nil
	}

	start := time.Now()
	sess := a.sessions[a.visibleIdx[0]]
	a.openReturnPrompt(sess)
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Fatalf("openReturnPrompt blocked on detail extraction: %s", elapsed)
	}
	modal, ok := a.overlay.(*OptionsModal)
	if !ok || modal.Context != OptionsModalContextReturn {
		t.Fatalf("overlay = %T, want return-context *OptionsModal", a.overlay)
	}
	if len(modal.TopEntries) != 0 {
		t.Fatalf("return modal should use the same single options list as session options, got %d top entries", len(modal.TopEntries))
	}
	if findModalAction(modal, "Quit clyde") == nil {
		t.Fatalf("return modal missing Quit clyde action")
	}
	if findModalAction(modal, "Return back to chat") == nil {
		t.Fatalf("return modal missing Return back to chat action")
	}

	close(block)
}

func TestUX_WriteSuspendTerminalPrepSequence(t *testing.T) {
	got := terminalcontrol.SuspendPrepSequence
	if !strings.HasSuffix(got, "\r") {
		t.Fatalf("terminal prep sequence should reset output column, got %q", got)
	}
	if !strings.Contains(got, "\x1b[2J\x1b[H") {
		t.Fatalf("terminal prep sequence should clear and home, got %q", got)
	}
}

func TestUX_TerminalModeResetSequenceDisablesAlternateScroll(t *testing.T) {
	for _, seq := range []string{
		"\x1b[?1000l", // X10 mouse
		"\x1b[?1002l", // drag mouse
		"\x1b[?1003l", // motion mouse
		"\x1b[?1006l", // SGR mouse
		"\x1b[?1007l", // alternate-scroll wheel translation
	} {
		if !strings.Contains(terminalModeResetSequence, seq) {
			t.Fatalf("terminalModeResetSequence missing %q", seq)
		}
	}
}

func TestUX_TerminalModeResetSequenceRestoresOutputColumn(t *testing.T) {
	if !strings.HasSuffix(terminalModeResetSequence, "\r") {
		t.Fatalf("terminal mode reset should reset output column, got %q", terminalModeResetSequence)
	}
	softReset := strings.LastIndex(terminalModeResetSequence, "\x1b[!p")
	autowrap := strings.LastIndex(terminalModeResetSequence, "\x1b[?7h")
	if softReset < 0 {
		t.Fatalf("terminalModeResetSequence missing soft reset")
	}
	if autowrap < 0 {
		t.Fatalf("terminalModeResetSequence missing autowrap enable")
	}
	if autowrap < softReset {
		t.Fatalf("autowrap enable must follow soft reset")
	}
}

func TestUX_NewSessionUsesConfigDefaultWithoutLiveURLPrompt(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()
	a.suspendImpl = func(fn func()) { fn() }

	var calls int
	a.cb.StartSessionWithBasedir = func(basedir string) error {
		if basedir != "/tmp/work" {
			t.Fatalf("basedir = %q, want /tmp/work", basedir)
		}
		calls++
		return nil
	}

	a.openNewSessionTypeModal("/tmp/work")
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	action := findModalAction(modal, "New tracked session")
	if action == nil {
		t.Fatalf("missing New tracked session action")
	}
	action()

	if calls != 1 {
		t.Fatalf("StartSessionWithBasedir calls = %d, want 1", calls)
	}
}

func TestUX_BasedirLaunchRanksMatchingSessionsFirst(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()

	target := session.CanonicalWorkspaceRoot("/Users/test/Sites/ws-1")
	a.launchBasedir = target
	a.sortSessions()
	a.populateTable()

	if len(a.visibleIdx) != 3 {
		t.Fatalf("visible sessions = %d, want 3", len(a.visibleIdx))
	}
	got := a.sessions[a.visibleIdx[0]]
	if got.Name != "test-session-01" {
		t.Fatalf("top session = %q, want test-session-01", got.Name)
	}
	if len(a.table.Rows) != 4 {
		t.Fatalf("table rows = %d, want 4 including separator", len(a.table.Rows))
	}
	if got := a.table.Rows[1][0].Text; got != "[global session list]" {
		t.Fatalf("separator row = %q, want [global session list]", got)
	}
	a.openSessionOptions(1)
	if a.overlay != nil {
		t.Fatalf("separator row should not open options, got %T", a.overlay)
	}
}

func TestUX_BasedirLaunchEnterUsesNormalOptions(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()
	a.launchBasedir = session.CanonicalWorkspaceRoot("/Users/test/Sites/ws-0")
	a.sortSessions()
	a.populateTable()

	a.openSessionOptions(0)
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	if findModalAction(modal, "Resume") == nil {
		t.Fatalf("basedir options missing Resume")
	}
	if findModalAction(modal, "New session here") != nil {
		t.Fatalf("basedir options should not include special New session here action")
	}
}

func TestUX_BasedirLaunchKeepsDefaultNewSessionFlow(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 0)
	defer cleanup()
	target := session.CanonicalWorkspaceRoot("/Users/test/Sites/no-existing-sessions")
	a.launchBasedir = target
	a.dashboardLaunchCWD = target

	a.newSession()

	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	startHere := findModalAction(modal, "Start in this directory")
	if startHere == nil {
		t.Fatalf("default new-session modal missing Start in this directory")
	}
	startHere()

	next, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay after start-here = %T, want *OptionsModal", a.overlay)
	}
	if findModalAction(next, "New tracked session") == nil {
		t.Fatalf("start-here did not open existing new-session type modal")
	}
}

func TestUX_SessionOptionsRespectProviderCapabilities(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	sess := a.sessions[0]
	sess.Metadata.Provider = session.ProviderCodex
	sess.Metadata.ProviderState = &session.ProviderOwnedMetadata{
		Current: session.ProviderSessionID{Provider: session.ProviderCodex, ID: "codex-123"},
	}
	a.cb.ViewContent = func(*session.Session) string { return "" }
	a.cb.ExportSession = func(*session.Session, SessionExportRequest) ([]byte, error) { return nil, nil }
	a.cb.ForkSession = func(*session.Session) error { return nil }

	a.openSessionOptionsFor(sess)
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	for _, label := range []string{
		"View transcript",
		"Export transcript",
		"Drive in sidecar",
		"Open live URL",
		"Copy live URL",
		"Compact",
		"Fork",
	} {
		entry := findModalEntry(modal, label)
		if entry == nil {
			t.Fatalf("missing %q entry", label)
		}
		if !entry.Disabled {
			t.Fatalf("%q disabled = false, want true for codex provider", label)
		}
	}
}

func TestUX_SnapshotRefreshKeepsHighlightedRowBySessionID(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()

	a.table.Active = true
	a.table.SelectedRow = 1
	highlighted := a.sessions[a.visibleIdx[1]]
	renamed := *highlighted
	renamed.Name = "renamed-highlight"
	renamed.Metadata.Name = "renamed-highlight"
	renamed.Metadata.LastAccessed = time.Now().Add(10 * time.Minute)

	a.applySessionSnapshot(SessionSnapshot{
		Sessions: []*session.Session{a.sessions[a.visibleIdx[0]], &renamed, a.sessions[a.visibleIdx[2]]},
		Models: map[string]string{
			a.sessions[a.visibleIdx[0]].Name: "opus",
			renamed.Name:                     "opus",
			a.sessions[a.visibleIdx[2]].Name: "opus",
		},
	})

	if !a.table.Active {
		t.Fatalf("table highlight inactive after snapshot refresh")
	}
	got := a.currentSession()
	if got == nil {
		t.Fatalf("no current session after snapshot refresh")
	}
	if got.Metadata.ProviderSessionID() != highlighted.Metadata.ProviderSessionID() {
		t.Fatalf("highlighted session ID = %q, want %q", got.Metadata.ProviderSessionID(), highlighted.Metadata.ProviderSessionID())
	}
	if got.Name != "renamed-highlight" {
		t.Fatalf("highlighted session name = %q, want renamed-highlight", got.Name)
	}
}

func TestUX_OpenOptionsModalStatsRefreshAfterDetailLoad(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()

	a.cb.GetSessionDetail = func(*session.Session) (SessionDetail, error) {
		return SessionDetail{
			Model:                 "opus",
			TranscriptStatsLoaded: true,
			TotalMessages:         42,
			LastMessageTokens:     128,
			ContextUsageLoaded:    true,
			ContextUsage: SessionContextUsage{
				TotalTokens:    1200,
				MaxTokens:      200000,
				Percentage:     1,
				MessagesTokens: 900,
			},
		}, nil
	}

	sess := a.sessions[a.visibleIdx[0]]
	a.openSessionOptionsFor(sess)
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	if !modal.StatsLoading {
		t.Fatalf("modal stats should start loading")
	}

	ev := scr.PollEvent()
	interrupt, ok := ev.(*tcell.EventInterrupt)
	if !ok {
		t.Fatalf("event = %T, want *tcell.EventInterrupt", ev)
	}
	a.handleEvent(interrupt)

	modal, ok = a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay after detail load = %T, want *OptionsModal", a.overlay)
	}
	if modal.StatsLoading {
		t.Fatalf("modal stats still loading after detail load")
	}
	lines := flattenSegments(modal.StatsSegments)
	if !strings.Contains(strings.Join(lines, "\n"), "42") {
		t.Fatalf("modal stats did not refresh from loaded detail:\n%s", strings.Join(lines, "\n"))
	}
}

func TestUX_ReturnPromptStatsRefreshAfterSessionListChanges(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()

	a.cb.GetSessionDetail = func(*session.Session) (SessionDetail, error) {
		return SessionDetail{
			Model:                 "opus",
			TranscriptStatsLoaded: true,
			TotalMessages:         42,
			ContextUsageLoaded:    true,
			ContextUsage: SessionContextUsage{
				TotalTokens: 1200,
				MaxTokens:   200000,
				Percentage:  1,
			},
		}, nil
	}

	sess := a.sessions[a.visibleIdx[0]]
	a.returnPathSession = sess
	a.openReturnPrompt(sess)
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	if !modal.StatsLoading {
		t.Fatalf("modal stats should start loading")
	}

	a.sessions = nil
	a.visibleIdx = nil
	ev := scr.PollEvent()
	interrupt, ok := ev.(*tcell.EventInterrupt)
	if !ok {
		t.Fatalf("event = %T, want *tcell.EventInterrupt", ev)
	}
	a.handleEvent(interrupt)

	modal, ok = a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay after detail load = %T, want *OptionsModal", a.overlay)
	}
	if modal.StatsLoading {
		t.Fatalf("modal stats still loading after detail load")
	}
	lines := flattenSegments(modal.StatsSegments)
	if !strings.Contains(strings.Join(lines, "\n"), "42") {
		t.Fatalf("modal stats did not refresh from loaded detail:\n%s", strings.Join(lines, "\n"))
	}
}

func TestUX_CloseCompactRestoresOptionsOverlay(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()

	sess := a.sessions[a.visibleIdx[0]]
	a.openSessionOptionsFor(sess)
	options, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	compact := findModalAction(options, "Compact")
	if compact == nil {
		t.Fatalf("options missing Compact action")
	}

	compact()
	if _, ok := a.overlay.(*CompactPanel); !ok {
		t.Fatalf("overlay after compact = %T, want *CompactPanel", a.overlay)
	}
	if len(a.overlayStack) != 1 {
		t.Fatalf("overlay stack = %d, want 1", len(a.overlayStack))
	}

	a.closeOverlay()
	if a.overlay != options {
		t.Fatalf("overlay after closing compact = %T, want original options", a.overlay)
	}
	if len(a.overlayStack) != 0 {
		t.Fatalf("overlay stack after close = %d, want 0", len(a.overlayStack))
	}
}

func TestUX_SidecarRemoteLaunchPinsCanonicalSession(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()

	a.cb.StartLiveSession = func(req LiveSessionStartRequest) (LiveSession, error) {
		if req.Basedir != "/tmp/remote" {
			t.Fatalf("basedir = %q, want /tmp/remote", req.Basedir)
		}
		if req.Incognito {
			t.Fatalf("incognito = true, want false")
		}
		return LiveSession{SessionName: "chat-remote", SessionID: "uuid-remote"}, nil
	}

	a.openSidecarLaunchTypeModal("/tmp/remote")
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	action := findModalAction(modal, "Tracked live session")
	if action == nil {
		t.Fatalf("missing tracked remote session action")
	}
	action()

	ev := scr.PollEvent()
	interrupt, ok := ev.(*tcell.EventInterrupt)
	if !ok {
		t.Fatalf("event = %T, want *tcell.EventInterrupt", ev)
	}
	a.handleEvent(interrupt)

	if a.sidecar == nil {
		t.Fatalf("sidecar not created")
	}
	if a.sidecar.SessionName != "chat-remote" {
		t.Fatalf("sidecar session name = %q", a.sidecar.SessionName)
	}
	if a.sidecar.SessionID != "uuid-remote" {
		t.Fatalf("sidecar session id = %q", a.sidecar.SessionID)
	}
	if a.sidecar.status == "" {
		t.Fatalf("sidecar status should show pending state")
	}
}

func TestUX_SessionOptionsIncludeExportWhenCallbackConfigured(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()
	a.cb.ExportSession = func(*session.Session, SessionExportRequest) ([]byte, error) {
		return []byte("demo"), nil
	}

	a.openSessionOptionsFor(a.sessions[a.visibleIdx[0]])
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	if findModalAction(modal, "Export transcript") == nil {
		t.Fatalf("session options missing Export transcript action")
	}
}

func TestUX_SessionOptionsIncludeClaudeContextWhenCallbackConfigured(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	a.cb.UpdateSessionContextWindow = func(*session.Session, string) error { return nil }

	a.openSessionOptionsFor(a.sessions[a.visibleIdx[0]])
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	entry := findModalEntry(modal, "Claude context")
	if entry == nil {
		t.Fatalf("session options missing Claude context action")
	}
	if entry.Disabled {
		t.Fatalf("Claude context entry disabled for Claude session with callback")
	}
}

func TestUX_SessionContextWindowOptionsPersistOnlySelectedValue(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	var gotSession string
	var gotContextWindow string
	a.cb.UpdateSessionContextWindow = func(sess *session.Session, contextWindow string) error {
		gotSession = sess.Name
		gotContextWindow = contextWindow
		return nil
	}

	sess := a.sessions[a.visibleIdx[0]]
	a.openSessionContextWindowOptions(sess, nil)
	modal, ok := a.overlay.(*OptionsModal)
	if !ok {
		t.Fatalf("overlay = %T, want *OptionsModal", a.overlay)
	}
	action := findModalAction(modal, "Force 200k")
	if action == nil {
		t.Fatalf("context options missing Force 200k action")
	}
	action()

	if gotSession != sess.Name {
		t.Fatalf("updated session = %q want %q", gotSession, sess.Name)
	}
	if gotContextWindow != "200k" {
		t.Fatalf("context window = %q want 200k", gotContextWindow)
	}
	if a.overlay != nil {
		t.Fatalf("overlay = %T, want nil", a.overlay)
	}
}

func TestUX_OpenExportOptionsDoesNotBlockOnExportStats(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	block := make(chan struct{})
	a.cb.ExportSession = func(*session.Session, SessionExportRequest) ([]byte, error) {
		return []byte("demo"), nil
	}
	a.cb.LoadExportStats = func(*session.Session) (SessionExportStats, error) {
		<-block
		return SessionExportStats{Compactions: 2, VisibleMessages: 12, VisibleTokensEstimate: 1200}, nil
	}

	sess := a.sessions[a.visibleIdx[0]]
	start := time.Now()
	a.openExportOptions(sess)
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("openExportOptions blocked on export stats: %s", elapsed)
	}
	panel, ok := a.overlay.(*ExportPanel)
	if !ok {
		t.Fatalf("overlay = %T, want *ExportPanel", a.overlay)
	}
	if panel.status != "loading export stats..." {
		t.Fatalf("panel status = %q want loading export stats...", panel.status)
	}
	close(block)
}

func TestUX_OpenExportOptionsUsesInteractivePanel(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	a.cb.ExportSession = func(*session.Session, SessionExportRequest) ([]byte, error) {
		return []byte("demo"), nil
	}
	a.cb.LoadExportStats = func(*session.Session) (SessionExportStats, error) {
		return SessionExportStats{Compactions: 2, VisibleMessages: 12, VisibleTokensEstimate: 1200}, nil
	}

	sess := a.sessions[a.visibleIdx[0]]
	a.openExportOptions(sess)
	panel, ok := a.overlay.(*ExportPanel)
	if !ok {
		t.Fatalf("overlay = %T, want *ExportPanel", a.overlay)
	}
	if panel.historyStart != 0 {
		t.Fatalf("historyStart before stats load = %d want 0", panel.historyStart)
	}
	if a.mode != StatusExport {
		t.Fatalf("mode = %v want StatusExport", a.mode)
	}
	if !strings.HasSuffix(panel.name, "-"+sess.Name+".md") {
		t.Fatalf("panel filename = %q, want dated session name", panel.name)
	}
	if panel.status != "loading export stats..." {
		t.Fatalf("panel status = %q want loading export stats...", panel.status)
	}
	ev := scr.PollEvent()
	interrupt, ok := ev.(*tcell.EventInterrupt)
	if !ok {
		t.Fatalf("event = %T, want *tcell.EventInterrupt", ev)
	}
	a.handleEvent(interrupt)
	panel, ok = a.overlay.(*ExportPanel)
	if !ok {
		t.Fatalf("overlay after stats load = %T, want *ExportPanel", a.overlay)
	}
	if panel.historyStart != 1 {
		t.Fatalf("historyStart after stats load = %d want latest compaction", panel.historyStart)
	}
	if panel.status != "adjust controls and preview export" {
		t.Fatalf("panel status after stats load = %q", panel.status)
	}
}

func TestUX_ExportPanelFolderUsesFilePicker(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	a.cb.ExportSession = func(*session.Session, SessionExportRequest) ([]byte, error) {
		return []byte("demo"), nil
	}
	sess := a.sessions[a.visibleIdx[0]]
	a.openExportOptions(sess)
	panel, ok := a.overlay.(*ExportPanel)
	if !ok {
		t.Fatalf("overlay = %T, want *ExportPanel", a.overlay)
	}
	panel.OnChooseFolder(panel)
	if _, ok := a.overlay.(*FilePickerOverlay); !ok {
		t.Fatalf("overlay = %T, want *FilePickerOverlay", a.overlay)
	}
}

func TestUX_ExportPanelActionCallsCallbackWithKnobs(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	called := make(chan SessionExportRequest, 1)
	a.cb.ExportSession = func(_ *session.Session, req SessionExportRequest) ([]byte, error) {
		called <- req
		return []byte("demo"), nil
	}
	sess := a.sessions[a.visibleIdx[0]]
	a.openExportOptions(sess)
	panel := a.overlay.(*ExportPanel)
	panel.copyToClipboard = false
	panel.saveToFile = false
	panel.includeSystemPrompts = true
	panel.actionIdx = 1
	panel.triggerAction()

	select {
	case req := <-called:
		if !req.IncludeSystemPrompts || req.CopyToClipboard || req.SaveToFile {
			t.Fatalf("request did not preserve export knobs: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for export callback")
	}
}

func TestUX_ExportCompletionStacksOverPanelAndShowsPath(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	a.cb.ExportSession = func(_ *session.Session, req SessionExportRequest) ([]byte, error) {
		return []byte("demo"), nil
	}
	sess := a.sessions[a.visibleIdx[0]]
	a.openExportOptions(sess)
	panel, ok := a.overlay.(*ExportPanel)
	if !ok {
		t.Fatalf("overlay = %T, want *ExportPanel", a.overlay)
	}
	req := panel.buildRequest()
	req.CopyToClipboard = false
	req.SaveToFile = true
	req.Directory = t.TempDir()
	req.Filename = "demo.md"

	a.exportSessionWithRequest(sess, req, panel)
	ev := scr.PollEvent()
	interrupt, ok := ev.(*tcell.EventInterrupt)
	if !ok {
		t.Fatalf("event = %T, want *tcell.EventInterrupt", ev)
	}
	a.handleEvent(interrupt)

	modal, ok := a.overlay.(*Modal)
	if !ok {
		t.Fatalf("overlay after export = %T, want *Modal", a.overlay)
	}
	wantPath := filepath.Join(req.Directory, req.Filename)
	if !strings.Contains(modal.Body, wantPath) {
		t.Fatalf("completion body = %q, want path %q", modal.Body, wantPath)
	}
	if len(a.overlayStack) != 1 || a.overlayStack[0].widget != panel {
		t.Fatalf("completion should stack over export panel, stack=%#v", a.overlayStack)
	}
	a.closeOverlay()
	if a.overlay != panel {
		t.Fatalf("closing completion should restore panel, got %T", a.overlay)
	}
}

func TestUX_RegistrySessionUpdateAppliesWithoutSnapshotReload(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()

	listCalls := 0
	a.cb.ListSessions = func() (SessionSnapshot, error) {
		listCalls++
		return SessionSnapshot{}, nil
	}

	updated := &session.Session{
		Name: "test-session-00",
		Metadata: session.Metadata{
			Name:          "test-session-00",
			SessionID:     "00000000-0000-0000-0000-000000000000",
			WorkspaceRoot: "/Users/test/Sites/updated",
			Context:       "fresh summary",
			Created:       time.Now().Add(-time.Hour),
			LastAccessed:  time.Now(),
		},
	}
	a.applySessionEvent(SessionEvent{
		Kind:         "SESSION_UPDATED",
		SessionName:  updated.Name,
		SessionID:    updated.Metadata.ProviderSessionID(),
		Session:      updated,
		Model:        "sonnet",
		MessageCount: 42,
	})

	if listCalls != 0 {
		t.Fatalf("ListSessions calls = %d, want 0", listCalls)
	}
	got := a.findSessionByName(updated.Name)
	if got == nil || got.Metadata.WorkspaceRoot != updated.Metadata.WorkspaceRoot {
		t.Fatalf("updated session not applied, got %#v", got)
	}
	if a.modelCache[updated.Name] != "sonnet" {
		t.Fatalf("model cache = %q, want sonnet", a.modelCache[updated.Name])
	}
	if a.messageCountCache[updated.Name] != 42 {
		t.Fatalf("message count cache = %d, want 42", a.messageCountCache[updated.Name])
	}
}

func TestUX_RegistrySessionUpdateCollapsesDuplicateSessionIDs(t *testing.T) {
	a := NewApp([]*session.Session{
		{Name: "clyde-dev-1a4837fd", Metadata: session.Metadata{Name: "clyde-dev-1a4837fd", SessionID: "shared"}},
		{Name: "unified-session-resolution", Metadata: session.Metadata{Name: "unified-session-resolution", SessionID: "shared"}},
	}, AppCallbacks{})

	a.applySessionEvent(SessionEvent{
		Kind: "SESSION_UPDATED",
		Session: &session.Session{
			Name: "unified-session-resolution",
			Metadata: session.Metadata{
				Name:      "unified-session-resolution",
				SessionID: "shared",
			},
		},
	})

	if len(a.sessions) != 1 {
		t.Fatalf("sessions=%d want 1", len(a.sessions))
	}
	if a.sessions[0].Name != "unified-session-resolution" {
		t.Fatalf("name=%q want unified-session-resolution", a.sessions[0].Name)
	}
}

func TestUX_RegistryRenamePreservesSelection(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()

	a.table.SelectedRow = 0
	a.table.Active = true
	a.openDetails(a.sessions[a.visibleIdx[0]])

	renamed := &session.Session{
		Name: "renamed-session",
		Metadata: session.Metadata{
			Name:          "renamed-session",
			SessionID:     "00000000-0000-0000-0000-000000000000",
			WorkspaceRoot: "/Users/test/Sites/ws-0",
			Created:       time.Now().Add(-time.Hour),
			LastAccessed:  time.Now(),
		},
	}
	a.applySessionEvent(SessionEvent{
		Kind:         "SESSION_RENAMED",
		OldName:      "test-session-00",
		SessionName:  renamed.Name,
		SessionID:    renamed.Metadata.ProviderSessionID(),
		Session:      renamed,
		Model:        "opus",
		MessageCount: 7,
	})

	if a.selected == nil || a.selected.Name != renamed.Name {
		t.Fatalf("selected session = %#v, want %q", a.selected, renamed.Name)
	}
	if row := a.findVisibleRowByName(renamed.Name); row < 0 {
		t.Fatalf("renamed session not visible")
	}
	if _, ok := a.modelCache["test-session-00"]; ok {
		t.Fatalf("old model cache entry still present")
	}
}

func TestUX_StartupLoadingStateHidesEmptyTableAndOfflineBadge(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 0)
	defer cleanup()

	a.startupLoading = true
	a.sessionsLoading = true
	a.daemonOnline = false
	a.spinnerFrame = 1

	a.draw()
	got := screenText(scr)

	if !strings.Contains(got, "Loading sessions") {
		t.Fatalf("screen missing startup loading copy:\n%s", got)
	}
	if strings.Contains(got, "NAME") {
		t.Fatalf("screen showed empty table header during startup:\n%s", got)
	}
	if strings.Contains(got, "DAEMON OFFLINE") {
		t.Fatalf("screen showed daemon offline during startup:\n%s", got)
	}
	if !strings.Contains(got, "connecting") {
		t.Fatalf("screen missing connecting badge:\n%s", got)
	}
	if strings.Contains(got, "0 sessions") {
		t.Fatalf("screen showed session count before initial load finished:\n%s", got)
	}
}

func TestUX_EmptySessionsStateHidesTableHeaders(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 0)
	defer cleanup()

	a.startupLoading = false
	a.sessionsLoading = false

	a.draw()
	got := screenText(scr)

	if !strings.Contains(got, "No sessions yet") {
		t.Fatalf("screen missing empty state copy:\n%s", got)
	}
	if strings.Contains(got, "NAME") {
		t.Fatalf("screen showed table header for empty state:\n%s", got)
	}
	if !strings.Contains(got, "0 sessions") {
		t.Fatalf("screen missing session count after load completed:\n%s", got)
	}
}

func TestUX_TableShowsVisibleMessageCounts(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 2)
	defer cleanup()

	a.messageCountCache["test-session-00"] = 1027
	a.messageCountCache["test-session-01"] = 44
	a.populateTable()

	row0 := a.table.Rows[0]
	row1 := a.table.Rows[1]
	if got := row0[4].Text; got != "1,027" {
		t.Fatalf("row0 msgs = %q, want %q", got, "1,027")
	}
	if got := row1[4].Text; got != "44" {
		t.Fatalf("row1 msgs = %q, want %q", got, "44")
	}
}

func screenText(scr tcell.SimulationScreen) string {
	scr.Show()
	cells, cw, ch := scr.GetContents()
	var b strings.Builder
	for y := range ch {
		row := make([]rune, 0, cw)
		for x := range cw {
			c := cells[y*cw+x]
			if len(c.Runes) == 0 || c.Runes[0] == 0 {
				row = append(row, ' ')
				continue
			}
			row = append(row, c.Runes[0])
		}
		b.WriteString(strings.TrimRight(string(row), " "))
		b.WriteByte('\n')
	}
	return b.String()
}

// TestUX_FirstDownArmsFirstRow verifies first-launch keyboard behavior:
// the first Down key press should arm/highlight row 0, and only the
// second Down key press should move to row 1.
func TestUX_FirstDownArmsFirstRow(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 5)
	defer cleanup()

	// Initial state assertion: dashboard opens on row 0.
	if a.table.SelectedRow != 0 {
		t.Fatalf("initial SelectedRow = %d, want 0  --  first-launch should not skip", a.table.SelectedRow)
	}
	// Active gets set by any navigation. Before first Down it may be false;
	// the highlight should not show until the user interacts.

	// ONE Down. First movement key should arm/highlight row 0.
	a.table.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if a.table.SelectedRow != 0 {
		t.Errorf("after 1×Down, SelectedRow = %d, want 0 (first row armed)", a.table.SelectedRow)
	}
	// Second Down  --  row 1.
	a.table.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if a.table.SelectedRow != 1 {
		t.Errorf("after 2×Down, SelectedRow = %d, want 1", a.table.SelectedRow)
	}
}

// TestUX_FirstEnterOpensOptions counts how many Enter presses it
// takes to open the options popup from the fresh-launch dashboard.
// Expected: exactly one.
func TestUX_FirstEnterOpensOptions(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()
	a.table.SelectedRow = 0
	a.table.Active = true

	// Simulate Enter via the table's OnActivate, which is what
	// handleEvent invokes when Enter hits the table widget.
	enters := 0
	for a.overlay == nil && enters < 3 {
		enters++
		a.table.OnActivate(a.table.SelectedRow)
	}
	if enters != 1 {
		t.Errorf("Enter-presses to open options popup = %d, want 1", enters)
	}
	if _, ok := a.overlay.(*OptionsModal); !ok {
		t.Errorf("overlay after Enter = %T, want *OptionsModal", a.overlay)
	}
}

func TestUX_ExecutableChangedDetectsReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clyde")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old executable: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat old executable: %v", err)
	}
	base := executableSnapshot{path: path, info: info}

	changed, _, err := executableChanged(base)
	if err != nil {
		t.Fatalf("check unchanged executable: %v", err)
	}
	if changed {
		t.Fatalf("unchanged executable reported changed")
	}

	replacement := filepath.Join(dir, "clyde.new")
	if err := os.WriteFile(replacement, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write replacement executable: %v", err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatalf("replace executable: %v", err)
	}

	changed, reason, err := executableChanged(base)
	if err != nil {
		t.Fatalf("check replaced executable: %v", err)
	}
	if !changed {
		t.Fatalf("replaced executable was not detected")
	}
	if reason == "" {
		t.Fatalf("expected replacement reason")
	}
}

// TestUX_PostSessionPromptCanCancelToList mirrors the user's
// concrete flow: resume a session, claude exits, the post-session
// prompt shows, user cancels back to the session list, expects the
// dashboard to be responsive and the prompt to be gone.
//
// The bug the user just hit this morning in post-session pane:
// unclear, but the most likely regressions are:
//   - Prompt doesn't appear after exit
//   - Cancel leaves the overlay stuck
//   - After cancel, keys don't route to the table
//
// This test covers all three.
func TestUX_PostSessionPromptCanCancelToList(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 4)
	defer cleanup()
	a.suspendImpl = func(fn func()) { fn() }
	resumeCalls := 0
	a.cb.ResumeSession = func(*session.Session) error {
		resumeCalls++
		return nil
	}

	// Trigger resume on row 0.
	a.table.SelectedRow = 0
	a.table.Active = true
	a.resumeRow(0)

	if resumeCalls != 1 {
		t.Fatalf("ResumeSession calls = %d, want 1", resumeCalls)
	}
	modal, ok := a.overlay.(*OptionsModal)
	if !ok || modal.Context != OptionsModalContextReturn {
		t.Fatalf("overlay = %T, want return-context *OptionsModal (post-session pane missing)", a.overlay)
	}

	// User cancels back to the session list.
	if modal.OnCancel == nil {
		t.Fatalf("return modal missing cancel handler")
	}
	modal.OnCancel()
	if a.overlay != nil {
		t.Errorf("after cancel, overlay = %T, want nil (prompt should dismiss)", a.overlay)
	}

	// Now the dashboard should be responsive again. Table should
	// accept a Down event and move the selection.
	before := a.table.SelectedRow
	a.table.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if a.table.SelectedRow == before {
		t.Errorf("after Dismissing prompt, Down did not move selection  --  table frozen")
	}
}

// TestUX_ReturnPromptResumeAgainClosesLoop drives the full loop
// the user described: resume → exit → prompt → resume → exit →
// prompt → … N times. Each iteration should land the prompt open
// with the four entries visible. If any iteration comes back with
// a nil overlay or wrong type, the test fails with the cycle index
// so the regression is easy to pinpoint.
func TestUX_ReturnPromptResumeAgainClosesLoop(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()
	a.suspendImpl = func(fn func()) { fn() }
	a.cb.ResumeSession = func(*session.Session) error { return nil }

	a.table.SelectedRow = 0
	a.table.Active = true

	for i := 1; i <= 5; i++ {
		a.resumeRow(0)
		modal, ok := a.overlay.(*OptionsModal)
		if !ok || modal.Context != OptionsModalContextReturn {
			t.Fatalf("cycle %d: overlay = %T, want return-context *OptionsModal", i, a.overlay)
		}
		// Cancel to reset the loop so the next resumeRow fires
		// from a clean slate (OnResume would recurse through another
		// resumeRow internally; testing that path is
		// TestHappyPath_ResumeCycleMultipleTimes).
		if modal.OnCancel == nil {
			t.Fatalf("cycle %d: return modal missing cancel handler", i)
		}
		modal.OnCancel()
		if a.overlay != nil {
			t.Errorf("cycle %d: cancel did not clear overlay", i)
		}
	}
}

func TestUX_DaemonRestartKeepsDashboardResponsive(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 4)
	defer cleanup()

	a.setDaemonOnline("test.startup")
	if !a.isDaemonOnline() {
		t.Fatalf("daemon should start online for test")
	}
	a.setDaemonOffline("stream_closed", fmt.Errorf("registry stream closed"))
	if a.isDaemonOnline() {
		t.Fatalf("daemon should report offline after stream close")
	}
	a.setDaemonOnline("subscribe_ok")
	if !a.isDaemonOnline() {
		t.Fatalf("daemon should report online after restart")
	}

	before := a.table.SelectedRow
	a.table.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	a.table.HandleEvent(tcell.NewEventKey(tcell.KeyDown, 0, tcell.ModNone))
	if len(a.visibleIdx) > 1 && a.table.SelectedRow == before {
		t.Fatalf("dashboard did not respond after daemon restart simulation")
	}
}

func TestUX_FocusTransitionsKeepReturnPromptDuringResume(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()
	a.cb.ResumeSession = func(*session.Session) error { return nil }
	a.suspendImpl = func(fn func()) {
		a.handleEvent(&tcell.EventFocus{Focused: false})
		fn()
		a.handleEvent(&tcell.EventFocus{Focused: true})
	}

	a.table.SelectedRow = 0
	a.table.Active = true
	a.resumeRow(0)

	modal, ok := a.overlay.(*OptionsModal)
	if !ok || modal.Context != OptionsModalContextReturn {
		t.Fatalf("overlay = %T, want return-context *OptionsModal", a.overlay)
	}
	if !a.appFocused {
		t.Fatalf("app should return to focused state after focus bounce")
	}
}

func TestUX_ReturnPromptDefersBehindCompactOverlay(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()

	sess := a.sessions[a.visibleIdx[0]]
	a.selected = sess
	a.openRichCompactForm(sess)
	if _, ok := a.overlay.(*CompactPanel); !ok {
		t.Fatalf("overlay = %T, want compact panel before focus event", a.overlay)
	}

	a.returnPathSession = sess
	a.trackReturnPathState(returnPathStateReturnPromptPending, "test.pending", sess)
	a.handleEvent(&tcell.EventFocus{Focused: true})
	if _, ok := a.overlay.(*CompactPanel); !ok {
		t.Fatalf("overlay = %T, want compact panel to survive return prompt reopen", a.overlay)
	}
	if a.returnPathState != returnPathStateReturnPromptPending {
		t.Fatalf("return path state = %s, want pending while compact is open", a.returnPathState)
	}

	a.closeOverlay()
	modal, ok := a.overlay.(*OptionsModal)
	if !ok || modal.Context != OptionsModalContextReturn {
		t.Fatalf("overlay = %T, want deferred return prompt after compact closes", a.overlay)
	}
}

func TestUX_CompactBusyCountsAsPendingVisualActivity(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()

	sess := a.sessions[a.visibleIdx[0]]
	a.selected = sess
	a.openRichCompactForm(sess)
	panel, ok := a.overlay.(*CompactPanel)
	if !ok {
		t.Fatalf("overlay = %T, want compact panel", a.overlay)
	}
	if a.hasPendingVisualActivity() {
		t.Fatalf("idle compact panel should not force spinner redraws")
	}

	panel.SetBusy("apply", true)
	if !a.hasPendingVisualActivity() {
		t.Fatalf("busy compact panel should keep spinner redraws active")
	}
}

// TestUX_SearchSlashWorksOnFreshLaunch catches the bug the PTY
// suite just uncovered: pressing `/` on a freshly-launched dashboard
// silently did nothing because openSearchForm bailed when
// a.selected was nil. Now rowSession() is the source of truth.
func TestUX_SearchSlashWorksOnFreshLaunch(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()
	a.table.SelectedRow = 0
	a.table.Active = true
	// a.selected is deliberately NIL  --  this is the post-launch
	// state before the user has pressed Space.
	if a.selected != nil {
		t.Fatalf("test precondition: a.selected should be nil")
	}
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, '/', tcell.ModNone))
	if a.overlay == nil {
		t.Errorf("after `/` on fresh launch, overlay is nil  --  silent no-op regression")
	}
}

// TestUX_EscapeOnBareDashboardDoesNotQuit confirms Esc on the
// dashboard (no overlay, no selection) is a safe no-op, not a quit.
// Earlier the user hit unexpected exits from stray Esc presses in
// various resumed states.
func TestUX_EscapeOnBareDashboardDoesNotQuit(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()
	a.running = true

	a.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if !a.running {
		t.Errorf("Esc on bare dashboard flipped a.running to false  --  unexpected quit")
	}
	if a.overlay != nil {
		t.Errorf("Esc on bare dashboard created an overlay: %T", a.overlay)
	}
}

// TestUX_QOnRowDeselectsThenQuits confirms the two-stage q behavior:
// first q with a session selected (details open) deselects, second
// q with nothing selected quits. The current production code checks
// a.selected != nil  --  so Space-then-q deselects, another q quits.
func TestUX_QOnRowDeselectsThenQuits(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()
	a.running = true
	// Open details on row 0 so a.selected is set.
	a.table.SelectedRow = 0
	a.openDetails(a.sessions[a.visibleIdx[0]])
	if a.selected == nil {
		t.Fatal("openDetails did not set a.selected")
	}

	// First q: should deselect (no quit).
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone))
	if !a.running {
		t.Errorf("first q with details open quit unexpectedly")
	}
	if a.selected != nil {
		t.Errorf("first q did not deselect (a.selected=%v)", a.selected)
	}

	// Second q: should flip running to false (quit signal).
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone))
	if a.running {
		t.Errorf("second q with nothing selected did not quit")
	}
}

// TestUX_PostSessionPaneEnterAcceptsBothCRandLF is the regression
// test for the bug the user reported as "I have to tap Enter two
// or three times before quit registers."
//
// Root cause: the ReturnPrompt (and every other Enter handler in the
// UI) was checking only tcell.KeyEnter (CR / 0x0D). After the screen
// teardown / reinit cycle inside suspendAndRun some terminals emit
// LF (0x0A) for Enter, which decodes as tcell.KeyLF  --  a separate
// key tcell does not collapse into KeyEnter. The first one or two
// keypresses landed as KeyLF, the handler ignored them, and the
// next CR-emitting press finally triggered the activation. From the
// user's seat that looks like an unresponsive prompt.
//
// This test drives a synthetic KeyLF and asserts the prompt's
// activate path fires exactly once.
func TestUX_PostSessionPaneEnterAcceptsBothCRandLF(t *testing.T) {
	for _, key := range []tcell.Key{tcell.KeyEnter, tcell.KeyLF} {
		t.Run(keyName(key), func(t *testing.T) {
			a, _, cleanup := mkAppWithSessions(t, 3)
			defer cleanup()
			a.suspendImpl = func(fn func()) { fn() }
			a.cb.ResumeSession = func(*session.Session) error { return nil }

			a.table.SelectedRow = 0
			a.table.Active = true
			a.resumeRow(0)

			modal, ok := a.overlay.(*OptionsModal)
			if !ok || modal.Context != OptionsModalContextReturn {
				t.Fatalf("post-session pane missing: overlay = %T", a.overlay)
			}
			// Default highlight is Quit. One Enter (or LF) must trigger quit.
			a.running = true
			handled := modal.HandleEvent(tcell.NewEventKey(key, 0, tcell.ModNone))
			if !handled {
				t.Errorf("HandleEvent returned false for key %v  --  prompt did not consume the press", key)
			}
			if a.running {
				t.Errorf("a.running still true after quit activation")
			}
		})
	}
}

// keyName renders a tcell.Key for sub-test naming.
func keyName(k tcell.Key) string {
	switch k {
	case tcell.KeyEnter:
		return "KeyEnter-CR-0x0D"
	case tcell.KeyLF:
		return "KeyLF-0x0A"
	default:
		return "Key-other"
	}
}

func findModalAction(modal *OptionsModal, label string) func() {
	for _, entry := range modal.TopEntries {
		if entry.Label == label {
			return entry.Action
		}
	}
	for _, entry := range modal.Entries {
		if entry.Label == label {
			return entry.Action
		}
	}
	return nil
}

func findModalEntry(modal *OptionsModal, label string) *OptionsModalEntry {
	for i := range modal.TopEntries {
		if modal.TopEntries[i].Label == label {
			return &modal.TopEntries[i]
		}
	}
	for i := range modal.Entries {
		if modal.Entries[i].Label == label {
			return &modal.Entries[i]
		}
	}
	return nil
}

// TestUX_OpenDetailsSpaceRequiresOneTap counts Space presses to open
// the details pane from the freshly-launched dashboard.
func TestUX_OpenDetailsSpaceRequiresOneTap(t *testing.T) {
	a, _, cleanup := mkAppWithSessions(t, 3)
	defer cleanup()
	a.table.SelectedRow = 0
	a.table.Active = true

	if a.selected != nil {
		t.Fatalf("a.selected should be nil pre-Space")
	}
	a.handleKey(tcell.NewEventKey(tcell.KeyRune, ' ', tcell.ModNone))
	if a.selected == nil {
		t.Errorf("one Space did not open details pane")
	}
	// Counting proof: exactly one tap was enough.
}

func TestSelfReloadDefersWhileOverlayOpen(t *testing.T) {
	stubSelfReloadProbe(t, nil)
	path := filepath.Join(t.TempDir(), "clyde")
	if err := os.WriteFile(path, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	panel := NewCompactPanel("demo")
	a := &App{
		overlay: panel,
		running: true,
	}

	a.handleEvent(tcell.NewEventInterrupt(selfReloadAvailable{path: path, reason: "mtime_changed"}))
	if !a.running {
		t.Fatalf("self reload should not stop the app while overlay is open")
	}
	if a.reloadExecPath != "" {
		t.Fatalf("reloadExecPath set before overlay close: %q", a.reloadExecPath)
	}
	if a.pendingReloadPath == "" {
		t.Fatalf("expected pending reload to be recorded")
	}
	if !strings.Contains(panel.status, "update available") {
		t.Fatalf("expected compact panel status to mention deferred reload, got %q", panel.status)
	}

	a.closeOverlay()
	if a.running {
		t.Fatalf("expected deferred reload to stop app after overlay close")
	}
	if a.reloadExecPath != path {
		t.Fatalf("reloadExecPath=%q want %s", a.reloadExecPath, path)
	}
}

func TestDaemonBinaryUpdateTriggersSelfReload(t *testing.T) {
	stubSelfReloadProbe(t, nil)
	a := &App{running: true}
	path := filepath.Join(t.TempDir(), "new-clyde")
	if err := os.WriteFile(path, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	a.applySessionEvent(SessionEvent{
		Kind:         "CLYDE_BINARY_UPDATED",
		BinaryPath:   path,
		BinaryReason: "file_replaced",
		BinaryHash:   "def456",
	})

	if a.running {
		t.Fatalf("daemon binary update should stop the app for exec reload")
	}
	if a.reloadExecPath != path {
		t.Fatalf("reloadExecPath=%q want %s", a.reloadExecPath, path)
	}
}

func TestDaemonBinaryUpdateIgnoresAlreadyRunningHash(t *testing.T) {
	a := &App{running: true, executableHash: "abc123"}

	a.applySessionEvent(SessionEvent{
		Kind:         "CLYDE_BINARY_UPDATED",
		BinaryPath:   "/tmp/new-clyde",
		BinaryReason: "file_replaced",
		BinaryHash:   "abc123",
	})

	if !a.running {
		t.Fatalf("matching daemon binary update should not stop the app")
	}
	if a.reloadExecPath != "" {
		t.Fatalf("reloadExecPath=%q want empty", a.reloadExecPath)
	}
}

func TestDaemonBinaryUpdateRejectsInvalidSelfReloadBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-clyde")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write invalid binary: %v", err)
	}
	a := &App{running: true}

	a.applySessionEvent(SessionEvent{
		Kind:         "CLYDE_BINARY_UPDATED",
		BinaryPath:   path,
		BinaryReason: "file_replaced",
		BinaryHash:   "def456",
	})

	if !a.running {
		t.Fatalf("invalid binary update should not stop the app")
	}
	if a.reloadExecPath != "" {
		t.Fatalf("reloadExecPath=%q want empty", a.reloadExecPath)
	}
}

func TestDaemonBinaryUpdateRejectsProbeFailingSelfReloadBinary(t *testing.T) {
	stubSelfReloadProbe(t, fmt.Errorf("probe failed"))
	path := filepath.Join(t.TempDir(), "new-clyde")
	if err := os.WriteFile(path, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	a := &App{running: true}

	a.applySessionEvent(SessionEvent{
		Kind:         "CLYDE_BINARY_UPDATED",
		BinaryPath:   path,
		BinaryReason: "file_replaced",
		BinaryHash:   "def456",
	})

	if !a.running {
		t.Fatalf("probe-failing binary update should not stop the app")
	}
	if a.reloadExecPath != "" {
		t.Fatalf("reloadExecPath=%q want empty", a.reloadExecPath)
	}
}

func TestSelfReloadExecsBeforeSuspendReinit(t *testing.T) {
	stubSelfReloadProbe(t, nil)
	path := filepath.Join(t.TempDir(), "clyde")
	if err := os.WriteFile(path, []byte("old"), 0o755); err != nil {
		t.Fatalf("write old binary: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat old binary: %v", err)
	}
	a := &App{executableBase: executableSnapshot{path: path, info: info}}

	if err := os.WriteFile(path, []byte("new-binary"), 0o755); err != nil {
		t.Fatalf("write new binary: %v", err)
	}

	oldExec := execCurrentProcess
	defer func() { execCurrentProcess = oldExec }()
	var execPath string
	execCurrentProcess = func(path string) error {
		execPath = path
		return nil
	}

	if !a.execNewBinaryBeforeReinit("test") {
		t.Fatalf("execNewBinaryBeforeReinit = false, want true")
	}
	if execPath != path {
		t.Fatalf("exec path = %q, want %q", execPath, path)
	}
}

func TestSelfReloadExportsReturnPromptForExec(t *testing.T) {
	t.Setenv(EnvTUIReturnSessionID, "")
	t.Setenv(EnvTUIReturnSessionName, "")
	sess := session.NewSession("chat-one", "session-uuid")
	a := &App{returnPathSession: sess}

	a.exportReturnPromptForExec("suspend_return")

	if got := os.Getenv(EnvTUIReturnSessionID); got != "session-uuid" {
		t.Fatalf("%s=%q want session-uuid", EnvTUIReturnSessionID, got)
	}
	if got := os.Getenv(EnvTUIReturnSessionName); got != "chat-one" {
		t.Fatalf("%s=%q want chat-one", EnvTUIReturnSessionName, got)
	}
}

func TestRuntimeStampIncludesExecutableHash(t *testing.T) {
	a, scr, cleanup := mkAppWithSessions(t, 1)
	defer cleanup()
	a.buildHash = "build1"
	a.executableHash = "exec22"
	a.runHash = "run333"
	a.lastHeartbeatAt = time.Now()

	a.draw()
	scr.Show()

	text := compactPanelScreenText(scr)
	if !strings.Contains(text, "b:build1 x:exec22 r:run333") {
		t.Fatalf("runtime stamp missing executable hash:\n%s", text)
	}
}
