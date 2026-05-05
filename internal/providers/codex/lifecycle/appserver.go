package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

// AppServerRuntimeOptions configures the Codex app-server process.
type AppServerRuntimeOptions struct {
	CodexBin       string
	Command        []string
	WorkDir        string
	Env            []string
	ClientName     string
	ClientTitle    string
	ClientVersion  string
	Experimental   bool
	ConfigOverride []string
}

// AppServerRuntime drives Codex through the app-server stdio protocol.
type AppServerRuntime struct {
	opts AppServerRuntimeOptions

	processMu sync.Mutex
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	stderr    *appServerStderrTail

	writeMu sync.Mutex

	stateMu      sync.Mutex
	nextID       uint64
	initialized  bool
	streamActive bool
	closed       bool
}

// NewAppServerRuntime creates a Codex app-server runtime with defaults.
func NewAppServerRuntime(opts AppServerRuntimeOptions) *AppServerRuntime {
	return &AppServerRuntime{
		opts:         opts.withDefaults(),
		processMu:    sync.Mutex{},
		cmd:          nil,
		stdin:        nil,
		stdout:       nil,
		stderr:       newAppServerStderrTail(80),
		writeMu:      sync.Mutex{},
		stateMu:      sync.Mutex{},
		nextID:       0,
		initialized:  false,
		streamActive: false,
		closed:       false,
	}
}

// Start creates a new Codex app-server thread.
func (r *AppServerRuntime) Start(ctx context.Context, req LiveStartRequest) (*LiveSession, error) {
	if err := validateLiveStartRequest(req); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.start_invalid",
			"component", "codex",
			"work_dir", req.WorkDir,
			"model", req.Model,
			"err", err,
		)
		return nil, err
	}
	if err := r.ensureReady(ctx); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.start_not_ready",
			"component", "codex",
			"work_dir", req.WorkDir,
			"model", req.Model,
			"err", err,
		)
		return nil, err
	}
	params := codexThreadStartParams{
		CWD:                   req.WorkDir,
		Model:                 req.Model,
		ModelProvider:         req.ModelProvider,
		ApprovalPolicy:        "",
		DeveloperInstructions: req.DeveloperInstructions,
		BaseInstructions:      req.BaseInstructions,
		Ephemeral:             req.Ephemeral,
		SessionStartSource:    "",
	}
	resp, err := requestAppServer[codexThreadStartResponse](ctx, r, "thread/start", params)
	if err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.thread_start_failed",
			"component", "codex",
			"work_dir", req.WorkDir,
			"model", req.Model,
			"err", err,
		)
		return nil, err
	}
	if req.SessionName != "" {
		_ = requestAppServerNoResult(ctx, r, "thread/name/set", codexThreadSetNameParams{
			ThreadID: resp.Thread.ID,
			Name:     req.SessionName,
		})
	}
	return &LiveSession{
		ThreadID: resp.Thread.ID,
		WorkDir:  resp.CWD,
		Model:    resp.Model,
	}, nil
}

// Attach resumes an existing Codex app-server thread.
func (r *AppServerRuntime) Attach(ctx context.Context, req LiveAttachRequest) (*LiveSession, error) {
	if err := validateLiveAttachRequest(req); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.attach_invalid",
			"component", "codex",
			"thread_id", req.ThreadID,
			"err", err,
		)
		return nil, err
	}
	if err := r.ensureReady(ctx); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.attach_not_ready",
			"component", "codex",
			"thread_id", req.ThreadID,
			"err", err,
		)
		return nil, err
	}
	resp, err := requestAppServer[codexThreadResumeResponse](ctx, r, "thread/resume", codexThreadResumeParams(req))
	if err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.thread_resume_failed",
			"component", "codex",
			"thread_id", req.ThreadID,
			"err", err,
		)
		return nil, err
	}
	return &LiveSession{
		ThreadID: resp.Thread.ID,
		WorkDir:  resp.CWD,
		Model:    resp.Model,
	}, nil
}

