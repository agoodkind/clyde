package openai

import (
	"encoding/json"
	"sort"
)

// RequestDiscovery describes the shape of a chat completions request body for
// research purposes. It captures every top-level key that arrived, separates
// known schema keys from unknown extension keys, and surfaces nested
// extension surfaces (metadata, message-level fields, tool-level fields).
//
// This is a diagnostic type. It is logged once per inbound request to
// produce empirical evidence of what Cursor server actually sends.
type RequestDiscovery struct {
	TopLevelKeys         []string `json:"top_level_keys"`
	UnknownKeys          []string `json:"unknown_keys"`
	MetadataKeys         []string `json:"metadata_keys,omitempty"`
	MetadataIsObject     bool     `json:"metadata_is_object,omitempty"`
	MessageFieldKeys     []string `json:"message_field_keys,omitempty"`
	InputItemKeys        []string `json:"input_item_keys,omitempty"`
	InputItemRoles       []string `json:"input_item_roles,omitempty"`
	InputItemTypes       []string `json:"input_item_types,omitempty"`
	InputContentTypes    []string `json:"input_content_types,omitempty"`
	ToolCount            int      `json:"tool_count"`
	ToolKinds            []string `json:"tool_kinds,omitempty"`
	ToolFieldKeys        []string `json:"tool_field_keys,omitempty"`
	ToolFunctionKeys     []string `json:"tool_function_keys,omitempty"`
	ToolFunctionTopKeys  []string `json:"tool_function_top_keys,omitempty"`
	ToolCustomTopKeys    []string `json:"tool_custom_top_keys,omitempty"`
	ToolCustomFormatKeys []string `json:"tool_custom_format_keys,omitempty"`
	ToolFunctionNames    []string `json:"tool_function_names,omitempty"`
	ToolCustomNames      []string `json:"tool_custom_names,omitempty"`
	MCPToolNames         []string `json:"mcp_tool_names,omitempty"`
	HasMCPLikeFields     bool     `json:"has_mcp_like_fields,omitempty"`
	MCPLikeFieldNames    []string `json:"mcp_like_field_names,omitempty"`
	BodyBytes            int      `json:"body_bytes"`
}

// HeaderNames returns a sorted list of HTTP header names from the supplied
// header set, lowercased. Values are not retained, so credential-bearing
// headers can be inspected for presence without leaking secrets.
func HeaderNames(h map[string][]string) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, lowercaseASCII(k))
	}
	sort.Strings(out)
	return out
}

// known mirrors the json tags on ChatRequest. Update this list whenever a
// new field is added to ChatRequest. A drift check would be nice but the
// list is short enough to maintain by hand for now.
var knownChatRequestKeys = map[string]bool{
	"model":                  true,
	"messages":               true,
	"input":                  true,
	"stream":                 true,
	"stream_options":         true,
	"reasoning_effort":       true,
	"reasoning":              true,
	"tools":                  true,
	"tool_choice":            true,
	"functions":              true,
	"function_call":          true,
	"n":                      true,
	"user":                   true,
	"temperature":            true,
	"top_p":                  true,
	"max_tokens":             true,
	"max_completion_tokens":  true,
	"max_output_tokens":      true,
	"presence_penalty":       true,
	"frequency_penalty":      true,
	"logit_bias":             true,
	"logprobs":               true,
	"top_logprobs":           true,
	"stop":                   true,
	"seed":                   true,
	"response_format":        true,
	"audio":                  true,
	"modalities":             true,
	"parallel_tool_calls":    true,
	"store":                  true,
	"metadata":               true,
	"include":                true,
	"service_tier":           true,
	"text":                   true,
	"truncation":             true,
	"prompt_cache_retention": true,
}

