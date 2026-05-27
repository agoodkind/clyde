package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	clydev1 "goodkind.io/clyde/api/clyde/v1"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/gklog"
)

func daemonClientLog(ctx context.Context) *slog.Logger {
	if ctx == nil {
		ctx = context.Background()
	}
	return gklog.LoggerFromContext(ctx).With("component", "daemon", "subcomponent", "client")
}

// Client is a gRPC client for the clyde daemon.
type Client struct {
	conn *grpc.ClientConn
	rpc  clydev1.ClydeServiceClient
}

// LifecycleOutcome normalizes daemon lifecycle state for callers that need to
// decide between online success, offline degradation, and hard failure.
type LifecycleOutcome string

const (
	LifecycleOutcomeReady           LifecycleOutcome = "ready"
	LifecycleOutcomeDegradedOffline LifecycleOutcome = "degraded_offline"
	LifecycleOutcomeFailed          LifecycleOutcome = "failed"
)

// NudgeDiscoveryScan asks the daemon to run an immediate discovery
// scan. The preferred path is the TriggerScan RPC because it carries
// no privileges and works regardless of how the daemon was launched.
// SIGUSR1 is the fallback for callers that already have a PID and
// cannot or do not want to open a gRPC connection.
func NudgeDiscoveryScan() {
	bg := context.Background()
	log := daemonClientLog(bg)
	log.DebugContext(bg, "daemon.client.nudge.begin")
	c, err := ConnectOrStart(bg)
	if err == nil {
		rpcCtx, cancel := context.WithTimeout(bg, 2*time.Second)
		defer cancel()
		log.DebugContext(rpcCtx, "daemon.client.nudge.trigger_scan_rpc")
		_, _ = c.rpc.TriggerScan(rpcCtx, &clydev1.TriggerScanRequest{})
		_ = c.conn.Close()
		return
	}
	log.DebugContext(bg, "daemon.client.nudge.connect_failed_try_sigusr1", "err", err)
	pid, pidErr := findDaemonPID(bg)
	if pidErr != nil || pid <= 0 {
		log.DebugContext(bg, "daemon.client.nudge.pid_lookup_skipped",
			"pid", pid,
			"err", pidErr,
		)
		return
	}
	if sigErr := syscall.Kill(pid, syscall.SIGUSR1); sigErr != nil {
		log.DebugContext(bg, "daemon.client.nudge.sigusr1_failed", "pid", pid, "err", sigErr)
		return
	}
	log.DebugContext(bg, "daemon.client.nudge.sigusr1_sent", "pid", pid)
}

// DeleteSessionViaDaemon asks the daemon to drop a session from the
// registry. Transcript and agent log cleanup remain the caller's job
// because they touch per-project state outside the daemon's view.
func DeleteSessionViaDaemon(ctx context.Context, name string) (bool, error) {
	outcome, err := DeleteSessionViaDaemonOutcome(ctx, name)
	if outcome == LifecycleOutcomeReady {
		return true, nil
	}
	return false, err
}

// DeleteSessionViaDaemonOutcome drops the session and returns the normalized
// daemon lifecycle outcome.
func DeleteSessionViaDaemonOutcome(ctx context.Context, name string) (LifecycleOutcome, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.delete_session.begin", "name", name)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.delete_session.connect_failed", "err", err)
		return LifecycleOutcomeDegradedOffline, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = c.rpc.DeleteSession(rpcCtx, &clydev1.DeleteSessionRequest{Name: name})
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.delete_session.rpc_failed", "err", err)
		return lifecycleOutcomeForError(err), err
	}
	log.DebugContext(rpcCtx, "daemon.client.delete_session.ok")
	return LifecycleOutcomeReady, nil
}

// SubscribeRegistry opens a long-lived stream of SubscribeRegistryResponse values
// from the daemon. The returned channel closes when the stream
// terminates for any reason. Callers can stop subscribing by calling
// the returned cancel function. The channel buffers a few events so a
// slow consumer does not lose recent activity, but the daemon will
// drop events once a per-subscriber buffer fills.
func SubscribeRegistry(parent context.Context) (<-chan *clydev1.SubscribeRegistryResponse, context.CancelFunc, error) {
	log := daemonClientLog(parent)
	log.DebugContext(parent, "daemon.client.subscribe_registry.begin")
	c, err := ConnectOrStart(parent)
	if err != nil {
		log.DebugContext(parent, "daemon.client.subscribe_registry.connect_failed", "err", err)
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	stream, err := c.rpc.SubscribeRegistry(ctx, &clydev1.SubscribeRegistryRequest{})
	if err != nil {
		log.DebugContext(ctx, "daemon.client.subscribe_registry.stream_open_failed", "err", err)
		cancel()
		c.conn.Close()
		return nil, nil, err
	}
	log.DebugContext(ctx, "daemon.client.subscribe_registry.stream_open")
	out := make(chan *clydev1.SubscribeRegistryResponse, 8)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				daemonClientLog(ctx).WarnContext(ctx, "daemon.client.subscribe_registry.panicked",
					"panic", r,
				)
			}
		}()
		defer close(out)
		defer func() { _ = c.conn.Close() }()
		loopLog := daemonClientLog(ctx)
		for {
			ev, err := stream.Recv()
			if err != nil {
				loopLog.DebugContext(ctx, "daemon.client.subscribe_registry.recv_done", "err", err)
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				loopLog.DebugContext(ctx, "daemon.client.subscribe_registry.consumer_cancelled")
				return
			}
		}
	}()
	return out, cancel, nil
}

