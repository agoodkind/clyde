// Package cmd holds the TUI dashboard, its daemon-backed callbacks,
// the `clyde resume` cobra verb, and the argument-routing helpers
// (ClassifyArgs, ForwardToClaude) used by cmd/clyde/main.go to assemble
// the cobra root.
//
// What lives here:
//
//   - RunDashboard / runPostSessionDashboard (the tcell TUI entrypoint)
//   - TUI callbacks for delete, rename, resume, live sessions,
//     registry, summary, view, model extract
//   - NewResumeCmd (the `clyde resume <name|uuid>` verb)
//   - ClassifyArgs and ForwardToClaude (passthrough routing)
//   - resumeSession / deleteSession helpers shared by the TUI and the
//     resume verb
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/daemonclient"
	claudeprovider "goodkind.io/clyde/internal/providers/claude"
	"goodkind.io/clyde/internal/providers/mitmcontrib"
	"goodkind.io/clyde/internal/providers/registry"
	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/ui"
	"goodkind.io/clyde/internal/util"
)

// RunDashboard is the entrypoint for `clyde` with no subcommand. It
// boots the tcell TUI dashboard for managing existing sessions
// (resume, delete, rename, view, live sessions). New sessions from the TUI launch `claude`
// with a stable session id and exact display name; the SessionStart hook adopts the row.
func RunDashboard(cmd *cobra.Command, args []string) int {
	// Non-interactive (piped) invocation: forward to real claude.
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		return ForwardToClaude(os.Args[1:])
	}

	// Non-TTY stdout: show help. Avoids drawing the TUI into a pipe.
	if !isatty.IsTerminal(os.Stdout.Fd()) {
		_ = cmd.Help()
		return 0
	}

	return runDashboardTUI()
}

// runDashboardTUI opens the session dashboard. Caller must ensure stdin and
// stdout are TTYs (see RunDashboard).
func runDashboardTUI() int {
	ctx := newCommandContext("dashboard")
	daemon.NudgeDiscoveryScan()
	cmdUILog.Logger().InfoContext(ctx, "dashboard.opened", "component", "tui")

	dashboardCwd, _ := os.Getwd()
	cb := buildAppCallbacks(ctx, dashboardCwd)
	app := ui.NewApp(nil, cb, dashboardAppOptions(ctx, dashboardCwd, "", consumeTUIReturnSession()))

	if err := app.Run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		cmdUILog.Logger().ErrorContext(ctx, "dashboard.tui_error",
			"component", "tui",
			"err", err,
		)
		return 1
	}
	cmdUILog.Logger().InfoContext(ctx, "dashboard.closed", "component", "tui")
	return 0
}

// RunBasedirLaunch opens the dashboard biased toward one workspace root.
// The caller is responsible for only invoking this for an existing directory.
func RunBasedirLaunch(basedir string) int {
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		return ForwardToClaude(os.Args[1:])
	}
	ctx := newCommandContext("dashboard.basedir")
	daemon.NudgeDiscoveryScan()
	canonical := session.CanonicalWorkspaceRoot(basedir)
	cmdUILog.Logger().InfoContext(ctx, "dashboard.basedir.opened",
		"component", "tui",
		"basedir", canonical,
	)

	dashboardCwd, _ := os.Getwd()
	cb := buildAppCallbacks(ctx, dashboardCwd)
	app := ui.NewApp(nil, cb, dashboardAppOptions(ctx, canonical, canonical, consumeTUIReturnSession()))

	if err := app.Run(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)

		cmdUILog.Logger().ErrorContext(ctx, "dashboard.basedir.tui_error",
			"component", "tui",
			"basedir", canonical,
			"err", err,
		)
		return 1
	}
	cmdUILog.Logger().InfoContext(ctx, "dashboard.basedir.closed",
		"component", "tui",
		"basedir", canonical,
	)
	return 0
}

func dashboardAppOptions(ctx context.Context, launchCWD, launchBasedir string, returnTo *session.Session) ui.AppOptions {
	return ui.AppOptions{
		Context:            ctx,
		DashboardLaunchCWD: launchCWD,
		LaunchBasedir:      launchBasedir,
		ReturnTo:           returnTo,
	}
}

func consumeTUIReturnSession() *session.Session {
	sessionID := strings.TrimSpace(os.Getenv(ui.EnvTUIReturnSessionID))
	sessionName := strings.TrimSpace(os.Getenv(ui.EnvTUIReturnSessionName))
	_ = os.Unsetenv(ui.EnvTUIReturnSessionID)
	_ = os.Unsetenv(ui.EnvTUIReturnSessionName)
	query := sessionID
	if query == "" {
		query = sessionName
	}
	if query == "" {
		return nil
	}
	store, err := session.NewGlobalFileStoreReadOnly()
	if err != nil {
		cmdLog.Warn("dashboard.return_session.store_failed",
			"component", "tui",
			"session", sessionName,
			"session_id", sessionID,
			"err", err)
		return nil
	}
	sess, err := store.Resolve(query)
	if err != nil || sess == nil {
		cmdLog.Warn("dashboard.return_session.resolve_failed",
			"component", "tui",
			"session", sessionName,
			"session_id", sessionID,
			"err", err)
		return nil
	}
	cmdUILog.Logger().Info("dashboard.return_session.restored",
		"component", "tui",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID())
	return sess
}

// buildAppCallbacks wires store + helpers into a ui.AppCallbacks.
// dashboardLaunchCWD is the process cwd when RunDashboard started; it
// is the default basedir for "new session" without picking a folder.
func buildAppCallbacks(parentCtx context.Context, dashboardLaunchCWD string) ui.AppCallbacks {
	builder := appCallbackBuilder{
		parentCtx:          parentCtx,
		dashboardLaunchCWD: dashboardLaunchCWD,
	}
	return ui.AppCallbacks{
		ListSessions:               builder.listSessions,
		LoadStats:                  builder.loadStats,
		SubscribeProviderStats:     builder.subscribeProviderStats,
		RestartDaemon:              builder.restartDaemon,
		StartSessionWithBasedir:    builder.startSessionWithBasedir,
		StartLiveSession:           builder.startLiveSession,
		ResumeSession:              builder.resumeSession,
		DeleteSession:              builder.deleteSession,
		RenameSession:              builder.renameSession,
		SetBasedir:                 builder.setBasedir,
		RefreshSummary:             builder.refreshSummary,
		ViewContent:                builder.viewContent,
		ExportSession:              builder.exportSession,
		LoadExportStats:            builder.loadExportStats,
		SubscribeRegistry:          builder.subscribeRegistry,
		LoadConfigControls:         builder.loadConfigControls,
		UpdateConfigControl:        builder.updateConfigControl,
		UpdateSessionContextWindow: builder.updateSessionContextWindow,
		SendLiveSession:            builder.sendLiveSession,
		StreamLiveSession:          builder.streamLiveSession,
		CompactPreview:             builder.compactPreview,
		CompactApply:               builder.compactApply,
		CompactUndo:                builder.compactUndo,
		GetSessionDetail:           builder.getSessionDetail,
	}
}

