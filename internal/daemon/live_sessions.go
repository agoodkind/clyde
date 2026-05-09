package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/correlation"
	"goodkind.io/clyde/internal/livetrack"
	claudeprovider "goodkind.io/clyde/internal/providers/claude"
	codex "goodkind.io/clyde/internal/providers/codex/lifecycle"
	"goodkind.io/clyde/internal/session"
	sessionsettings "goodkind.io/clyde/internal/session/settings"
	"goodkind.io/clyde/internal/util"
	"goodkind.io/clyde/internal/webapp"
)

var newCodexLiveRuntime = codex.NewLiveRuntime

// claudeRemoteSuspendTimeout bounds how long suspendClaudeRemoteForForeground
// waits for a Claude remote worker to exit before escalation.
var claudeRemoteSuspendTimeout = 2 * time.Second

type liveRuntimeSessionState struct {
	effort codex.ReasoningEffort
	stream *liveStreamFanout
}

var liveRuntimeStates = struct {
	mu     sync.Mutex
	values map[string]*liveRuntimeSessionState
}{
	mu:     sync.Mutex{},
	values: make(map[string]*liveRuntimeSessionState),
}

func liveRuntimeState(sessionID string) *liveRuntimeSessionState {
	liveRuntimeStates.mu.Lock()
	defer liveRuntimeStates.mu.Unlock()
	state := liveRuntimeStates.values[sessionID]
	if state == nil {
		state = &liveRuntimeSessionState{
			effort: "",
			stream: nil,
		}
		liveRuntimeStates.values[sessionID] = state
	}
	return state
}

func setLiveRuntimeState(sessionID string, state *liveRuntimeSessionState) {
	liveRuntimeStates.mu.Lock()
	defer liveRuntimeStates.mu.Unlock()
	liveRuntimeStates.values[sessionID] = state
}

// registerCodexLiveSession registers a Codex live session with the livetrack
// registry so reload/drain can account for it.
func (s *Server) registerCodexLiveSession(ctx context.Context, record *liveRuntimeSession, runtime codex.LiveRuntime) {
	if s == nil || s.liveWorkers == nil || record == nil {
		return
	}
	lsess, err := s.liveWorkers.Register(ctx, "codex.live", LiveMeta{
		Provider:      "codex",
		LiveSessionID: record.id,
		WorkerPID:     0,
		Lease:         "background",
	}, &codexRuntimeCloser{runtime: runtime})
	if err == nil {
		s.remoteMu.Lock()
		record.livetrackSession = lsess
		s.remoteMu.Unlock()
		return
	}
	s.log.WarnContext(ctx, "daemon.live_session.codex_livetrack_register_failed",
		"component", "daemon",
		"provider", session.ProviderCodex,
		"session", record.name,
		"session_id", record.id,
		"err", err,
	)
}

func (s *Server) releaseCodexLiveSession(ctx context.Context, record *liveRuntimeSession, reason string) {
	if s == nil || s.liveWorkers == nil || record == nil {
		return
	}
	s.remoteMu.Lock()
	livetrackSession := record.livetrackSession
	record.livetrackSession = nil
	s.remoteMu.Unlock()
	if livetrackSession == nil {
		return
	}
	s.liveWorkers.Release(ctx, livetrackSession, reason)
}

func deleteLiveRuntimeState(sessionID string) {
	liveRuntimeStates.mu.Lock()
	defer liveRuntimeStates.mu.Unlock()
	delete(liveRuntimeStates.values, sessionID)
}

// ListLiveSessions returns daemon-owned live sessions and reattachable Claude
// sessions.
func (s *Server) ListLiveSessions(context.Context, *clydev1.ListLiveSessionsRequest) (*clydev1.ListLiveSessionsResponse, error) {
	s.remoteMu.Lock()
	out := make([]*clydev1.LiveSession, 0, len(s.liveSessions)+len(s.remoteWorkers))
	seenClaude := make(map[string]bool, len(s.remoteWorkers))
	for _, live := range s.liveSessions {
		out = append(out, protoLiveSessionFromRecord(live))
	}
	for _, worker := range s.remoteWorkers {
		if worker == nil {
			continue
		}
		out = append(out, protoClaudeLiveSessionFromWorker(worker, "running", s.bridgeURLForSession(worker.sessionID)))
		seenClaude[worker.sessionID] = true
	}
	s.remoteMu.Unlock()
	out = append(out, s.discoverClaudeLiveSessions(seenClaude)...)
	return &clydev1.ListLiveSessionsResponse{Sessions: out}, nil
}

// StartLiveSession starts a provider live session through the daemon.
func (s *Server) StartLiveSession(ctx context.Context, req *clydev1.StartLiveSessionRequest) (*clydev1.StartLiveSessionResponse, error) {
	peerInfo, _ := peer.FromContext(ctx)
	provider := session.NormalizeProviderID(session.ProviderID(strings.TrimSpace(req.GetProvider())))
	if provider == session.ProviderUnknown {
		provider = session.ProviderCodex
	}
	switch provider {
	case session.ProviderClaude:
		return s.startClaudeLiveSession(ctx, req, daemonPeerAddr(peerInfo))
	case session.ProviderCodex:
		return s.startCodexLiveSession(ctx, req, daemonPeerAddr(peerInfo))
	case session.ProviderUnknown:
		return nil, fmt.Errorf("%w", status.Errorf(codes.InvalidArgument, "live provider %q is not supported", provider))
	default:
		return nil, fmt.Errorf("%w", status.Errorf(codes.InvalidArgument, "live provider %q is not supported", provider))
	}
}

