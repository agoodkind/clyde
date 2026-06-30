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
		"[features]",
		"hooks = true",
		"[[hooks.PreCompact]]",
		"command = \"/usr/local/bin/clyde hooks run reorient-before-compact\"",
		"trusted_hash = \"sha256:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("body missing %q:\n%s", want, text)
		}
	}
}
