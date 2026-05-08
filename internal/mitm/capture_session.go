package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"goodkind.io/clyde/internal/config"
)

// CaptureSessionOptions configures one capture run.
type CaptureSessionOptions struct {
	Profile     LaunchProfile
	CaptureRoot string // base dir; the session writes to <root>/<upstream>/<timestamp>/
	CACertPath  string // mitmproxy CA cert path (e.g. ~/.mitmproxy/mitmproxy-ca-cert.pem)
	ProxyHost   string // host:port for the proxy listener (e.g. [::1]:8888)
	ExtraArgs   []string
	Log         *slog.Logger
}

// CaptureSessionResult reports where the JSONL transcript landed.
type CaptureSessionResult struct {
	TranscriptPath string
	UpstreamBinary string
	StartedAt      time.Time
	EndedAt        time.Time
}

// RunCaptureSession starts the mitm proxy listener, spawns the
// upstream client with the right env and Chromium flags, and waits
// for the upstream binary to exit (or for the parent context to
// cancel). The transcript lands at
// `<CaptureRoot>/<upstream>/<timestamp>/capture.jsonl`.
//
// The caller is responsible for ensuring the mitmproxy CA cert
// already exists at CACertPath. We don't auto-install or auto-trust;
// that is a one-time user setup step (run mitmproxy once to seed the
// cert, then trust it system-wide if needed).
func RunCaptureSession(ctx context.Context, opts CaptureSessionOptions) (CaptureSessionResult, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	if opts.CaptureRoot == "" {
		return CaptureSessionResult{}, fmt.Errorf("capture session: CaptureRoot is required")
	}

	binary, err := opts.Profile.ResolvedBinary()
	if err != nil {
		log.WarnContext(ctx, "mitm.capture.binary_resolve_failed",
			"upstream", opts.Profile.Name,
			"err", err,
		)
		return CaptureSessionResult{}, fmt.Errorf("resolve binary: %w", err)
	}

	timestamp := currentTime().UTC().Format("20060102T150405Z")
	captureDir := filepath.Join(opts.CaptureRoot, opts.Profile.Name, timestamp)
	if err := os.MkdirAll(captureDir, 0o755); err != nil {
		log.WarnContext(ctx, "mitm.capture.mkdir_failed",
			"upstream", opts.Profile.Name,
			"capture_dir", captureDir,
			"err", err,
		)
		return CaptureSessionResult{}, fmt.Errorf("create capture dir: %w", err)
	}

	cfg := rawCaptureSessionConfig(captureDir)

	proxy, err := startCaptureProxy(opts.ProxyHost, cfg, log)
	if err != nil {
		log.WarnContext(ctx, "mitm.capture.proxy_start_failed",
			"upstream", opts.Profile.Name,
			"proxy_host", opts.ProxyHost,
			"err", err,
		)
		return CaptureSessionResult{}, fmt.Errorf("start proxy: %w", err)
	}
	defer func() { proxy.Shutdown() }()
	proxyURL := proxy.URL()
	_ = opts.CACertPath
	launch := buildCaptureLaunch(opts.Profile, proxyURL, opts.ExtraArgs)

	startedAt := currentTime()
	log.InfoContext(ctx, "mitm.capture.session_starting",
		"upstream", opts.Profile.Name,
		"binary", binary,
		"capture_dir", captureDir,
		"proxy_url", proxyURL,
		"is_electron", opts.Profile.IsElectron,
		"args", launch.args,
	)
	cmd := exec.CommandContext(ctx, binary, launch.args...)
	cmd.Env = launch.env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		log.WarnContext(ctx, "mitm.capture.upstream_start_failed",
			"upstream", opts.Profile.Name,
			"binary", binary,
			"err", err,
		)
		return CaptureSessionResult{}, fmt.Errorf("start %s: %w", opts.Profile.Name, err)
	}
	waitForCaptureCommand(ctx, cmd, opts.Profile.Name, log)

	endedAt := currentTime()
	transcript := filepath.Join(captureDir, "capture.jsonl")
	log.InfoContext(ctx, "mitm.capture.session_ended",
		"upstream", opts.Profile.Name,
		"transcript", transcript,
		"duration_ms", endedAt.Sub(startedAt).Milliseconds(),
	)

	return CaptureSessionResult{
		TranscriptPath: transcript,
		UpstreamBinary: binary,
		StartedAt:      startedAt,
		EndedAt:        endedAt,
	}, nil
}