// SendLiveSession sends text to a live session.
func (s *Server) SendLiveSession(ctx context.Context, req *clydev1.SendLiveSessionRequest) (*clydev1.SendLiveSessionResponse, error) {
	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, "session_id is required"))
	}
	if live := s.liveSessionByID(sessionID); live != nil {
		if live.provider == session.ProviderCodex {
			return s.sendCodexLiveSession(ctx, live, req.GetText())
		}
	}
	target, err := resolveSessionRuntime("", "", sessionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session runtime: %v", err)
	}
	if target.Provider == session.ProviderClaude {
		resp, err := s.SendToSession(ctx, &clydev1.SendToSessionRequest{
			SessionId: sessionID,
			Text:      req.GetText(),
			Provider:  string(session.ProviderClaude),
		})
		if err != nil {
			return nil, err
		}
		return &clydev1.SendLiveSessionResponse{Accepted: resp.GetDelivered()}, nil
	}
	live, err := s.liveSessionRecord(ctx, sessionID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "live session %q: %v", sessionID, err)
	}
	return s.sendCodexLiveSession(ctx, live, req.GetText())
}

// StreamLiveSession streams live-session events over gRPC.
func (s *Server) StreamLiveSession(req *clydev1.StreamLiveSessionRequest, stream clydev1.ClydeService_StreamLiveSessionServer) error {
	streamCtx, streamCancel := context.WithCancel(stream.Context())
	defer streamCancel()
	if s.RPCs != nil {
		peerInfo, _ := peer.FromContext(streamCtx)
		corr := correlation.FromContext(streamCtx)
		rpcSess, err := s.RPCs.Register(streamCtx, "daemon.rpc.stream_live_session", RPCMeta{
			Method:     "/clyde.v1.ClydeService/StreamLiveSession",
			Direction:  "inbound",
			PeerActor:  daemonPeerAddr(peerInfo),
			RequestID:  corr.RequestID,
			TraceID:    string(corr.TraceID),
			LeaseToken: "",
		}, rpcCloser{cancel: streamCancel})
		if err != nil {
			if errors.Is(err, livetrack.ErrRegistryClosed) {
				return status.Error(codes.Unavailable, "daemon streaming RPC registry is draining")
			}
			return status.Errorf(codes.Internal, "register live session stream RPC: %v", err)
		}
		defer s.RPCs.Release(streamCtx, rpcSess, "stream_live_session.done")
	}
	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return fmt.Errorf("%w", status.Error(codes.InvalidArgument, "session_id is required"))
	}
	events, err := s.streamLiveSessionEvents(streamCtx, sessionID)
	if err != nil {
		return err
	}
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return nil
			}
			if err := stream.Send(protoStreamLiveSessionEvent(event)); err != nil {
				return fmt.Errorf("send live session event: %w", err)
			}
		case <-streamCtx.Done():
			return nil
		}
	}
}

// StopLiveSession stops a daemon-owned live session.
func (s *Server) StopLiveSession(ctx context.Context, req *clydev1.StopLiveSessionRequest) (*clydev1.StopLiveSessionResponse, error) {
	sessionID := strings.TrimSpace(req.GetSessionId())
	if sessionID == "" {
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, "session_id is required"))
	}
	live := s.liveSessionByID(sessionID)
	if live == nil {
		target, err := resolveSessionRuntime("", "", sessionID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "resolve session runtime: %v", err)
		}
		if target.Provider == session.ProviderClaude {
			return nil, fmt.Errorf("%w", status.Error(codes.FailedPrecondition, "claude live sessions are stopped from their owning terminal"))
		}
		return nil, status.Errorf(codes.NotFound, "live session %q not found", sessionID)
	}
	if live.provider != session.ProviderCodex {
		return nil, status.Errorf(codes.FailedPrecondition, "live provider %q does not support daemon stop", live.provider)
	}
	state := liveRuntimeState(live.id)
	if live.lastTurnID != "" {
		if err := live.codexRuntime.Stop(ctx, codex.LiveStopRequest{ThreadID: live.id, TurnID: live.lastTurnID}); err != nil {
			return nil, status.Errorf(codes.Internal, "stop live turn: %v", err)
		}
	}
	if state.stream != nil {
		state.stream.close()
	}
	if err := live.codexRuntime.Close(); err != nil {
		return nil, status.Errorf(codes.Internal, "close live runtime: %v", err)
	}
	s.releaseCodexLiveSession(ctx, live, "codex.live.stopped")
	s.remoteMu.Lock()
	delete(s.liveSessions, live.id)
	s.remoteMu.Unlock()
	deleteLiveRuntimeState(live.id)
	return &clydev1.StopLiveSessionResponse{Stopped: true}, nil
}