func GetProviderStats(parent context.Context) (*clydev1.GetProviderStatsResponse, error) {
	log := daemonClientLog(parent)
	log.DebugContext(parent, "daemon.client.get_provider_stats.begin")
	c, err := ConnectOrStart(parent)
	if err != nil {
		log.DebugContext(parent, "daemon.client.get_provider_stats.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	resp, err := c.rpc.GetProviderStats(ctx, &clydev1.GetProviderStatsRequest{})
	if err != nil {
		log.DebugContext(ctx, "daemon.client.get_provider_stats.rpc_failed", "err", err)
		return nil, err
	}
	return resp, nil
}

func SubscribeProviderStats(parent context.Context) (<-chan *clydev1.ProviderStatsEvent, context.CancelFunc, error) {
	log := daemonClientLog(parent)
	log.DebugContext(parent, "daemon.client.subscribe_provider_stats.begin")
	c, err := ConnectOrStart(parent)
	if err != nil {
		log.DebugContext(parent, "daemon.client.subscribe_provider_stats.connect_failed", "err", err)
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	stream, err := c.rpc.SubscribeProviderStats(ctx, &clydev1.SubscribeProviderStatsRequest{})
	if err != nil {
		log.DebugContext(ctx, "daemon.client.subscribe_provider_stats.stream_open_failed", "err", err)
		cancel()
		c.conn.Close()
		return nil, nil, err
	}
	out := make(chan *clydev1.ProviderStatsEvent, 8)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "daemon.client.subscribe_provider_stats.panicked",
					"panic", r,
				)
			}
		}()
		defer close(out)
		defer func() { _ = c.conn.Close() }()
		for {
			ev, err := stream.Recv()
			if err != nil {
				log.DebugContext(ctx, "daemon.client.subscribe_provider_stats.recv_done", "err", err)
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, cancel, nil
}

func ReloadDaemon(ctx context.Context) (*clydev1.ReloadDaemonResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.reload.begin")
	unlock, err := lockDaemonReload(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	retryCtx, cancel := context.WithTimeout(ctx, reloadClientOverallTimeout)
	defer cancel()
	for attempt := 1; ; attempt++ {
		c, err := connectOrStart(retryCtx, connectOptions{skipCompatibilityProbe: true})
		if err != nil {
			log.DebugContext(retryCtx, "daemon.client.reload.connect_failed",
				"attempt", attempt,
				"err", err,
			)
			return nil, err
		}
		rpcCtx, rpcCancel := context.WithTimeout(retryCtx, reloadClientRPCTimeout)
		resp, err := c.rpc.ReloadDaemon(rpcCtx, &clydev1.ReloadDaemonRequest{})
		rpcCancel()
		_ = c.conn.Close()
		if err == nil {
			log.DebugContext(retryCtx, "daemon.client.reload.ok",
				"attempt", attempt,
				"active_sessions", resp.GetActiveSessions(),
				"binary_reloaded", resp.GetBinaryReloaded(),
				"new_pid", resp.GetNewPid(),
			)
			return resp, nil
		}
		if status.Code(err) != codes.FailedPrecondition {
			log.DebugContext(retryCtx, "daemon.client.reload.rpc_failed",
				"attempt", attempt,
				"err", err,
			)
			return nil, err
		}
		log.DebugContext(retryCtx, "daemon.client.reload.retry_not_owner",
			"attempt", attempt,
			"err", err,
		)
		select {
		case <-retryCtx.Done():
			return nil, retryCtx.Err()
		case <-time.After(reloadClientRetryDelay):
		}
	}
}

func lockDaemonReload(ctx context.Context) (func(), error) {
	log := daemonClientLog(ctx)
	if err := config.EnsureRuntimeDir(); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(config.RuntimeDir(), "daemon.reload.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		log.WarnContext(ctx, "daemon.client.reload_lock.open_failed",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("open reload lock: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "daemon.client.reload_lock.panicked",
					"lock_path", lockPath,
					"panic", r,
				)
				select {
				case done <- fmt.Errorf("reload lock goroutine panic: %v", r):
				case <-ctx.Done():
				}
			}
		}()
		done <- syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX)
	}()
	select {
	case <-ctx.Done():
		_ = lockFile.Close()
		return nil, ctx.Err()
	case err := <-done:
		if err != nil {
			_ = lockFile.Close()
			log.WarnContext(ctx, "daemon.client.reload_lock.lock_failed",
				"lock_path", lockPath,
				"err", err,
			)
			return nil, fmt.Errorf("lock reload: %w", err)
		}
	}
	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}, nil
}

type connectOptions struct {
	skipCompatibilityProbe bool
}

// findDaemonPID locates the running daemon's pid via launchctl. Returns
// 0 with no error when the daemon is not registered.
func findDaemonPID(ctx context.Context) (int, error) {
	log := daemonClientLog(ctx)
	out, err := clientCommandRunner.Output("launchctl", "list", "io.goodkind.clyde.daemon")
	if err != nil {
		log.WarnContext(ctx, "daemon.client.find_pid.launchctl_failed", "err", err)
		return 0, fmt.Errorf("list launch agent pid: %w", err)
	}
	for _, line := range splitLines(string(out)) {
		line = trimSpace(line)
		if !startsWith(line, `"PID" = `) {
			continue
		}
		raw := line[len(`"PID" = `):]
		raw = trimRight(raw, ";")
		raw = trimSpace(raw)
		n, convErr := strconv.Atoi(raw)
		if convErr != nil {
			log.DebugContext(ctx, "daemon.client.find_pid.pid_parse_failed", "err", convErr)
			return 0, convErr
		}
		log.DebugContext(ctx, "daemon.client.find_pid.ok", "pid", n)
		return n, nil
	}
	log.DebugContext(ctx, "daemon.client.find_pid.no_pid_field")
	return 0, nil
}

// Tiny string helpers kept local so the daemon client does not depend
// on strings just for this one nudge function.
func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func trimRight(s, suffix string) string {
	for len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		s = s[:len(s)-len(suffix)]
	}
	return s
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// connect opens a connection to a running daemon and verifies it responds.
func connect(ctx context.Context) (*Client, error) {
	return connectWithOptions(ctx, connectOptions{skipCompatibilityProbe: false})
}