type appCallbackBuilder struct {
	parentCtx          context.Context
	dashboardLaunchCWD string
}

func (builder appCallbackBuilder) childContext(operation string) context.Context {
	return childCommandContext(builder.parentCtx, operation)
}

func (builder appCallbackBuilder) openStore() (session.Store, error) {
	return session.NewGlobalFileStore()
}

func (builder appCallbackBuilder) listSessions() (ui.SessionSnapshot, error) {
	resp, err := daemon.ListSessionsViaDaemon(builder.childContext("dashboard.list_sessions"))
	if err != nil {
		return ui.SessionSnapshot{}, err
	}
	return sessionSnapshotFromProto(resp), nil
}

func (builder appCallbackBuilder) loadStats() (ui.DashboardStats, error) {
	return loadDashboardStats(builder.childContext("dashboard.load_stats"))
}

func (builder appCallbackBuilder) subscribeProviderStats() (<-chan ui.ProviderStats, func(), error) {
	ctx := builder.childContext("dashboard.provider_stats.subscribe")
	raw, cancel, err := daemon.SubscribeProviderStats(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan ui.ProviderStats, 8)
	go func() { //nolint:goroutine_without_recover
		forwardProviderStats(ctx, raw, out)
	}()
	return out, cancel, nil
}

func forwardProviderStats(ctx context.Context, raw <-chan *clydev1.ProviderStatsEvent, out chan<- ui.ProviderStats) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cmdUILog.Logger().ErrorContext(ctx, "dashboard.provider_stats.forwarder_panic",
				"component", "tui",
				"err", fmt.Errorf("panic: %v", recovered),
			)
		}
	}()
	defer close(out)
	for ev := range raw {
		if ev == nil || ev.GetStats() == nil {
			continue
		}
		stats := providerStatsFromProto([]*clydev1.ProviderStats{ev.GetStats()})
		if len(stats) == 0 {
			continue
		}
		out <- stats[0]
	}
}

func (builder appCallbackBuilder) restartDaemon() error {
	return daemon.RestartManagedDaemon(builder.childContext("dashboard.daemon.restart"))
}

func (builder appCallbackBuilder) startSessionWithBasedir(basedir string) error {
	store, err := builder.openStore()
	if err != nil {
		return err
	}
	return startNewSessionInDir(builder.childContext("dashboard.session.start"), basedir, store, builder.dashboardLaunchCWD, false)
}

func (builder appCallbackBuilder) startLiveSession(req ui.LiveSessionStartRequest) (ui.LiveSession, error) {
	ctx := builder.childContext("dashboard.live_session.start")
	resp, err := daemon.StartLiveSessionViaDaemon(ctx, &clydev1.StartLiveSessionRequest{
		Provider:  req.Provider,
		Name:      req.Name,
		Basedir:   req.Basedir,
		Model:     req.Model,
		Effort:    req.Effort,
		Incognito: req.Incognito,
	})
	if err != nil {
		return ui.LiveSession{}, err
	}
	live := resp.GetSession()
	return ui.LiveSession{
		Provider:       live.GetProvider(),
		SessionName:    live.GetSessionName(),
		SessionID:      live.GetSessionId(),
		Status:         live.GetStatus(),
		Basedir:        live.GetBasedir(),
		URL:            live.GetUrl(),
		SupportsSend:   live.GetSupportsSend(),
		SupportsStream: live.GetSupportsStream(),
		SupportsStop:   live.GetSupportsStop(),
	}, nil
}

func (builder appCallbackBuilder) resumeSession(sess *session.Session) error {
	store, err := builder.openStore()
	if err != nil {
		return err
	}
	return resumeSession(builder.childContext("dashboard.session.resume"), sess, store, true)
}

func (builder appCallbackBuilder) deleteSession(sess *session.Session) error {
	ctx := builder.childContext("dashboard.session.delete")
	outcome, err := daemon.DeleteSessionViaDaemonOutcome(ctx, sess.Name)
	if outcome != daemon.LifecycleOutcomeReady {
		return daemonLifecycleError(ctx, "delete", outcome, err)
	}
	return nil
}

func (builder appCallbackBuilder) renameSession(sess *session.Session) (string, error) {
	ctx := builder.childContext("dashboard.session.rename")
	if sess == nil {
		return "", fmt.Errorf("nil session")
	}
	title := strings.TrimSpace(sess.Metadata.Title)
	if err := session.ValidateDisplayName(title); err != nil {
		slog.WarnContext(ctx, "dashboard.session.rename_invalid_name",
			"component", "cli",
			"session", sess.Name,
			"title", title,
			"err", err,
		)
		return title, fmt.Errorf("validate display name: %w", err)
	}
	store, err := builder.openStore()
	if err != nil {
		slog.WarnContext(ctx, "dashboard.session.rename_store_failed",
			"component", "cli",
			"session", sess.Name,
			"err", err,
		)
		return title, fmt.Errorf("open session store: %w", err)
	}
	current, err := store.Resolve(sess.Name)
	if err != nil {
		slog.WarnContext(ctx, "dashboard.session.rename_resolve_failed",
			"component", "cli",
			"session", sess.Name,
			"err", err,
		)
		return title, fmt.Errorf("resolve session: %w", err)
	}
	if current == nil {
		return title, fmt.Errorf("session %q not found", sess.Name)
	}
	current.Metadata.Title = title
	if err := store.Update(current); err != nil {
		slog.WarnContext(ctx, "dashboard.session.rename_update_failed",
			"component", "cli",
			"session", sess.Name,
			"title", title,
			"err", err,
		)
		return title, fmt.Errorf("update session title: %w", err)
	}
	return title, nil
}

func (builder appCallbackBuilder) setBasedir(sess *session.Session, newPath string) error {
	if sess == nil || sess.Name == "" {
		return fmt.Errorf("nil session")
	}
	ctx := builder.childContext("dashboard.session.set_basedir")
	outcome, err := daemon.UpdateSessionWorkspaceRootViaDaemonOutcome(ctx, sess.Name, newPath)
	if outcome != daemon.LifecycleOutcomeReady {
		return daemonLifecycleError(ctx, "update_session_workspace_root", outcome, err)
	}
	return nil
}