// AcquireForegroundSession hands a live session back to a foreground client.
func (s *Server) AcquireForegroundSession(ctx context.Context, req *clydev1.AcquireForegroundSessionRequest) (*clydev1.AcquireForegroundSessionResponse, error) {
	_, _ = peer.FromContext(ctx)
	if strings.TrimSpace(req.GetSessionName()) == "" && strings.TrimSpace(req.GetSessionId()) == "" {
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, "session_name or session_id is required"))
	}
	target, err := resolveSessionRuntime(req.GetSessionName(), req.GetProvider(), req.GetSessionId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session runtime: %v", err)
	}
	if target.Session == nil {
		return nil, status.Errorf(codes.NotFound, "no session runtime for %q", req.GetSessionId())
	}
	token, err := util.GenerateUUIDE()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate lease token: %v", err)
	}
	lease := &foregroundLease{
		token:         token,
		provider:      target.Provider,
		sessionName:   target.Session.Name,
		sessionID:     target.SessionID,
		basedir:       liveSessionStoredBasedir(target.Session),
		model:         "",
		status:        "",
		incognito:     false,
		shouldRestore: false,
		restoreReason: "",
		acquiredAt:    daemonNow(),
	}
	if lease.basedir == "" {
		lease.basedir = strings.TrimSpace(target.Session.Metadata.WorkDir)
	}
	switch target.Provider {
	case session.ProviderCodex:
		if err := s.suspendCodexLiveForForeground(ctx, lease); err != nil {
			return nil, err
		}
	case session.ProviderClaude:
		if err := s.suspendClaudeRemoteForForeground(ctx, lease); err != nil {
			return nil, err
		}
	case session.ProviderUnknown:
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support foreground handoff", target.Provider)
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "session provider %q does not support foreground handoff", target.Provider)
	}
	s.remoteMu.Lock()
	s.liveLeases[token] = lease
	s.remoteMu.Unlock()
	s.log.InfoContext(ctx, "daemon.foreground_session.acquired",
		"component", "daemon",
		"provider", lease.provider,
		"session", lease.sessionName,
		"session_id", lease.sessionID,
		"restore", lease.shouldRestore,
		"restore_reason", lease.restoreReason,
	)
	return &clydev1.AcquireForegroundSessionResponse{
		LeaseToken:    lease.token,
		Provider:      string(lease.provider),
		SessionName:   lease.sessionName,
		SessionId:     lease.sessionID,
		ShouldRestore: lease.shouldRestore,
		RestoreReason: lease.restoreReason,
	}, nil
}

// ReleaseForegroundSession restores a suspended live session after foreground
// control exits.
func (s *Server) ReleaseForegroundSession(ctx context.Context, req *clydev1.ReleaseForegroundSessionRequest) (*clydev1.ReleaseForegroundSessionResponse, error) {
	_, _ = peer.FromContext(ctx)
	token := strings.TrimSpace(req.GetLeaseToken())
	if token == "" {
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, "lease_token is required"))
	}
	s.remoteMu.Lock()
	lease := s.liveLeases[token]
	delete(s.liveLeases, token)
	s.remoteMu.Unlock()
	if lease == nil {
		return &clydev1.ReleaseForegroundSessionResponse{}, nil
	}
	if !lease.shouldRestore {
		s.log.InfoContext(ctx, "daemon.foreground_session.released",
			"component", "daemon",
			"provider", lease.provider,
			"session", lease.sessionName,
			"session_id", lease.sessionID,
			"exit_state", strings.TrimSpace(req.GetExitState()),
			"restored", false,
		)
		return &clydev1.ReleaseForegroundSessionResponse{}, nil
	}
	var live *clydev1.LiveSession
	var err error
	switch lease.provider {
	case session.ProviderCodex:
		live, err = s.restoreCodexLiveAfterForeground(ctx, lease)
	case session.ProviderClaude:
		live, err = s.restoreClaudeRemoteAfterForeground(ctx, lease)
	case session.ProviderUnknown:
		err = fmt.Errorf("unsupported provider %q", lease.provider)
	default:
		err = fmt.Errorf("unsupported provider %q", lease.provider)
	}
	if err != nil {
		s.log.WarnContext(ctx, "daemon.foreground_session.restore_failed",
			"component", "daemon",
			"provider", lease.provider,
			"session", lease.sessionName,
			"session_id", lease.sessionID,
			"exit_state", strings.TrimSpace(req.GetExitState()),
			"err", err,
		)
		return &clydev1.ReleaseForegroundSessionResponse{Restored: false}, nil
	}
	s.log.InfoContext(ctx, "daemon.foreground_session.released",
		"component", "daemon",
		"provider", lease.provider,
		"session", lease.sessionName,
		"session_id", lease.sessionID,
		"exit_state", strings.TrimSpace(req.GetExitState()),
		"restored", true,
	)
	return &clydev1.ReleaseForegroundSessionResponse{Restored: true, LiveSession: live}, nil
}

func (s *Server) startClaudeLiveSession(ctx context.Context, req *clydev1.StartLiveSessionRequest, peerAddr string) (*clydev1.StartLiveSessionResponse, error) {
	_, _ = peer.FromContext(ctx)
	basedir, err := liveSessionLaunchBasedir(req.GetName(), req.GetBasedir())
	if err != nil {
		return nil, err
	}
	effort, err := claudeprovider.ParseEffortLevel(req.GetEffort())
	if err != nil {
		s.log.ErrorContext(ctx, "daemon.live_session.claude_effort_invalid",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"effort", req.GetEffort(),
			"err", err,
		)
		return nil, status.Errorf(codes.InvalidArgument, "claude live effort: %v", err)
	}
	resp, err := s.StartRemoteSession(ctx, &clydev1.StartRemoteSessionRequest{
		SessionName: req.GetName(),
		Basedir:     basedir,
		Incognito:   req.GetIncognito(),
	})
	if err != nil {
		s.log.ErrorContext(ctx, "daemon.live_session.claude_start_failed",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"session", req.GetName(),
			"basedir", basedir,
			"err", err,
		)
		return nil, err
	}
	if err := saveClaudeLiveEffort(resp.GetSessionName(), effort); err != nil {
		s.log.ErrorContext(ctx, "daemon.live_session.claude_effort_save_failed",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"session", resp.GetSessionName(),
			"session_id", resp.GetSessionId(),
			"effort", effort,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "save claude live effort: %v", err)
	}
	s.log.InfoContext(ctx, "daemon.live_session.started",
		"component", "daemon",
		"provider", session.ProviderClaude,
		"session", resp.GetSessionName(),
		"session_id", resp.GetSessionId(),
		"peer_addr", peerAddr,
	)
	return &clydev1.StartLiveSessionResponse{Session: &clydev1.LiveSession{
		Provider:       string(session.ProviderClaude),
		SessionName:    resp.GetSessionName(),
		SessionId:      resp.GetSessionId(),
		Status:         resp.GetLaunchState().String(),
		Basedir:        basedir,
		SupportsSend:   true,
		SupportsStream: true,
		SupportsStop:   false,
	}}, nil
}

