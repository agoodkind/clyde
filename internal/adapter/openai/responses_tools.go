package openai

import (
	"encoding/json"
	"strings"
)

// responsesToolLabelUnnamed labels a dropped entry that carries no usable
// type discriminator and no function shape.
const responsesToolLabelUnnamed = "unnamed"

// responsesToolLabelMalformed labels a dropped entry whose JSON could not
// be decoded as a tool object.
const responsesToolLabelMalformed = "malformed"

// responsesToolLabelUnsupported labels a dropped entry whose type is not a
// recognized OpenAI built-in or custom tool kind. Clamping unrecognized
// types to this fixed label keeps an arbitrary client-supplied type string
// out of the compatibility warning, because warnings must not carry request
// values.
const responsesToolLabelUnsupported = "unsupported"

// knownDroppableToolTypes is the set of recognized OpenAI Responses built-in
// and custom tool kinds Clyde drops when projecting to a chat-completions
// provider. Only these stable discriminators are safe to report verbatim in
// a dropped-tool warning label; any other type clamps to "unsupported".
var knownDroppableToolTypes = map[string]bool{
	"web_search":           true,
	"web_search_preview":   true,
	"file_search":          true,
	"computer_use":         true,
	"computer_use_preview": true,
	"code_interpreter":     true,
	"image_generation":     true,
	"local_shell":          true,
	"mcp":                  true,
	"custom":               true,
}

// responsesToolEnvelope is the lenient view of one Responses `tools`
// entry. It mirrors the fields Tool.UnmarshalJSON reads, but it is decoded
// per element without the strict type rejection so built-in and custom
// tools decode into fields the classifier can inspect instead of failing
// the whole request. The Responses tools array is an external contract
// that mixes client function tools with OpenAI built-ins, so it stays raw
// until this classifier splits it.
type responsesToolEnvelope struct {
	Type        string              `json:"type"`
	Function    *ToolFunctionSchema `json:"function"`
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Parameters  json.RawMessage     `json:"parameters"`
	InputSchema json.RawMessage     `json:"input_schema"`
	Strict      *bool               `json:"strict"`
}

// SplitResponsesTools leniently parses a Responses `tools` array. It keeps
// function tools as typed Tool values (client-owned functions) and reports
// the tool `type` label of every entry it drops (built-in / custom tools
// Clyde cannot project to a chat-completions provider), so the caller can
// forward the function tools and warn about the dropped ones. A nil or
// empty raw yields (nil, nil). Malformed entries are dropped and reported
// with the label "malformed".
func SplitResponsesTools(raw json.RawMessage) (functionTools []Tool, droppedTypes []string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, nil
	}
	for _, entry := range entries {
		var env responsesToolEnvelope
		if err := json.Unmarshal(entry, &env); err != nil {
			droppedTypes = append(droppedTypes, responsesToolLabelMalformed)
			continue
		}
		if envelopeIsFunctionTool(env) {
			functionTools = append(functionTools, toolFromEnvelope(env))
			continue
		}
		droppedTypes = append(droppedTypes, droppedToolLabel(env))
	}
	return functionTools, droppedTypes
}

// envelopeIsFunctionTool reports whether a decoded entry is a client
// function tool. An entry qualifies when its type is "function", when it
// carries a nested function object, or when it carries a top-level name
// plus a parameters or input_schema schema and no non-function type. Any
// other entry (web_search, file_search, computer_use, mcp, custom) is a
// built-in or custom tool the chat-completions projection drops.
func envelopeIsFunctionTool(env responsesToolEnvelope) bool {
	if env.Type == string(openAIToolWireTypeFunction) {
		return true
	}
	if env.Function != nil {
		return true
	}
	if env.Type != string(openAIToolWireTypeEmpty) {
		return false
	}
	if env.Name == "" {
		return false
	}
	return len(env.Parameters) > 0 || len(env.InputSchema) > 0
}

// toolFromEnvelope builds a typed function Tool from a decoded entry. It
// reuses the same nested-vs-flattened field mapping Tool.UnmarshalJSON
// applies, without calling the strict unmarshal that rejects built-ins.
func toolFromEnvelope(env responsesToolEnvelope) Tool {
	if env.Function != nil {
		return Tool{Type: string(openAIToolWireTypeFunction), Function: *env.Function, Format: nil}
	}
	parameters := env.Parameters
	if len(parameters) == 0 {
		parameters = env.InputSchema
	}
	return Tool{
		Type: string(openAIToolWireTypeFunction),
		Function: ToolFunctionSchema{
			Name:        env.Name,
			Description: env.Description,
			Parameters:  parameters,
			Strict:      env.Strict,
		},
		Format: nil,
	}
}

// droppedToolLabel returns the reported label for a dropped entry. It is the
// entry's type value only when that value is a recognized tool kind (e.g.
// "web_search", "custom"), "unsupported" when the type is present but not
// recognized, or "unnamed" when the entry has no usable type discriminator.
// Unrecognized types clamp to the fixed label so a compatibility warning
// never carries an arbitrary client-supplied string.
func droppedToolLabel(env responsesToolEnvelope) string {
	if env.Type == string(openAIToolWireTypeEmpty) {
		return responsesToolLabelUnnamed
	}
	if knownDroppableToolTypes[env.Type] {
		return env.Type
	}
	return responsesToolLabelUnsupported
}
