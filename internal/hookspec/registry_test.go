package hookspec

import (
	"slices"
	"testing"
)

func TestRegisterRejectsDuplicateHookID(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	hook, ok := registry.Hook(HookIDClaudeCodeReorientAfterCompact)
	if !ok {
		t.Fatal("default registry did not include Claude Code reorient hook")
	}

	err := Register(&registry, hook)
	if err == nil {
		t.Fatal("Register duplicate hook id returned nil error")
	}
}

func TestNewRegistryDeclaresClaudeCodeReorientHook(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	hook, ok := registry.Hook(HookIDClaudeCodeReorientAfterCompact)
	if !ok {
		t.Fatal("default registry did not include Claude Code reorient hook")
	}

	if hook.Client != ClientClaudeCode {
		t.Fatalf("client = %q, want %q", hook.Client, ClientClaudeCode)
	}
	if hook.ClaudeCode.Event != ClaudeCodeEventSessionStart {
		t.Fatalf("event = %q, want %q", hook.ClaudeCode.Event, ClaudeCodeEventSessionStart)
	}
	if hook.ClaudeCode.Matcher != "compact" {
		t.Fatalf("matcher = %q, want compact", hook.ClaudeCode.Matcher)
	}
	if hook.ClaudeCode.TimeoutSeconds != 600 {
		t.Fatalf("timeout = %d, want 600", hook.ClaudeCode.TimeoutSeconds)
	}
	expectedArgs := []string{"hooks", "run", string(HookIDClaudeCodeReorientAfterCompact)}
	if !slices.Equal(hook.ClaudeCode.Args, expectedArgs) {
		t.Fatalf("args = %#v, want %#v", hook.ClaudeCode.Args, expectedArgs)
	}
	if hook.Run == nil {
		t.Fatal("run handler was nil")
	}
}