// Send starts a user turn on an existing Codex app-server thread.
func (r *AppServerRuntime) Send(ctx context.Context, req LiveSendRequest) (*LiveTurn, error) {
	if err := validateLiveSendRequest(req); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.send_invalid",
			"component", "codex",
			"thread_id", req.ThreadID,
			"model", req.Model,
			"effort", req.Effort,
			"err", err,
		)
		return nil, err
	}
	if err := r.ensureReady(ctx); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.send_not_ready",
			"component", "codex",
			"thread_id", req.ThreadID,
			"model", req.Model,
			"effort", req.Effort,
			"err", err,
		)
		return nil, err
	}
	resp, err := requestAppServer[codexTurnStartResponse](ctx, r, "turn/start", codexTurnStartParams(adaptercodex.RPCTurnStartParams{
		ThreadID: req.ThreadID,
		Input: []adaptercodex.RPCTurnInputItem{
			{
				Type:         "text",
				Text:         req.Text,
				TextElements: []adaptercodex.RPCTextElement{},
			},
		},
		CWD:            req.WorkDir,
		ApprovalPolicy: "",
		Model:          req.Model,
		Effort:         req.Effort,
		Summary:        "",
	}))
	if err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.turn_start_failed",
			"component", "codex",
			"thread_id", req.ThreadID,
			"model", req.Model,
			"effort", req.Effort,
			"err", err,
		)
		return nil, err
	}
	return &LiveTurn{
		ThreadID: req.ThreadID,
		TurnID:   resp.Turn.ID,
		Status:   LiveTurnStatus(resp.Turn.Status),
	}, nil
}

// Stream subscribes to notifications for a Codex live turn.
func (r *AppServerRuntime) Stream(ctx context.Context, req LiveStreamRequest) (<-chan LiveEvent, error) {
	if err := validateLiveStreamRequest(req); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.stream_invalid",
			"component", "codex",
			"thread_id", req.ThreadID,
			"turn_id", req.TurnID,
			"err", err,
		)
		return nil, err
	}
	if err := r.ensureReady(ctx); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.stream_not_ready",
			"component", "codex",
			"thread_id", req.ThreadID,
			"turn_id", req.TurnID,
			"err", err,
		)
		return nil, err
	}
	if err := r.acquireStream(); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.stream_acquire_failed",
			"component", "codex",
			"thread_id", req.ThreadID,
			"turn_id", req.TurnID,
			"err", err,
		)
		return nil, err
	}
	events := make(chan LiveEvent, 32)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				codexLifecycleLog.Logger().Error("codex.appserver.stream_panic",
					"component", "codex",
					"thread_id", req.ThreadID,
					"turn_id", req.TurnID,
					"panic", recovered,
				)
			}
		}()
		defer close(events)
		defer r.releaseStream()
		for {
			msg, err := r.readMessage(ctx)
			if err != nil {
				codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.stream_read_failed",
					"component", "codex",
					"thread_id", req.ThreadID,
					"turn_id", req.TurnID,
					"err", err,
				)
				events <- LiveEvent{
					Kind:     LiveEventError,
					ThreadID: req.ThreadID,
					TurnID:   req.TurnID,
					ItemID:   "",
					Delta:    "",
					Status:   "",
					Err:      err,
				}
				return
			}
			if msg.Method == "" {
				continue
			}
			if msg.ID != nil {
				r.respondToServerRequest(ctx, msg)
				continue
			}
			event, matched, done := liveEventFromNotification(msg.Method, msg.Params, req)
			if matched {
				events <- event
			}
			if done {
				return
			}
		}
	}()
	return events, nil
}