func (builder appCallbackBuilder) refreshSummary(sess *session.Session, onDone func(*session.Session)) error {
	if sess == nil || sess.Name == "" {
		return fmt.Errorf("nil session")
	}
	ctx := builder.childContext("dashboard.summary.refresh")
	go func() { //nolint:goroutine_without_recover
		refreshSummaryWorker(ctx, sess.Name, sess.Metadata.WorkspaceRoot, onDone)
	}()
	return nil
}

func refreshSummaryWorker(ctx context.Context, name, workspaceRoot string, onDone func(*session.Session)) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cmdUILog.Logger().ErrorContext(ctx, "dashboard.refresh_summary.worker_panic",
				"component", "tui",
				"session", name,
				"err", fmt.Errorf("panic: %v", recovered),
			)
		}
	}()
	defer func() {
		if onDone != nil {
			onDone(nil)
		}
	}()
	resp, err := daemon.GetSessionDetailViaDaemon(ctx, name)
	if err != nil {
		return
	}
	messages := summaryMessagesFromProto(resp.GetAllMessages())
	if len(messages) == 0 {
		return
	}
	client, err := daemon.ConnectOrStart(ctx)
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()
	_ = client.UpdateContext(name, workspaceRoot, messages)
}

func summaryMessagesFromProto(all []*clydev1.DetailMessage) []string {
	if len(all) == 0 {
		return nil
	}
	start := 0
	if len(all) > 100 {
		start = len(all) - 100
	}
	messages := make([]string, 0, len(all)-start)
	for _, message := range all[start:] {
		role := strings.TrimSpace(message.GetRole())
		text := strings.TrimSpace(message.GetText())
		if role == "" || text == "" {
			continue
		}
		roleLabel := "User"
		if strings.EqualFold(role, "assistant") {
			roleLabel = "Assistant"
		}
		runes := []rune(text)
		if len(runes) > 300 {
			text = string(runes[:300])
		}
		messages = append(messages, fmt.Sprintf("[%s] %s", roleLabel, text))
	}
	return messages
}

func (builder appCallbackBuilder) viewContent(sess *session.Session) string {
	resp, err := daemon.GetSessionDetailViaDaemon(builder.childContext("dashboard.session.view_content"), sess.Name)
	if err != nil || len(resp.GetAllMessages()) == 0 {
		return ""
	}
	var content strings.Builder
	for _, message := range resp.GetAllMessages() {
		if message.GetRole() == "" || message.GetText() == "" {
			continue
		}
		content.WriteString(strings.ToUpper(message.GetRole()))
		content.WriteString(":\n")
		content.WriteString(message.GetText())
		content.WriteString("\n\n")
	}
	return strings.TrimSpace(content.String())
}

func (builder appCallbackBuilder) exportSession(sess *session.Session, req ui.SessionExportRequest) ([]byte, error) {
	rpcReq := exportRequestToProto(sess, req)
	resp, err := daemon.ExportSessionViaDaemon(builder.childContext("dashboard.session.export"), rpcReq)
	if err != nil {
		return nil, err
	}
	return resp.GetBody(), nil
}

func (builder appCallbackBuilder) loadExportStats(sess *session.Session) (ui.SessionExportStats, error) {
	if sess == nil {
		return ui.SessionExportStats{}, fmt.Errorf("nil session")
	}
	resp, err := daemon.GetSessionExportStatsViaDaemon(builder.childContext("dashboard.session.export_stats"), sess.Name)
	if err != nil {
		return ui.SessionExportStats{}, err
	}
	return exportStatsFromProto(resp), nil
}

func (builder appCallbackBuilder) subscribeRegistry() (<-chan ui.SessionEvent, func(), error) {
	ctx := builder.childContext("dashboard.registry.subscribe")
	raw, cancel, err := daemon.SubscribeRegistry(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan ui.SessionEvent, 8)
	go func() { //nolint:goroutine_without_recover
		forwardRegistryEvents(ctx, raw, out)
	}()
	return out, cancel, nil
}

func forwardRegistryEvents(ctx context.Context, raw <-chan *clydev1.SubscribeRegistryResponse, out chan<- ui.SessionEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cmdUILog.Logger().ErrorContext(ctx, "dashboard.registry.forwarder_panic",
				"component", "tui",
				"err", fmt.Errorf("panic: %v", recovered),
			)
		}
	}()
	defer close(out)
	for ev := range raw {
		out <- sessionEventFromProto(ev)
	}
}

func (builder appCallbackBuilder) loadConfigControls() ([]ui.ConfigControl, error) {
	raw, err := daemon.ListConfigControlsViaDaemon(builder.childContext("dashboard.config_controls.list"))
	if err != nil {
		return nil, err
	}
	return configControlsFromProto(raw), nil
}

func (builder appCallbackBuilder) updateConfigControl(key, value string) error {
	_, err := daemon.UpdateConfigControlViaDaemon(builder.childContext("dashboard.config_controls.update"), key, value)
	return err
}

func (builder appCallbackBuilder) updateSessionContextWindow(sess *session.Session, contextWindow string) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	if err := daemon.UpdateSessionContextWindowViaDaemon(
		builder.childContext("dashboard.session.context_window.update"),
		sess.Name,
		contextWindow,
	); err != nil {
		slog.WarnContext(
			builder.childContext("dashboard.session.context_window.update_failed"),
			"dashboard.session.context_window.update_failed",
			"component", "tui",
			"session", sess.Name,
			"err", err,
		)
		return fmt.Errorf("update session context window %q: %w", sess.Name, err)
	}
	return nil
}

func (builder appCallbackBuilder) sendLiveSession(sessionID, text string) error {
	ok, err := daemon.SendLiveSessionViaDaemon(builder.childContext("dashboard.live_session.send"), sessionID, text)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("session not listening on inject socket")
	}
	return nil
}

func (builder appCallbackBuilder) streamLiveSession(sessionID string) (<-chan ui.LiveSessionEvent, func(), error) {
	ctx := builder.childContext("dashboard.live_session.stream")
	raw, cancel, err := daemon.StreamLiveSessionViaDaemon(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan ui.LiveSessionEvent, 32)
	go func() { //nolint:goroutine_without_recover
		forwardLiveSessionEvents(ctx, sessionID, raw, out)
	}()
	return out, cancel, nil
}

func forwardLiveSessionEvents(ctx context.Context, sessionID string, raw <-chan *clydev1.StreamLiveSessionResponse, out chan<- ui.LiveSessionEvent) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cmdUILog.Logger().ErrorContext(ctx, "dashboard.live_session.forwarder_panic",
				"component", "tui",
				"session_id", sessionID,
				"err", fmt.Errorf("panic: %v", recovered),
			)
		}
	}()
	defer close(out)
	for event := range raw {
		timestamp := time.Time{}
		if event.GetTimestampNanos() > 0 {
			timestamp = time.Unix(0, event.GetTimestampNanos())
		}
		out <- ui.LiveSessionEvent{
			SessionID: event.GetSessionId(),
			Kind:      event.GetKind(),
			Role:      event.GetRole(),
			Text:      event.GetText(),
			Timestamp: timestamp,
		}
	}
}