func connectWithOptions(ctx context.Context, options connectOptions) (*Client, error) {
	log := daemonClientLog(ctx)
	socketPath := config.DaemonSocketPath()
	target := "unix://" + socketPath
	log.DebugContext(ctx, "daemon.client.connect.dial", "socket_path", socketPath)

	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(daemonUnaryClientCorrelationInterceptor()),
		grpc.WithStreamInterceptor(daemonStreamClientCorrelationInterceptor()),
	)
	if err != nil {
		log.WarnContext(ctx, "daemon.client.connect.new_client_failed", "err", err)
		return nil, fmt.Errorf("dial daemon at %s: %w", socketPath, err)
	}

	// Verify the daemon is alive by making a trivial RPC.
	client := &Client{conn: conn, rpc: clydev1.NewClydeServiceClient(conn)}
	_, err = client.rpc.AcquireSession(dialCtx, &clydev1.AcquireSessionRequest{
		WrapperId: "__probe__",
	})
	// InvalidArgument means the daemon is alive (it rejected the probe).
	// Any other error means it's dead or unreachable.
	if err != nil {
		// Check if the error is "connection refused" or similar transport error
		// vs a legitimate RPC error (which means daemon IS running).
		// gRPC returns codes.Unavailable for transport failures.
		if isTransportError(err) {
			log.WarnContext(dialCtx, "daemon.client.connect.probe_transport_error", "err", err)
			_ = conn.Close()
			return nil, fmt.Errorf("daemon not responding: %w", err)
		}
		log.DebugContext(dialCtx, "daemon.client.connect.probe_alive", "err", err)
	}

	if options.skipCompatibilityProbe {
		log.DebugContext(ctx, "daemon.client.connect.compatibility_probe_skipped")
		log.DebugContext(ctx, "daemon.client.connect.ok")
		return client, nil
	}

	// Verify the daemon speaks the current dashboard RPC surface. Older
	// daemons can answer AcquireSession but lack newer methods like
	// ListSessions, which leaves the TUI looking empty while data still
	// exists on disk.
	_, err = client.rpc.ListSessions(dialCtx, &clydev1.ListSessionsRequest{})
	if err != nil {
		if isIncompatibleDaemonError(err) {
			log.WarnContext(dialCtx, "daemon.client.connect.probe_incompatible", "err", err)
			_ = conn.Close()
			return nil, fmt.Errorf("daemon incompatible with this client: %w", err)
		}
		if isTransportError(err) {
			log.WarnContext(dialCtx, "daemon.client.connect.probe_list_sessions_transport_error", "err", err)
			_ = conn.Close()
			return nil, fmt.Errorf("daemon not responding: %w", err)
		}
	}

	log.DebugContext(ctx, "daemon.client.connect.ok")
	return client, nil
}

// isTransportError returns true if the gRPC error indicates the daemon
// is not reachable (vs a legitimate application-level error).
func isTransportError(err error) bool {
	s := err.Error()
	// gRPC wraps transport failures with "Unavailable" or connection errors.
	for _, substr := range []string{"Unavailable", "connection refused", "no such file"} {
		if contains(s, substr) {
			return true
		}
	}
	return false
}

