package mitm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchProfilesCoverSupportedUpstreams(t *testing.T) {
	profiles := LaunchProfiles()
	for _, name := range []string{"codex-cli", "codex-desktop", "claude-code", "claude-desktop", "cursor", "vscode"} {
		if _, ok := profiles[name]; !ok {
			t.Errorf("missing profile %q", name)
		}
	}
}

func TestLookupLaunchProfileUnknownReturnsHelpfulError(t *testing.T) {
	_, err := LookupLaunchProfile("not-a-real-thing")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "supported:") {
		t.Errorf("error should list supported set, got %q", err.Error())
	}
}

func TestComposeEnvOverridesParent(t *testing.T) {
	p := LaunchProfile{Name: "codex-cli"}
	parent := []string{"PATH=/usr/bin", "HTTPS_PROXY=http://stale:1234", "OTHER=keep"}
	got := p.ComposeEnv(parent, map[string]string{
		"HTTPS_PROXY": "http://[::1]:8888",
	})
	hasNew := false
	hasOther := false
	hasStale := false
	for _, kv := range got {
		switch kv {
		case "HTTPS_PROXY=http://[::1]:8888":
			hasNew = true
		case "OTHER=keep":
			hasOther = true
		case "HTTPS_PROXY=http://stale:1234":
			hasStale = true
		}
	}
	if !hasNew {
		t.Errorf("override missing")
	}
	if !hasOther {
		t.Errorf("non-overridden parent var dropped")
	}
	if hasStale {
		t.Errorf("stale parent value not removed")
	}
}

func TestChromiumFlagsOnlyForElectron(t *testing.T) {
	codex := LaunchProfile{Name: "codex-cli"}
	if got := codex.ChromiumFlags("http://[::1]:8888"); len(got) != 0 {
		t.Errorf("non-electron should have no chromium flags, got %v", got)
	}

	desktop := LaunchProfile{Name: "codex-desktop", IsElectron: true}
	flags := desktop.ChromiumFlags("http://[::1]:8888")
	if len(flags) == 0 {
		t.Fatal("electron should have chromium flags")
	}
	hasProxy := false
	hasIgnoreCert := false
	for _, f := range flags {
		if f == "--proxy-server=http://[::1]:8888" {
			hasProxy = true
		}
		if f == "--ignore-certificate-errors" {
			hasIgnoreCert = true
		}
	}
	if !hasProxy {
		t.Errorf("missing --proxy-server, got %v", flags)
	}
	if !hasIgnoreCert {
		t.Errorf("missing --ignore-certificate-errors, got %v", flags)
	}
}

func TestCursorLaunchProfileUsesCursorBundleAndDomains(t *testing.T) {
	profile := NewCursorLaunchProfile()
	if profile.Name != "cursor" {
		t.Fatalf("name=%q", profile.Name)
	}
	if profile.BinaryFinder == nil {
		t.Fatalf("cursor profile missing binary finder")
	}
	if !profile.IsElectron {
		t.Fatalf("cursor profile must be electron")
	}
	if !containsString(profile.UpstreamDomains, "api2.cursor.sh") {
		t.Fatalf("cursor profile missing api2.cursor.sh domain: %v", profile.UpstreamDomains)
	}
}

func TestCursorLaunchArgsDefaultNormalProfile(t *testing.T) {
	args, err := cursorLaunchArgs("http://[::1]:8888", LaunchProfileOptions{})
	if err != nil {
		t.Fatalf("CursorLaunchArgs: %v", err)
	}
	if !containsString(args, "--proxy-server=http://[::1]:8888") {
		t.Fatalf("missing proxy arg: %v", args)
	}
	if containsPrefix(args, "--user-data-dir=") {
		t.Fatalf("normal profile must not set user-data-dir: %v", args)
	}
}

func TestCursorLaunchArgsIsolatedProfile(t *testing.T) {
	args, err := cursorLaunchArgs("http://[::1]:8888", LaunchProfileOptions{
		Isolated:           true,
		IsolatedProfileDir: "tmp/cursor-mitm",
	})
	if err != nil {
		t.Fatalf("CursorLaunchArgs: %v", err)
	}
	if !containsPrefix(args, "--user-data-dir=") {
		t.Fatalf("isolated profile missing user-data-dir: %v", args)
	}
}

func TestCursorLaunchArgsIsolatedRequiresProfileDir(t *testing.T) {
	_, err := cursorLaunchArgs("http://[::1]:8888", LaunchProfileOptions{Isolated: true})
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "IsolatedProfileDir") {
		t.Fatalf("error should mention IsolatedProfileDir, got %q", err.Error())
	}
}