func (builder appCallbackBuilder) compactPreview(req ui.CompactRunRequest) (<-chan ui.CompactEvent, <-chan error, func(), error) {
	ctx := builder.childContext("dashboard.compact.preview")
	return builder.streamCompactEvents(ctx, req, daemon.CompactPreviewViaDaemon, "dashboard.compact_preview.forwarder_panic")
}

func (builder appCallbackBuilder) compactApply(req ui.CompactRunRequest) (<-chan ui.CompactEvent, <-chan error, func(), error) {
	ctx := builder.childContext("dashboard.compact.apply")
	return builder.streamCompactEvents(ctx, req, daemon.CompactApplyViaDaemon, "dashboard.compact_apply.forwarder_panic")
}

type compactRunner func(context.Context, daemon.CompactRunOptions) (<-chan *clydev1.CompactEvent, <-chan error, context.CancelFunc, error)

func (builder appCallbackBuilder) streamCompactEvents(ctx context.Context, req ui.CompactRunRequest, run compactRunner, panicEvent string) (<-chan ui.CompactEvent, <-chan error, func(), error) {
	raw, done, cancel, err := run(ctx, compactRunOptionsFromUI(req))
	if err != nil {
		return nil, nil, nil, err
	}
	out := make(chan ui.CompactEvent, 64)
	go func() { //nolint:goroutine_without_recover
		forwardCompactEvents(ctx, req.SessionName, raw, out, panicEvent)
	}()
	return out, done, cancel, nil
}

func compactRunOptionsFromUI(req ui.CompactRunRequest) daemon.CompactRunOptions {
	return daemon.CompactRunOptions{
		SessionName:    req.SessionName,
		TargetTokens:   req.TargetTokens,
		ReservedTokens: req.ReservedTokens,
		Model:          req.Model,
		ModelExplicit:  req.ModelExplicit,
		Thinking:       req.Thinking,
		Images:         req.Images,
		Tools:          req.Tools,
		Chat:           req.Chat,
		Summarize:      req.Summarize,
		SummarizeMode:  req.SummarizeMode,
		Force:          req.Force,
		Refresh:        false,
	}
}

func forwardCompactEvents(ctx context.Context, sessionName string, raw <-chan *clydev1.CompactEvent, out chan<- ui.CompactEvent, panicEvent string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			cmdUILog.Logger().ErrorContext(ctx, panicEvent,
				"component", "tui",
				"session", sessionName,
				"err", fmt.Errorf("panic: %v", recovered),
			)
		}
	}()
	defer close(out)
	for ev := range raw {
		out <- compactEventFromProto(ev)
	}
}

func (builder appCallbackBuilder) compactUndo(sessionName string) (*ui.CompactUndoResult, error) {
	resp, err := daemon.CompactUndoViaDaemon(builder.childContext("dashboard.compact.undo"), sessionName)
	if err != nil {
		return nil, err
	}
	return &ui.CompactUndoResult{
		AppliedAt:     resp.GetAppliedAt(),
		BoundaryUUID:  resp.GetBoundaryUuid(),
		SyntheticUUID: resp.GetSyntheticUuid(),
	}, nil
}

func (builder appCallbackBuilder) getSessionDetail(sess *session.Session) (ui.SessionDetail, error) {
	resp, err := daemon.GetSessionDetailViaDaemon(builder.childContext("dashboard.session.detail"), sess.Name)
	if err != nil {
		return ui.SessionDetail{}, err
	}
	return sessionDetailFromProto(resp), nil
}

func compactEventFromProto(ev *clydev1.CompactEvent) ui.CompactEvent {
	out := ui.CompactEvent{}
	switch ev.GetKind() {
	case clydev1.CompactEvent_KIND_STATUS:
		out.Kind = "status"
		out.Message = ev.GetMessage()
	case clydev1.CompactEvent_KIND_UPFRONT:
		upfront := ev.GetUpfront()
		out.Kind = "upfront"
		out.Upfront = &ui.CompactUpfront{
			SessionName:    upfront.GetSessionName(),
			SessionID:      upfront.GetSessionId(),
			Model:          upfront.GetModel(),
			CurrentTotal:   int(upfront.GetCurrentTotal()),
			MaxTokens:      int(upfront.GetMaxTokens()),
			MessagesTokens: int(upfront.GetMessagesTokens()),
			BufferTokens:   int(upfront.GetCompactBufferTokens()),
			FreeTokens:     int(upfront.GetFreeTokens()),
			TargetTokens:   int(upfront.GetTargetTokens()),
			ReservedTokens: int(upfront.GetReservedTokens()),
			OverheadTokens: int(upfront.GetContextOverheadTokens()),
			ThinkingBlocks: int(upfront.GetThinkingBlocks()),
			ImageBlocks:    int(upfront.GetImageBlocks()),
			ToolPairs:      int(upfront.GetToolPairs()),
			ChatTurns:      int(upfront.GetChatTurns()),
		}
	case clydev1.CompactEvent_KIND_ITERATION:
		it := ev.GetIteration()
		out.Kind = "iteration"
		out.Iteration = &ui.CompactIteration{
			Iteration: int(it.GetIteration()),
			Step:      it.GetStep(),
			CtxTotal:  int(it.GetCtxTotal()),
			Delta:     int(it.GetDelta()),
			Probe:     it.GetProbe(),
		}
	case clydev1.CompactEvent_KIND_FINAL:
		fin := ev.GetFinal()
		out.Kind = "final"
		out.Final = &ui.CompactFinal{
			FinalTail:      int(fin.GetFinalTail()),
			TargetTokens:   int(fin.GetTargetTokens()),
			StaticFloor:    int(fin.GetStaticFloor()),
			ReservedTokens: int(fin.GetReservedTokens()),
		}
	case clydev1.CompactEvent_KIND_APPLY_MUTATION:
		m := ev.GetApplyMutation()
		out.Kind = "apply_mutation"
		out.ApplyMutation = &ui.CompactApplyMutation{
			BoundaryUUID:  m.GetBoundaryUuid(),
			SyntheticUUID: m.GetSyntheticUuid(),
			SnapshotPath:  m.GetSnapshotPath(),
			LedgerPath:    m.GetLedgerPath(),
		}
	default:
		out.Kind = "status"
		out.Message = "received compact stream update"
	}
	return out
}

