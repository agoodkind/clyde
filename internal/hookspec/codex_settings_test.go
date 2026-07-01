package hookspec

import (
	"strings"
	"testing"
)

func TestMarshalCodexHookInstalls(t *testing.T) {
	t.Parallel()

	body, err := marshalCodexHookInstalls(
		nil,
		[]RegisteredInstall{{
			HookID: HookIDReorientBeforeCompact,
			Spec: InstallSpec{
				Client:         ClientCodex,
				Event:          EventPreCompact,
				Matcher:        "",
				Args:           []string{"hooks", "run", string(HookIDReorientBeforeCompact)},
				TimeoutSeconds: 600,
				StatusMessage:  "Capturing Clyde reorient context",
			},
		}},
		nil,
		"/usr/local/bin/clyde",
		"/Users/me/.codex/config.toml",
	)
	if err != nil {
		t.Fatalf("marshalCodexHookInstalls: %v", err)
	}

	text := string(body)
	for _, want := range []string{
		codexManagedBegin,
		codexManagedEnd,
		"[features]",
		"hooks = true",
		"[hooks.state.\"/Users/me/.codex/config.toml:pre_compact:0:0\"]",
		"[[hooks.pre_compact]]",
		"command = \"/usr/local/bin/clyde hooks run reorient-before-compact\"",
		"trusted_hash = \"sha256:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("body missing %q:\n%s", want, text)
		}
	}
	assertCodexTrustedHash(t, text)
}

func TestRemoveCodexCommandHookGroupsMatchesInlineComments(t *testing.T) {
	t.Parallel()

	command := "/usr/local/bin/clyde hooks run reorient-before-compact"
	body := strings.Join([]string{
		"[[hooks.pre_compact]] # old clyde hook",
		"",
		"[[hooks.pre_compact.hooks]]",
		"command = \"" + command + "\" # clyde",
		"",
		"[other]",
		"value = true",
	}, "\n")

	got := removeCodexCommandHookGroups(body, [][]string{{"hooks", "run", string(HookIDReorientBeforeCompact)}})
	if strings.Contains(got, command) {
		t.Fatalf("command hook group survived:\n%s", got)
	}
	if strings.Contains(got, "[[hooks.pre_compact]]") {
		t.Fatalf("command hook group header survived:\n%s", got)
	}
	if !strings.Contains(got, "[other]") {
		t.Fatalf("unrelated table missing:\n%s", got)
	}
}

func TestRemoveCodexCommandHookGroupsMatchesManagedArgsAcrossClydeBinChanges(t *testing.T) {
	t.Parallel()

	command := "/opt/old/clyde hooks run reorient-before-compact"
	body := strings.Join([]string{
		"[[hooks.pre_compact]]",
		"",
		"[[hooks.pre_compact.hooks]]",
		"command = \"" + command + "\"",
		"",
		"[other]",
		"value = true",
	}, "\n")

	got := removeCodexCommandHookGroups(body, [][]string{{"hooks", "run", string(HookIDReorientBeforeCompact)}})
	if strings.Contains(got, command) {
		t.Fatalf("command hook group survived:\n%s", got)
	}
	if !strings.Contains(got, "[other]") {
		t.Fatalf("unrelated table missing:\n%s", got)
	}
}

func TestRemoveCodexManagedBlockLeavesOutOfOrderMarkersAlone(t *testing.T) {
	t.Parallel()

	body := strings.Join([]string{
		codexManagedEnd,
		"[features]",
		"hooks = false",
		codexManagedBegin,
	}, "\n")

	got := removeCodexManagedBlock(body)
	if got != body {
		t.Fatalf("removeCodexManagedBlock changed out-of-order markers:\n%s", got)
	}
}

func assertCodexTrustedHash(t *testing.T, body string) {
	t.Helper()

	for _, line := range strings.Split(body, "\n") {
		value, ok := strings.CutPrefix(line, "trusted_hash = \"sha256:")
		if !ok {
			continue
		}
		value = strings.TrimSuffix(value, "\"")
		if len(value) != 64 {
			t.Fatalf("trusted hash hex len = %d, want 64 in line %q", len(value), line)
		}
		return
	}
	t.Fatalf("trusted hash line missing:\n%s", body)
}
