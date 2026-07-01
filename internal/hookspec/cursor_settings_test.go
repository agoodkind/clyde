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
	Command   string `json:"command"`
	Timeout   int    `json:"timeout"`
	LoopLimit int    `json:"loop_limit"`
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
	var decoded cursorHooksTestDocument
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, string(body))
	}
	if len(decoded.Hooks[EventCursorStop]) != 1 {
		t.Fatalf("stop hooks len = %d, want 1", len(decoded.Hooks[EventCursorStop]))
	}
	if decoded.Hooks[EventCursorStop][0].Command != "/usr/local/bin/clyde hooks run reorient-stop-followup" {
		t.Fatalf("command = %q", decoded.Hooks[EventCursorStop][0].Command)
	}
	if decoded.Hooks[EventCursorStop][0].LoopLimit != 1 {
		t.Fatalf("loop_limit = %d, want 1", decoded.Hooks[EventCursorStop][0].LoopLimit)
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

func TestCursorHooksDocumentReplacesManagedCommandAcrossClydeBinChanges(t *testing.T) {
	t.Parallel()

	document, err := unmarshalCursorHooksDocument([]byte(`{
  "version": 1,
  "hooks": {
    "stop": [{"command": "/opt/old/clyde hooks run reorient-stop-followup", "timeout": 10, "loop_limit": 1}]
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
	var decoded cursorHooksTestDocument
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("Unmarshal output: %v\n%s", err, string(body))
	}
	if len(decoded.Hooks[EventCursorStop]) != 1 {
		t.Fatalf("stop hooks len = %d, want 1", len(decoded.Hooks[EventCursorStop]))
	}
	if decoded.Hooks[EventCursorStop][0].Command != "/usr/local/bin/clyde hooks run reorient-stop-followup" {
		t.Fatalf("command = %q", decoded.Hooks[EventCursorStop][0].Command)
	}
}