func sessionSnapshotFromProto(resp *clydev1.ListSessionsResponse) ui.SessionSnapshot {
	out := ui.SessionSnapshot{
		Sessions:      make([]*session.Session, 0, len(resp.GetSessions())),
		Models:        make(map[string]string, len(resp.GetSessions())),
		MessageCounts: make(map[string]int, len(resp.GetSessions())),
		ContextStates: make(map[string]ui.SessionContextState, len(resp.GetSessions())),
	}
	for _, raw := range resp.GetSessions() {
		sess, model, messageCount, contextState, bridge := sessionSummaryFromProto(raw)
		out.Sessions = append(out.Sessions, sess)
		out.Models[sess.Name] = model
		out.MessageCounts[sess.Name] = messageCount
		out.ContextStates[sess.Name] = contextState
		if bridge != nil {
			out.LiveURLs = append(out.LiveURLs, *bridge)
		}
	}
	return out
}

func sessionSummaryFromProto(raw *clydev1.SessionSummary) (*session.Session, string, int, ui.SessionContextState, *ui.LiveURL) {
	sess := &session.Session{
		Name: raw.GetName(),
		Metadata: session.Metadata{
			Name:                 raw.GetMetadataName(),
			ClydeUUID:            raw.GetClydeUuid(),
			Provider:             session.NormalizeProviderID(session.ProviderID(raw.GetProvider())),
			SessionID:            raw.GetSessionId(),
			TranscriptPath:       raw.GetTranscriptPath(),
			ProviderState:        nil,
			WorkDir:              raw.GetWorkDir(),
			Created:              timeFromNanos(raw.GetCreatedNanos()),
			LastAccessed:         timeFromNanos(raw.GetLastActivityNanos()),
			ParentSession:        raw.GetParentSession(),
			ParentClydeUUID:      raw.GetParentClydeUuid(),
			IsForkedSession:      raw.GetIsForkedSession(),
			IsIncognito:          raw.GetIsIncognito(),
			PreviousSessionIDs:   append([]string(nil), raw.GetPreviousSessionIds()...),
			Context:              raw.GetContext(),
			HasCustomOutputStyle: raw.GetHasCustomOutputStyle(),
			WorkspaceRoot:        raw.GetWorkspaceRoot(),
			ContextMessageCount:  int(raw.GetContextMessageCount()),
			Title:                raw.GetDisplayTitle(),
			AutoNameState:        session.AutoNameState(raw.GetAutoNameState()),
			AutoNameSource:       session.AutoNameSource(raw.GetAutoNameSource()),
			LastAutoNameAt:       timeFromNanos(raw.GetLastAutoNameNanos()),
			AutoNameSourceHash:   raw.GetAutoNameSourceHash(),
		},
	}
	if sess.Metadata.Name == "" {
		sess.Metadata.Name = sess.Name
	}
	contextState := ui.SessionContextState{
		Usage: ui.SessionContextUsage{
			TotalTokens:    int(raw.GetContextTotalTokens()),
			MaxTokens:      int(raw.GetContextMaxTokens()),
			Percentage:     int(raw.GetContextPercentage()),
			MessagesTokens: int(raw.GetContextMessagesTokens()),
		},
		Loaded: raw.GetContextUsageLoaded(),
		Status: raw.GetContextUsageStatus(),
	}

	var liveURL *ui.LiveURL
	if live := raw.GetRuntime().GetLive(); live != nil && live.GetBridgeUrl() != "" {
		liveURL = &ui.LiveURL{
			SessionID: live.GetCurrent().GetSessionId(),
			URL:       live.GetBridgeUrl(),
		}
	} else if b := raw.GetBridge(); b != nil {
		liveURL = &ui.LiveURL{
			SessionID: b.GetSessionId(),
			URL:       b.GetUrl(),
		}
	}
	return sess, raw.GetModel(), int(raw.GetMessageCount()), contextState, liveURL
}

func sessionEventFromProto(ev *clydev1.SubscribeRegistryResponse) ui.SessionEvent {
	out := ui.SessionEvent{
		Kind:         strings.TrimPrefix(ev.GetKind().String(), "KIND_"),
		SessionName:  ev.GetSessionName(),
		SessionID:    ev.GetSessionId(),
		OldName:      ev.GetOldName(),
		LiveURL:      ev.GetBridgeUrl(),
		BinaryPath:   ev.GetBinaryPath(),
		BinaryReason: ev.GetBinaryReason(),
		BinaryHash:   ev.GetBinaryHash(),
	}
	if raw := ev.GetSessionSummary(); raw != nil {
		sess, model, messageCount, contextState, bridge := sessionSummaryFromProto(raw)
		out.Session = sess
		out.Model = model
		out.MessageCount = messageCount
		out.ContextState = &contextState
		out.LiveURLRecord = bridge
	}
	return out
}

func sessionDetailFromProto(resp *clydev1.GetSessionDetailResponse) ui.SessionDetail {
	out := ui.SessionDetail{
		Provider:              resp.GetProvider(),
		Model:                 resp.GetModel(),
		TotalMessages:         int(resp.GetTotalMessages()),
		VisibleTokensEstimate: int(resp.GetVisibleTokensEstimate()),
		LastMessageTokens:     int(resp.GetLastMessageTokens()),
		CompactionCount:       int(resp.GetCompactionCount()),
		LastPreCompactTokens:  int(resp.GetLastPreCompactTokens()),
		TranscriptSizeBytes:   resp.GetTranscriptSizeBytes(),
		TranscriptStatsLoaded: true,
		TranscriptStatsStatus: "",
		Messages:              nil,
		AllMessages:           nil,
		Tools:                 nil,
		ConversationLoading:   false,
		ContextUsage: ui.SessionContextUsage{
			TotalTokens:    int(resp.GetContextTotalTokens()),
			MaxTokens:      int(resp.GetContextMaxTokens()),
			Percentage:     int(resp.GetContextPercentage()),
			MessagesTokens: int(resp.GetContextMessagesTokens()),
		},
		ContextUsageLoaded: resp.GetContextUsageLoaded(),
		ContextUsageStatus: resp.GetContextUsageStatus(),
		ResumeInstructions: append([]string(nil), resp.GetResumeInstructions()...),
	}
	for _, m := range resp.GetRecentMessages() {
		out.Messages = append(out.Messages, ui.DetailMessage{
			Role:      m.GetRole(),
			Text:      m.GetText(),
			Timestamp: timeFromNanos(m.GetTimestampNanos()),
		})
	}
	for _, m := range resp.GetAllMessages() {
		out.AllMessages = append(out.AllMessages, ui.DetailMessage{
			Role:      m.GetRole(),
			Text:      m.GetText(),
			Timestamp: timeFromNanos(m.GetTimestampNanos()),
		})
	}
	for _, t := range resp.GetTools() {
		out.Tools = append(out.Tools, ui.ToolUse{Name: t.GetName(), Count: int(t.GetCount())})
	}
	return out
}

