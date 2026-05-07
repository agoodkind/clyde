package mitm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	cursorLaunchProfileName = "cursor"
	cursorDefaultBinaryPath = "/Applications/Cursor.app/Contents/MacOS/Cursor"

	cursorSettingDisableHTTP2               = "cursor.general.disableHttp2"
	cursorSettingDisableHTTP1SSE            = "cursor.general.disableHttp1SSE"
	cursorSettingProxy                      = "http.proxy"
	cursorSettingProxyStrictSSL             = "http.proxyStrictSSL"
	cursorSettingProxySupport               = "http.proxySupport"
	cursorSettingUseLocalProxyConfiguration = "http.useLocalProxyConfiguration"
)

var cursorProcessList = defaultCursorProcessList

// NewCursorLaunchProfile returns the MITM launch profile for Cursor.
// The zero LaunchProfileOptions value targets the user's normal Cursor
// profile; callers can opt into an isolated profile by setting
// LaunchProfileOptions.Isolated.
func NewCursorLaunchProfile() LaunchProfile {
	return LaunchProfile{
		Name:            cursorLaunchProfileName,
		BinaryFinder:    findApp(cursorDefaultBinaryPath),
		BaseArgs:        []string{},
		EnvKeys:         []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY"},
		IsElectron:      true,
		UpstreamDomains: []string{"cursor.com", "api2.cursor.sh", "api3.cursor.sh", "api4.cursor.sh", "chatgpt.com", "openai.com", "api.anthropic.com", "anthropic.com"},
		ArgBuilder:      cursorLaunchArgs,
		Configurator:    configureCursorLaunch,
		Preflight:       validateCursorLaunch,
	}
}

func cursorLaunchArgs(proxyURL string, opts LaunchProfileOptions) ([]string, error) {
	args := []string{
		"--proxy-server=" + strings.TrimSpace(proxyURL),
		"--ignore-certificate-errors",
		"--ignore-certificate-errors-spki-list",
	}
	if opts.Isolated {
		profileDir := strings.TrimSpace(opts.IsolatedProfileDir)
		if profileDir == "" {
			return nil, errors.New("cursor isolated profile requires IsolatedProfileDir")
		}
		absProfileDir, err := filepath.Abs(profileDir)
		if err != nil {
			slog.Warn("mitm.cursor.launch.resolve_isolated_profile_failed", "profile_dir", profileDir, "err", err)
			return nil, fmt.Errorf("resolve cursor isolated profile dir: %w", err)
		}
		args = append(args, "--user-data-dir="+absProfileDir)
	}
	return args, nil
}

type cursorConfigurationValueKind string

const (
	cursorConfigurationBool   cursorConfigurationValueKind = "bool"
	cursorConfigurationString cursorConfigurationValueKind = "string"
)

type cursorConfigurationValue struct {
	Key         string
	Kind        cursorConfigurationValueKind
	BoolValue   bool
	StringValue string
}

type cursorProbeSettings struct {
	DisableHTTP2               bool
	DisableHTTP1SSE            bool
	Proxy                      string
	ProxyStrictSSL             bool
	ProxySupport               string
	UseLocalProxyConfiguration bool
}

func newCursorProbeSettings(proxyURL string) cursorProbeSettings {
	return cursorProbeSettings{
		DisableHTTP2:               true,
		DisableHTTP1SSE:            false,
		Proxy:                      strings.TrimSpace(proxyURL),
		ProxyStrictSSL:             false,
		ProxySupport:               "override",
		UseLocalProxyConfiguration: true,
	}
}

func (s cursorProbeSettings) configurationValues() []cursorConfigurationValue {
	return []cursorConfigurationValue{
		{Key: cursorSettingDisableHTTP2, Kind: cursorConfigurationBool, BoolValue: s.DisableHTTP2, StringValue: ""},
		{Key: cursorSettingDisableHTTP1SSE, Kind: cursorConfigurationBool, BoolValue: s.DisableHTTP1SSE, StringValue: ""},
		{Key: cursorSettingProxy, Kind: cursorConfigurationString, BoolValue: false, StringValue: s.Proxy},
		{Key: cursorSettingProxyStrictSSL, Kind: cursorConfigurationBool, BoolValue: s.ProxyStrictSSL, StringValue: ""},
		{Key: cursorSettingProxySupport, Kind: cursorConfigurationString, BoolValue: false, StringValue: s.ProxySupport},
		{Key: cursorSettingUseLocalProxyConfiguration, Kind: cursorConfigurationBool, BoolValue: s.UseLocalProxyConfiguration, StringValue: ""},
	}
}