func (s *Server) startCodexLiveSession(ctx context.Context, req *clydev1.StartLiveSessionRequest, peerAddr string) (*clydev1.StartLiveSessionResponse, error) {
	_, _ = peer.FromContext(ctx)
	basedir, err := liveSessionLaunchBasedir(req.GetName(), req.GetBasedir())
	if err != nil {
		return nil, err
	}
	effort, err := codex.ParseReasoningEffort(req.GetEffort())
	if err != nil {
		s.log.ErrorContext(ctx, "daemon.live_session.codex_effort_invalid",
			"component", "daemon",
			"provider", session.ProviderCodex,
			"effort", req.GetEffort(),
			"err", err,
		)
		return nil, status.Errorf(codes.InvalidArgument, "codex live effort: %v", err)
	}
	runtime := newCodexLiveRuntime(codex.LiveRuntimeOptions{
		CodexBin:       "",
		Command:        nil,
		WorkDir:        basedir,
		Env:            nil,
		ClientName:     "",
		ClientTitle:    "",
		ClientVersion:  "",
		Experimental:   false,
		ConfigOverride: nil,
	})
	live, err := runtime.Start(ctx, codex.LiveStartRequest{
		WorkDir:               basedir,
		Model:                 strings.TrimSpace(req.GetModel()),
		ModelProvider:         "",
		DeveloperInstructions: "",
		BaseInstructions:      "",
		SessionName:           strings.TrimSpace(req.GetName()),
		Ephemeral:             req.GetIncognito(),
	})
	if err != nil {
		if closeErr := runtime.Close(); closeErr != nil {
			s.log.ErrorContext(ctx, "daemon.live_session.codex_runtime_close_failed",
				"component", "daemon",
				"provider", session.ProviderCodex,
				"session", req.GetName(),
				"basedir", basedir,
				"err", closeErr,
			)
		}
		s.log.ErrorContext(ctx, "daemon.live_session.codex_start_failed",
			"component", "daemon",
			"provider", session.ProviderCodex,
			"session", req.GetName(),
			"basedir", basedir,
			"model", strings.TrimSpace(req.GetModel()),
			"effort", effort,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "start codex live session: %v", err)
	}
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		name = live.ThreadID
	}
	record := &liveRuntimeSession{
		provider:         session.ProviderCodex,
		name:             name,
		id:               live.ThreadID,
		basedir:          live.WorkDir,
		model:            live.Model,
		status:           "idle",
		startedAt:        daemonNow(),
		lastTurnID:       "",
		codexRuntime:     runtime,
		livetrackSession: nil,
	}
	if record.basedir == "" {
		record.basedir = basedir
	}
	if record.model == "" {
		record.model = strings.TrimSpace(req.GetModel())
	}
	s.remoteMu.Lock()
	s.liveSessions[record.id] = record
	s.remoteMu.Unlock()
	setLiveRuntimeState(record.id, &liveRuntimeSessionState{
		effort: effort,
		stream: nil,
	})
	s.registerCodexLiveSession(ctx, record, runtime)
	s.log.InfoContext(ctx, "daemon.live_session.started",
		"component", "daemon",
		"provider", session.ProviderCodex,
		"session", record.name,
		"session_id", record.id,
		"basedir", record.basedir,
		"peer_addr", peerAddr,
	)
	return &clydev1.StartLiveSessionResponse{Session: protoLiveSessionFromRecord(record)}, nil
}

func liveSessionLaunchBasedir(sessionName, requestedBasedir string) (string, error) {
	basedir := strings.TrimSpace(requestedBasedir)
	if basedir != "" {
		return basedir, nil
	}
	name := strings.TrimSpace(sessionName)
	if name == "" {
		return "", fmt.Errorf("%w", status.Error(codes.InvalidArgument, "basedir is required when no stored session name is provided"))
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return "", status.Errorf(codes.Internal, "store init: %v", err)
	}
	sess, err := store.Resolve(name)
	if err != nil {
		return "", status.Errorf(codes.NotFound, "resolve session %q: %v", name, err)
	}
	if sess == nil {
		return "", status.Errorf(codes.NotFound, "session %q not found", name)
	}
	if basedir = strings.TrimSpace(sess.Metadata.WorkDir); basedir != "" {
		return basedir, nil
	}
	if basedir = strings.TrimSpace(sess.Metadata.WorkspaceRoot); basedir != "" {
		return basedir, nil
	}
	return "", status.Errorf(codes.FailedPrecondition, "stored session %q has no basedir; choose a folder explicitly", name)
}