func exportStatsFromProto(resp *clydev1.GetSessionExportStatsResponse) ui.SessionExportStats {
	if resp == nil {
		return ui.SessionExportStats{}
	}
	return ui.SessionExportStats{
		VisibleTokensEstimate: int(resp.GetVisibleTokensEstimate()),
		VisibleMessages:       int(resp.GetVisibleMessages()),
		UserMessages:          int(resp.GetUserMessages()),
		AssistantMessages:     int(resp.GetAssistantMessages()),
		ToolResultMessages:    int(resp.GetToolResultMessages()),
		ToolCalls:             int(resp.GetToolCalls()),
		SystemPrompts:         int(resp.GetSystemPrompts()),
		Compactions:           int(resp.GetCompactions()),
		TranscriptSizeBytes:   resp.GetTranscriptSizeBytes(),
	}
}

func exportRequestToProto(sess *session.Session, req ui.SessionExportRequest) *clydev1.ExportSessionRequest {
	sessionName := strings.TrimSpace(req.SessionName)
	if sessionName == "" && sess != nil {
		sessionName = sess.Name
	}
	return &clydev1.ExportSessionRequest{
		SessionName:            sessionName,
		Format:                 exportFormatToProto(req.Format),
		HistoryStart:           int32(req.HistoryStart),
		WhitespaceCompression:  exportWhitespaceToProto(req.WhitespaceCompression),
		IncludeChat:            req.IncludeChat,
		IncludeThinking:        req.IncludeThinking,
		IncludeSystemPrompts:   req.IncludeSystemPrompts,
		IncludeToolCalls:       req.IncludeToolCalls,
		IncludeToolOutputs:     req.IncludeToolOutputs,
		IncludeRawJsonMetadata: req.IncludeRawJSONMetadata,
	}
}

func exportFormatToProto(format ui.SessionExportFormat) clydev1.SessionExportFormat {
	switch format {
	case ui.SessionExportHTML:
		return clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_HTML
	case ui.SessionExportJSON:
		return clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_JSON
	case ui.SessionExportPlainText:
		return clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_PLAIN_TEXT
	case ui.SessionExportMarkdown:
		fallthrough
	default:
		return clydev1.SessionExportFormat_SESSION_EXPORT_FORMAT_MARKDOWN
	}
}

func exportWhitespaceToProto(mode ui.SessionExportWhitespaceCompression) clydev1.SessionExportWhitespaceCompression {
	switch mode {
	case ui.SessionExportWhitespacePreserve:
		return clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_PRESERVE
	case ui.SessionExportWhitespaceCompact:
		return clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_COMPACT
	case ui.SessionExportWhitespaceDense:
		return clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_DENSE
	case ui.SessionExportWhitespaceTidy:
		fallthrough
	default:
		return clydev1.SessionExportWhitespaceCompression_SESSION_EXPORT_WHITESPACE_COMPRESSION_TIDY
	}
}

func configControlsFromProto(raw []*clydev1.ConfigControl) []ui.ConfigControl {
	out := make([]ui.ConfigControl, 0, len(raw))
	for _, control := range raw {
		if control == nil {
			continue
		}
		entry := ui.ConfigControl{
			Key:          control.GetKey(),
			Section:      control.GetSection(),
			Label:        control.GetLabel(),
			Description:  control.GetDescription(),
			Type:         strings.ToLower(strings.TrimPrefix(control.GetType().String(), "CONFIG_CONTROL_TYPE_")),
			Value:        control.GetValue(),
			DefaultValue: control.GetDefaultValue(),
			Sensitive:    control.GetSensitive(),
			ReadOnly:     control.GetReadOnly(),
		}
		for _, opt := range control.GetOptions() {
			entry.Options = append(entry.Options, ui.ConfigControlOption{
				Value:       opt.GetValue(),
				Label:       opt.GetLabel(),
				Description: opt.GetDescription(),
			})
		}
		out = append(out, entry)
	}
	return out
}

func timeFromNanos(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// ForwardToClaude runs the real claude binary (bypassing the shell
// alias) with the given args, inheriting stdin/stdout/stderr. Returns
// the exit code. Used by the dispatch path and by RunDashboard's
// piped-input shortcut.
func ForwardToClaude(args []string) int {
	ctx := newCommandContext("forward.claude")
	return runClaudeWithEnv(ctx, args, applyClaudeMITMEnv(ctx, os.Environ()))
}

func runClaudeWithEnv(ctx context.Context, args []string, env []string) int {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "clyde: cannot find claude binary: %v\n", err)
		cmdUILog.Logger().ErrorContext(ctx, "forward.claude_not_found",
			"component", "cli",
			"err", err,
		)
		return 1
	}
	cmdUILog.Logger().DebugContext(ctx, "forward.claude.invoked",
		"component", "cli",
		"argc", len(args),
	)
	c := exec.Command(claudePath, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Env = env
	if runErr := c.Run(); runErr != nil {
		if c.ProcessState != nil {
			return c.ProcessState.ExitCode()
		}
		return 1
	}
	return 0
}

func applyClaudeMITMEnv(ctx context.Context, env []string) []string {
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return env
	}
	if !cfg.MITM.EnabledDefault || !cfg.MITM.EnabledFor("claude") {
		return env
	}
	out := env
	for _, contributor := range mitmcontrib.Contributors() {
		sanitizer, ok := contributor.(mitmcontrib.Sanitizer)
		if !ok {
			continue
		}
		out = sanitizer.Sanitize(out)
	}
	for _, providerID := range mitmcontrib.IDs() {
		extra, fetchErr := claudeLaunchEnvironmentViaDaemon(ctx, providerID)
		if fetchErr != nil {
			cmdDispatchLog.Logger().WarnContext(ctx, "forward.mitm.env_fetch_failed",
				"component", "cli",
				"provider", providerID,
				"err", fetchErr,
			)
			continue
		}
		for _, item := range extra {
			if item.GetKey() == "" {
				continue
			}
			// CLAUDE_CONFIG_DIR is the rotator launch-credentials contribution
			// (CLYDE-448). It plants a scratch dir the launching process must
			// remove on child exit. This passthrough-forward helper has no such
			// cleanup, so it would leak planted token material; the lifecycle
			// launch path (internal/providers/claude/lifecycle) owns that flow
			// and its cleanup. Skip the key here. Follow-up: extend rotator
			// launch credentials to the `clyde claude ...` forward path with
			// matching scratch-dir cleanup.
			if item.GetKey() == claudeprovider.ClaudeConfigDirEnv {
				continue
			}
			out = withEnvValue(out, item.GetKey(), item.GetValue())
		}
	}
	return out
}

