package mitm

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LaunchProfile describes how to start one upstream client through
// the MITM proxy. This is internal infrastructure for daemon/config-driven
// capture workflows.
// Each profile knows where to find its binary, what env vars to
// inject, what Chromium flags Electron renderers need, and which
// upstream domains to expect in the capture.
type LaunchProfile struct {
	Name            string
	BinaryFinder    func() (string, error)
	BaseArgs        []string
	EnvKeys         []string
	IsElectron      bool
	UpstreamDomains []string
	ArgBuilder      func(proxyURL string, opts LaunchProfileOptions) ([]string, error)
	Configurator    func(proxyURL string, opts LaunchProfileOptions) error
	Preflight       func(opts LaunchProfileOptions) error
}

// LaunchProfileOptions carries launch-mode knobs for future CLI and
// daemon surfaces. The zero value launches the normal application
// profile; IsolatedProfileDir is used only when Isolated is true.
type LaunchProfileOptions struct {
	Isolated           bool
	IsolatedProfileDir string
	Force              bool
}

// LaunchProfiles is the registry of supported upstreams. Keys
// match the values accepted by the --upstream flag.
func LaunchProfiles() map[string]LaunchProfile {
	return map[string]LaunchProfile{
		"codex-cli": {
			Name:            "codex-cli",
			BinaryFinder:    findOnPath("codex"),
			BaseArgs:        nil,
			EnvKeys:         []string{"SSL_CERT_FILE", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY"},
			IsElectron:      false,
			UpstreamDomains: []string{"chatgpt.com", "openai.com"},
			ArgBuilder:      nil,
			Configurator:    nil,
			Preflight:       nil,
		},
		"codex-desktop": {
			Name:            "codex-desktop",
			BinaryFinder:    findApp("/Applications/Codex.app/Contents/MacOS/Codex"),
			BaseArgs:        []string{},
			EnvKeys:         []string{"SSL_CERT_FILE", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS"},
			IsElectron:      true,
			UpstreamDomains: []string{"chatgpt.com", "openai.com"},
			ArgBuilder:      nil,
			Configurator:    nil,
			Preflight:       nil,
		},
		"claude-code": {
			Name:            "claude-code",
			BinaryFinder:    findOnPath("claude"),
			BaseArgs:        nil,
			EnvKeys:         []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS"},
			IsElectron:      false,
			UpstreamDomains: []string{"api.anthropic.com", "claude.ai"},
			ArgBuilder:      nil,
			Configurator:    nil,
			Preflight:       nil,
		},
		"claude-desktop": {
			Name:            "claude-desktop",
			BinaryFinder:    findApp("/Applications/Claude.app/Contents/MacOS/Claude"),
			BaseArgs:        nil,
			EnvKeys:         []string{"SSL_CERT_FILE", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS"},
			IsElectron:      true,
			UpstreamDomains: []string{"api.anthropic.com", "claude.ai"},
			ArgBuilder:      nil,
			Configurator:    nil,
			Preflight:       nil,
		},
		// VS Code is generic Electron. The proxy intercepts whatever
		// extensions hit chatgpt.com / api.anthropic.com (e.g.
		// Continue, GitHub Copilot Chat with custom endpoints). The
		// LaunchProfile thread Chromium flags through; first-party
		// extensions that pin certs (Copilot today) bypass the proxy
		// regardless. CLYDE-131 tracks Copilot pinning specifically.
		"vscode": {
			Name:            "vscode",
			BinaryFinder:    findApp("/Applications/Visual Studio Code.app/Contents/MacOS/Electron"),
			BaseArgs:        nil,
			EnvKeys:         []string{"SSL_CERT_FILE", "HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "NODE_EXTRA_CA_CERTS"},
			IsElectron:      true,
			UpstreamDomains: []string{"api.anthropic.com", "claude.ai", "chatgpt.com", "openai.com", "api.githubcopilot.com"},
			ArgBuilder:      nil,
			Configurator:    nil,
			Preflight:       nil,
		},
		"cursor": NewCursorLaunchProfile(),
	}
}

// LookupLaunchProfile returns the profile by name. Unknown names
// return an error listing the supported set.
func LookupLaunchProfile(name string) (LaunchProfile, error) {
	profiles := LaunchProfiles()
	profile, ok := profiles[name]
	if !ok {
		known := make([]string, 0, len(profiles))
		for k := range profiles {
			known = append(known, k)
		}
		return LaunchProfile{}, fmt.Errorf("unknown upstream %q. supported: %s", name, strings.Join(known, ", "))
	}
	return profile, nil
}

// ResolvedBinary is the binary path the profile resolves to.
func (p LaunchProfile) ResolvedBinary() (string, error) {
	if p.BinaryFinder == nil {
		return "", fmt.Errorf("launch profile %q has no binary finder", p.Name)
	}
	return p.BinaryFinder()
}

// ComposeEnv assembles the env var slice in `KEY=VALUE` form for
// `exec.Cmd.Env`. The base env is the parent process env; profile
// keys are overridden from the supplied overrides map.
func (p LaunchProfile) ComposeEnv(parent []string, overrides map[string]string) []string {
	overridden := map[string]string{}
	maps.Copy(overridden, overrides)
	out := make([]string, 0, len(parent)+len(overridden))
	for _, kv := range parent {
		before, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		key := before
		if _, ok := overridden[key]; ok {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overridden {
		out = append(out, k+"="+v)
	}
	return out
}

// LaunchArgs returns profile-specific process arguments for a proxy
// launch. Generic Electron profiles receive Chromium proxy flags;
// profiles with an ArgBuilder own their complete launch argument set.
func (p LaunchProfile) LaunchArgs(proxyURL string, opts LaunchProfileOptions) ([]string, error) {
	args := append([]string{}, p.BaseArgs...)
	if p.ArgBuilder != nil {
		profileArgs, err := p.ArgBuilder(proxyURL, opts)
		if err != nil {
			return nil, err
		}
		args = append(args, profileArgs...)
		return args, nil
	}
	args = append(args, p.ChromiumFlags(proxyURL)...)
	return args, nil
}

// ValidateLaunch runs profile-specific launch guards. Generic profiles
// have no guard.
func (p LaunchProfile) ValidateLaunch(opts LaunchProfileOptions) error {
	if p.Preflight == nil {
		return nil
	}
	return p.Preflight(opts)
}

// ConfigureLaunch applies profile-specific persistent launch settings.
// Generic profiles do not need any profile mutation.
func (p LaunchProfile) ConfigureLaunch(proxyURL string, opts LaunchProfileOptions) error {
	if p.Configurator == nil {
		return nil
	}
	return p.Configurator(proxyURL, opts)
}

// ChromiumFlags returns the Chromium command-line flags Electron
// renderers need so they trust the mitm CA. Empty for non-Electron
// profiles.
func (p LaunchProfile) ChromiumFlags(proxyURL string) []string {
	if !p.IsElectron {
		return nil
	}
	return []string{
		"--proxy-server=" + proxyURL,
		"--ignore-certificate-errors",
		"--ignore-certificate-errors-spki-list",
	}
}

// findOnPath returns a finder that resolves `name` via $PATH.
func findOnPath(name string) func() (string, error) {
	return func() (string, error) {
		path, err := exec.LookPath(name)
		if err != nil {
			slog.Warn("mitm.launch_profile.path_lookup_failed",
				"component", "mitm",
				"binary", name,
				"err", err,
			)
			return "", fmt.Errorf("%s not on PATH: %w", name, err)
		}
		return path, nil
	}
}

// findApp returns a finder for a fixed bundle path. The path must
// exist; the finder returns an error pointing at the missing bundle
// otherwise.
func findApp(path string) func() (string, error) {
	return func() (string, error) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil {
			slog.Warn("mitm.launch_profile.app_missing",
				"component", "mitm",
				"path", abs,
				"err", err,
			)
			return "", fmt.Errorf("app bundle missing at %s: %w", abs, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("expected executable at %s, found directory", abs)
		}
		return abs, nil
	}
}
