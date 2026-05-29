package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// Request is the Cursor-focused translation layer that sits on top of the
// OpenAI-compatible request shape. The root adapter still accepts OpenAI wire
// JSON, but the rest of the adapter can use this type when behavior depends on
// Cursor-specific conventions rather than generic OpenAI semantics.
type Request struct {
	OpenAI          adapteropenai.ChatRequest
	User            string
	RequestID       string
	ConversationID  string
	GenerationID    string
	WorkspacePath   string
	NormalizedModel string
	Metadata        map[string]json.RawMessage
	RawToolNames    []string
	Mode            Mode
	CanSwitchMode   bool
	CanSpawnAgent   bool
	PathKind        RequestPathKind

	// Tool-presence flags grounded in empirical 2026-04-27 captures.
	// Cursor signals product semantics by which tools are available
	// in the request, not by separate request fields. The classifier
	// in TranslateRequest sets these from the raw tool name list.
	HasSubagentTool    bool
	HasSwitchModeTool  bool
	HasAskQuestionTool bool
	HasCreatePlanTool  bool
	HasApplyPatchTool  bool

	// MCPToolNames lists every function tool whose name matches the
	// Cursor MCP convention. Today: CallMcpTool, FetchMcpResource,
	// plus any future MCP-prefixed names. Custom tools (ApplyPatch
	// etc.) are not MCP and are tracked via the Has*Tool flags above.
	MCPToolNames []string
}

// TranslateRequest derives Cursor-specific metadata from an OpenAI-compatible
// request without changing the underlying request payload.
func TranslateRequest(req adapteropenai.ChatRequest) Request {
	translated := Request{
		OpenAI:         req,
		User:           strings.TrimSpace(req.User),
		RequestID:      metadataString(req.Metadata, "cursorRequestId"),
		ConversationID: metadataString(req.Metadata, "cursorConversationId"),
		GenerationID: metadataString(req.Metadata,
			"cursorGenerationId",
			"cursor_generation_id",
			"generationId",
			"generation_id",
			"requestId",
			"request_id",
		),
		WorkspacePath:   workspacePath(req),
		NormalizedModel: NormalizeModelAlias(req.Model),
		Metadata:        metadataMap(req.Metadata), RawToolNames: nil, Mode: "", CanSwitchMode: false, CanSpawnAgent: false, PathKind: "", HasSubagentTool: false, HasSwitchModeTool: false, HasAskQuestionTool: false, HasCreatePlanTool: false, HasApplyPatchTool: false, MCPToolNames: nil,
	}
	translated.RawToolNames = rawToolNames(req)
	translated.Mode = requestMode(translated.RawToolNames)
	translated.CanSwitchMode = hasRawToolName(translated.RawToolNames, "SwitchMode")
	translated.CanSpawnAgent = hasRawToolName(translated.RawToolNames, "Subagent") || hasRawToolName(translated.RawToolNames, "Task")
	translated.HasSubagentTool = translated.CanSpawnAgent
	translated.HasSwitchModeTool = translated.CanSwitchMode
	translated.HasAskQuestionTool = hasRawToolName(translated.RawToolNames, "AskQuestion")
	translated.HasCreatePlanTool = hasRawToolName(translated.RawToolNames, "CreatePlan")
	translated.HasApplyPatchTool = hasRawToolName(translated.RawToolNames, "ApplyPatch")
	translated.MCPToolNames = collectMCPToolNames(translated.RawToolNames)
	translated.PathKind = RequestPath(translated)
	return translated
}

