package mitm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	cursorLaunchProfileName = "cursor"
	cursorDefaultBinaryPath = "/Applications/Cursor.app/Contents/MacOS/Cursor"

	CursorSettingDisableHTTP2               = "cursor.general.disableHttp2"
	CursorSettingDisableHTTP1SSE            = "cursor.general.disableHttp1SSE"
	CursorSettingProxy                      = "http.proxy"
	CursorSettingProxyStrictSSL             = "http.proxyStrictSSL"
	CursorSettingProxySupport               = "http.proxySupport"
	CursorSettingUseLocalProxyConfiguration = "http.useLocalProxyConfiguration"
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
		EnvKeys:         []string{"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY"},
		IsElectron:      true,
		UpstreamDomains: []string{"cursor.com", "api2.cursor.sh", "api3.cursor.sh", "api4.cursor.sh", "chatgpt.com", "openai.com", "api.anthropic.com", "anthropic.com"},
		ArgBuilder:      CursorLaunchArgs,
		Configurator:    ConfigureCursorLaunch,
		Preflight:       ValidateCursorLaunch,
	}
}

// CursorLaunchArgs returns the Cursor process arguments needed for
// MITM probing. Cursor-specific HTTP/2 and SSE settings are exposed
// separately by NewCursorProbeSettings because they belong in the
// selected Cursor profile's settings.json.
func CursorLaunchArgs(proxyURL string, opts LaunchProfileOptions) ([]string, error) {
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
			return nil, fmt.Errorf("resolve cursor isolated profile dir: %w", err)
		}
		args = append(args, "--user-data-dir="+absProfileDir)
	}
	return args, nil
}

type CursorConfigurationValueKind string

const (
	CursorConfigurationBool   CursorConfigurationValueKind = "bool"
	CursorConfigurationString CursorConfigurationValueKind = "string"
)

// CursorConfigurationValue is a typed VS Code/Cursor settings value.
// Future CLI or daemon surfaces can serialize these values into the
// selected profile's settings.json without open-ended payloads here.
type CursorConfigurationValue struct {
	Key         string
	Kind        CursorConfigurationValueKind
	BoolValue   bool
	StringValue string
}

type CursorProbeSettings struct {
	DisableHTTP2               bool
	DisableHTTP1SSE            bool
	Proxy                      string
	ProxyStrictSSL             bool
	ProxySupport               string
	UseLocalProxyConfiguration bool
}

// NewCursorProbeSettings returns the settings future CLI or daemon
// surfaces should merge into the selected Cursor profile before launch.
func NewCursorProbeSettings(proxyURL string) CursorProbeSettings {
	return CursorProbeSettings{
		DisableHTTP2:               true,
		DisableHTTP1SSE:            false,
		Proxy:                      strings.TrimSpace(proxyURL),
		ProxyStrictSSL:             false,
		ProxySupport:               "override",
		UseLocalProxyConfiguration: true,
	}
}

func (s CursorProbeSettings) ConfigurationValues() []CursorConfigurationValue {
	return []CursorConfigurationValue{
		{Key: CursorSettingDisableHTTP2, Kind: CursorConfigurationBool, BoolValue: s.DisableHTTP2},
		{Key: CursorSettingDisableHTTP1SSE, Kind: CursorConfigurationBool, BoolValue: s.DisableHTTP1SSE},
		{Key: CursorSettingProxy, Kind: CursorConfigurationString, StringValue: s.Proxy},
		{Key: CursorSettingProxyStrictSSL, Kind: CursorConfigurationBool, BoolValue: s.ProxyStrictSSL},
		{Key: CursorSettingProxySupport, Kind: CursorConfigurationString, StringValue: s.ProxySupport},
		{Key: CursorSettingUseLocalProxyConfiguration, Kind: CursorConfigurationBool, BoolValue: s.UseLocalProxyConfiguration},
	}
}

// ConfigureCursorLaunch writes the proxy settings Cursor's extension host
// reads from the active profile. Chromium flags alone do not reliably affect
// extension-host network calls.
func ConfigureCursorLaunch(proxyURL string, opts LaunchProfileOptions) error {
	settingsPath, err := cursorSettingsPath(opts)
	if err != nil {
		return err
	}
	return writeCursorProbeSettings(settingsPath, NewCursorProbeSettings(proxyURL))
}

func cursorSettingsPath(opts LaunchProfileOptions) (string, error) {
	if opts.Isolated {
		profileDir := strings.TrimSpace(opts.IsolatedProfileDir)
		if profileDir == "" {
			return "", errors.New("cursor isolated profile requires IsolatedProfileDir")
		}
		absProfileDir, err := filepath.Abs(profileDir)
		if err != nil {
			return "", fmt.Errorf("resolve cursor isolated profile dir: %w", err)
		}
		return filepath.Join(absProfileDir, "User", "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "settings.json"), nil
}

func writeCursorProbeSettings(path string, settings CursorProbeSettings) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("cursor settings path is empty")
	}
	existing := map[string]json.RawMessage{}
	if raw, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("parse cursor settings: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read cursor settings: %w", err)
	}
	for _, value := range settings.ConfigurationValues() {
		raw, err := marshalCursorConfigurationValue(value)
		if err != nil {
			return err
		}
		existing[value.Key] = raw
	}
	encoded, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("encode cursor settings: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create cursor settings dir: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write cursor settings: %w", err)
	}
	return nil
}

func marshalCursorConfigurationValue(value CursorConfigurationValue) (json.RawMessage, error) {
	switch value.Kind {
	case CursorConfigurationBool:
		raw, err := json.Marshal(value.BoolValue)
		if err != nil {
			return nil, fmt.Errorf("marshal cursor bool setting %s: %w", value.Key, err)
		}
		return raw, nil
	case CursorConfigurationString:
		raw, err := json.Marshal(value.StringValue)
		if err != nil {
			return nil, fmt.Errorf("marshal cursor string setting %s: %w", value.Key, err)
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("unknown cursor setting kind %q for %s", value.Kind, value.Key)
	}
}

// ValidateCursorLaunch rejects launching a second normal-profile
// Cursor because Electron will usually hand off to the already-running
// instance, leaving the MITM flags unused. Isolated launches are
// allowed because they use a separate user-data-dir.
func ValidateCursorLaunch(opts LaunchProfileOptions) error {
	if opts.Isolated || opts.Force {
		return nil
	}
	output, err := cursorProcessList()
	if err != nil {
		return fmt.Errorf("inspect running Cursor processes: %w", err)
	}
	if cursorNormalProfileRunning(output) {
		return errors.New("Cursor appears to already be running with the normal profile; close Cursor first, request an isolated profile, or allow force")
	}
	return nil
}

func defaultCursorProcessList() ([]byte, error) {
	cmd := exec.Command("pgrep", "-afil", "Cursor.app/Contents/MacOS/Cursor")
	output, err := cmd.Output()
	if err == nil {
		return output, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil
	}
	return nil, err
}

func cursorNormalProfileRunning(output []byte) bool {
	for _, rawLine := range bytes.Split(output, []byte{'\n'}) {
		line := strings.TrimSpace(string(rawLine))
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
