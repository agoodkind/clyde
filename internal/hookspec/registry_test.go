package hookspec

import (
	"slices"
	"strings"
	"testing"
)

func TestRegisterRejectsDuplicateHookID(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	hook, ok := registry.Hook(HookIDReorientAfterCompact)
	if !ok {
		t.Fatal("default registry did not include reorient-after-compact")
	}

	err := Register(&registry, hook)
	if err == nil {
		t.Fatal("Register duplicate hook id returned nil error")
	}
}

func TestRegisterRejectsDuplicateAliases(t *testing.T) {
	t.Parallel()

	registry := Registry{}
	err := Register(&registry, Hook{
		ID:      "hook-a",
		Aliases: []HookID{"alias-a", "alias-a"},
	})
	if err == nil {
		t.Fatal("Register duplicate aliases returned nil error")
	}
}

func TestRegisterRejectsAliasMatchingHookID(t *testing.T) {
	t.Parallel()

	registry := Registry{}
	err := Register(&registry, Hook{
		ID:      "hook-a",
		Aliases: []HookID{"hook-a"},
	})
	if err == nil {
		t.Fatal("Register alias matching hook id returned nil error")
	}
}

func TestNewRegistryDoesNotExposeLegacyRuntimeAliases(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if _, ok := registry.Hook(HookIDClaudeCodeReorientAfterCompact); ok {
		t.Fatal("legacy Claude Code hook alias unexpectedly resolved")
	}
}

func TestNewRegistryLegacyCommandSignaturesIncludeUnpublishedForms(t *testing.T) {
	t.Parallel()

	signatures := NewRegistry().LegacyCommandSignatures()
	for _, want := range []string{
		"hook sessionstart",
		"hooks run reorient-before-compact",
		"hooks run reorient-after-compact",
		"hooks run reorient-stop-followup",
		"hooks run claude-code-reorient-after-compact",
	} {
		if !hasSignature(signatures, want) {
			t.Fatalf("LegacyCommandSignatures() missing %q: %#v", want, signatures)
		}
	}
}

func TestNewRegistryDeclaresExpectedInstallSpecs(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	hook, ok := registry.Hook(HookIDReorientBeforeCompact)
	if !ok {
		t.Fatal("missing reorient-before-compact hook")
	}
	if hook.ClaudeCode.Event != ClaudeCodeEventPreCompact || !slices.Equal(hook.ClaudeCode.Args, []string{"hooks", "run", "reorient", "before-compact"}) {
		t.Fatalf("Claude Code pre-compact shape = %#v", hook.ClaudeCode)
	}
	assertInstallSpec(t, registry, ClientClaudeCode, HookIDReorientBeforeCompact, EventPreCompact, "", []string{"hooks", "run", "reorient", "before-compact"}, 600, 0)
	assertInstallSpec(t, registry, ClientClaudeCode, HookIDReorientAfterCompact, EventSessionStart, "compact", []string{"hooks", "run", "reorient", "after-compact"}, 600, 0)
	assertInstallSpec(t, registry, ClientCodex, HookIDReorientBeforeCompact, EventPreCompact, "", []string{"hooks", "run", "reorient", "before-compact"}, 600, 0)
	assertInstallSpec(t, registry, ClientCodex, HookIDReorientAfterCompact, EventSessionStart, "compact", []string{"hooks", "run", "reorient", "after-compact"}, 600, 0)
	assertInstallSpec(t, registry, ClientCursor, HookIDReorientBeforeCompact, EventCursorPre, "", []string{"hooks", "run", "reorient", "before-compact"}, 600, 0)
	assertInstallSpec(t, registry, ClientCursor, HookIDReorientStopFollowup, EventCursorStop, "", []string{"hooks", "run", "reorient", "stop-followup"}, 600, 1)
}

func assertInstallSpec(t *testing.T, registry Registry, client Client, id HookID, event string, matcher string, args []string, timeout int, loopLimit int) {
	t.Helper()

	for _, install := range registry.InstallsForClient(client) {
		if install.HookID != id {
			continue
		}
		if install.Spec.Event != event {
			t.Fatalf("%s/%s event = %q, want %q", client, id, install.Spec.Event, event)
		}
		if install.Spec.Matcher != matcher {
			t.Fatalf("%s/%s matcher = %q, want %q", client, id, install.Spec.Matcher, matcher)
		}
		if !slices.Equal(install.Spec.Args, args) {
			t.Fatalf("%s/%s args = %#v, want %#v", client, id, install.Spec.Args, args)
		}
		if install.Spec.TimeoutSeconds != timeout {
			t.Fatalf("%s/%s timeout = %d, want %d", client, id, install.Spec.TimeoutSeconds, timeout)
		}
		if install.Spec.LoopLimit != loopLimit {
			t.Fatalf("%s/%s loop_limit = %d, want %d", client, id, install.Spec.LoopLimit, loopLimit)
		}
		if install.Spec.Client != client {
			t.Fatalf("%s/%s client = %q, want %q", client, id, install.Spec.Client, client)
		}
		return
	}
	t.Fatalf("missing install spec %s/%s", client, id)
}

func hasSignature(signatures [][]string, want string) bool {
	for _, signature := range signatures {
		if slices.Equal(signature, strings.Split(want, " ")) {
			return true
		}
	}
	return false
}
