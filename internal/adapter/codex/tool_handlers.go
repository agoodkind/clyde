package codex

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/codexwire"
)

// toolReplyKind is the closed enum of Codex reply-item types that map
// back to a client tool call. Codex answers an OpenAI-public-schema
// request with one of these item types; clyde declares only plain
// function tools, yet Codex's backend can inject native apply_patch and
// local_shell tools server-side, so all three kinds are live.
type toolReplyKind int

const (
	// toolReplyFunction is a plain `function_call`: name and arguments
	// ride through verbatim.
	toolReplyFunction toolReplyKind = iota
	// toolReplyPatch is a freeform `custom_tool_call` (apply_patch): the
	// payload is raw patch text that is unwrapped and cleaned.
	toolReplyPatch
	// toolReplyShell is a `local_shell_call`: a structured action that is
	// rendered under the client's declared shell-tool field names.
	toolReplyShell
)

// toolReplyKindForItemType maps a Codex reply item type to the handler
// kind. The boolean is false for item types that are not tool calls.
func toolReplyKindForItemType(itemType codexwire.ItemType) (toolReplyKind, bool) {
	switch itemType {
	case codexwire.ItemTypeFunctionCall:
		return toolReplyFunction, true
	case codexwire.ItemTypeCustomToolCall:
		return toolReplyPatch, true
	case codexwire.ItemTypeLocalShellCall:
		return toolReplyShell, true
	case codexwire.ItemTypeMessage, codexwire.ItemTypeFunctionCallOutput,
		codexwire.ItemTypeCustomToolCallOutput, codexwire.ItemTypeReasoning:
		return toolReplyFunction, false
	default:
		return toolReplyFunction, false
	}
}

// toolHandlers is the provider-neutral set of reply-item handlers plus
// the chooser. Each handler maps one Codex reply item type back to the
// client-declared tool: the function handler relays verbatim, the patch
// handler unwraps and cleans apply_patch text, and the shell handler
// renders a local_shell action under the client's declared field names.
// The chooser ([toolHandlers.declared]) resolves the client-facing tool
// name and the shell field names by inspecting the declared tools' SHAPE,
// never by matching a hardcoded client tool name.
type toolHandlers struct {
	declared declaredToolResolver
}

func newToolHandlers(declared declaredToolResolver) toolHandlers {
	return toolHandlers{declared: declared}
}

// functionToolName returns the client-facing name for a plain
// function_call. Function calls already carry the client-declared name,
// so the native name passes through; a trimmed empty name falls back to
// the codex function_call item type so the call still has a stable name.
func (h toolHandlers) functionToolName(nativeName string) string {
	if trimmed := strings.TrimSpace(nativeName); trimmed != "" {
		return trimmed
	}
	return string(codexwire.ItemTypeFunctionCall)
}

// patchToolName resolves the client-declared tool name for a freeform
// apply_patch custom_tool_call. It prefers a declared patch-like tool,
// falls back to the native name carried on the item, then to the codex
// apply_patch type when the native name is empty (e.g. on a streaming
// input delta) so the emitted call always has a stable name.
func (h toolHandlers) patchToolName(nativeName string) string {
	if name, ok := h.declared.patchToolName(); ok {
		return name
	}
	if trimmed := strings.TrimSpace(nativeName); trimmed != "" {
		return trimmed
	}
	return codexApplyPatchToolType
}

// customToolReply resolves one custom_tool_call to the client-facing tool
// name, and reports whether the CLIENT declared that tool itself.
//
// An exact name match means the client declared this freeform tool, so the
// call already carries a name the client recognizes and its payload is that
// tool's own content. Anything else is codex's apply_patch tool injected
// server-side, which still maps back to a declared patch-like tool by shape.
func (h toolHandlers) customToolReply(nativeName string) (string, bool) {
	if name, ok := h.declared.declaredCustomToolName(nativeName); ok {
		return name, true
	}
	return h.patchToolName(nativeName), false
}

// shellToolName resolves the client-declared tool name for a native
// local_shell_call. It prefers a declared command-like tool, falling
// back to the codex local_shell_call item type when the declared set is
// ambiguous so the emitted call always has a stable name.
func (h toolHandlers) shellToolName() string {
	if name, ok := h.declared.commandToolName(); ok {
		return name
	}
	return string(codexwire.ItemTypeLocalShellCall)
}

// handleFunctionArguments relays a function_call arguments string back to
// the client unchanged. The arguments are opaque JSON the model emitted,
// so the handler does not reshape them; it returns the input verbatim and
// reports whether there is anything to emit.
func (h toolHandlers) handleFunctionArguments(arguments string) (string, bool) {
	if arguments == "" {
		return "", false
	}
	return arguments, true
}

// handlePatchInput unwraps and cleans a freeform apply_patch payload into
// the raw patch body the client receives. It reuses [ApplyPatchArgs]
// (UnwrapApplyPatchInput + RepairApplyPatchInput) so the vendored
// apply_patch.lark grammar contract is honored.
func (h toolHandlers) handlePatchInput(input string) (string, bool) {
	return ApplyPatchArgs(input)
}

