package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

// LaunchUpstreamOptions configures a non-blocking launch of an
// upstream client. The proxy is ensured to be running; the binary is
// then spawned with the LaunchProfile's env + Chromium flags applied
// and immediately detached so the caller can return. Suitable for a
// dock-launched wrapper .app: click icon -> clyde returns -> the
// real client runs.
type LaunchUpstreamOptions struct {
	Profile        LaunchProfile
	ProfileOptions LaunchProfileOptions
	CACertPath     string
	ProxyHost      string
	CaptureDir     string
	Force          bool
	Log            *slog.Logger
	ExtraArgs      []string
}

// LaunchUpstream starts the upstream client through the local MITM
// proxy and returns once the child process is launched. The child
// runs detached from clyde; clyde itself exits as soon as the spawn
// succeeds. Stdout/stderr go to /dev/null because GUI apps don't
// stream useful output to a wrapper.
func LaunchUpstream(ctx context.Context, opts LaunchUpstreamOptions) error {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernProviderMITMLifecycle)
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		log.WarnContext(ctx, "mitm.launch.config_load_failed", "err", err)
		return fmt.Errorf("load config: %w", err)
	}
	// Force MITM enablement for the upstream's family so the always-on
	// gate doesn't suppress the proxy. The wrapper exists specifically
	// to route through MITM, even if the global default is off.
	if !cfg.MITM.EnabledDefault {
		cfg.MITM.EnabledDefault = true
		cfg.MITM.Providers = config.MITMProviderSet{"all"}
	}
	if captureDir := strings.TrimSpace(opts.CaptureDir); captureDir != "" {
		cfg.MITM.CaptureDir = captureDir
	}
	proxy, err := EnsureStarted(cfg.MITM, log.With("subcomponent", "mitm-launch"))
	if err != nil {
		log.WarnContext(ctx, "mitm.launch.ensure_proxy_failed", "err", err)
		return fmt.Errorf("ensure proxy: %w", err)
	}
	proxyURL := proxy.base
	if opts.Profile.BinaryFinder == nil {
		return fmt.Errorf("upstream %s has no BinaryFinder", opts.Profile.Name)
	}
	profileOptions := opts.ProfileOptions
	if opts.Force {
		profileOptions.Force = true
	}
	if err := opts.Profile.ValidateLaunch(profileOptions); err != nil {
		return err
	}
	binary, err := opts.Profile.BinaryFinder()
	if err != nil {
		log.WarnContext(ctx, "mitm.launch.binary_resolve_failed",
			"upstream", opts.Profile.Name,
			"err", err,
		)
		return fmt.Errorf("resolve %s binary: %w", opts.Profile.Name, err)
	}
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return fmt.Errorf("upstream %s has no resolved binary path", opts.Profile.Name)
	}
	// Deliberately omit SSL_CERT_FILE / NODE_EXTRA_CA_CERTS. Our
	// proxy never intercepts TLS; tunneled upstreams verify real
	// certs end-to-end, and direct-routed upstreams talk plain HTTP
	// to the proxy. See capture_session.go for the longer note.
	_ = opts.CACertPath
	envOverrides := map[string]string{
		"HTTPS_PROXY": proxyURL,
		"HTTP_PROXY":  proxyURL,
		"ALL_PROXY":   proxyURL,
	}
	env := opts.Profile.ComposeEnv(os.Environ(), envOverrides)

	args, err := opts.Profile.LaunchArgs(proxyURL, profileOptions)
	if err != nil {
		return fmt.Errorf("build %s launch args: %w", opts.Profile.Name, err)
	}
	args = append(args, opts.ExtraArgs...)
	if err := opts.Profile.ConfigureLaunch(proxyURL, profileOptions); err != nil {
		return fmt.Errorf("configure %s launch: %w", opts.Profile.Name, err)
	}

	log.InfoContext(ctx, "mitm.launch.starting",
		"upstream", opts.Profile.Name,
		"binary", binary,
		"proxy_url", proxyURL,
		"is_electron", opts.Profile.IsElectron,
		"capture_dir", cfg.MITM.CaptureDir,
		"force", opts.Force,
	)

	cmd := exec.Command(binary, args...)
	cmd.Env = env
	// Detach: no stdin, no stdout/stderr inheritance. The wrapper
	// returns; the upstream owns its own session.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		log.WarnContext(ctx, "mitm.launch.start_failed",
			"upstream", opts.Profile.Name,
			"binary", binary,
			"err", err,
		)
		return fmt.Errorf("start %s: %w", opts.Profile.Name, err)
	}
	// Release the child so it survives the clyde process exit.
	if err := cmd.Process.Release(); err != nil {
		log.WarnContext(ctx, "mitm.launch.release_failed", "err", err)
	}
	log.InfoContext(ctx, "mitm.launch.detached",
		"upstream", opts.Profile.Name,
		"pid", cmd.Process.Pid,
	)
	return nil
}