var claudeLaunchEnvironmentViaDaemon = daemon.ProviderLaunchEnvironmentViaDaemon

// claudePassthroughFirstArgSkipsPostSessionTUI lists argv[0] values that
// usually run a one-shot or long-lived non-dashboard claude subcommand (see
// claude-code entrypoints/cli.tsx fast paths and main.tsx program.command
// registrations). When clyde forwards to claude and these are the first
// token, skip the post-claude TUI (same intent as api / print).
var claudePassthroughFirstArgSkipsPostSessionTUI = map[string]bool{
	"agents":             true,
	"assistant":          true,
	"attach":             true,
	"auth":               true,
	"auto-mode":          true,
	"bridge":             true,
	"daemon":             true,
	"doctor":             true,
	"environment-runner": true,
	"install":            true,
	"kill":               true,
	"logs":               true,
	"plugin":             true,
	"plugins":            true,
	"ps":                 true,
	"rc":                 true,
	"remote":             true,
	"remote-control":     true,
	"self-hosted-runner": true,
	"server":             true,
	"setup-token":        true,
	"sync":               true,
	"update":             true,
	"upgrade":            true,
}

// passthroughSkipsPostSessionTUI reports args that should not open the
// dashboard after claude exits (non-interactive or API-style invocations).
func passthroughSkipsPostSessionTUI(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if args[0] == "api" {
		return true
	}
	if _, skip := claudePassthroughFirstArgSkipsPostSessionTUI[args[0]]; skip {
		return true
	}
	for _, a := range args {
		if a == "-p" || a == "--print" {
			return true
		}
	}
	return false
}

// ForwardToClaudeThenDashboard runs claude like ForwardToClaude, but for an
// interactive terminal it assigns a stable session id and exact display name
// for new Claude sessions, then opens the TUI when claude exits. Pipe, resume,
// and print-style invocations behave like ForwardToClaude only.
func ForwardToClaudeThenDashboard(args []string) int {
	if !shouldOpenDashboardAfterPassthrough(args) {
		return ForwardToClaude(args)
	}
	ctx := newCommandContext("forward.claude.dashboard")
	env := withEnvValue(os.Environ(), "CLYDE_LAUNCH_CWD", currentWorkingDirectory())
	store, serr := session.NewGlobalFileStore()
	if serr != nil {
		cmdUILog.Logger().WarnContext(ctx, "forward.passthrough_store_unavailable",
			"component", "cli",
			"err", serr,
		)
	}
	args, env = applyPassthroughBootstrapIdentity(ctx, args, env, store)
	_ = runClaudeWithEnv(ctx, args, applyClaudeMITMEnv(ctx, env))
	_ = runDashboardTUI()
	return 0
}

func shouldOpenDashboardAfterPassthrough(args []string) bool {
	if passthroughSkipsPostSessionTUI(args) {
		return false
	}
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

func currentWorkingDirectory() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(wd)
}

func withEnvValue(env []string, key, value string) []string {
	if key == "" || value == "" {
		return env
	}
	prefix := key + "="
	out := append([]string(nil), env...)
	for i, item := range out {
		if strings.HasPrefix(item, prefix) {
			out[i] = prefix + value
			return out
		}
	}
	return append(out, prefix+value)
}

func envListValue(env []string, key string) string {
	prefix := key + "="
	for i := len(env) - 1; i >= 0; i-- {
		item := env[i]
		if value, ok := strings.CutPrefix(item, prefix); ok {
			return value
		}
	}
	return ""
}

func applyPassthroughBootstrapIdentity(
	ctx context.Context,
	args []string,
	env []string,
	store session.Store,
) ([]string, []string) {
	if !passthroughStartsNewSession(args) {
		return args, env
	}

	sessionName := envListValue(env, "CLYDE_SESSION_NAME")
	if strings.TrimSpace(sessionName) == "" {
		if store == nil {
			return args, env
		}
		name, err := nextChatSessionName(store)
		if err != nil {
			cmdUILog.Logger().WarnContext(ctx, "forward.passthrough_name_failed",
				"component", "cli",
				"err", err,
			)
			return args, env
		}
		sessionName = name
		env = withEnvValue(env, "CLYDE_SESSION_NAME", sessionName)
	}

	sessionID := strings.TrimSpace(envListValue(env, "CLYDE_SESSION_ID"))
	if sessionID == "" {
		generatedID, err := util.GenerateUUIDE()
		if err != nil {
			cmdUILog.Logger().WarnContext(ctx, "forward.passthrough_session_id_failed",
				"component", "cli",
				"session", sessionName,
				"err", err,
			)
			return args, env
		}
		sessionID = generatedID
		env = withEnvValue(env, "CLYDE_SESSION_ID", sessionID)
	}

	args = appendSessionIDArg(args, sessionID)
	cmdUILog.Logger().InfoContext(ctx, "forward.passthrough_wrapped",
		"component", "cli",
		"session", sessionName,
		"session_id", sessionID,
	)
	return args, env
}

type passthroughControlArg string

const (
	passthroughControlArgResumeLong    passthroughControlArg = "--resume"
	passthroughControlArgResumeShort   passthroughControlArg = "-r"
	passthroughControlArgContinueLong  passthroughControlArg = "--continue"
	passthroughControlArgContinueShort passthroughControlArg = "-c"
	passthroughControlArgSessionID     passthroughControlArg = "--session-id"
)

func passthroughStartsNewSession(args []string) bool {
	for _, arg := range args {
		switch passthroughControlArg(arg) {
		case passthroughControlArgResumeLong,
			passthroughControlArgResumeShort,
			passthroughControlArgContinueLong,
			passthroughControlArgContinueShort,
			passthroughControlArgSessionID:
			return false
		}
		if strings.HasPrefix(arg, "--resume=") ||
			strings.HasPrefix(arg, "--continue=") ||
			strings.HasPrefix(arg, "--session-id=") {
			return false
		}
	}
	return true
}

func appendSessionIDArg(args []string, sessionID string) []string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "--session-id", sessionID)
	out = append(out, args...)
	return out
}

// nextChatSessionName returns a new display-safe chat-* name that does not
// collide with existing sessions.
func nextChatSessionName(store session.Store) (string, error) {
	list, err := store.List()
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(list))
	for _, s := range list {
		taken[s.Name] = true
	}
	base := "chat-" + currentTime().UTC().Format("20060102-150405")
	name := session.UniqueDisplayName(base, taken)
	if taken[name] {
		return "", fmt.Errorf("could not allocate a unique session name")
	}
	return name, nil
}