// handleShellAction renders a native local_shell_call action into the
// JSON arguments the client receives, using the field names the client
// declared on its command-like tool (resolved by [toolHandlers.shellFields]).
// The command string, working-directory, and timeout VALUES are unchanged
// from the codex action; only the field NAMES become the client's declared
// names. Returns ("", false) when the action carries no command.
func (h toolHandlers) handleShellAction(item codexwire.InputItem) (string, bool) {
	return shellArgsFromLocalShellItem(item, h.shellFields())
}

// shellFields are the property names the client declared for the three
// roles a shell tool exposes: the command string, the working directory,
// and the timeout. Each is read off the declared command-like tool's
// schema. An empty role name means the client did not declare that field,
// so the shell handler omits it rather than inventing one.
type shellFields struct {
	command string
	workdir string
	timeout string
}

// shellWorkdirParameterNames are the property aliases a command-like tool
// may expose for its working directory, grounded in the codex-rs shell
// tool parameter names plus the aliases [codexwire.LocalShellAction.WorkingDir]
// already accepts (research/codex/codex-rs/core/src/tools/handlers/shell_spec.rs).
var shellWorkdirParameterNames = map[string]bool{
	"workdir":           true,
	"working_directory": true,
	"cwd":               true,
}

// shellTimeoutParameterNames are the property aliases a command-like tool
// may expose for its command timeout, grounded in the codex-rs shell tool
// parameter name `timeout_ms`.
var shellTimeoutParameterNames = map[string]bool{
	"timeout_ms": true,
	"timeout":    true,
}

// shellFields resolves the declared field names for the command-like tool
// the chooser picks. When no command-like tool is declared (the sole-tool
// or ambiguous fallback), the codex-rs shell field names are used so the
// emitted shape matches the codex shell tool the call originated from.
func (h toolHandlers) shellFields() shellFields {
	schema, ok := h.declared.commandToolSchema()
	out := shellFields{command: "command", workdir: "workdir", timeout: "timeout_ms"}
	if !ok {
		return out
	}
	out = shellFields{command: "", workdir: "", timeout: ""}
	for name := range schema.Properties {
		lower := strings.ToLower(strings.TrimSpace(name))
		switch {
		case commandParameterNames[lower] && out.command == "":
			out.command = strings.TrimSpace(name)
		case shellWorkdirParameterNames[lower] && out.workdir == "":
			out.workdir = strings.TrimSpace(name)
		case shellTimeoutParameterNames[lower] && out.timeout == "":
			out.timeout = strings.TrimSpace(name)
		}
	}
	if out.command == "" {
		// The chooser matched this tool as command-like, so a command
		// property exists; keep the codex-rs default name as a guard.
		out.command = "command"
	}
	return out
}

// shellArgsFromLocalShellItem renders the JSON arguments for a codex
// native local_shell_call output item under the declared field names in
// fields. local_shell_call is a codex-internal item TYPE whose payload is
// a structured `action` object (argv array plus working_directory), not a
// JSON arguments string the model emitted, so it needs type-keyed
// conversion rather than opaque passthrough. The command VALUE is the
// flattened argv (research/codex/codex-rs/core/src/tools/handlers/shell_spec.rs
// create_shell_command_tool); the working-directory and timeout VALUES are
// unchanged. Fields the client did not declare (empty name in fields) are
// omitted. Returns ("", false) when the action is empty or carries no
// command.
func shellArgsFromLocalShellItem(item codexwire.InputItem, fields shellFields) (string, bool) {
	if len(item.Action) == 0 {
		return "", false
	}
	var action codexwire.LocalShellAction
	if err := json.Unmarshal(item.Action, &action); err != nil {
		return "", false
	}
	command := CommandString(action.Command)
	if command == "" {
		return "", false
	}
	out := make(map[string]json.RawMessage, 3)
	if name := strings.TrimSpace(fields.command); name != "" {
		if encoded, err := json.Marshal(command); err == nil {
			out[name] = encoded
		}
	}
	if name := strings.TrimSpace(fields.workdir); name != "" {
		if cwd := action.WorkingDir(); cwd != "" {
			if encoded, err := json.Marshal(cwd); err == nil {
				out[name] = encoded
			}
		}
	}
	if name := strings.TrimSpace(fields.timeout); name != "" {
		if ms := localShellTimeoutMs(action); ms != nil {
			if encoded, err := json.Marshal(*ms); err == nil {
				out[name] = encoded
			}
		}
	}
	if len(out) == 0 {
		return "", false
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// localShellTimeoutMs picks the timeout value from a local-shell action,
// preferring the explicit timeout_ms and falling back to block_until_ms,
// matching the codex-rs action shape. Returns nil when neither is set.
func localShellTimeoutMs(action codexwire.LocalShellAction) *int64 {
	if action.TimeoutMs != nil {
		ms := *action.TimeoutMs
		return &ms
	}
	if action.BlockUntilMs != nil {
		ms := *action.BlockUntilMs
		return &ms
	}
	return nil
}