func isIncompatibleDaemonError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	incompatibleHints := []string{
		"Unimplemented",
		"unknown method ListSessions",
	}
	for _, hint := range incompatibleHints {
		if contains(s, hint) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ConnectOrStart connects to the daemon, starting it if not running.
// Uses flock to prevent multiple processes from spawning the daemon
// simultaneously.
func ConnectOrStart(ctx context.Context) (*Client, error) {
	return connectOrStart(ctx, connectOptions{skipCompatibilityProbe: false})
}

func connectOrStart(ctx context.Context, options connectOptions) (*Client, error) {
	log := daemonClientLog(ctx)
	// Tests and CI can disable the daemon entirely by setting
	// CLYDE_DISABLE_DAEMON=1. The wrapper then behaves as if the
	// daemon is unreachable, which gives the classic stdio path
	// without settings injection, global sync, or context summary
	// side effects.
	if os.Getenv("CLYDE_DISABLE_DAEMON") != "" {
		log.DebugContext(ctx, "daemon.client.connect_or_start.disabled")
		return nil, fmt.Errorf("daemon disabled by CLYDE_DISABLE_DAEMON")
	}
	// Fast path: daemon is already running.
	if client, err := connectWithOptions(ctx, options); err == nil {
		if err := ensureConnectedDaemonOwnedByLaunchd(ctx); err != nil {
			_ = client.Close()
			log.WarnContext(ctx, "daemon.client.connect_or_start.unowned_daemon", "err", err)
			return nil, err
		}
		log.DebugContext(ctx, "daemon.client.connect_or_start.fast_path")
		return client, nil
	}

	log.DebugContext(ctx, "daemon.client.connect_or_start.slow_path")
	// Slow path: need to start daemon. Take flock to serialize.
	lockPath := filepath.Join(config.RuntimeDir(), "daemon.lock")
	if err := config.EnsureRuntimeDir(); err != nil {
		return nil, err
	}

	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		log.WarnContext(ctx, "daemon.client.connect_or_start.lock_open_failed",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	defer func() { _ = lockFile.Close() }()

	// Block until we get the lock.
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		log.WarnContext(ctx, "daemon.client.connect_or_start.lock_failed",
			"lock_path", lockPath,
			"err", err,
		)
		return nil, fmt.Errorf("flock: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	// Double-check: another process may have started the daemon while we waited.
	if client, err := connectWithOptions(ctx, options); err == nil {
		if err := ensureConnectedDaemonOwnedByLaunchd(ctx); err != nil {
			_ = client.Close()
			log.WarnContext(ctx, "daemon.client.connect_or_start.unowned_daemon_after_lock", "err", err)
			return nil, err
		}
		log.DebugContext(ctx, "daemon.client.connect_or_start.ready_after_lock_wait")
		return client, nil
	}

	log.DebugContext(ctx, "daemon.client.connect_or_start.starting_daemon")
	// We hold the lock and daemon is not running. Start it.
	// Darwin startup must stay under launchd ownership; other platforms direct-spawn.
	if err := startDaemon(ctx); err != nil {
		log.WarnContext(ctx, "daemon.client.connect_or_start.start_daemon_failed", "err", err)
		return nil, fmt.Errorf("start daemon: %w", err)
	}

	// Retry with backoff until daemon is ready.
	delay := 50 * time.Millisecond
	for attempt := range 8 {
		select {
		case <-ctx.Done():
			log.DebugContext(ctx, "daemon.client.connect_or_start.wait_cancelled")
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if client, err := connectWithOptions(ctx, options); err == nil {
			if err := ensureConnectedDaemonOwnedByLaunchd(ctx); err != nil {
				_ = client.Close()
				log.WarnContext(ctx, "daemon.client.connect_or_start.unowned_daemon_after_start", "err", err)
				return nil, err
			}
			log.DebugContext(ctx, "daemon.client.connect_or_start.ready_after_start", "attempt", attempt)
			return client, nil
		}
		delay = min(delay*2, 500*time.Millisecond)
	}

	log.DebugContext(ctx, "daemon.client.connect_or_start.not_ready_after_retries")
	return nil, fmt.Errorf("daemon did not become ready after start")
}

func ensureConnectedDaemonOwnedByLaunchd(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	uid := strconv.Itoa(os.Getuid())
	target := "gui/" + uid + "/" + launchAgentLabel
	log := daemonClientLog(ctx)
	if err := clientCommandRunner.Run("launchctl", "print", target); err != nil {
		log.WarnContext(ctx, "daemon.client.launchd_owner_check_failed",
			"target", target,
			"err", err)
		return fmt.Errorf("daemon is reachable but launchd does not own %s; run `make service-install`: %w",
			target, err)
	}
	return nil
}

func (c *Client) Connection() *grpc.ClientConn {
	return c.conn
}

// Close closes the gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// UpdateContext asks the daemon to generate a context summary for a session
// in the background. Fire-and-forget: the caller does not wait for the result.
// Messages should be pre-extracted by the caller (to avoid import cycles).
func (c *Client) UpdateContext(sessionName, workspaceRoot string, messages []string) error {
	payload, _ := json.Marshal(map[string]any{
		"type":           "update_context",
		"session_name":   sessionName,
		"workspace_root": workspaceRoot,
		"messages":       messages,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.update_context.begin",
		"session_name", sessionName,
		"workspace_root", workspaceRoot,
		"message_count", len(messages),
	)
	_, err := c.rpc.HookEvent(ctx, &clydev1.HookEventRequest{
		RawJson: payload,
	})
	return err
}

// UpdateSessionWorkspaceRootViaDaemonOutcome updates a session's metadata
// workspace root through the daemon.
func UpdateSessionWorkspaceRootViaDaemonOutcome(ctx context.Context, name, workspaceRoot string) (LifecycleOutcome, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.update_session_workspace_root.begin",
		"name", name,
		"workspace_root", workspaceRoot,
	)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.update_session_workspace_root.connect_failed", "err", err)
		return LifecycleOutcomeDegradedOffline, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = c.rpc.UpdateSessionMetadata(rpcCtx, &clydev1.UpdateSessionMetadataRequest{
		Name:          name,
		WorkspaceRoot: workspaceRoot,
	})
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.update_session_workspace_root.rpc_failed", "err", err)
		return lifecycleOutcomeForError(err), err
	}
	log.DebugContext(rpcCtx, "daemon.client.update_session_workspace_root.ok")
	return LifecycleOutcomeReady, nil
}

func ListConfigControlsViaDaemon(ctx context.Context) ([]*clydev1.ConfigControl, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.list_config_controls.begin")
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.list_config_controls.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.rpc.ListConfigControls(rpcCtx, &clydev1.ListConfigControlsRequest{})
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.list_config_controls.rpc_failed", "err", err)
		return nil, err
	}
	log.DebugContext(rpcCtx, "daemon.client.list_config_controls.ok", "count", len(resp.GetControls()))
	return resp.GetControls(), nil
}

func UpdateConfigControlViaDaemon(ctx context.Context, key, value string) (*clydev1.ConfigControl, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.update_config_control.begin", "key", key)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.update_config_control.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.rpc.UpdateConfigControl(rpcCtx, &clydev1.UpdateConfigControlRequest{
		Key:   key,
		Value: value,
	})
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.update_config_control.rpc_failed", "key", key, "err", err)
		return nil, err
	}
	log.DebugContext(rpcCtx, "daemon.client.update_config_control.ok", "key", key)
	return resp.GetControl(), nil
}

func lifecycleOutcomeForError(err error) LifecycleOutcome {
	if err == nil {
		return LifecycleOutcomeReady
	}
	if lifecycleErrorLooksOffline(err) {
		return LifecycleOutcomeDegradedOffline
	}
	return LifecycleOutcomeFailed
}

func lifecycleErrorLooksOffline(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	offlineHints := []string{
		"Unavailable",
		"connection refused",
		"no such file",
		"transport is closing",
		"daemon disabled by CLYDE_DISABLE_DAEMON",
		"daemon not responding",
		"daemon did not become ready",
		"dial daemon",
	}
	for _, hint := range offlineHints {
		if contains(msg, hint) {
			return true
		}
	}
	return false
}