func TestCursorProbeSettingsCarryRequiredProbeConfig(t *testing.T) {
	settings := newCursorProbeSettings(" http://[::1]:8888 ")
	values := settings.configurationValues()
	assertCursorBoolSetting(t, values, cursorSettingDisableHTTP2, true)
	assertCursorBoolSetting(t, values, cursorSettingDisableHTTP1SSE, false)
	assertCursorStringSetting(t, values, cursorSettingProxy, "http://[::1]:8888")
	assertCursorBoolSetting(t, values, cursorSettingProxyStrictSSL, false)
	assertCursorStringSetting(t, values, cursorSettingProxySupport, "override")
	assertCursorBoolSetting(t, values, cursorSettingUseLocalProxyConfiguration, true)
}

func TestWriteCursorProbeSettingsMergesExistingSettings(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "User", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(settingsPath, []byte("{\"editor.fontSize\":14}\n"), 0o600); err != nil {
		t.Fatalf("write seed settings: %v", err)
	}
	if err := writeCursorProbeSettings(settingsPath, newCursorProbeSettings("http://localhost:8899")); err != nil {
		t.Fatalf("writeCursorProbeSettings: %v", err)
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	got := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	assertRawJSON(t, got, "editor.fontSize", "14")
	assertRawJSON(t, got, cursorSettingDisableHTTP2, "true")
	assertRawJSON(t, got, cursorSettingDisableHTTP1SSE, "false")
	assertRawJSON(t, got, cursorSettingProxy, `"http://localhost:8899"`)
	assertRawJSON(t, got, cursorSettingProxySupport, `"override"`)
}

func TestCursorSettingsPathUsesIsolatedProfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cursor-profile")
	got, err := cursorSettingsPath(LaunchProfileOptions{Isolated: true, IsolatedProfileDir: dir})
	if err != nil {
		t.Fatalf("cursorSettingsPath: %v", err)
	}
	want := filepath.Join(dir, "User", "settings.json")
	if got != want {
		t.Fatalf("settings path=%q want %q", got, want)
	}
}

func TestValidateCursorLaunchRejectsRunningNormalProfile(t *testing.T) {
	restoreCursorProcessList := cursorProcessList
	t.Cleanup(func() { cursorProcessList = restoreCursorProcessList })
	cursorProcessList = func() ([]byte, error) {
		return []byte("123 /Applications/Cursor.app/Contents/MacOS/Cursor\n"), nil
	}

	err := validateCursorLaunch(LaunchProfileOptions{})
	if err == nil {
		t.Fatalf("expected running normal-profile error")
	}
	if !strings.Contains(err.Error(), "already be running") {
		t.Fatalf("error should explain running Cursor, got %q", err.Error())
	}
}

func TestValidateCursorLaunchAllowsForceAndIsolated(t *testing.T) {
	restoreCursorProcessList := cursorProcessList
	t.Cleanup(func() { cursorProcessList = restoreCursorProcessList })
	cursorProcessList = func() ([]byte, error) {
		t.Fatalf("force or isolated launch should not inspect process list")
		return nil, nil
	}

	if err := validateCursorLaunch(LaunchProfileOptions{Force: true}); err != nil {
		t.Fatalf("force ValidateCursorLaunch: %v", err)
	}
	if err := validateCursorLaunch(LaunchProfileOptions{Isolated: true}); err != nil {
		t.Fatalf("isolated ValidateCursorLaunch: %v", err)
	}
}

func TestCursorNormalProfileRunningIgnoresIsolatedProfile(t *testing.T) {
	output := []byte("123 /Applications/Cursor.app/Contents/MacOS/Cursor --user-data-dir=/tmp/cursor-mitm\n")
	if cursorNormalProfileRunning(output) {
		t.Fatalf("isolated user-data-dir launch should not count as normal profile")
	}
}

func containsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func assertCursorBoolSetting(t *testing.T, values []cursorConfigurationValue, key string, want bool) {
	t.Helper()
	for _, value := range values {
		if value.Key != key {
			continue
		}
		if value.Kind != cursorConfigurationBool || value.BoolValue != want {
			t.Fatalf("%s=%+v, want bool %v", key, value, want)
		}
		return
	}
	t.Fatalf("missing setting %s", key)
}

func assertCursorStringSetting(t *testing.T, values []cursorConfigurationValue, key string, want string) {
	t.Helper()
	for _, value := range values {
		if value.Key != key {
			continue
		}
		if value.Kind != cursorConfigurationString || value.StringValue != want {
			t.Fatalf("%s=%+v, want string %q", key, value, want)
		}
		return
	}
	t.Fatalf("missing setting %s", key)
}

func assertRawJSON(t *testing.T, values map[string]json.RawMessage, key string, want string) {
	t.Helper()
	got, ok := values[key]
	if !ok {
		t.Fatalf("missing setting %s", key)
	}
	if string(got) != want {
		t.Fatalf("%s=%s want %s", key, got, want)
	}
}
