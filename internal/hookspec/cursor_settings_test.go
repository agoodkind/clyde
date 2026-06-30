package hookspec

import (
	"encoding/json"
	"strings"
	"testing"
)

type cursorHooksTestDocument struct {
	Hooks map[string][]cursorHooksTestHook `json:"hooks"`
}

type cursorHooksTestHook struct {
	Timeout int `json:"timeout"`
}

func TestCursorHooksDocumentMarshalCursorHookInstalls(t *testing.T) {
	t.Parallel()

	document, err := unmarshalCursorHooksDocument(nil)
	if err != nil {
		t.Fatalf("unmarshalCursorHooksDocument: %v", err)
	}
	installs := []RegisteredInstall{{
		HookID: HookIDReorientStopFollowup,
		Spec: InstallSpec{
			Client:         ClientCursor,
			Event:          EventCursorStop,
			Matcher:        "",
			Args:           []string{"hooks", "run", string(HookIDReorientStopFollowup)},
			TimeoutSeconds: 600,
			FailClosed:     false,
			LoopLimit:      1,
		},
	}}
	if err := document.marshalCursorHookInstalls(installs, nil, "/usr/local/bin/clyde"); err != nil {
		t.Fatalf("marshalCursorHookInstalls: %v", err)
	}
	body, err := document.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	text := string(body)
	for _, want := range []string{
		`"version": 1`,
		`"stop": [`,
		`"command": "/usr/local/bin/clyde hooks run reorient-stop-followup"`,
		`"loop_limit": 1`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("body missing %q:\n%s", want, text)
		}
	}
}

func TestCursorHooksDocumentPreservesUnrelatedHooksAndReplacesClydeCommands(t *testing.T) {
	t.Parallel()

	document, err := unmarshalCursorHooksDocument([]byte(`{
  "version": 1,
  "extra": true,
  "hooks": {
    "preToolUse": [{"command": "/bin/echo old", "timeout": 1}],
    "stop": [{"command": "/usr/local/bin/clyde hooks run reorient-stop-followup", "timeout": 10}]
  }
}`))
	if err != nil {
		t.Fatalf("unmarshalCursorHooksDocument: %v", err)
	}
	installs := []RegisteredInstall{{
		HookID: HookIDReorientStopFollowup,
		Spec: InstallSpec{
			Client:         ClientCursor,
			Event:          EventCursorStop,
			Args:           []string{"hooks", "run", string(HookIDReorientStopFollowup)},
			TimeoutSeconds: 600,
			LoopLimit:      1,
		},
	}}
	signatures := [][]string{{"hooks", "run", string(HookIDReorientStopFollowup)}}
	if err := document.marshalCursorHookInstalls(installs, signatures, "/usr/local/bin/clyde"); err != nil {
		t.Fatalf("marshalCursorHookInstalls: %v", err)
	}
	body, err := document.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, `"extra": true`) {
		t.Fatalf("top-level extra field missing:\n%s", text)
	}
	if !strings.Contains(text, `/bin/echo old`) {
		t.Fatalf("unrelated hook missing:\n%s", text)
	}
	if strings.Contains(text, `"timeout":10`) {
		t.Fatalf("old Clyde stop hook survived:\n%s", text)
	}

	var decoded cursorHooksTestDocument
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, text)
	}
	if len(decoded.Hooks[EventCursorStop]) != 1 {
		t.Fatalf("stop hooks len = %d, want 1", len(decoded.Hooks[EventCursorStop]))
	}
	if decoded.Hooks[EventCursorStop][0].Timeout != 600 {
		t.Fatalf("stop timeout = %d, want 600", decoded.Hooks[EventCursorStop][0].Timeout)
	}
}