// StartLiveSessionViaDaemon asks the daemon to start a provider-neutral live
// session. The daemon owns provider compatibility and launch policy.
func StartLiveSessionViaDaemon(ctx context.Context, req *clydev1.StartLiveSessionRequest) (*clydev1.StartLiveSessionResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.start_live_session.begin",
		"provider", req.GetProvider(),
		"session", req.GetName(),
		"basedir", req.GetBasedir(),
		"incognito", req.GetIncognito(),
	)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.start_live_session.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.rpc.StartLiveSession(rpcCtx, req)
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.start_live_session.rpc_failed", "err", err)
		return nil, err
	}
	log.DebugContext(rpcCtx, "daemon.client.start_live_session.ok",
		"provider", resp.GetSession().GetProvider(),
		"session", resp.GetSession().GetSessionName(),
		"session_id", resp.GetSession().GetSessionId(),
	)
	return resp, nil
}

// SendLiveSessionViaDaemon delivers text through the daemon's live-session
// backend. Returns false when the backend rejects the input.
func SendLiveSessionViaDaemon(ctx context.Context, sessionID, text string) (bool, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.send_live_session.begin",
		"session_id", sessionID,
		"text_len", len(text),
	)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.send_live_session.connect_failed", "err", err)
		return false, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := c.rpc.SendLiveSession(rpcCtx, &clydev1.SendLiveSessionRequest{
		SessionId: sessionID,
		Text:      text,
	})
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.send_live_session.rpc_failed", "err", err)
		return false, err
	}
	accepted := resp.GetAccepted()
	log.DebugContext(rpcCtx, "daemon.client.send_live_session.ok", "accepted", accepted)
	return accepted, nil
}

// StreamLiveSessionViaDaemon opens the daemon live-session stream.
// Calling cancel stops the subscription.
func StreamLiveSessionViaDaemon(parent context.Context, sessionID string) (<-chan *clydev1.StreamLiveSessionResponse, context.CancelFunc, error) {
	log := daemonClientLog(parent)
	log.DebugContext(parent, "daemon.client.stream_live_session.begin",
		"session_id", sessionID,
	)
	c, err := ConnectOrStart(parent)
	if err != nil {
		log.DebugContext(parent, "daemon.client.stream_live_session.connect_failed", "err", err)
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	stream, err := c.rpc.StreamLiveSession(ctx, &clydev1.StreamLiveSessionRequest{SessionId: sessionID})
	if err != nil {
		log.DebugContext(ctx, "daemon.client.stream_live_session.stream_open_failed", "err", err)
		cancel()
		c.conn.Close()
		return nil, nil, err
	}
	log.DebugContext(ctx, "daemon.client.stream_live_session.stream_open")
	out := make(chan *clydev1.StreamLiveSessionResponse, 64)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				daemonClientLog(ctx).WarnContext(ctx, "daemon.client.stream_live_session.panicked",
					"session_id", sessionID,
					"panic", r,
				)
			}
		}()
		defer close(out)
		defer func() { _ = c.conn.Close() }()
		loopLog := daemonClientLog(ctx)
		for {
			event, err := stream.Recv()
			if err != nil {
				loopLog.DebugContext(ctx, "daemon.client.stream_live_session.recv_done", "err", err)
				return
			}
			select {
			case out <- event:
			case <-ctx.Done():
				loopLog.DebugContext(ctx, "daemon.client.stream_live_session.consumer_cancelled")
				return
			}
		}
	}()
	return out, cancel, nil
}

// AcquireForegroundSessionViaDaemon asks the daemon to suspend any daemon-owned
// live backend before an interactive foreground provider process starts.
func AcquireForegroundSessionViaDaemon(ctx context.Context, req *clydev1.AcquireForegroundSessionRequest) (*clydev1.AcquireForegroundSessionResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.acquire_foreground_session.begin",
		"provider", req.GetProvider(),
		"session", req.GetSessionName(),
		"session_id", req.GetSessionId(),
	)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.acquire_foreground_session.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.rpc.AcquireForegroundSession(rpcCtx, req)
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.acquire_foreground_session.rpc_failed", "err", err)
		return nil, err
	}
	log.DebugContext(rpcCtx, "daemon.client.acquire_foreground_session.ok",
		"provider", resp.GetProvider(),
		"session", resp.GetSessionName(),
		"session_id", resp.GetSessionId(),
		"restore", resp.GetShouldRestore(),
	)
	return resp, nil
}

// ReleaseForegroundSessionViaDaemon releases a foreground lease and asks the
// daemon to restore the prior live backend when one existed.
func ReleaseForegroundSessionViaDaemon(ctx context.Context, leaseToken, exitState string) (*clydev1.ReleaseForegroundSessionResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.release_foreground_session.begin",
		"exit_state", exitState,
	)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.release_foreground_session.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.rpc.ReleaseForegroundSession(rpcCtx, &clydev1.ReleaseForegroundSessionRequest{
		LeaseToken: leaseToken,
		ExitState:  exitState,
	})
	if err != nil {
		log.DebugContext(rpcCtx, "daemon.client.release_foreground_session.rpc_failed", "err", err)
		return nil, err
	}
	log.DebugContext(rpcCtx, "daemon.client.release_foreground_session.ok", "restored", resp.GetRestored())
	return resp, nil
}

// LaunchCredentialReauthKind names why the rotator could not plant a usable
// account for a launch. It mirrors the proto enum without leaking generated
// types past this boundary.
type LaunchCredentialReauthKind int

const (
	// LaunchReauthNone means selection succeeded or the rotator path is off.
	LaunchReauthNone LaunchCredentialReauthKind = iota
	// LaunchReauthNeedsReauth means the soonest-recoverable account has a dead
	// refresh credential and needs an operator login.
	LaunchReauthNeedsReauth
	// LaunchReauthAllThrottled means every account is rate-limited and recovers
	// on its own at the reported reset time.
	LaunchReauthAllThrottled
)

// LaunchCredentialReauth is the typed view of a rotator selection that yielded
// no usable account. Kind is LaunchReauthNone when the launch can proceed
// normally.
type LaunchCredentialReauth struct {
	Kind             LaunchCredentialReauthKind
	Provider         string
	Account          string
	ThrottledUntilMS int64
}

