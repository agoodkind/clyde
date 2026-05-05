package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	adaptercontent "goodkind.io/clyde/internal/adapter/content"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// GetwdFn lets tests control workspace path rewriting.
var GetwdFn = os.Getwd

func MessageContent(role, textType, text string) map[string]any {
	return MessageContentItems(role, []map[string]any{{
		"type": textType,
		"text": text,
	}})
}

func MessageContentItems(role string, content []map[string]any) map[string]any {
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": content,
	}
}

func codexContentFromRaw(raw json.RawMessage, textType string, strategy adapterrender.MaterializationStrategy) []map[string]any {
	parts, _ := adaptercontent.NormalizeRaw(raw)
	return codexContentFromParts(parts, textType, strategy)
}

func codexContentFromAny(raw any, textType string, strategy adapterrender.MaterializationStrategy) []map[string]any {
	if raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return codexContentFromRaw(json.RawMessage(b), textType, strategy)
}

func codexContentFromParts(parts []adaptercontent.Part, textType string, strategy adapterrender.MaterializationStrategy) []map[string]any {
	content := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case adaptercontent.PartText:
			text := strings.TrimSpace(SanitizeForUpstreamCacheWithStrategy(part.Text, strategy))
			if text == "" {
				continue
			}
			content = append(content, map[string]any{
				"type": textType,
				"text": text,
			})
		case adaptercontent.PartImage:
			if part.Image == nil || strings.TrimSpace(part.Image.URL) == "" {
				continue
			}
			// Codex app-server ContentItem uses a string `image_url` for
			// input_image parts; see research/codex/.../ContentItem.ts.
			item := map[string]any{
				"type":      "input_image",
				"image_url": strings.TrimSpace(part.Image.URL),
			}
			if detail := strings.TrimSpace(part.Image.Detail); detail != "" {
				item["detail"] = detail
			}
			content = append(content, item)
		case adaptercontent.PartRefusal:
			text := strings.TrimSpace(SanitizeForUpstreamCacheWithStrategy(part.Refusal, strategy))
			if text == "" {
				continue
			}
			content = append(content, map[string]any{
				"type": textType,
				"text": text,
			})
		}
	}
	return content
}

type RequestBuilderConfig struct {
	ReasoningSummary string
	// InboundThinkingMaterialization picks how round-tripped synthetic
	// thinking envelopes on assistant content are shaped before forwarding
	// upstream. Empty string falls through to [adapterrender.MaterializeDrop]
	// which is the Codex default (the Codex Responses API has no native
	// thinking content block).
	InboundThinkingMaterialization adapterrender.MaterializationStrategy
}

