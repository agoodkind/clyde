package codex

import (
	"log/slog"
	"strings"

	"goodkind.io/clyde/codexwire"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// declaredCustomTools is the set of tool names the client declared as
// OpenAI custom (freeform) tools on this request. A custom tool's
// payload is raw text rather than JSON arguments, so both the outbound
// tool spec and the replay of a prior call use the codex
// custom_tool_call shape. Cursor echoes prior custom calls back inside
// `tool_calls` with type "function", so this declared set is the only
// reliable classifier for the replay.
type declaredCustomTools map[string]bool

func newDeclaredCustomTools(tools []adapteropenai.Tool) declaredCustomTools {
	names := make(declaredCustomTools, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		if !adapteropenai.ToolIsCustom(tool) {
			// A name declared as a function tool is never replayed as a
			// custom call, even when a same-named custom declaration also
			// exists. Tool names are unique in a well-formed request, so
			// a collision is a malformed client; resolving it toward the
			// function shape keeps ordinary JSON arguments intact.
			names[name] = false
			continue
		}
		if _, seen := names[name]; !seen {
			names[name] = true
		}
	}
	return names
}

// has reports whether the client declared a custom tool under this name.
func (d declaredCustomTools) has(name string) bool {
	if len(d) == 0 {
		return false
	}
	return d[strings.TrimSpace(name)]
}

// toolSpecs renders the client's declared tools onto the codex
// Responses `tools` array, keeping each declaration's own variant. A
// custom tool forwarded as a parameterless function tool would leave
// the model no place to put its payload, which is what produced empty
// ApplyPatch calls on the Cursor native-GPT route.
func toolSpecs(req adapteropenai.ChatRequest) []codexwire.ToolSpec {
	tools := requestTools(req)
	if len(tools) == 0 {
		return nil
	}
	out := make([]codexwire.ToolSpec, 0, len(tools))
	for _, tool := range tools {
		// The tool name passes through verbatim: it is opaque
		// application content owned by whoever declared the tool, so
		// clyde does not rewrite it into a codex-internal vocabulary.
		name := strings.TrimSpace(tool.Function.Name)
		if adapteropenai.ToolIsCustom(tool) {
			format := codexToolFormat(tool.Format)
			logUnconstrainedCustomTool(name, format)
			out = append(out, CustomToolSpec(name, tool.Function.Description, format))
			continue
		}
		out = append(out, FunctionToolSpec(name, tool.Function.Description, tool.Function.Parameters, tool.Function.Strict))
	}
	return out
}

// codexToolFormat projects the adapter-side custom-tool format onto the
// codex wire format. A nil format stays nil: a custom tool may declare
// none, and the upstream then treats the payload as unconstrained text.
func codexToolFormat(format *adapteropenai.ToolGrammarFormat) *codexwire.ToolFormat {
	if format == nil {
		return nil
	}
	out := &codexwire.ToolFormat{Type: codexwire.ToolFormatTypeText, Syntax: "", Definition: ""}
	switch format.Type {
	case adapteropenai.ToolGrammarFormatGrammar:
		out.Type = codexwire.ToolFormatTypeGrammar
		out.Syntax = format.Syntax
		out.Definition = format.Definition
	case adapteropenai.ToolGrammarFormatText:
		out.Type = codexwire.ToolFormatTypeText
	default:
		// An unrecognized format type degrades to unconstrained text
		// rather than forwarding a value the upstream may reject.
		out.Type = codexwire.ToolFormatTypeText
	}
	return out
}

// logUnconstrainedCustomTool reports a client-declared custom tool that
// reaches the upstream with no usable payload constraint. A grammar
// format carrying no definition, or no format at all, leaves the model
// free to emit anything, which is how an empty ApplyPatch payload
// previously reached Cursor unnoticed. Only names and type
// discriminators are logged; the description and the grammar body are
// request content and stay out.
func logUnconstrainedCustomTool(name string, format *codexwire.ToolFormat) {
	hasDefinition := format != nil && strings.TrimSpace(format.Definition) != ""
	if hasDefinition {
		return
	}
	formatType := ""
	if format != nil {
		formatType = string(format.Type)
	}
	slog.Warn("adapter.codex.custom_tool_unconstrained",
		"concern", "adapter.chat.dispatch",
		"component", "adapter",
		"subcomponent", "codex",
		"tool_name", name,
		"declared_type", string(codexwire.ToolSpecTypeCustom),
		"format_type", formatType,
		"has_format", format != nil,
		"has_definition", hasDefinition,
	)
}