// Stop interrupts a running Codex live turn.
func (r *AppServerRuntime) Stop(ctx context.Context, req LiveStopRequest) error {
	if err := validateLiveStopRequest(req); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.stop_invalid",
			"component", "codex",
			"thread_id", req.ThreadID,
			"turn_id", req.TurnID,
			"err", err,
		)
		return err
	}
	if err := r.ensureReady(ctx); err != nil {
		codexLifecycleLog.Logger().ErrorContext(ctx, "codex.appserver.stop_not_ready",
			"component", "codex",
			"thread_id", req.ThreadID,
			"turn_id", req.TurnID,
			"err", err,
		)
		return err
	}
	if r.hasActiveStream() {
		return r.writeRequest(ctx, "turn/interrupt", codexTurnInterruptParams(req))
	}
	return requestAppServerNoResult(ctx, r, "turn/interrupt", codexTurnInterruptParams(req))
}

// Close stops the Codex app-server process owned by this runtime.
func (r *AppServerRuntime) Close() error {
	r.processMu.Lock()
	defer r.processMu.Unlock()
	r.stateMu.Lock()
	r.closed = true
	r.initialized = false
	r.streamActive = false
	r.stateMu.Unlock()
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
	if r.cmd == nil || r.cmd.Process == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				codexLifecycleLog.Logger().Error("codex.appserver.wait_panic",
					"component", "codex",
					"panic", recovered,
				)
			}
		}()
		done <- r.cmd.Wait()
	}()
	_ = r.cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
		return nil
	case <-time.After(2 * time.Second):
		_ = r.cmd.Process.Kill()
		<-done
		return nil
	}
}

func (r *AppServerRuntime) ensureReady(ctx context.Context) error {
	r.processMu.Lock()
	defer r.processMu.Unlock()
	r.stateMu.Lock()
	closed := r.closed
	initialized := r.initialized
	r.stateMu.Unlock()
	if closed {
		return fmt.Errorf("codex app-server runtime is closed")
	}
	if r.cmd == nil {
		if err := r.startProcess(ctx); err != nil {
			return err
		}
	}
	if initialized {
		return nil
	}
	if _, err := requestAppServer[codexInitializeResponse](ctx, r, "initialize", codexInitializeParams{
		ClientInfo: codexClientInfo{
			Name:    r.opts.ClientName,
			Title:   r.opts.ClientTitle,
			Version: r.opts.ClientVersion,
		},
		Capabilities: codexInitializeCapabilities{
			ExperimentalAPI: r.opts.Experimental,
		},
	}); err != nil {
		return err
	}
	if err := r.writeNotification(ctx, "initialized"); err != nil {
		return err
	}
	r.stateMu.Lock()
	r.initialized = true
	r.stateMu.Unlock()
	return nil
}

func (r *AppServerRuntime) startProcess(ctx context.Context) error {
	name, args := r.command()
	cmd := exec.CommandContext(ctx, name, args...)
	if r.opts.WorkDir != "" {
		cmd.Dir = r.opts.WorkDir
	}
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, r.opts.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open codex app-server stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open codex app-server stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start codex app-server: %w", err)
	}
	r.cmd = cmd
	r.stdin = stdin
	r.stdout = bufio.NewReader(stdout)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				codexLifecycleLog.Logger().Error("codex.appserver.stderr_drain_panic",
					"component", "codex",
					"panic", recovered,
				)
			}
		}()
		r.stderr.drain(stderr)
	}()
	codexLifecycleLog.Logger().Info("codex.appserver.started",
		"component", "codex",
		"args_count", len(args),
		"work_dir", r.opts.WorkDir,
	)
	return nil
}

func (r *AppServerRuntime) command() (string, []string) {
	if len(r.opts.Command) > 0 {
		return r.opts.Command[0], append([]string{}, r.opts.Command[1:]...)
	}
	bin := strings.TrimSpace(r.opts.CodexBin)
	if bin == "" {
		bin = BinaryPathFunc()
	}
	args := make([]string, 0, 3+len(r.opts.ConfigOverride)*2)
	for _, override := range r.opts.ConfigOverride {
		args = append(args, "--config", override)
	}
	args = append(args, "app-server", "--listen", "stdio://")
	return bin, args
}

