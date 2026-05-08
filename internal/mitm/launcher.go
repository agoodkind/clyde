package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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

// PrepareLaunchOptions configures an executable MITM launch plan without
// starting the upstream process.
type PrepareLaunchOptions struct {
	Profile        LaunchProfile
	ProfileOptions LaunchProfileOptions
	CACertPath     string
	ProxyHost      string
	CaptureDir     string
	Force          bool
	Log            *slog.Logger
	ExtraArgs      []string
}

// PreparedLaunch is the executable launch plan returned by PrepareLaunch.
type PreparedLaunch struct {
	ProfileName string
	Binary      string
	Args        []string
	Env         []string
	ProxyURL    string
	CaptureDir  string
}

var (
	loadGlobalConfigForLaunch = config.LoadGlobalOrDefault
	ensureProxyForLaunch      = EnsureStarted
	parentEnvForLaunch        = os.Environ
)

// PrepareLaunch starts or updates the daemon-owned MITM proxy, applies
// profile configuration, and returns the process plan a caller may spawn or
// exec in the foreground.
func PrepareLaunch(ctx context.Context, opts PrepareLaunchOptions) (PreparedLaunch, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	log = slogger.WithConcern(log, slogger.ConcernProviderMITMLifecycle)
	cfg, err := loadGlobalConfigForLaunch()
	if err != nil {
		log.WarnContext(ctx, "mitm.launch.config_load_failed", "err", err)
		return PreparedLaunch{}, fmt.Errorf("load config: %w", err)
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
	proxy, err := ensureProxyForLaunch(cfg.MITM, log.With("subcomponent", "mitm-launch"))
	if err != nil {
		log.WarnContext(ctx, "mitm.launch.ensure_proxy_failed", "err", err)
		return PreparedLaunch{}, fmt.Errorf("ensure proxy: %w", err)
	}
	proxyURL := proxy.base
	if opts.Profile.BinaryFinder == nil {
		return PreparedLaunch{}, fmt.Errorf("upstream %s has no BinaryFinder", opts.Profile.Name)
	}
	profileOptions := opts.ProfileOptions
	if opts.Force {
		profileOptions.Force = true
	}
	if err := opts.Profile.ValidateLaunch(profileOptions); err != nil {
		return PreparedLaunch{}, err
	}
	binary, err := opts.Profile.BinaryFinder()
	if err != nil {
		log.WarnContext(ctx, "mitm.launch.binary_resolve_failed",
			"upstream", opts.Profile.Name,
			"err", err,
		)
		return PreparedLaunch{}, fmt.Errorf("resolve %s binary: %w", opts.Profile.Name, err)
	}
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return PreparedLaunch{}, fmt.Errorf("upstream %s has no resolved binary path", opts.Profile.Name)
	}
	// Deliberately omit SSL_CERT_FILE / NODE_EXTRA_CA_CERTS. Our
	// proxy never intercepts TLS; tunneled upstreams verify real
	// certs end-to-end, and direct-routed upstreams talk plain HTTP
	// to the proxy. See capture_session.go for the longer note.
	_ = opts.CACertPath
	_ = opts.ProxyHost
	envOverrides := map[string]string{
		"HTTPS_PROXY": proxyURL,
		"HTTP_PROXY":  proxyURL,
		"ALL_PROXY":   proxyURL,
	}
	env := opts.Profile.ComposeEnv(parentEnvForLaunch(), envOverrides)

	args, err := opts.Profile.LaunchArgs(proxyURL, profileOptions)
	if err != nil {
		return PreparedLaunch{}, fmt.Errorf("build %s launch args: %w", opts.Profile.Name, err)
	}
	args = append(args, opts.ExtraArgs...)
	if err := opts.Profile.ConfigureLaunch(proxyURL, profileOptions); err != nil {
		return PreparedLaunch{}, fmt.Errorf("configure %s launch: %w", opts.Profile.Name, err)
	}
	log.InfoContext(ctx, "mitm.launch.prepared",
		"upstream", opts.Profile.Name,
		"binary", binary,
		"proxy_url", proxyURL,
		"is_electron", opts.Profile.IsElectron,
		"capture_dir", cfg.MITM.CaptureDir,
		"force", opts.Force,
		"arg_count", len(args),
	)
	return PreparedLaunch{
		ProfileName: opts.Profile.Name,
		Binary:      binary,
		Args:        args,
		Env:         env,
		ProxyURL:    proxyURL,
		CaptureDir:  cfg.MITM.CaptureDir,
	}, nil
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
	prepared, err := PrepareLaunch(ctx, PrepareLaunchOptions{
		Profile:        opts.Profile,
		ProfileOptions: opts.ProfileOptions,
		CACertPath:     opts.CACertPath,
		ProxyHost:      opts.ProxyHost,
		CaptureDir:     opts.CaptureDir,
		Force:          opts.Force,
		Log:            log,
		ExtraArgs:      opts.ExtraArgs,
	})
	if err != nil {
		return err
	}

	log.InfoContext(ctx, "mitm.launch.starting",
		"upstream", opts.Profile.Name,
		"binary", prepared.Binary,
		"proxy_url", prepared.ProxyURL,
		"is_electron", opts.Profile.IsElectron,
		"capture_dir", prepared.CaptureDir,
		"force", opts.Force,
	)

	process, err := startDetachedPreparedLaunch(prepared, log)
	if err != nil {
		log.WarnContext(ctx, "mitm.launch.start_failed",
			"upstream", opts.Profile.Name,
			"binary", prepared.Binary,
			"err", err,
		)
		return fmt.Errorf("start %s: %w", opts.Profile.Name, err)
	}
	// Release the child so it survives the clyde process exit.
	if err := process.Release(); err != nil {
		log.WarnContext(ctx, "mitm.launch.release_failed", "err", err)
	}
	log.InfoContext(ctx, "mitm.launch.detached",
		"upstream", opts.Profile.Name,
		"pid", process.Pid,
	)
	return nil
}

func startDetachedPreparedLaunch(prepared PreparedLaunch, log *slog.Logger) (*os.Process, error) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		log.Warn("mitm.launch.devnull_open_failed", "err", err)
		return nil, fmt.Errorf("open devnull: %w", err)
	}
	defer func() { _ = devNull.Close() }()
	argv := append([]string{prepared.Binary}, prepared.Args...)
	process, err := os.StartProcess(prepared.Binary, argv, &os.ProcAttr{
		Env:   prepared.Env,
		Files: []*os.File{devNull, devNull, devNull},
	})
	if err != nil {
		log.Warn("mitm.launch.start_process_failed", "binary", prepared.Binary, "err", err)
		return nil, fmt.Errorf("start process %s: %w", prepared.Binary, err)
	}
	return process, nil
}