// startNewSessionInDir launches the default interactive provider for a new named
// session in workDir.
// basedir may be empty; dashboardFallbackCWD is used when the trimmed path is empty.
func startNewSessionInDir(ctx context.Context, basedir string, store session.Store, dashboardFallbackCWD string, enableRemoteControl bool) error {
	if ctx == nil {
		ctx = newCommandContext("session.new")
	}
	workDir := strings.TrimSpace(basedir)
	if workDir == "" {
		workDir = strings.TrimSpace(dashboardFallbackCWD)
	}
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			slog.WarnContext(ctx, "session.new.workdir_resolve_failed",
				"component", "cli",
				"err", err,
			)
			return fmt.Errorf("resolve working directory: %w", err)
		}
		workDir = wd
	}

	suggestedName, err := nextChatSessionName(store)
	if err != nil {
		return err
	}
	// The daemon owns session-record creation. Ask it to mint the provider id
	// and persist the record, then launch the tool locally with that id, so the
	// SessionStart hook resolves this chat by UUID rather than by name.
	name, sessionID, err := createSessionRecordViaDaemon(ctx, suggestedName, workDir)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "Starting new session %q in %s\n\n", name, workDir)
	cmdUILog.Logger().InfoContext(ctx, "session.new.started",
		"component", "cli",
		"session", name,
		"session_id", sessionID,
		"workdir", workDir,
		"remote_control", enableRemoteControl,
	)

	runtime, err := registry.Default(store)
	if err != nil {
		return err
	}
	err = runtime.StartInteractive(ctx, session.StartRequest{
		SessionName: name,
		SessionID:   sessionID,
		Launch: session.LaunchOptions{
			WorkDir:             workDir,
			Intent:              session.LaunchIntentNewSession,
			EnableRemoteControl: enableRemoteControl,
		},
	})
	if err != nil {
		return err
	}
	sess, gerr := store.Get(name)
	if gerr == nil && sess != nil {
		if fs, ok := store.(*session.FileStore); ok {
			autoUpdateContext(ctx, fs, sess)
		}
		printResumeInstructions(ctx, sess)
	}
	return nil
}

// createSessionRecordViaDaemon asks the daemon to mint a provider session id and
// persist the session record, returning the resolved name and id. The daemon is
// the sole writer of session records; it auto-starts via ConnectOrStart when not
// already running.
func createSessionRecordViaDaemon(ctx context.Context, suggestedName, workDir string) (string, string, error) {
	dc, err := daemon.ConnectOrStart(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "session.new.daemon_connect_failed", "component", "cli", "err", err)
		return "", "", fmt.Errorf("connect to daemon: %w", err)
	}
	defer func() { _ = dc.Close() }()
	resp, err := daemonclient.New(dc.Connection()).CreateSession(ctx, suggestedName, workDir, string(session.ProviderClaude), false)
	if err != nil {
		slog.ErrorContext(ctx, "session.new.create_session_failed", "component", "cli", "session", suggestedName, "err", err)
		return "", "", fmt.Errorf("create session via daemon: %w", err)
	}
	return resp.GetSessionName(), resp.GetSessionId(), nil
}

// resumeSession resumes an existing clyde session through the provider-owned
// lifecycle, reattaching its workspace add-dir if the user invoked from a
// different cwd. Shared by the resume cobra verb and the TUI dashboard's
// resume callback.
func resumeSession(ctx context.Context, sess *session.Session, store session.Store, allowSelfReload bool) error {
	if ctx == nil {
		ctx = newCommandContext("session.resume")
	}
	currentWorkDir := ""
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		currentWorkDir = cwd
	}

	_, _ = fmt.Fprintf(os.Stdout, "Resuming session '%s' (%s)\n\n", sess.Name, sess.Metadata.ProviderSessionID())
	_, _ = fmt.Fprintln(os.Stdout, "Dashboard is suspended while Claude runs. Exit Claude to return.")
	_, _ = fmt.Fprintln(os.Stdout)
	cmdUILog.Logger().InfoContext(ctx, "session.resume.started",
		"component", "cli",
		"session", sess.Name,
		"session_id", sess.Metadata.ProviderSessionID(),
	)

	runtime, err := registry.ForSession(sess, store)
	if err != nil {
		return err
	}
	leaseToken := ""
	leaseCtx := childCommandContext(ctx, "session.foreground.acquire")
	if lease, leaseErr := daemon.AcquireForegroundSessionViaDaemon(leaseCtx, &clydev1.AcquireForegroundSessionRequest{
		SessionName: sess.Name,
		SessionId:   sess.Metadata.ProviderSessionID(),
		Provider:    string(sess.ProviderID()),
	}); leaseErr != nil {
		cmdUILog.Logger().WarnContext(leaseCtx, "session.foreground.acquire_failed",
			"component", "cli",
			"session", sess.Name,
			"session_id", sess.Metadata.ProviderSessionID(),
			"provider", sess.ProviderID(),
			"err", leaseErr,
		)
	} else if lease != nil {
		leaseToken = lease.GetLeaseToken()
	}
	exitState := "ok"
	defer func() {
		if leaseToken == "" {
			return
		}
		releaseCtx := childCommandContext(ctx, "session.foreground.release")
		if _, releaseErr := daemon.ReleaseForegroundSessionViaDaemon(releaseCtx, leaseToken, exitState); releaseErr != nil {
			cmdUILog.Logger().WarnContext(releaseCtx, "session.foreground.release_failed",
				"component", "cli",
				"session", sess.Name,
				"session_id", sess.Metadata.ProviderSessionID(),
				"provider", sess.ProviderID(),
				"err", releaseErr,
			)
		}
	}()
	err = runtime.ResumeInteractive(ctx, session.ResumeRequest{
		Session: sess,
		Options: session.ResumeOptions{
			CurrentWorkDir:   currentWorkDir,
			EnableSelfReload: allowSelfReload,
		},
	})
	if err != nil {
		exitState = "error"
	}
	if fs, ok := store.(*session.FileStore); ok {
		autoUpdateContext(ctx, fs, sess)
	}
	printResumeInstructions(ctx, sess)
	return err
}

func daemonLifecycleError(ctx context.Context, action string, outcome daemon.LifecycleOutcome, err error) error {
	if err == nil {
		return fmt.Errorf("daemon %s %s", action, outcome)
	}
	slog.WarnContext(ctx, "daemon.lifecycle.failed",
		"component", "cli",
		"action", action,
		"outcome", outcome,
		"err", err,
	)
	return fmt.Errorf("daemon %s %s: %w", action, outcome, err)
}