// ProviderLaunchEnvironment is the typed result of a launch-environment fetch:
// the daemon-owned environment variables to merge into the child env, plus an
// optional reauth signal the launching client prompts on (CLYDE-453).
type ProviderLaunchEnvironment struct {
	Environment []*clydev1.EnvironmentVariable
	Reauth      LaunchCredentialReauth
}

// ProviderLaunchEnvironmentViaDaemon asks the daemon for provider launch
// environment while keeping provider-owned setup in the daemon process.
func ProviderLaunchEnvironmentViaDaemon(ctx context.Context, provider string) (ProviderLaunchEnvironment, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.provider_launch_environment.begin",
		"provider", provider,
	)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.provider_launch_environment.connect_failed", "err", err)
		return ProviderLaunchEnvironment{}, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.rpc.ProviderLaunchEnvironment(rpcCtx, &clydev1.ProviderLaunchEnvironmentRequest{
		Provider: provider,
	})
	if err != nil {
		log.WarnContext(rpcCtx, "daemon.client.provider_launch_environment.rpc_failed", "err", err)
		return ProviderLaunchEnvironment{}, fmt.Errorf("provider launch environment rpc: %w", err)
	}
	log.DebugContext(rpcCtx, "daemon.client.provider_launch_environment.ok",
		"provider", provider,
		"env_count", len(resp.GetEnvironment()),
	)
	return ProviderLaunchEnvironment{
		Environment: resp.GetEnvironment(),
		Reauth:      launchReauthFromProto(resp.GetReauth()),
	}, nil
}

// launchReauthFromProto maps the wire reauth message to the typed client view.
// A nil message (selection succeeded or path off) yields LaunchReauthNone.
func launchReauthFromProto(reauth *clydev1.LaunchCredentialReauth) LaunchCredentialReauth {
	if reauth == nil {
		return LaunchCredentialReauth{
			Kind:             LaunchReauthNone,
			Provider:         "",
			Account:          "",
			ThrottledUntilMS: 0,
		}
	}
	kind := LaunchReauthNone
	switch reauth.GetKind() {
	case clydev1.LaunchCredentialReauthKind_LAUNCH_CREDENTIAL_REAUTH_KIND_NEEDS_REAUTH:
		kind = LaunchReauthNeedsReauth
	case clydev1.LaunchCredentialReauthKind_LAUNCH_CREDENTIAL_REAUTH_KIND_ALL_THROTTLED:
		kind = LaunchReauthAllThrottled
	case clydev1.LaunchCredentialReauthKind_LAUNCH_CREDENTIAL_REAUTH_KIND_UNSPECIFIED:
		kind = LaunchReauthNone
	}
	return LaunchCredentialReauth{
		Kind:             kind,
		Provider:         reauth.GetProvider(),
		Account:          reauth.GetAccount(),
		ThrottledUntilMS: reauth.GetThrottledUntilMs(),
	}
}

type CompactRunOptions struct {
	SessionName    string
	TargetTokens   int
	ReservedTokens int
	Model          string
	ModelExplicit  bool
	Thinking       bool
	Images         bool
	Tools          bool
	Chat           bool
	Summarize      bool
	SummarizeMode  string
	Force          bool
	Refresh        bool
}

func CompactPreviewViaDaemon(parent context.Context, in CompactRunOptions) (<-chan *clydev1.CompactEvent, <-chan error, context.CancelFunc, error) {
	return openCompactStream(parent, in, false)
}

func CompactApplyViaDaemon(parent context.Context, in CompactRunOptions) (<-chan *clydev1.CompactEvent, <-chan error, context.CancelFunc, error) {
	return openCompactStream(parent, in, true)
}