func (r *AppServerRuntime) acquireStream() error {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.streamActive {
		return fmt.Errorf("codex live stream already active")
	}
	r.streamActive = true
	return nil
}

func (r *AppServerRuntime) releaseStream() {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.streamActive = false
}

func (r *AppServerRuntime) hasActiveStream() bool {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	return r.streamActive
}

func (r *AppServerRuntime) writeRequest(ctx context.Context, method string, params codexAppServerParams) error {
	id := r.nextRequestID()
	return r.writeMessage(ctx, codexRequestEnvelope{
		ID:     id,
		Method: method,
		Params: params,
	})
}

func (r *AppServerRuntime) writeNotification(ctx context.Context, method string) error {
	return r.writeMessage(ctx, codexNotificationEnvelope{Method: method})
}

func (r *AppServerRuntime) writeMessage(ctx context.Context, payload codexOutboundMessage) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("write codex app-server message: %w", ctx.Err())
	default:
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.stdin == nil {
		return fmt.Errorf("codex app-server stdin is unavailable")
	}
	if err := json.NewEncoder(r.stdin).Encode(payload); err != nil {
		slog.WarnContext(ctx, "codex.appserver.write_failed",
			"component", "codex",
			"err", err,
		)
		return fmt.Errorf("write codex app-server message: %w", err)
	}
	return nil
}

func (r *AppServerRuntime) readMessage(ctx context.Context) (codexInboundMessage, error) {
	select {
	case <-ctx.Done():
		return codexInboundMessage{}, fmt.Errorf("read codex app-server message: %w", ctx.Err())
	default:
	}
	if r.stdout == nil {
		return codexInboundMessage{}, fmt.Errorf("codex app-server stdout is unavailable")
	}
	line, err := r.stdout.ReadBytes('\n')
	if err != nil {
		tail := r.stderr.tail()
		if tail != "" {
			slog.WarnContext(ctx, "codex.appserver.read_failed",
				"component", "codex",
				"stderr_tail", tail,
				"err", err,
			)
			return codexInboundMessage{}, fmt.Errorf("read codex app-server message: %w; stderr_tail=%s", err, tail)
		}
		slog.WarnContext(ctx, "codex.appserver.read_failed",
			"component", "codex",
			"err", err,
		)
		return codexInboundMessage{}, fmt.Errorf("read codex app-server message: %w", err)
	}
	var msg codexInboundMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		slog.WarnContext(ctx, "codex.appserver.decode_failed",
			"component", "codex",
			"err", err,
		)
		return codexInboundMessage{}, fmt.Errorf("decode codex app-server message: %w", err)
	}
	return msg, nil
}

func (r *AppServerRuntime) nextRequestID() string {
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	r.nextID++
	return fmt.Sprintf("clyde-%d", r.nextID)
}

func (r *AppServerRuntime) respondToServerRequest(ctx context.Context, msg codexInboundMessage) {
	if msg.ID == nil {
		return
	}
	resp := codexServerRequestResponseEnvelope{ID: *msg.ID, Result: nil}
	switch msg.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		resp.Result = codexApprovalResult{Decision: "accept"}
	default:
		resp.Result = codexUnsupportedServerRequestResult{Ignored: true}
	}
	_ = r.writeMessage(ctx, resp)
}

func requestAppServer[T codexAppServerResponse](ctx context.Context, r *AppServerRuntime, method string, params codexAppServerParams) (T, error) {
	var zero T
	id := r.nextRequestID()
	if err := r.writeMessage(ctx, codexRequestEnvelope{
		ID:     id,
		Method: method,
		Params: params,
	}); err != nil {
		return zero, err
	}
	for {
		msg, err := r.readMessage(ctx)
		if err != nil {
			return zero, err
		}
		if msg.Method != "" && msg.ID != nil {
			r.respondToServerRequest(ctx, msg)
			continue
		}
		if msg.Method != "" {
			continue
		}
		if msg.ID == nil || *msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return zero, msg.Error.asError(method)
		}
		var result T
		if err := json.Unmarshal(msg.Result, &result); err != nil {
			slog.WarnContext(ctx, "codex.appserver.result_decode_failed",
				"component", "codex",
				"method", method,
				"err", err,
			)
			return zero, fmt.Errorf("decode %s result: %w", method, err)
		}
		return result, nil
	}
}