// collectMCPToolNames returns the subset of raw tool names that match
// the Cursor MCP convention. Today the canonical names are
// `CallMcpTool` and `FetchMcpResource`; the substring fallback catches
// any future MCP-prefixed function tool without a code change. The
// result preserves the order the names appeared in the request.
func collectMCPToolNames(toolNames []string) []string {
	if len(toolNames) == 0 {
		return nil
	}
	out := make([]string, 0, 2)
	for _, name := range toolNames {
		if isMCPToolName(name) {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// mcpToolName enumerates the Cursor-emitted MCP tool aliases the
// adapter recognizes as MCP routing entry points.
type mcpToolName string

const (
	mcpToolCallTool      mcpToolName = "CallMcpTool"
	mcpToolFetchResource mcpToolName = "FetchMcpResource"
)

func isMCPToolName(name string) bool {
	switch mcpToolName(name) {
	case mcpToolCallTool, mcpToolFetchResource:
		return true
	}
	lowered := strings.ToLower(name)
	return strings.Contains(lowered, "mcp")
}

// Context is part of Clyde's typed adapter surface.
func (r Request) Context() Context {
	return Context{
		User:           r.User,
		RequestID:      r.RequestID,
		ConversationID: r.ConversationID,
		WorkspacePath:  r.WorkspacePath,
	}
}

func metadataString(raw json.RawMessage, keys ...string) string {
	m := metadataMap(raw)
	if len(m) == 0 {
		return ""
	}
	for _, key := range keys {
		if v, ok := m[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func metadataMap(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

func metadataHasAny(m map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		var asBool bool
		if json.Unmarshal(raw, &asBool) == nil {
			if asBool {
				return true
			}
			continue
		}
		var asString string
		if json.Unmarshal(raw, &asString) == nil {
			trimmed := strings.TrimSpace(asString)
			if trimmed != "" && trimmed != "false" {
				return true
			}
			continue
		}
		var asNumber float64
		if json.Unmarshal(raw, &asNumber) == nil {
			if asNumber != 0 {
				return true
			}
			continue
		}
		if string(raw) != "null" {
			return true
		}
	}
	return false
}

func rawToolNames(req adapteropenai.ChatRequest) []string {
	names := make([]string, 0, len(req.Tools)+len(req.Functions))
	for _, tool := range req.Tools {
		if name := strings.TrimSpace(tool.Function.Name); name != "" {
			names = append(names, name)
		}
	}
	for _, fn := range req.Functions {
		if name := strings.TrimSpace(fn.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func requestMode(toolNames []string) Mode {
	if hasRawToolName(toolNames, "CreatePlan") {
		return ModePlan
	}
	return ModeAgent
}

// DeriveChatKey returns a stable per-chat identifier for transcripts when
// Cursor's request body and headers carry no native conversation id.
//
// Cursor BYOK does not surface a chat-level identifier. The body has model,
// messages, tools, user (per-account), stream. Headers carry cf-warp-tag-id
// (per-installation), traceparent (per-request), and the x-clyde-* trace
// headers we inject ourselves. Empirically, the first user message text and
// the model selection are byte-stable across turns of the same chat.
//
// Hash inputs are deliberately narrow:
//   - model: switching models intentionally yields a new chat key. Acceptable.
//   - first user-role message content: stable across turns; what the operator
//     actually typed. Two distinct chats that begin with identical first
//     messages will collide. The collision is a known limitation.
//
// System prompt is excluded because Cursor injects per-turn context
// (timestamps, current files, lint errors) into the system prompt, so it is
// not stable across turns. The first 200 characters are stable but encode the
// model name; using only model + first user message avoids that coupling.
//
// Returns the empty string when no first user message is present, leaving the
// caller to skip transcript routing for that record.
func DeriveChatKey(req adapteropenai.ChatRequest) string {
	firstUser := firstUserMessageContent(req.Messages)
	if firstUser == "" {
		return ""
	}
	model := strings.TrimSpace(req.Model)
	hasher := sha256.New()
	hasher.Write([]byte(model))
	hasher.Write([]byte{0})
	hasher.Write([]byte(firstUser))
	sum := hasher.Sum(nil)
	return hex.EncodeToString(sum[:8])
}

// firstUserMessageContent returns the textual content of the first message
// with role == "user" in the slice. Returns the empty string when none exists
// or when the content is not a JSON string.
func firstUserMessageContent(messages []adapteropenai.ChatMessage) string {
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		return decodeUserMessageText(msg.Content)
	}
	return ""
}

// decodeUserMessageText extracts user-typed text from a Cursor message body.
// Two shapes are supported. The plain shape encodes the message as a JSON
// string. The multipart shape encodes it as a JSON array of typed parts where
// the first text-shaped part wins.
func decodeUserMessageText(content json.RawMessage) string {
	var plain string
	if err := json.Unmarshal(content, &plain); err == nil && plain != "" {
		return plain
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	for _, part := range parts {
		text := firstTextPart(part)
		if text != "" {
			return text
		}
	}
	return ""
}

// firstTextPart returns the text payload of a multipart content entry whose
// type field is "text", or the empty string when the entry is a different
// shape (image_url, audio, tool result).
func firstTextPart(part map[string]json.RawMessage) string {
	typeRaw, ok := part["type"]
	if !ok {
		return ""
	}
	var partType string
	if err := json.Unmarshal(typeRaw, &partType); err != nil || partType != "text" {
		return ""
	}
	textRaw, ok := part["text"]
	if !ok {
		return ""
	}
	var partText string
	if err := json.Unmarshal(textRaw, &partText); err != nil {
		return ""
	}
	return partText
}