func openCompactStream(parent context.Context, in CompactRunOptions, apply bool) (<-chan *clydev1.CompactEvent, <-chan error, context.CancelFunc, error) {
	log := daemonClientLog(parent)
	log.DebugContext(parent, "daemon.client.compact.begin",
		"session", in.SessionName,
		"target", in.TargetTokens,
		"apply", apply,
	)
	c, err := ConnectOrStart(parent)
	if err != nil {
		log.DebugContext(parent, "daemon.client.compact.connect_failed", "err", err)
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	req := &clydev1.CompactRunRequest{
		SessionName:    in.SessionName,
		TargetTokens:   int32(in.TargetTokens),
		ReservedTokens: int32(in.ReservedTokens),
		Model:          in.Model,
		ModelExplicit:  in.ModelExplicit,
		Strippers: &clydev1.CompactStrippers{
			Thinking: in.Thinking,
			Images:   in.Images,
			Tools:    in.Tools,
			Chat:     in.Chat,
		},
		Summarize:     in.Summarize,
		SummarizeMode: in.SummarizeMode,
		Force:         in.Force,
		Refresh:       in.Refresh,
	}
	var stream grpc.ServerStreamingClient[clydev1.CompactEvent]
	if apply {
		stream, err = c.rpc.CompactApply(ctx, req)
	} else {
		stream, err = c.rpc.CompactPreview(ctx, req)
	}
	if err != nil {
		log.DebugContext(ctx, "daemon.client.compact.stream_open_failed", "apply", apply, "err", err)
		cancel()
		c.conn.Close()
		return nil, nil, nil, err
	}
	log.DebugContext(ctx, "daemon.client.compact.stream_open", "apply", apply)
	out := make(chan *clydev1.CompactEvent, 64)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(ctx, "daemon.client.compact_stream.panicked",
					"apply", apply,
					"panic", r,
				)
			}
		}()
		defer close(out)
		defer close(done)
		defer func() { _ = c.conn.Close() }()
		for {
			ev, recvErr := stream.Recv()
			if recvErr != nil {
				log.DebugContext(ctx, "daemon.client.compact.recv_done", "apply", apply, "err", recvErr)
				if errors.Is(recvErr, io.EOF) {
					done <- nil
				} else {
					done <- recvErr
				}
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, done, cancel, nil
}

func CompactUndoViaDaemon(ctx context.Context, sessionName string) (*clydev1.CompactUndoResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.compact_undo.begin", "session", sessionName)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.compact_undo.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.CompactUndo(rpcCtx, &clydev1.CompactUndoRequest{SessionName: sessionName})
	if rpcErr != nil {
		log.DebugContext(rpcCtx, "daemon.client.compact_undo.rpc_failed", "err", rpcErr)
		return nil, rpcErr
	}
	log.DebugContext(rpcCtx, "daemon.client.compact_undo.ok", "session", sessionName)
	return resp, nil
}

func ListSessionsViaDaemon(ctx context.Context) (*clydev1.ListSessionsResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.list_sessions.begin")
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.list_sessions.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.ListSessions(rpcCtx, &clydev1.ListSessionsRequest{})
	if rpcErr != nil {
		log.DebugContext(rpcCtx, "daemon.client.list_sessions.rpc_failed", "err", rpcErr)
		return nil, rpcErr
	}
	log.DebugContext(rpcCtx, "daemon.client.list_sessions.ok", "sessions", len(resp.GetSessions()))
	return resp, nil
}

// OAuthAccountListViaDaemon returns rotation-store account snapshots for one
// provider, or across every registered provider when providerName is empty.
// It follows the ListSessionsViaDaemon pattern: connect, single RPC, close.
func OAuthAccountListViaDaemon(ctx context.Context, providerName string) (*clydev1.OAuthAccountListResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.oauth_account_list.begin", "provider", providerName)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.oauth_account_list.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.OAuthAccountList(rpcCtx, &clydev1.OAuthAccountListRequest{Provider: providerName})
	if rpcErr != nil {
		log.ErrorContext(rpcCtx, "daemon.client.oauth_account_list.rpc_failed", "err", rpcErr)
		return nil, fmt.Errorf("oauth account list rpc: %w", rpcErr)
	}
	log.DebugContext(rpcCtx, "daemon.client.oauth_account_list.ok", "accounts", len(resp.GetAccounts()))
	return resp, nil
}

func GetSessionDetailViaDaemon(ctx context.Context, sessionName string) (*clydev1.GetSessionDetailResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.session_detail.begin", "session", sessionName)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.session_detail.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.GetSessionDetail(rpcCtx, &clydev1.GetSessionDetailRequest{SessionName: sessionName})
	if rpcErr != nil {
		log.DebugContext(rpcCtx, "daemon.client.session_detail.rpc_failed", "session", sessionName, "err", rpcErr)
		return nil, rpcErr
	}
	log.DebugContext(rpcCtx, "daemon.client.session_detail.ok",
		"session", sessionName,
		"messages", resp.GetTotalMessages())
	return resp, nil
}

func GetSessionExportStatsViaDaemon(ctx context.Context, sessionName string) (*clydev1.GetSessionExportStatsResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.session_export_stats.begin", "session", sessionName)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.session_export_stats.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.GetSessionExportStats(rpcCtx, &clydev1.GetSessionExportStatsRequest{SessionName: sessionName})
	if rpcErr != nil {
		log.DebugContext(rpcCtx, "daemon.client.session_export_stats.rpc_failed", "session", sessionName, "err", rpcErr)
		return nil, rpcErr
	}
	log.DebugContext(rpcCtx, "daemon.client.session_export_stats.ok",
		"session", sessionName,
		"visible_messages", resp.GetVisibleMessages(),
		"compactions", resp.GetCompactions())
	return resp, nil
}

func ExportSessionViaDaemon(ctx context.Context, req *clydev1.ExportSessionRequest) (*clydev1.ExportSessionResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.session_export.begin", "session", req.GetSessionName())
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.session_export.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.ExportSession(rpcCtx, req)
	if rpcErr != nil {
		log.DebugContext(rpcCtx, "daemon.client.session_export.rpc_failed", "session", req.GetSessionName(), "err", rpcErr)
		return nil, rpcErr
	}
	log.DebugContext(rpcCtx, "daemon.client.session_export.ok",
		"session", req.GetSessionName(),
		"bytes", len(resp.GetBody()))
	return resp, nil
}

const launchAgentLabel = "io.goodkind.clyde.daemon"

type daemonCommandRunner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
	CombinedOutput(name string, args ...string) ([]byte, error)
}

type osDaemonCommandRunner struct{}