// DiscoverRequest reads the raw chat completions body and produces a
// RequestDiscovery describing every key that appeared. It does not retain
// any payload values. It is safe to log the result.
func DiscoverRequest(body []byte) RequestDiscovery {
	disc := RequestDiscovery{BodyBytes: len(body), TopLevelKeys: nil, UnknownKeys: nil, MetadataKeys: nil, MetadataIsObject: false, MessageFieldKeys: nil, InputItemKeys: nil, InputItemRoles: nil, InputItemTypes: nil, InputContentTypes: nil, ToolCount: 0, ToolKinds: nil, ToolFieldKeys: nil, ToolFunctionKeys: nil, ToolFunctionTopKeys: nil, ToolCustomTopKeys: nil, ToolCustomFormatKeys: nil, ToolFunctionNames: nil, ToolCustomNames: nil, MCPToolNames: nil, HasMCPLikeFields: false, MCPLikeFieldNames: nil}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return disc
	}
	disc.TopLevelKeys = sortedKeys(top)
	for _, key := range disc.TopLevelKeys {
		if _, ok := knownChatRequestKeys[key]; !ok {
			disc.UnknownKeys = append(disc.UnknownKeys, key)
		}
	}
	if raw, ok := top["metadata"]; ok && len(raw) > 0 && string(raw) != "null" {
		var asObject map[string]json.RawMessage
		if err := json.Unmarshal(raw, &asObject); err == nil {
			disc.MetadataIsObject = true
			disc.MetadataKeys = sortedKeys(asObject)
		}
	}
	if raw, ok := top["input"]; ok && len(raw) > 0 {
		ii := inspectInput(raw)
		disc.InputItemKeys = ii.itemKeys
		disc.InputItemRoles = ii.roles
		disc.InputItemTypes = ii.types
		disc.InputContentTypes = ii.contentTypes
	}
	if raw, ok := top["tools"]; ok && len(raw) > 0 {
		ti := inspectTools(raw)
		disc.ToolCount = ti.count
		disc.ToolKinds = ti.kinds
		disc.ToolFunctionTopKeys = ti.functionTopKeys
		disc.ToolCustomTopKeys = ti.customTopKeys
		disc.ToolCustomFormatKeys = ti.customFormatKeys
		disc.ToolFunctionNames = ti.functionNames
		disc.ToolCustomNames = ti.customNames
		disc.MCPToolNames = ti.mcpNames
	}
	disc.MCPLikeFieldNames = collectMCPLikeNames(disc.UnknownKeys, disc.MetadataKeys, disc.ToolFunctionTopKeys, disc.ToolCustomTopKeys, disc.ToolFunctionNames, disc.ToolCustomNames)
	disc.HasMCPLikeFields = len(disc.MCPLikeFieldNames) > 0
	return disc
}

type inputInspect struct {
	itemKeys     []string
	roles        []string
	types        []string
	contentTypes []string
}

// inspectInput walks the Responses-API style `input` array. Each item has at
// least a `role` plus either a `content` array (system/user) or a `type` like
// `function_call` / `function_call_output`. Content elements have their own
// `type` (input_text, output_text, image, etc).
func inspectInput(raw json.RawMessage) inputInspect {
	out := inputInspect{itemKeys: nil, roles: nil, types: nil, contentTypes: nil}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return out
	}
	keySet := map[string]bool{}
	roleSet := map[string]bool{}
	typeSet := map[string]bool{}
	contentTypeSet := map[string]bool{}
	for _, elem := range arr {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(elem, &item); err != nil {
			continue
		}
		for k := range item {
			keySet[k] = true
		}
		var probe struct {
			Role    string            `json:"role"`
			Type    string            `json:"type"`
			Content []json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(elem, &probe); err == nil {
			if probe.Role != "" {
				roleSet[probe.Role] = true
			}
			if probe.Type != "" {
				typeSet[probe.Type] = true
			}
			for _, c := range probe.Content {
				var cprobe struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(c, &cprobe); err == nil && cprobe.Type != "" {
					contentTypeSet[cprobe.Type] = true
				}
			}
		}
	}
	out.itemKeys = sortedSet(keySet)
	out.roles = sortedSet(roleSet)
	out.types = sortedSet(typeSet)
	out.contentTypes = sortedSet(contentTypeSet)
	return out
}

type toolsInspect struct {
	count            int
	kinds            []string
	functionTopKeys  []string
	customTopKeys    []string
	customFormatKeys []string
	functionNames    []string
	customNames      []string
	mcpNames         []string
}

// inspectTools walks the Responses-API style `tools` array. Tools are flat
// objects with `type` plus `name`/`description`/`parameters` directly on the
// element. There is no `function` sub-object. Custom tools carry a `format`
// object instead of `parameters`. MCP tools come through as `function` type
// with names matching the Cursor MCP convention (`CallMcpTool`,
// `FetchMcpResource`).
// toolInspectAccumulator collects the running set state inspectTools
// builds while walking a Tools array. It exists to keep inspectTools
// flat and shareable with the per-element folder.
type toolInspectAccumulator struct {
	kinds            map[string]bool
	functionTopKeys  map[string]bool
	customTopKeys    map[string]bool
	customFormatKeys map[string]bool
	functionNames    map[string]bool
	customNames      map[string]bool
	mcpNames         map[string]bool
}

