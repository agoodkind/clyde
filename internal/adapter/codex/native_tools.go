package codex

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"goodkind.io/clyde/codexwire"
)

// FunctionToolSpec is part of Clyde's typed adapter surface. It
// renders one function-tool spec for the codex `tools` array. The
// caller-provided name and parameters pass through verbatim: the tool
// name and its JSON-schema parameters are opaque application content
// owned by whoever declared the tool, so clyde does not rewrite them.
func FunctionToolSpec(name, description string, parameters json.RawMessage, strict *bool) codexwire.ToolSpec {
	spec := codexwire.ToolSpec{
		Type: codexwire.ToolSpecTypeFunction,
		Name: strings.TrimSpace(name),
	}
	if desc := strings.TrimSpace(description); desc != "" {
		spec.Description = desc
	}
	if len(parameters) > 0 && string(parameters) != "null" {
		// Validate the parameters value parses as JSON; preserve the
		// caller's raw bytes so the spec round-trips byte-identically.
		var sink json.RawMessage
		if err := json.Unmarshal(parameters, &sink); err == nil {
			spec.Parameters = append(json.RawMessage(nil), parameters...)
		}
	}
	if strict != nil {
		v := *strict
		spec.Strict = &v
	}
	return spec
}

// CustomToolCallOutputItem is part of Clyde's typed adapter surface.
// The Name field is fixed to codex's freeform apply_patch tool type
// because custom_tool_call outputs on this path are apply_patch
// results; the codex tool type is "apply_patch"
// (research/codex/codex-rs/core/src/tools/handlers/apply_patch_spec.rs
// create_apply_patch_freeform_tool).
func CustomToolCallOutputItem(callID, text string) codexwire.InputItem {
	return codexwire.InputItem{
		Type:   codexwire.ItemTypeCustomToolCallOutput,
		CallID: strings.TrimSpace(callID),
		Name:   codexApplyPatchToolType,
		Output: codexwire.RawOutput(text),
	}
}

// codexApplyPatchToolType is the codex-internal freeform tool type for
// apply_patch. It is NOT a client tool name: it is the type codex emits
// on custom_tool_call items and the name codex expects on
// custom_tool_call_output items
// (research/codex/codex-rs/core/src/tools/handlers/apply_patch_spec.rs).
const codexApplyPatchToolType = "apply_patch"

// CommandString is part of Clyde's typed adapter surface.
func CommandString(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	if len(argv) >= 3 && strings.HasSuffix(filepath.Base(argv[0]), "sh") && (argv[1] == "-lc" || argv[1] == "-c") {
		return strings.Join(argv[2:], " ")
	}
	parts := make([]string, 0, len(argv))
	for _, arg := range argv {
		parts = append(parts, ShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

// ShellQuote is part of Clyde's typed adapter surface.
func ShellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if strings.IndexFunc(arg, func(r rune) bool {
		if r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '=' || r == '+' || r == ',' || r == '%' || r == '@' {
			return false
		}
		if r >= '0' && r <= '9' {
			return false
		}
		if r >= 'A' && r <= 'Z' {
			return false
		}
		return r < 'a' || r > 'z'
	}) == -1 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// ApplyPatchArgs is part of Clyde's typed adapter surface. It enforces
// codex's freeform apply_patch contract on the input emitted for a
// native custom_tool_call: unwrap any JSON wrapper (codex's freeform
// tool says "do not wrap the patch in JSON") and repair a known
// malformed-header case. The result is the raw patch body.
func ApplyPatchArgs(input string) (string, bool) {
	input = UnwrapApplyPatchInput(input)
	input = RepairApplyPatchInput(input)
	if strings.TrimSpace(input) == "" {
		return "", false
	}
	return input, true
}

// UnwrapApplyPatchInput strips a JSON wrapper from an apply_patch
// payload so the raw patch body survives. codex's apply_patch is a
// FREEFORM tool whose description states "do not wrap the patch in
// JSON"
// (research/codex/codex-rs/core/src/tools/handlers/apply_patch_spec.rs
// create_apply_patch_freeform_tool, the FreeformTool description), so a
// client that wraps the patch in {"input":...} or {"patch":...} must be
// unwrapped before forwarding the freeform body.
func UnwrapApplyPatchInput(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	var obj map[string]string
	trimmed := strings.TrimSpace(input)
	if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
		if v := obj["input"]; strings.TrimSpace(v) != "" {
			return v
		}
		if v := obj["patch"]; strings.TrimSpace(v) != "" {
			return v
		}
	}
	return input
}

// RepairApplyPatchInput drops a stray `*** Update File:` header that
// immediately precedes another hunk header (e.g. `*** Add File:`). The
// codex apply_patch LARK grammar (vendored at apply_patch.lark) defines
// each hunk as exactly one of add_hunk / delete_hunk / update_hunk, so
// an `*** Update File:` line followed directly by an `*** Add File:`
// line is not a well-formed update_hunk (it has no change body) and the
// upstream parser rejects it. Dropping the orphaned update header leaves
// the well-formed add_hunk that follows.
func RepairApplyPatchInput(input string) string {
	if strings.TrimSpace(input) == "" {
		return ""
	}
	lines := strings.SplitAfter(input, "\n")
	if len(lines) == 0 {
		return input
	}
	out := make([]string, 0, len(lines))
	for i := range lines {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "*** Update File: ") && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if startsPatchHunkHeader(next) {
				continue
			}
		}
		out = append(out, line)
	}
	return strings.Join(out, "")
}

func startsPatchHunkHeader(line string) bool {
	return strings.HasPrefix(line, "*** Add File: ") ||
		strings.HasPrefix(line, "*** Delete File: ") ||
		strings.HasPrefix(line, "*** Update File: ")
}