func configureCursorLaunch(proxyURL string, opts LaunchProfileOptions) error {
	settingsPath, err := cursorSettingsPath(opts)
	if err != nil {
		return err
	}
	return writeCursorProbeSettings(settingsPath, newCursorProbeSettings(proxyURL))
}

func cursorSettingsPath(opts LaunchProfileOptions) (string, error) {
	if opts.Isolated {
		profileDir := strings.TrimSpace(opts.IsolatedProfileDir)
		if profileDir == "" {
			return "", errors.New("cursor isolated profile requires IsolatedProfileDir")
		}
		absProfileDir, err := filepath.Abs(profileDir)
		if err != nil {
			slog.Warn("mitm.cursor.settings.resolve_isolated_profile_failed", "profile_dir", profileDir, "err", err)
			return "", fmt.Errorf("resolve cursor isolated profile dir: %w", err)
		}
		return filepath.Join(absProfileDir, "User", "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("mitm.cursor.settings.home_failed", "err", err)
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "settings.json"), nil
}

func writeCursorProbeSettings(path string, settings cursorProbeSettings) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("cursor settings path is empty")
	}
	existing := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			slog.Warn("mitm.cursor.settings.parse_failed", "path", path, "err", err)
			return fmt.Errorf("parse cursor settings: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("mitm.cursor.settings.read_failed", "path", path, "err", err)
		return fmt.Errorf("read cursor settings: %w", err)
	}
	for _, value := range settings.configurationValues() {
		raw, err := marshalCursorConfigurationValue(value)
		if err != nil {
			return err
		}
		existing[value.Key] = raw
	}
	encoded, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		slog.Warn("mitm.cursor.settings.encode_failed", "path", path, "err", err)
		return fmt.Errorf("encode cursor settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.Warn("mitm.cursor.settings.mkdir_failed", "path", path, "err", err)
		return fmt.Errorf("create cursor settings dir: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		slog.Warn("mitm.cursor.settings.write_failed", "path", path, "err", err)
		return fmt.Errorf("write cursor settings: %w", err)
	}
	return nil
}

func marshalCursorConfigurationValue(value cursorConfigurationValue) (json.RawMessage, error) {
	switch value.Kind {
	case cursorConfigurationBool:
		raw, err := json.Marshal(value.BoolValue)
		if err != nil {
			slog.Warn("mitm.cursor.settings.marshal_bool_failed", "key", value.Key, "err", err)
			return nil, fmt.Errorf("marshal cursor bool setting %s: %w", value.Key, err)
		}
		return raw, nil
	case cursorConfigurationString:
		raw, err := json.Marshal(value.StringValue)
		if err != nil {
			slog.Warn("mitm.cursor.settings.marshal_string_failed", "key", value.Key, "err", err)
			return nil, fmt.Errorf("marshal cursor string setting %s: %w", value.Key, err)
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("unknown cursor setting kind %q for %s", value.Kind, value.Key)
	}
}

func validateCursorLaunch(opts LaunchProfileOptions) error {
	if opts.Isolated || opts.Force {
		return nil
	}
	output, err := cursorProcessList()
	if err != nil {
		slog.Warn("mitm.cursor.launch.process_list_failed", "err", err)
		return fmt.Errorf("inspect running Cursor processes: %w", err)
	}
	if cursorNormalProfileRunning(output) {
		return errors.New("cursor appears to already be running with the normal profile; close Cursor first, request an isolated profile, or allow force")
	}
	return nil
}

func defaultCursorProcessList() ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "pgrep", "-afil", "Cursor.app/Contents/MacOS/Cursor")
	output, err := cmd.Output()
	if err == nil {
		return output, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil
	}
	slog.Warn("mitm.cursor.launch.pgrep_failed", "err", err)
	return nil, fmt.Errorf("pgrep Cursor processes: %w", err)
}

func cursorNormalProfileRunning(output []byte) bool {
	for rawLine := range strings.SplitSeq(string(output), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if !strings.Contains(line, "Cursor.app/Contents/MacOS/Cursor") {
			continue
		}
		if strings.Contains(line, "--user-data-dir") {
			continue
		}
		return true
	}
	return false
}