type captureLaunch struct {
	args []string
	env  []string
}

func buildCaptureLaunch(profile LaunchProfile, proxyURL string, extraArgs []string) captureLaunch {
	envOverrides := map[string]string{
		"HTTPS_PROXY": proxyURL,
		"HTTP_PROXY":  proxyURL,
		"ALL_PROXY":   proxyURL,
		"NO_PROXY":    "",
	}
	args := append([]string{}, profile.BaseArgs...)
	args = append(args, profile.ChromiumFlags(proxyURL)...)
	args = append(args, extraArgs...)
	return captureLaunch{
		args: args,
		env:  profile.ComposeEnv(os.Environ(), envOverrides),
	}
}

func waitForCaptureCommand(ctx context.Context, cmd *exec.Cmd, upstreamName string, log *slog.Logger) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer func() { signal.Stop(sigCh) }()

	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.ErrorContext(ctx, "mitm.capture.wait_panic",
					"upstream", upstreamName,
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		done <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
	case <-sigCh:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
	case err := <-done:
		if err != nil && !isExpectedExit(err) {
			log.WarnContext(ctx, "mitm.capture.upstream_exited_with_error", "err", err)
		}
	}
}

func rawCaptureSessionConfig(captureDir string) config.MITMConfig {
	return config.MITMConfig{
		EnabledDefault: false,
		Providers:      nil,
		BodyMode:       "raw",
		CaptureDir:     captureDir,
		CaptureRules:   nil,
		Capture: config.MITMCapture{
			Rotation: config.LoggingRotation{
				Enabled:    nil,
				MaxSizeMB:  0,
				MaxBackups: 0,
				MaxAgeDays: 0,
				Compress:   nil,
			},
		},
		Drift: config.MITMDriftConfig{
			Enabled:     false,
			Interval:    0,
			DriftLogDir: "",
			CaptureRoot: "",
			CACertPath:  "",
			Upstreams:   nil,
		},
		Listen: config.MITMListenConfig{
			Host: "",
			Port: 0,
		},
		CA: config.MITMCAConfig{
			CertPath: "",
			KeyPath:  "",
		},
	}
}

// isExpectedExit recognizes user-driven shutdown errors from
// exec.Cmd that should not be treated as failures.
func isExpectedExit(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "signal:") || strings.Contains(msg, "killed")
}

// captureProxyHandle wraps a Proxy with its listener for clean
// shutdown.
type captureProxyHandle struct {
	proxy    *Proxy
	listener net.Listener
	server   *http.Server
}

func (h *captureProxyHandle) URL() string {
	return "http://" + h.listener.Addr().String()
}

func (h *captureProxyHandle) Shutdown() {
	if h.server != nil {
		_ = h.server.Close()
	}
}

func startCaptureProxy(addr string, cfg config.MITMConfig, log *slog.Logger) (*captureProxyHandle, error) {
	if addr == "" {
		addr = "[::1]:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	proxy := &Proxy{
		log:                   log.With("component", "mitm-capture"),
		client:                http.DefaultClient,
		dialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		certMu:                sync.Mutex{},
		ca:                    nil,
		cursorTLSClientConfig: nil,
		rawCaptureSeq:         atomic.Uint64{},
		mu:                    sync.RWMutex{},
		cfg:                   cfg,
		base:                  "http://" + ln.Addr().String(),
		listener:              ln,
		server:                nil,
	}
	srv := &http.Server{Handler: http.HandlerFunc(proxy.handle)}
	proxy.server = srv
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("mitm.capture.proxy_serve_panic",
					"err", fmt.Errorf("panic: %v", recovered),
				)
			}
		}()
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Warn("mitm.capture.proxy_serve_failed", "err", err)
		}
	}()
	return &captureProxyHandle{
		proxy:    proxy,
		listener: ln,
		server:   srv,
	}, nil
}