func newToolInspectAccumulator() toolInspectAccumulator {
	return toolInspectAccumulator{
		kinds:            map[string]bool{},
		functionTopKeys:  map[string]bool{},
		customTopKeys:    map[string]bool{},
		customFormatKeys: map[string]bool{},
		functionNames:    map[string]bool{},
		customNames:      map[string]bool{},
		mcpNames:         map[string]bool{},
	}
}

func inspectTools(raw json.RawMessage) toolsInspect {
	out := toolsInspect{count: 0, kinds: nil, functionTopKeys: nil, customTopKeys: nil, customFormatKeys: nil, functionNames: nil, customNames: nil, mcpNames: nil}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return out
	}
	out.count = len(arr)
	acc := newToolInspectAccumulator()
	for _, elem := range arr {
		acc.foldElement(elem)
	}
	out.kinds = sortedSet(acc.kinds)
	out.functionTopKeys = sortedSet(acc.functionTopKeys)
	out.customTopKeys = sortedSet(acc.customTopKeys)
	out.customFormatKeys = sortedSet(acc.customFormatKeys)
	out.functionNames = sortedSet(acc.functionNames)
	out.customNames = sortedSet(acc.customNames)
	out.mcpNames = sortedSet(acc.mcpNames)
	return out
}

// foldElement folds one Tools-array element into the accumulator,
// dispatching on the element's "type" discriminator.
func (a *toolInspectAccumulator) foldElement(elem json.RawMessage) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(elem, &top); err != nil {
		return
	}
	var probe struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Format json.RawMessage `json:"format"`
	}
	_ = json.Unmarshal(elem, &probe)
	if probe.Type != "" {
		a.kinds[probe.Type] = true
	}
	switch openAIToolWireType(probe.Type) {
	case openAIToolWireTypeFunction:
		a.foldFunctionTool(top, probe.Name)
	case openAIToolWireTypeCustom:
		a.foldCustomTool(top, probe.Name, probe.Format)
	case openAIToolWireTypeEmpty:
		// Empty type means the caller did not supply one; defaults
		// have already been applied by the kinds tally above.
	}
}

// foldFunctionTool records the top-level keys and name of one
// `type:"function"` tool entry, marking MCP-like names.
func (a *toolInspectAccumulator) foldFunctionTool(top map[string]json.RawMessage, name string) {
	for k := range top {
		a.functionTopKeys[k] = true
	}
	if name == "" {
		return
	}
	a.functionNames[name] = true
	if isMCPLikeToolName(name) {
		a.mcpNames[name] = true
	}
}

// foldCustomTool records the top-level keys, name, and format-object
// keys of one `type:"custom"` tool entry.
func (a *toolInspectAccumulator) foldCustomTool(top map[string]json.RawMessage, name string, format json.RawMessage) {
	for k := range top {
		a.customTopKeys[k] = true
	}
	if name != "" {
		a.customNames[name] = true
	}
	if len(format) == 0 || string(format) == "null" {
		return
	}
	var fmtObj map[string]json.RawMessage
	if err := json.Unmarshal(format, &fmtObj); err != nil {
		return
	}
	for k := range fmtObj {
		a.customFormatKeys[k] = true
	}
}

func isMCPLikeToolName(name string) bool {
	lower := lowercaseASCII(name)
	for _, needle := range []string{"mcp", "callmcp", "fetchmcp"} {
		if containsASCII(lower, needle) {
			return true
		}
	}
	return false
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// collectMCPLikeNames sweeps the named key sets and returns any that look
// MCP-related. Heuristic: substring match on "mcp", "subagent", "agent_mode",
// "background", "resume", "workspace", "cursor".
func collectMCPLikeNames(sets ...[]string) []string {
	needles := []string{"mcp", "subagent", "agent_mode", "background", "resume", "workspace", "cursor"}
	seen := map[string]bool{}
	for _, set := range sets {
		for _, k := range set {
			lower := lowercaseASCII(k)
			for _, needle := range needles {
				if containsASCII(lower, needle) {
					seen[k] = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func lowercaseASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func containsASCII(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
