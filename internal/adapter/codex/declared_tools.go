package codex

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/codexwire"
)

// declaredToolResolver maps a codex-internal native tool TYPE back to
// the tool name the client declared this turn. It never matches by
// hardcoded client tool names: a native call is routed back to a
// declared tool by inspecting that declared tool's parameter SHAPE.
//
// The motivating constraint is that codex can answer an
// OpenAI-public-schema request by emitting one of its own native tool
// types (a freeform `apply_patch` custom_tool_call, or a
// `local_shell_call`) rather than a plain function_call. The client
// (Cursor or any OpenAI-compatible client) only recognizes a tool name
// it declared in `tools`, so the response-side name must be the
// client's declared name, derived by shape:
//   - a native apply_patch / freeform call -> the declared tool whose
//     shape is patch-like (a single freeform/string patch parameter).
//   - a native shell / exec call -> the declared tool whose shape is
//     command-like (a string `cmd`/`command` parameter).
//
// If exactly one tool was declared and codex used a native type, that
// single declared tool is the target regardless of shape, because there
// is no ambiguity about which declared tool the native call satisfies.
type declaredToolResolver struct {
	tools []codexwire.ToolSpec
}

func newDeclaredToolResolver(tools []codexwire.ToolSpec) declaredToolResolver {
	return declaredToolResolver{tools: tools}
}

// patchToolName returns the client-declared tool name to use for a
// codex native apply_patch / freeform call, and whether a target was
// found. It prefers a declared tool whose parameter schema is
// patch-like; failing that, the sole declared tool when exactly one was
// declared.
func (r declaredToolResolver) patchToolName() (string, bool) {
	if name, ok := r.firstByShape(toolShapeIsPatchLike); ok {
		return name, true
	}
	return r.soleDeclaredToolName()
}

// commandToolName returns the client-declared tool name to use for a
// codex native shell / exec call, and whether a target was found. It
// prefers a declared tool whose parameter schema is command-like;
// failing that, the sole declared tool when exactly one was declared.
func (r declaredToolResolver) commandToolName() (string, bool) {
	if name, ok := r.firstByShape(toolShapeIsCommandLike); ok {
		return name, true
	}
	return r.soleDeclaredToolName()
}

func (r declaredToolResolver) firstByShape(match func(toolParameterSchema) bool) (string, bool) {
	for _, tool := range r.tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		if match(parseToolParameterSchema(tool.Parameters)) {
			return name, true
		}
	}
	return "", false
}

// commandToolSchema returns the parsed parameter schema of the declared
// command-like tool the chooser would pick for a native local_shell_call.
// The shell handler reads its property names to render the action under
// the field names the client declared. The boolean is false when no
// declared tool is command-like, so the handler falls back to the
// codex-rs shell field names.
func (r declaredToolResolver) commandToolSchema() (toolParameterSchema, bool) {
	for _, tool := range r.tools {
		if strings.TrimSpace(tool.Name) == "" {
			continue
		}
		schema := parseToolParameterSchema(tool.Parameters)
		if toolShapeIsCommandLike(schema) {
			return schema, true
		}
	}
	return toolParameterSchema{Type: "", Format: "", Properties: nil}, false
}

func (r declaredToolResolver) soleDeclaredToolName() (string, bool) {
	named := make([]string, 0, len(r.tools))
	for _, tool := range r.tools {
		if name := strings.TrimSpace(tool.Name); name != "" {
			named = append(named, name)
		}
	}
	if len(named) == 1 {
		return named[0], true
	}
	return "", false
}

// toolParameterSchema is the minimal JSON-schema view the shape
// classifiers read off a declared tool's `parameters`. Only the
// structural signals that distinguish a patch tool from a command tool
// are decoded; everything else stays opaque.
type toolParameterSchema struct {
	Type       string                        `json:"type"`
	Format     string                        `json:"format"`
	Properties map[string]toolPropertySchema `json:"properties"`
}

type toolPropertySchema struct {
	Type string `json:"type"`
}

func parseToolParameterSchema(raw json.RawMessage) toolParameterSchema {
	out := toolParameterSchema{Type: "", Format: "", Properties: nil}
	if len(raw) == 0 || string(raw) == "null" {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return toolParameterSchema{Type: "", Format: "", Properties: nil}
	}
	return out
}

// patchParameterNames are the property names a patch-like tool exposes
// for its single freeform patch body. They are read as a structural
// signal on the DECLARED tool's schema, not matched against the client
// tool name.
var patchParameterNames = map[string]bool{
	"input": true,
	"patch": true,
	"diff":  true,
}

// commandParameterNames are the property names a command-like tool
// exposes for its shell command string. Grounded in codex-rs shell
// tool parameter names (`cmd` for exec_command, `command` for
// shell_command) in
// research/codex/codex-rs/core/src/tools/handlers/shell_spec.rs.
var commandParameterNames = map[string]bool{
	"cmd":     true,
	"command": true,
}

// toolShapeIsPatchLike reports whether a declared tool's parameter
// schema looks like a single freeform/string patch parameter. A
// freeform string schema (no object properties) counts, as does an
// object whose only string-typed property is a patch parameter name.
func toolShapeIsPatchLike(schema toolParameterSchema) bool {
	if len(schema.Properties) == 0 {
		// A bare string/freeform schema with no properties is treated as
		// a single freeform patch parameter.
		return strings.EqualFold(schema.Type, "string")
	}
	stringProps := 0
	patchProps := 0
	for name, prop := range schema.Properties {
		if !strings.EqualFold(prop.Type, "string") {
			return false
		}
		stringProps++
		if patchParameterNames[strings.ToLower(strings.TrimSpace(name))] {
			patchProps++
		}
	}
	return stringProps == 1 && patchProps == 1
}

// toolShapeIsCommandLike reports whether a declared tool's parameter
// schema exposes a string command property (`cmd` or `command`),
// matching the codex-rs shell tools.
func toolShapeIsCommandLike(schema toolParameterSchema) bool {
	for name, prop := range schema.Properties {
		if commandParameterNames[strings.ToLower(strings.TrimSpace(name))] && strings.EqualFold(prop.Type, "string") {
			return true
		}
	}
	return false
}