func liveSessionStoredBasedir(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	if basedir := strings.TrimSpace(sess.Metadata.WorkDir); basedir != "" {
		return basedir
	}
	return strings.TrimSpace(sess.Metadata.WorkspaceRoot)
}

func saveClaudeLiveEffort(sessionName string, effort claudeprovider.EffortLevel) error {
	if effort == claudeprovider.EffortLevelUnset {
		return nil
	}
	store, err := session.NewGlobalFileStore()
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	sess, err := store.Resolve(sessionName)
	if err != nil {
		return fmt.Errorf("resolve session %q: %w", sessionName, err)
	}
	settings, err := sessionsettings.Load(store, sess)
	if err != nil {
		return fmt.Errorf("load session settings: %w", err)
	}
	if settings == nil {
		settings = &session.Settings{
			Model:         "",
			EffortLevel:   "",
			OutputStyle:   "",
			RemoteControl: true,
			ContextWindow: "",
			Permissions: session.Permissions{
				Allow:                        nil,
				Ask:                          nil,
				Deny:                         nil,
				AdditionalDirectories:        nil,
				DefaultMode:                  "",
				DisableBypassPermissionsMode: "",
			},
		}
	}
	settings.EffortLevel = string(effort)
	settings.RemoteControl = true
	if err := sessionsettings.Save(store, sess, settings); err != nil {
		return fmt.Errorf("save session settings: %w", err)
	}
	return nil
}

func (s *Server) suspendCodexLiveForForeground(ctx context.Context, lease *foregroundLease) error {
	if lease == nil {
		return nil
	}
	s.remoteMu.Lock()
	live := s.liveSessions[lease.sessionID]
	if live != nil {
		delete(s.liveSessions, lease.sessionID)
		lease.shouldRestore = true
		lease.restoreReason = "codex_live_runtime"
		lease.basedir = firstNonEmpty(live.basedir, lease.basedir)
		lease.model = live.model
		lease.status = live.status
	}
	s.remoteMu.Unlock()
	if live == nil {
		return nil
	}
	state := liveRuntimeState(live.id)
	if live.lastTurnID != "" {
		if err := live.codexRuntime.Stop(ctx, codex.LiveStopRequest{ThreadID: live.id, TurnID: live.lastTurnID}); err != nil {
			s.log.WarnContext(ctx, "daemon.foreground_session.codex_stop_failed",
				"component", "daemon",
				"session", lease.sessionName,
				"session_id", lease.sessionID,
				"err", err,
			)
		}
	}
	if state.stream != nil {
		state.stream.close()
	}
	if err := live.codexRuntime.Close(); err != nil {
		s.releaseCodexLiveSession(ctx, live, "codex.live.foreground_suspend_failed")
		s.log.ErrorContext(ctx, "daemon.foreground_session.codex_close_failed",
			"component", "daemon",
			"provider", session.ProviderCodex,
			"session", lease.sessionName,
			"session_id", lease.sessionID,
			"err", err,
		)
		return status.Errorf(codes.Internal, "close codex live runtime: %v", err)
	}
	s.releaseCodexLiveSession(ctx, live, "codex.live.foreground_suspended")
	return nil
}

func (s *Server) restoreCodexLiveAfterForeground(ctx context.Context, lease *foregroundLease) (*clydev1.LiveSession, error) {
	_, _ = peer.FromContext(ctx)
	runtime := newCodexLiveRuntime(codex.LiveRuntimeOptions{
		CodexBin:       "",
		Command:        nil,
		WorkDir:        lease.basedir,
		Env:            nil,
		ClientName:     "",
		ClientTitle:    "",
		ClientVersion:  "",
		Experimental:   false,
		ConfigOverride: nil,
	})
	attached, err := runtime.Attach(ctx, codex.LiveAttachRequest{ThreadID: lease.sessionID})
	if err != nil {
		if closeErr := runtime.Close(); closeErr != nil {
			s.log.ErrorContext(ctx, "daemon.foreground_session.codex_restore_close_failed",
				"component", "daemon",
				"provider", session.ProviderCodex,
				"session", lease.sessionName,
				"session_id", lease.sessionID,
				"err", closeErr,
			)
		}
		s.log.WarnContext(ctx, "daemon.foreground_session.codex_restore_attach_failed",
			"component", "daemon",
			"session", lease.sessionName,
			"session_id", lease.sessionID,
			"err", err,
		)
		return nil, fmt.Errorf("attach codex live runtime: %w", err)
	}
	record := &liveRuntimeSession{
		provider:         session.ProviderCodex,
		name:             firstNonEmpty(lease.sessionName, attached.ThreadID),
		id:               attached.ThreadID,
		basedir:          firstNonEmpty(attached.WorkDir, lease.basedir),
		model:            firstNonEmpty(attached.Model, lease.model),
		status:           firstNonEmpty(lease.status, "attached"),
		startedAt:        daemonNow(),
		lastTurnID:       "",
		codexRuntime:     runtime,
		livetrackSession: nil,
	}
	s.remoteMu.Lock()
	s.liveSessions[record.id] = record
	s.remoteMu.Unlock()
	setLiveRuntimeState(record.id, &liveRuntimeSessionState{
		effort: liveRuntimeState(lease.sessionID).effort,
		stream: nil,
	})
	s.registerCodexLiveSession(ctx, record, runtime)
	return protoLiveSessionFromRecord(record), nil
}

