package contextusage

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/contextusage"
)

// TestBuildProbeArgs_FullOptions asserts the argv the probe spawns
// when the caller supplies a session id and a per-session settings
// file. The probe runs claude in print mode with the /context slash
// command, pins the session via --resume, hands the settings file
// via --settings so claude resolves the model from there, suppresses
// transcript writes, and caps the spawn at one non-model turn.
//
// The probe argv must not contain `--model`; model resolution
// happens claude-side via the settings layering chain.
func TestBuildProbeArgs_FullOptions(t *testing.T) {
	args := buildProbeArgs(ProbeOptions{
		SessionID:    "sess-123",
		SettingsFile: "/tmp/clyde-sess-123/settings.json",
	})
	joined := joinArgs(args)
	for _, want := range []string{
		"-p /context",
		"--no-session-persistence",
		"--output-format json",
		"--max-turns 1",
		"--resume sess-123",
		"--settings /tmp/clyde-sess-123/settings.json",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
	for _, reject := range []string{
		"--model",
		"--input-format",
		"--verbose",
		"--fork-session",
		"--session-id",
	} {
		if strings.Contains(joined, reject) {
			t.Fatalf("args unexpectedly contain %q: %s", reject, joined)
		}
	}
}

// TestBuildProbeArgs_OmitsEmptyFlags asserts that an empty
// SessionID omits --resume and an empty SettingsFile omits
// --settings. claude without --resume reads no transcript and
// without --settings uses its global settings.json chain; both are
// degraded but valid for a workspace-only probe.
func TestBuildProbeArgs_OmitsEmptyFlags(t *testing.T) {
	args := buildProbeArgs(ProbeOptions{})
	joined := joinArgs(args)
	for _, reject := range []string{
		"--resume",
		"--settings",
		"--model",
	} {
		if strings.Contains(joined, reject) {
			t.Fatalf("args should omit %q without an opts value: %s", reject, joined)
		}
	}
	for _, want := range []string{
		"-p /context",
		"--no-session-persistence",
		"--output-format json",
		"--max-turns 1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args missing %q: %s", want, joined)
		}
	}
}

// TestClaudeProber_PassesSettingsFileWhenNoModelOverride asserts the
// Claude prober wrapper forwards the per-session settings file
// directly to the probe when the caller supplies no explicit model
// override.
func TestClaudeProber_PassesSettingsFileWhenNoModelOverride(t *testing.T) {
	tempDir := t.TempDir()
	settingsPath := tempDir + "/session-settings.json"
	if err := os.WriteFile(settingsPath, []byte(`{"model":"claude-opus-4-7"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	resolved, cleanup, err := resolveProbeSettingsFile(contextusage.ProbeOptions{
		SessionSettingsFile: settingsPath,
		Model:               "",
	})
	if err != nil {
		t.Fatalf("resolveProbeSettingsFile returned err: %v", err)
	}
	defer cleanup()
	if resolved != settingsPath {
		t.Fatalf("resolved=%q want %q", resolved, settingsPath)
	}
}

// TestClaudeProber_WritesTempSettingsForOverrideModel asserts that
// when the caller supplies an explicit model override the wrapper
// writes a one-shot temp settings file that copies the per-session
// fields and swaps in the override model, then returns a cleanup
// that removes it after the probe runs.
func TestClaudeProber_WritesTempSettingsForOverrideModel(t *testing.T) {
	tempDir := t.TempDir()
	settingsPath := tempDir + "/session-settings.json"
	if err := os.WriteFile(settingsPath, []byte(`{"model":"claude-opus-4-7","outputStyle":"compact"}`), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	resolved, cleanup, err := resolveProbeSettingsFile(contextusage.ProbeOptions{
		SessionSettingsFile: settingsPath,
		Model:               "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatalf("resolveProbeSettingsFile returned err: %v", err)
	}
	defer cleanup()
	if resolved == "" {
		t.Fatalf("resolved path is empty")
	}
	if resolved == settingsPath {
		t.Fatalf("resolved=%q must be a fresh temp file, not the session settings", resolved)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read temp settings: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode temp settings: %v", err)
	}
	if string(got["model"]) != `"claude-haiku-4-5"` {
		t.Fatalf("temp settings model = %s want %q", got["model"], "claude-haiku-4-5")
	}
	if string(got["outputStyle"]) != `"compact"` {
		t.Fatalf("temp settings outputStyle = %s want preserved %q", got["outputStyle"], "compact")
	}

	// Sanity: cleanup actually removes the temp file.
	cleanupCopy := cleanup
	cleanupCopy()
	if _, statErr := os.Stat(resolved); statErr == nil {
		t.Fatalf("cleanup did not remove temp file %q", resolved)
	}
	// re-stub cleanup so the deferred call above is a no-op.
	cleanup = func() {}
	_ = cleanup
}

// TestClaudeProber_OverrideWithoutSessionSettings asserts that an
// override model still works when the session has no settings file
// to base the override on. The temp file should contain just the
// override model.
func TestClaudeProber_OverrideWithoutSessionSettings(t *testing.T) {
	resolved, cleanup, err := resolveProbeSettingsFile(contextusage.ProbeOptions{
		SessionSettingsFile: "",
		Model:               "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatalf("resolveProbeSettingsFile returned err: %v", err)
	}
	defer cleanup()
	if resolved == "" {
		t.Fatalf("resolved path is empty")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatalf("read temp settings: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode temp settings: %v", err)
	}
	if string(got["model"]) != `"claude-haiku-4-5"` {
		t.Fatalf("temp settings model = %s want %q", got["model"], "claude-haiku-4-5")
	}
}

// TestClaudeProber_ProbeUsesContextForCancellation guards the
// context propagation path: the Probe wrapper accepts a context and
// forwards it to ProbeContextUsage, which honors deadlines via
// context.WithTimeout. The wrapper itself doesn't take action on the
// context other than to pass it through, so this test simply
// exercises the constructor path and confirms the wrapper compiles
// against the generic Prober interface.
func TestClaudeProber_ProbeUsesContextForCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var p claudeProber
	_, err := p.Probe(ctx, "", contextusage.ProbeOptions{})
	if err == nil {
		t.Fatalf("expected an error from cancelled context probe")
	}
}

func joinArgs(args []string) string {
	var out strings.Builder
	for i, arg := range args {
		if i > 0 {
			out.WriteString(" ")
		}
		out.WriteString(arg)
	}
	return out.String()
}