func (osDaemonCommandRunner) Run(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func (osDaemonCommandRunner) Output(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

func (osDaemonCommandRunner) CombinedOutput(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

var (
	clientCommandRunner          daemonCommandRunner = osDaemonCommandRunner{}
	spawnDaemonDirectForPlatform                     = spawnDaemonDirect
)

var (
	reloadClientOverallTimeout = 60 * time.Second
	reloadClientRPCTimeout     = 30 * time.Second
	reloadClientRetryDelay     = 100 * time.Millisecond
)

// RestartManagedDaemon restarts the daemon through the service manager on
// macOS. Other platforms fall back to a best-effort direct spawn.
func RestartManagedDaemon(ctx context.Context) error {
	log := daemonClientLog(ctx)
	if runtime.GOOS != "darwin" {
		log.DebugContext(ctx, "daemon.client.restart.non_darwin_spawn_direct")
		return spawnDaemonDirectForPlatform()
	}
	target, err := ensureDarwinLaunchdOwnership(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.restart.ensure_launchd_ownership_failed", "err", err)
		return err
	}
	if out, err := clientCommandRunner.CombinedOutput("launchctl", "kickstart", "-k", target); err != nil {
		log.WarnContext(ctx, "daemon.client.restart.kickstart_failed",
			"target", target,
			"err", err,
			"output", string(out))
		return fmt.Errorf("kickstart launch agent %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	log.DebugContext(ctx, "daemon.client.restart.ok", "target", target)
	return nil
}

// startDaemon starts the daemon process. On macOS, launchd ownership is
// mandatory; other platforms retain the direct-spawn fallback.
func startDaemon(ctx context.Context) error {
	log := daemonClientLog(ctx)
	if runtime.GOOS == "darwin" {
		return startDarwinLaunchdDaemon(ctx, log)
	}

	return spawnDaemonDirectForPlatform()
}

func startDarwinLaunchdDaemon(ctx context.Context, log *slog.Logger) error {
	target, err := ensureDarwinLaunchdOwnership(ctx)
	if err != nil {
		log.WarnContext(ctx, "daemon.client.start_daemon.ensure_launchd_ownership_failed", "err", err)
		return fmt.Errorf("ensure launchd ownership for %s: %w", launchAgentLabel, err)
	}
	if out, err := clientCommandRunner.CombinedOutput("launchctl", "kickstart", "-k", target); err != nil {
		log.WarnContext(ctx, "daemon.client.start_daemon.kickstart_failed",
			"target", target,
			"err", err,
			"output", string(out))
		return fmt.Errorf("kickstart launch agent %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	log.DebugContext(ctx, "daemon.client.start_daemon.kickstart_ok", "target", target)
	return nil
}

// spawnDaemonDirect starts the daemon as a detached child process.
func spawnDaemonDirect() error {
	bg := context.Background()
	log := daemonClientLog(bg)
	self, err := os.Executable()
	if err != nil {
		log.WarnContext(bg, "daemon.client.spawn_direct.executable_failed", "err", err)
		return fmt.Errorf("resolve own path: %w", err)
	}

	daemonCmd := exec.Command(self, "daemon")
	daemonCmd.Stdout = nil
	daemonCmd.Stderr = nil
	daemonCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := daemonCmd.Start(); err != nil {
		log.DebugContext(bg, "daemon.client.spawn_direct.start_failed", "err", err)
		return err
	}
	log.DebugContext(bg, "daemon.client.spawn_direct.started", "pid", daemonCmd.Process.Pid)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.WarnContext(bg, "daemon.client.spawn_direct.wait_panicked",
					"pid", daemonCmd.Process.Pid,
					"panic", r,
				)
			}
		}()
		_ = daemonCmd.Wait()
	}()
	return nil
}

func ensureDarwinLaunchdOwnership(ctx context.Context) (string, error) {
	log := daemonClientLog(ctx)
	target := darwinLaunchdTarget()
	if err := clientCommandRunner.Run("launchctl", "print", target); err == nil {
		return target, nil
	}
	plistPath, err := darwinLaunchAgentPath()
	if err != nil {
		log.WarnContext(ctx, "daemon.client.launchd_path_failed", "target", target, "err", err)
		return "", fmt.Errorf("resolve launch agent path: %w", err)
	}
	if _, err := os.Stat(plistPath); err != nil {
		if os.IsNotExist(err) {
			log.WarnContext(ctx, "daemon.client.launchd_plist_missing", "target", target, "plist_path", plistPath)
			return "", fmt.Errorf("launchd does not own %s and %s is missing; run `make service-install`", target, plistPath)
		}
		log.WarnContext(ctx, "daemon.client.launchd_plist_stat_failed", "target", target, "plist_path", plistPath, "err", err)
		return "", fmt.Errorf("launchd does not own %s and stat %s failed: %w", target, plistPath, err)
	}
	log.WarnContext(ctx, "daemon.client.launchd_ownership_missing", "target", target, "plist_path", plistPath)
	return "", fmt.Errorf("launchd does not own %s; run `make service-install` to bootstrap %s", target, plistPath)
}

func darwinLaunchdTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + launchAgentLabel
}

func darwinLaunchAgentPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"), nil
}

// CalibrateSessionViaDaemon asks the daemon to probe the live
// session, derive static_overhead, persist the calibration, and
// return the stored record.
func CalibrateSessionViaDaemon(ctx context.Context, sessionName string) (*clydev1.CalibrateSessionResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.calibrate_session.begin", "session", sessionName)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.calibrate_session.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.CalibrateSession(rpcCtx, &clydev1.CalibrateSessionRequest{SessionName: sessionName})
	if rpcErr != nil {
		log.WarnContext(rpcCtx, "daemon.client.calibrate_session.rpc_failed", "session", sessionName, "err", rpcErr)
		return nil, fmt.Errorf("calibrate session %q: %w", sessionName, rpcErr)
	}
	log.DebugContext(rpcCtx, "daemon.client.calibrate_session.ok",
		"session", sessionName,
		"static_overhead", resp.GetCalibration().GetStaticOverhead())
	return resp, nil
}

// SetCalibrationViaDaemon asks the daemon to persist an
// operator-supplied static_overhead value for the session and return
// the stored record.
func SetCalibrationViaDaemon(ctx context.Context, sessionName string, staticOverhead int64) (*clydev1.SetCalibrationResponse, error) {
	log := daemonClientLog(ctx)
	log.DebugContext(ctx, "daemon.client.set_calibration.begin", "session", sessionName, "static_overhead", staticOverhead)
	c, err := ConnectOrStart(ctx)
	if err != nil {
		log.DebugContext(ctx, "daemon.client.set_calibration.connect_failed", "err", err)
		return nil, err
	}
	defer func() { _ = c.conn.Close() }()
	rpcCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, rpcErr := c.rpc.SetCalibration(rpcCtx, &clydev1.SetCalibrationRequest{
		SessionName:    sessionName,
		StaticOverhead: staticOverhead,
	})
	if rpcErr != nil {
		log.WarnContext(rpcCtx, "daemon.client.set_calibration.rpc_failed", "session", sessionName, "err", rpcErr)
		return nil, fmt.Errorf("set calibration %q: %w", sessionName, rpcErr)
	}
	log.DebugContext(rpcCtx, "daemon.client.set_calibration.ok",
		"session", sessionName,
		"static_overhead", resp.GetCalibration().GetStaticOverhead())
	return resp, nil
}