func (s *Server) suspendClaudeRemoteForForeground(ctx context.Context, lease *foregroundLease) error {
	if lease == nil {
		return nil
	}
	s.remoteMu.Lock()
	worker := s.remoteWorkers[remoteWorkerKey(lease.sessionName, lease.sessionID)]
	if worker == nil {
		for _, candidate := range s.remoteWorkers {
			if candidate != nil && candidate.sessionID == lease.sessionID {
				worker = candidate
				break
			}
		}
	}
	if worker != nil {
		worker.skipCleanup.Store(true)
		removeRemoteWorkerLocked(s.remoteWorkers, worker)
		lease.shouldRestore = true
		lease.restoreReason = "claude_remote_worker"
		lease.incognito = worker.incognito
	}
	s.remoteMu.Unlock()
	if worker == nil || worker.cmd == nil || worker.cmd.Process == nil {
		return s.suspendClaudeRemoteByInjectSocket(ctx, lease)
	}
	if err := worker.cmd.Process.Signal(os.Interrupt); err != nil {
		if killErr := worker.cmd.Process.Kill(); killErr != nil {
			return status.Errorf(codes.Internal, "stop claude remote worker: %v", killErr)
		}
		return nil
	}
	if worker.done == nil {
		return nil
	}
	select {
	case <-worker.done:
		return nil
	case <-time.After(2 * time.Second):
		if err := worker.cmd.Process.Kill(); err != nil {
			return status.Errorf(codes.Internal, "kill claude remote worker after interrupt timeout: %v", err)
		}
		select {
		case <-worker.done:
		case <-time.After(1 * time.Second):
		}
	}
	return nil
}

func (s *Server) suspendClaudeRemoteByInjectSocket(ctx context.Context, lease *foregroundLease) error {
	if lease == nil || !injectSocketExists(lease.sessionID) {
		return nil
	}
	lease.shouldRestore = true
	lease.restoreReason = "claude_remote_socket"
	if err := writeInjectSocket(ctx, lease.sessionID, []byte{0x03}); err != nil {
		s.log.ErrorContext(ctx, "daemon.foreground_session.claude_socket_suspend_failed",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"session", lease.sessionName,
			"session_id", lease.sessionID,
			"err", err,
		)
		return status.Errorf(codes.FailedPrecondition, "suspend claude remote worker through inject socket: %v", err)
	}
	released := false
	deadline := daemonNow().Add(2 * time.Second)
	for daemonNow().Before(deadline) {
		if !injectSocketExists(lease.sessionID) {
			released = true
			break
		}
		timer := time.NewTimer(25 * time.Millisecond)
		<-timer.C
	}
	if released {
		s.log.InfoContext(ctx, "daemon.foreground_session.claude_socket_suspended",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"session", lease.sessionName,
			"session_id", lease.sessionID,
		)
		return nil
	}
	return status.Errorf(codes.FailedPrecondition, "claude remote worker for %q did not release inject socket", lease.sessionID)
}

func (s *Server) restoreClaudeRemoteAfterForeground(ctx context.Context, lease *foregroundLease) (*clydev1.LiveSession, error) {
	_, _ = peer.FromContext(ctx)

	basedir := strings.TrimSpace(lease.basedir)
	if basedir == "" {
		return nil, fmt.Errorf("missing basedir for claude remote restore")
	}
	sessionName := strings.TrimSpace(lease.sessionName)
	if target, resolveErr := resolveSessionRuntime(sessionName, string(session.ProviderClaude), lease.sessionID); resolveErr == nil && target.Session != nil {
		sessionName = target.Session.Name
	}
	sessionName = firstNonEmpty(sessionName, lease.sessionID)
	cmd, err := s.startRemoteWorkerProcess(ctx, sessionName, lease.sessionID, basedir, lease.incognito)
	if err != nil {
		return nil, fmt.Errorf("launch claude remote worker: %w", err)
	}
	worker := &remoteWorker{
		sessionName:      sessionName,
		sessionID:        lease.sessionID,
		basedir:          basedir,
		incognito:        lease.incognito,
		cmd:              cmd,
		done:             make(chan struct{}),
		skipCleanup:      atomic.Bool{},
		livetrackSession: nil,
	}
	s.remoteMu.Lock()
	s.remoteWorkers[remoteWorkerKey(worker.sessionName, worker.sessionID)] = worker
	s.remoteMu.Unlock()
	workerCtx := daemonDetachedCorrelationContext(ctx, s.log)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WarnContext(workerCtx, "daemon.remote_session.wait_panicked",
					"component", "daemon",
					"session", worker.sessionName,
					"session_id", worker.sessionID,
					"panic", r,
				)
			}
		}()
		s.waitRemoteWorker(workerCtx, worker)
	}()
	return &clydev1.LiveSession{
		Provider:       string(session.ProviderClaude),
		SessionName:    sessionName,
		SessionId:      lease.sessionID,
		Status:         "launching",
		Basedir:        basedir,
		SupportsSend:   true,
		SupportsStream: true,
		SupportsStop:   false,
	}, nil
}

func protoClaudeLiveSessionFromWorker(worker *remoteWorker, status, url string) *clydev1.LiveSession {
	if worker == nil {
		return nil
	}
	return &clydev1.LiveSession{
		Provider:       string(session.ProviderClaude),
		SessionName:    worker.sessionName,
		SessionId:      worker.sessionID,
		Status:         status,
		Basedir:        worker.basedir,
		Url:            url,
		SupportsSend:   true,
		SupportsStream: true,
		SupportsStop:   false,
	}
}