func requestAppServerNoResult(ctx context.Context, r *AppServerRuntime, method string, params codexAppServerParams) error {
	_, err := requestAppServer[codexNoResult](ctx, r, method, params)
	return err
}

func liveEventFromNotification(method string, raw json.RawMessage, req LiveStreamRequest) (LiveEvent, bool, bool) {
	switch method {
	case "item/agentMessage/delta":
		var notif codexAgentMessageDeltaNotification
		if err := json.Unmarshal(raw, &notif); err != nil {
			return LiveEvent{
				Kind:     LiveEventError,
				ThreadID: req.ThreadID,
				TurnID:   req.TurnID,
				ItemID:   "",
				Delta:    "",
				Status:   "",
				Err:      err,
			}, true, false
		}
		if notif.ThreadID != req.ThreadID || notif.TurnID != req.TurnID {
			return LiveEvent{
				Kind:     "",
				ThreadID: "",
				TurnID:   "",
				ItemID:   "",
				Delta:    "",
				Status:   "",
				Err:      nil,
			}, false, false
		}
		return LiveEvent{
			Kind:     LiveEventDelta,
			ThreadID: notif.ThreadID,
			TurnID:   notif.TurnID,
			ItemID:   notif.ItemID,
			Delta:    notif.Delta,
			Status:   "",
			Err:      nil,
		}, true, false
	case "turn/completed":
		var notif codexTurnCompletedNotification
		if err := json.Unmarshal(raw, &notif); err != nil {
			return LiveEvent{
				Kind:     LiveEventError,
				ThreadID: req.ThreadID,
				TurnID:   req.TurnID,
				ItemID:   "",
				Delta:    "",
				Status:   "",
				Err:      err,
			}, true, true
		}
		if notif.ThreadID != req.ThreadID || notif.Turn.ID != req.TurnID {
			return LiveEvent{
				Kind:     "",
				ThreadID: "",
				TurnID:   "",
				ItemID:   "",
				Delta:    "",
				Status:   "",
				Err:      nil,
			}, false, false
		}
		return LiveEvent{
			Kind:     LiveEventCompleted,
			ThreadID: notif.ThreadID,
			TurnID:   notif.Turn.ID,
			ItemID:   "",
			Delta:    "",
			Status:   LiveTurnStatus(notif.Turn.Status),
			Err:      nil,
		}, true, true
	default:
		return LiveEvent{
			Kind:     "",
			ThreadID: "",
			TurnID:   "",
			ItemID:   "",
			Delta:    "",
			Status:   "",
			Err:      nil,
		}, false, false
	}
}

func (opts AppServerRuntimeOptions) withDefaults() AppServerRuntimeOptions {
	if opts.ClientName == "" {
		opts.ClientName = "clyde"
	}
	if opts.ClientTitle == "" {
		opts.ClientTitle = "Clyde"
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "0.1.0"
	}
	if !opts.Experimental {
		opts.Experimental = true
	}
	return opts
}

type appServerStderrTail struct {
	mu    sync.Mutex
	limit int
	lines []string
}

func newAppServerStderrTail(limit int) *appServerStderrTail {
	return &appServerStderrTail{
		mu:    sync.Mutex{},
		limit: limit,
		lines: nil,
	}
}

func (t *appServerStderrTail) drain(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		t.add(scanner.Text())
	}
}

func (t *appServerStderrTail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > t.limit {
		t.lines = append([]string{}, t.lines[len(t.lines)-t.limit:]...)
	}
}

func (t *appServerStderrTail) tail() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}