func BuildRequestWithConfig(req adapteropenai.ChatRequest, model adaptermodel.ResolvedModel, effort string, cfg RequestBuilderConfig) HTTPTransportRequest {
	strategy := cfg.InboundThinkingMaterialization
	if strategy == "" {
		strategy = adapterrender.MaterializeDrop
	}
	cursorReq := adaptercursor.TranslateRequest(req)
	input := make([]map[string]any, 0, len(req.Messages))
	systemSections := make([]string, 0, 8)
	modelName := strings.TrimSpace(model.ClaudeModel)
	if modelName == "" {
		modelName = model.Alias
	}
	workspacePath := cursorReq.WorkspacePath
	if rawInput, ok := inputFromResponsesInput(req.Input, workspacePath, &systemSections, strategy); ok {
		input = rawInput
	} else {
		for _, msg := range req.Messages {
			text := strings.TrimSpace(SanitizeForUpstreamCacheWithStrategy(adaptercontent.FlattenRaw(msg.Content), strategy))
			switch strings.ToLower(msg.Role) {
			case "system", "developer":
				if text != "" {
					systemSections = append(systemSections, text)
				}
				continue
			case "assistant":
				for _, tc := range msg.ToolCalls {
					if strings.TrimSpace(tc.Function.Name) == "" {
						continue
					}
					input = append(input, FunctionCallItem(tc))
				}
				if content := codexContentFromRaw(msg.Content, "output_text", strategy); len(content) > 0 {
					input = append(input, MessageContentItems("assistant", content))
				}
			case "tool", "function":
				if text != "" && strings.TrimSpace(msg.ToolCallID) != "" {
					input = append(input, FunctionCallOutputItem(msg.ToolCallID, text))
				} else if text != "" {
					input = append(input, MessageContent("user", "input_text", "tool: "+text))
				}
			default:
				if content := codexContentFromRaw(msg.Content, "input_text", strategy); len(content) > 0 {
					input = append(input, MessageContentItems("user", content))
				}
			}
		}
	}
	instructions := strings.TrimSpace(strings.Join(systemSections, "\n\n"))
	if base := strings.TrimSpace(model.Instructions); base != "" {
		if instructions == "" {
			instructions = base
		} else {
			instructions = base + "\n\n" + instructions
		}
	}
	if len(input) == 0 {
		input = append(input, MessageContent("user", "input_text", " "))
	}
	reasoning := EffectiveReasoningWithDefaultSummary(req, effort, cfg.ReasoningSummary)
	include := RequestInclude(req.Include, reasoning != nil)
	outputControls := BuildOutputControls(req)
	identity := requestContextIdentity(cursorReq, model.Alias)
	return HTTPTransportRequest{
		Model:        modelName,
		Instructions: instructions,
		// Store MUST be false for ChatGPT Pro Codex. The upstream
		// rejects store=true with "Store must be set to false" on
		// this auth path. Empirical (2026-04-27 capture). This
		// means the adapter cannot use previous_response_id reuse
		// on this provider; the response is never persisted on the
		// upstream side, so any reference to it returns "not
		// found". Cost savings come from the prompt cache
		// (prompt_cache_key) instead, which works independently
		// of stored responses.
		Store:   false,
		Stream:  true,
		Include: include,
		// WARNING: prompt_cache_key and websocket session identity are
		// intentionally not the same field. Codex upstream uses the real
		// conversation/thread id for websocket headers and
		// previous_response_id chaining, while prompt_cache_key is only a
		// cache partition and may be content-derived. Reusing a websocket
		// session from a cache key can cross-wire unrelated Cursor chats
		// that share the same account, first prompt, or cache partition.
		WebsocketSessionKey:  identity.WebsocketSessionKey,
		PromptCache:          identity.PromptCacheKey,
		PromptCacheRetention: outputControls.PromptCacheRetention,
		ServiceTier:          ServiceTierFromRequest(req),
		Reasoning:            reasoning,
		MaxCompletion:        outputControls.MaxCompletion,
		Text:                 outputControls.Text,
		Truncation:           outputControls.Truncation,
		Input:                input,
		Tools:                toolSpecs(req),
		ToolChoice:           "auto",
		ParallelToolCalls:    parallelToolCalls(req),
	}
}

func parallelToolCalls(req adapteropenai.ChatRequest) bool {
	if req.ParallelTools == nil {
		return true
	}
	return *req.ParallelTools
}

func requestTools(req adapteropenai.ChatRequest) []adapteropenai.Tool {
	var tools []adapteropenai.Tool
	if len(req.Tools) > 0 {
		tools = append(tools, req.Tools...)
	} else if len(req.Functions) > 0 {
		for _, fn := range req.Functions {
			tools = append(tools, adapteropenai.Tool{
				Type: "function",
				Function: adapteropenai.ToolFunctionSchema{
					Name:        fn.Name,
					Description: fn.Description,
					Parameters:  fn.Parameters,
				},
			})
		}
	}
	return tools
}

func toolSpecs(req adapteropenai.ChatRequest) []any {
	tools := requestTools(req)
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		toolName := strings.TrimSpace(tool.Function.Name)
		out = append(out, FunctionToolSpec(OutboundToolName(toolName), tool.Function.Description, tool.Function.Parameters, tool.Function.Strict))
	}
	return out
}