func (s *Server) discoverClaudeLiveSessions(seen map[string]bool) []*clydev1.LiveSession {
	store, err := session.NewGlobalFileStore()
	if err != nil {
		s.log.Warn("daemon.live_session.claude_discovery_store_failed",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"err", err,
		)
		return nil
	}
	sessions, err := store.List()
	if err != nil {
		s.log.Warn("daemon.live_session.claude_discovery_list_failed",
			"component", "daemon",
			"provider", session.ProviderClaude,
			"err", err,
		)
		return nil
	}
	out := make([]*clydev1.LiveSession, 0)
	for _, sess := range sessions {
		if sess == nil || sess.ProviderID() != session.ProviderClaude {
			continue
		}
		sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
		if sessionID == "" || seen[sessionID] {
			continue
		}
		url := s.bridgeURLForSession(sessionID)
		if !injectSocketExists(sessionID) && url == "" {
			continue
		}
		out = append(out, &clydev1.LiveSession{
			Provider:       string(session.ProviderClaude),
			SessionName:    sess.Name,
			SessionId:      sessionID,
			Status:         "reattachable",
			Basedir:        liveSessionStoredBasedir(sess),
			Url:            url,
			SupportsSend:   true,
			SupportsStream: true,
			SupportsStop:   false,
		})
	}
	return out
}

func (s *Server) bridgeURLForSession(sessionID string) string {
	s.bridgeMu.RLock()
	defer s.bridgeMu.RUnlock()
	bridge := s.bridges[sessionID]
	if bridge == nil {
		return ""
	}
	return bridge.GetUrl()
}

func injectSocketExists(sessionID string) bool {
	if strings.TrimSpace(sessionID) == "" {
		return false
	}
	info, err := os.Stat(injectSocketPath(sessionID))
	return err == nil && !info.IsDir()
}

func writeInjectSocket(ctx context.Context, sessionID string, payload []byte) error {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", injectSocketPath(sessionID))
	if err != nil {
		return fmt.Errorf("dial claude inject socket: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("write claude inject socket: %w", err)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (s *Server) sendCodexLiveSession(ctx context.Context, live *liveRuntimeSession, text string) (*clydev1.SendLiveSessionResponse, error) {
	turn, err := live.codexRuntime.Send(ctx, codex.LiveSendRequest{
		ThreadID: live.id,
		Text:     text,
		WorkDir:  live.basedir,
		Model:    live.model,
		Effort:   liveRuntimeState(live.id).effort,
	})
	if err != nil {
		s.log.ErrorContext(ctx, "daemon.live_session.codex_send_failed",
			"component", "daemon",
			"provider", session.ProviderCodex,
			"session", live.name,
			"session_id", live.id,
			"model", live.model,
			"err", err,
		)
		return nil, status.Errorf(codes.Internal, "send codex live turn: %v", err)
	}
	fanout := newLiveStreamFanout()
	s.remoteMu.Lock()
	state := liveRuntimeState(live.id)
	if state.stream != nil {
		state.stream.close()
	}
	live.lastTurnID = turn.TurnID
	live.status = string(turn.Status)
	state.stream = fanout
	s.remoteMu.Unlock()
	s.startCodexLiveStreamPump(daemonDetachedCorrelationContext(ctx, s.log), live, turn.TurnID, fanout)
	return &clydev1.SendLiveSessionResponse{Accepted: true}, nil
}

func (s *Server) streamLiveSessionEvents(ctx context.Context, sessionID string) (<-chan webapp.LiveSessionEvent, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w", status.Error(codes.InvalidArgument, "session_id is required"))
	}
	if live := s.liveSessionByID(sessionID); live != nil {
		if live.provider != session.ProviderCodex {
			return nil, status.Errorf(codes.FailedPrecondition, "live provider %q does not support daemon stream", live.provider)
		}
		return s.streamCodexLiveSessionEvents(ctx, live)
	}
	target, err := resolveSessionRuntime("", "", sessionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve session runtime: %v", err)
	}
	if target.Provider == session.ProviderClaude {
		return s.streamClaudeLiveSessionEvents(ctx, target)
	}
	live, err := s.liveSessionRecord(ctx, sessionID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "live session %q: %v", sessionID, err)
	}
	return s.streamCodexLiveSessionEvents(ctx, live)
}