func responsesContentText(raw any) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return SanitizeForUpstreamCache(v)
	case []any:
		var parts []string
		for _, part := range v {
			m, _ := part.(map[string]any)
			if m == nil {
				continue
			}
			switch strings.TrimSpace(mapString(m, "type")) {
			case "text", "input_text", "output_text":
				if text := rawString(m, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return SanitizeForUpstreamCache(strings.Join(parts, "\n"))
	case map[string]any:
		switch strings.TrimSpace(mapString(v, "type")) {
		case "text", "input_text", "output_text":
			return SanitizeForUpstreamCache(rawString(v, "text"))
		}
	}
	return ""
}

func responsesOutputText(raw any) string {
	text := responsesContentText(raw)
	if text != "" {
		return text
	}
	switch v := raw.(type) {
	case string:
		return SanitizeForUpstreamCache(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return SanitizeForUpstreamCache(string(b))
	}
}

// rewriteWorkspacePath translates daemon-cwd-rooted paths to the
// caller's workspace path. Originally added for the codex subprocess
// path where the codex CLI ran in the daemon's cwd; the websocket
// direct path has no such translation need because Cursor and the
// upstream both speak in absolute paths anchored at the workspace.
//
// Several guards keep this from corrupting absolute paths when the
// daemon is launched without an explicit working directory:
//   - bail when workspacePath or text is empty.
//   - bail when GetwdFn returns the same path.
//   - bail when the daemon cwd is `/` (launchd default), because
//     replacing every `/` with the workspace mashes every absolute
//     path beyond recognition.
//   - bail when the daemon cwd does not actually appear in the text
//     as a directory-bounded substring; avoids partial-prefix
//     mashing on text that merely happens to contain a similar
//     short string.
func rewriteWorkspacePath(text, workspacePath string) string {
	workspacePath = strings.TrimSpace(workspacePath)
	if workspacePath == "" || text == "" {
		return text
	}
	cwd, err := GetwdFn()
	if err != nil {
		return text
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" || cwd == workspacePath {
		return text
	}
	// Reject cwd values that are too short to be meaningful as a
	// path prefix. `/` is the launchd default and trips a
	// catastrophic global slash replace. Anything shorter than 2
	// characters or containing only path separators is treated as
	// "no real prefix".
	if len(cwd) < 2 || strings.Trim(cwd, "/") == "" {
		return text
	}
	// Replace only bounded occurrences. An unbounded ReplaceAll would
	// rewrite substrings inside unrelated identifiers (e.g. `/var`
	// inside `/variable`), which is the original mashing failure.
	return pathBoundedReplaceAll(text, cwd, workspacePath)
}

// pathBoundedReplaceAll replaces every bounded occurrence of needle
// in haystack with replacement. Unbounded occurrences (substrings
// inside identifiers) pass through untouched.
func pathBoundedReplaceAll(haystack, needle, replacement string) string {
	if needle == "" {
		return haystack
	}
	var out strings.Builder
	out.Grow(len(haystack))
	idx := 0
	for idx < len(haystack) {
		pos, ok := pathBoundedFirstMatch(haystack, needle, idx)
		if !ok {
			out.WriteString(haystack[idx:])
			return out.String()
		}
		out.WriteString(haystack[idx:pos])
		out.WriteString(replacement)
		idx = pos + len(needle)
	}
	return out.String()
}

func pathBoundedFirstMatch(haystack, needle string, from int) (int, bool) {
	if needle == "" || from >= len(haystack) {
		return 0, false
	}
	for {
		rel := strings.Index(haystack[from:], needle)
		if rel < 0 {
			return 0, false
		}
		pos := from + rel
		end := pos + len(needle)
		if isPathBoundary(haystack, pos, end, needle) {
			return pos, true
		}
		from = pos + 1
		if from >= len(haystack) {
			return 0, false
		}
	}
}

func isPathBoundary(haystack string, pos, end int, needle string) bool {
	startOK := pos == 0 || isPathSeparatorByte(haystack[pos-1]) || needle[0] == '/'
	endOK := end == len(haystack) || isPathSeparatorByte(haystack[end])
	return startOK && endOK
}

func isPathSeparatorByte(b byte) bool {
	switch b {
	case '/', ' ', '\n', '\t', '"', '\'', '(', ')', ',', ':', ';':
		return true
	}
	return false
}

func functionCallItem(tc adapteropenai.ToolCall) map[string]any {
	callID := strings.TrimSpace(tc.ID)
	if callID == "" {
		callID = fmt.Sprintf("call_%d", tc.Index)
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      OutboundToolName(tc.Function.Name),
		"arguments": tc.Function.Arguments,
	}
}

func FunctionCallItem(tc adapteropenai.ToolCall) map[string]any {
	return functionCallItem(tc)
}

func FunctionCallOutputItem(callID, text string) map[string]any {
	return map[string]any{
		"type":    "function_call_output",
		"call_id": strings.TrimSpace(callID),
		"output":  text,
	}
}

func functionCallFromResponsesItem(item map[string]any, workspacePath string) map[string]any {
	callID := mapString(item, "call_id")
	name := mapString(item, "name")
	args := rewriteWorkspacePath(rawString(item, "arguments"), workspacePath)
	tc := adapteropenai.ToolCall{
		ID:   callID,
		Type: "function",
		Function: adapteropenai.ToolCallFunction{
			Name:      InboundToolName(name),
			Arguments: args,
		},
	}
	return functionCallItem(tc)
}

func inputFromResponsesInput(
	raw json.RawMessage,
	workspacePath string,
	developerSections *[]string,
	strategy adapterrender.MaterializationStrategy,
) ([]map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, false
	}
	input := make([]map[string]any, 0, len(items))
	customToolCallIDs := make(map[string]bool)
	for _, item := range items {
		role := strings.ToLower(mapString(item, "role"))
		itemType := strings.TrimSpace(mapString(item, "type"))
		switch {
		case role == "system" || role == "developer":
			if text := strings.TrimSpace(responsesContentText(item["content"])); text != "" {
				*developerSections = append(*developerSections, text)
			}
		case role == "user":
			if content := codexContentFromAny(item["content"], "input_text", strategy); len(content) > 0 {
				input = append(input, MessageContentItems("user", content))
			}
		case role == "assistant":
			if content := codexContentFromAny(item["content"], "output_text", strategy); len(content) > 0 {
				input = append(input, MessageContentItems("assistant", content))
			}
		case itemType == "function_call":
			input = append(input, functionCallFromResponsesItem(item, workspacePath))
		case itemType == "function_call_output":
			callID := mapString(item, "call_id")
			output := strings.TrimSpace(rewriteWorkspacePath(responsesOutputText(item["output"]), workspacePath))
			if output == "" {
				continue
			}
			if customToolCallIDs[callID] {
				input = append(input, CustomToolCallOutputItem(callID, output))
			} else {
				input = append(input, FunctionCallOutputItem(callID, output))
			}
		case itemType == "custom_tool_call":
			callID := mapString(item, "call_id")
			name := mapString(item, "name")
			inputText := rewriteWorkspacePath(UnwrapApplyPatchInput(rawString(item, "input")), workspacePath)
			if callID != "" {
				customToolCallIDs[callID] = true
			}
			input = append(input, map[string]any{
				"type":    "custom_tool_call",
				"call_id": callID,
				"name":    name,
				"input":   inputText,
			})
		case itemType == "custom_tool_call_output":
			callID := mapString(item, "call_id")
			output := strings.TrimSpace(rewriteWorkspacePath(responsesOutputText(item["output"]), workspacePath))
			if output != "" {
				input = append(input, CustomToolCallOutputItem(callID, output))
			}
		}
	}
	return input, len(input) > 0
}

type codexRequestContextIdentity struct {
	PromptCacheKey      string
	WebsocketSessionKey string
}

func requestContextIdentity(req adaptercursor.Request, modelAlias string) codexRequestContextIdentity {
	if cursor := req.Context(); cursor.StrongConversationKey() != "" {
		key := cursor.StrongConversationKey()
		return codexRequestContextIdentity{
			PromptCacheKey:      key,
			WebsocketSessionKey: key,
		}
	}
	if v := requestMetadataString(req.OpenAI.Metadata, "conversation_id", "conversationId", "composerId", "composer_id", "thread_id", "threadId", "chat_id", "chatId"); v != "" {
		key := "meta:" + v
		return codexRequestContextIdentity{
			PromptCacheKey:      key,
			WebsocketSessionKey: key,
		}
	}
	firstUser := ""
	for _, msg := range req.OpenAI.Messages {
		if strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			firstUser = strings.TrimSpace(adapteropenai.FlattenContent(msg.Content))
			if firstUser != "" {
				break
			}
		}
	}
	if firstUser == "" {
		if v := strings.TrimSpace(req.User); v != "" {
			return codexRequestContextIdentity{PromptCacheKey: "user:" + v}
		}
		return codexRequestContextIdentity{}
	}
	sum := sha256.Sum256([]byte(modelAlias + "\n" + firstUser))
	// A content fingerprint is useful as an upstream prompt-cache
	// partition, but it is not proof that two requests are the same
	// live chat. Do not use it as WebsocketSessionKey: repeated fresh
	// Cursor chats can start with identical text and must not inherit
	// each other's previous_response_id.
	return codexRequestContextIdentity{PromptCacheKey: "fingerprint:" + hex.EncodeToString(sum[:16])}
}

func requestMetadataString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}