func (s *Server) streamCodexLiveSessionEvents(ctx context.Context, live *liveRuntimeSession) (<-chan webapp.LiveSessionEvent, error) {
	_, _ = peer.FromContext(ctx)
	s.remoteMu.Lock()
	state := liveRuntimeState(live.id)
	fanout := state.stream
	lastTurnID := live.lastTurnID
	s.remoteMu.Unlock()
	if lastTurnID == "" || fanout == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "live session %q has no active turn", live.id)
	}
	events, cleanup := fanout.subscribe()
	out := make(chan webapp.LiveSessionEvent, 64)
	go func() {
		defer cleanup()
		defer close(out)
		for {
			select {
			case event, ok := <-events:
				if !ok {
					return
				}
				out <- event
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func (s *Server) startCodexLiveStreamPump(ctx context.Context, live *liveRuntimeSession, turnID string, fanout *liveStreamFanout) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WarnContext(ctx, "daemon.live_session.codex_pump_panicked",
					"component", "daemon",
					"session_id", live.id,
					"turn_id", turnID,
					"panic", r,
				)
			}
			fanout.close()
		}()
		events, err := live.codexRuntime.Stream(ctx, codex.LiveStreamRequest{
			ThreadID: live.id,
			TurnID:   turnID,
		})
		if err != nil {
			s.log.ErrorContext(ctx, "daemon.live_session.codex_stream_start_failed",
				"component", "daemon",
				"provider", session.ProviderCodex,
				"session", live.name,
				"session_id", live.id,
				"turn_id", turnID,
				"err", err,
			)
			fanout.publish(webapp.LiveSessionEvent{
				SessionID: live.id,
				Kind:      "error",
				Role:      "",
				Text:      err.Error(),
				Timestamp: daemonNow(),
			})
			return
		}
		for event := range events {
			if event.Err != nil {
				s.log.ErrorContext(ctx, "daemon.live_session.codex_stream_event_failed",
					"component", "daemon",
					"provider", session.ProviderCodex,
					"session", live.name,
					"session_id", live.id,
					"turn_id", turnID,
					"err", event.Err,
				)
				fanout.publish(webapp.LiveSessionEvent{
					SessionID: live.id,
					Kind:      "error",
					Role:      "",
					Text:      event.Err.Error(),
					Timestamp: daemonNow(),
				})
				return
			}
			role := "assistant"
			text := event.Delta
			if event.Kind == codex.LiveEventCompleted {
				role = ""
				text = string(event.Status)
			}
			fanout.publish(webapp.LiveSessionEvent{
				SessionID: live.id,
				Kind:      string(event.Kind),
				Role:      role,
				Text:      text,
				Timestamp: daemonNow(),
			})
		}
	}()
}

func (s *Server) streamClaudeLiveSessionEvents(ctx context.Context, target resolvedSessionRuntime) (<-chan webapp.LiveSessionEvent, error) {
	_, _ = peer.FromContext(ctx)
	if target.Session == nil {
		return nil, status.Errorf(codes.NotFound, "no session runtime for %q", target.SessionID)
	}
	out := make(chan webapp.LiveSessionEvent, 32)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.WarnContext(ctx, "daemon.live_session.claude_stream_panicked",
					"component", "daemon",
					"session_id", target.SessionID,
					"panic", r,
				)
			}
		}()
		defer close(out)
		historyTarget, ok := waitForClaudeLiveSessionHistory(ctx, target, out)
		if !ok {
			return
		}
		ch, cleanup, err := s.transcripts.SubscribeContext(ctx, historyTarget.SessionID, historyTarget.HistoryArtifact, -1)
		if err != nil {
			s.log.ErrorContext(ctx, "daemon.live_session.claude_stream_open_failed",
				"component", "daemon",
				"provider", session.ProviderClaude,
				"session_id", target.SessionID,
				"path", historyTarget.HistoryArtifact,
				"err", err,
			)
			out <- webapp.LiveSessionEvent{
				SessionID: target.SessionID,
				Kind:      "error",
				Role:      "",
				Text:      "open tailer: " + err.Error(),
				Timestamp: daemonNow(),
			}
			return
		}
		defer cleanup()
		for {
			select {
			case line, ok := <-ch:
				if !ok {
					return
				}
				out <- webapp.LiveSessionEvent{
					SessionID: target.SessionID,
					Kind:      "message",
					Role:      line.GetRole(),
					Text:      line.GetText(),
					Timestamp: daemonTimeFromNanos(line.GetTimestampNanos()),
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func waitForClaudeLiveSessionHistory(ctx context.Context, target resolvedSessionRuntime, out chan<- webapp.LiveSessionEvent) (resolvedSessionRuntime, bool) {
	if target.HistoryArtifact != "" {
		return target, true
	}
	out <- webapp.LiveSessionEvent{
		SessionID: target.SessionID,
		Kind:      "status",
		Role:      "",
		Text:      "waiting for transcript",
		Timestamp: daemonNow(),
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	sessionName := target.Session.Name
	for {
		select {
		case <-ticker.C:
			next, err := resolveSessionRuntime(sessionName, string(session.ProviderClaude), target.SessionID)
			if err == nil && next.HistoryArtifact != "" {
				return next, true
			}
		case <-deadline.C:
			out <- webapp.LiveSessionEvent{
				SessionID: target.SessionID,
				Kind:      "error",
				Role:      "",
				Text:      "no history artifact for session " + target.SessionID,
				Timestamp: daemonNow(),
			}
			return target, false
		case <-ctx.Done():
			return target, false
		}
	}
}

func (s *Server) liveSessionByID(sessionID string) *liveRuntimeSession {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	return s.liveSessions[sessionID]
}

func protoLiveSessionFromRecord(live *liveRuntimeSession) *clydev1.LiveSession {
	if live == nil {
		return nil
	}
	return &clydev1.LiveSession{
		Provider:       string(live.provider),
		SessionName:    live.name,
		SessionId:      live.id,
		Status:         live.status,
		Basedir:        live.basedir,
		StartedAtNanos: live.startedAt.UnixNano(),
		SupportsSend:   true,
		SupportsStream: true,
		SupportsStop:   live.provider == session.ProviderCodex,
	}
}

func protoStreamLiveSessionEvent(event webapp.LiveSessionEvent) *clydev1.StreamLiveSessionResponse {
	return &clydev1.StreamLiveSessionResponse{
		SessionId:      event.SessionID,
		Kind:           event.Kind,
		Role:           event.Role,
		Text:           event.Text,
		TimestampNanos: event.Timestamp.UnixNano(),
	}
}

func daemonTimeFromNanos(nanos int64) time.Time {
	if nanos <= 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}
