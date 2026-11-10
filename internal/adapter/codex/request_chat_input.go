package codex

import (
	"strings"

	"goodkind.io/clyde/codexwire"
	adaptercontent "goodkind.io/clyde/internal/adapter/content"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// appendChatMessageInputs walks the Chat-shaped messages once and
// folds each into the Codex Responses input slice and system
// fragment list. The returned slices replace the originals so the
// caller can keep this call pure.
func appendChatMessageInputs(
	input []codexwire.InputItem,
	systemSections []string,
	messages []adapteropenai.ChatMessage,
	strategy adapterrender.MaterializationStrategy,
	cfg RequestBuilderConfig,
	toolIDs *toolIDProjection,
	customTools declaredCustomTools,
) ([]codexwire.InputItem, []string) {
	// Maps a replayed custom tool call's id to its tool name. An
	// assistant tool call always precedes its tool result in the
	// message order, so the map is populated before it is read.
	customCallNames := make(map[string]string)
	for _, msg := range messages {
		rawText := adaptercontent.FlattenRaw(msg.Content)
		text := strings.TrimSpace(sanitizeForUpstreamCacheWithRequestConfig(rawText, strategy, cfg))
		switch codexChatRole(strings.ToLower(msg.Role)) {
		case codexChatRoleSystem, codexChatRoleDeveloper:
			if text != "" {
				systemSections = append(systemSections, text)
			}
		case codexChatRoleAssistant:
			input = appendAssistantInput(input, msg, rawText, strategy, cfg, toolIDs, customTools, customCallNames)
		case codexChatRoleTool, codexChatRoleFunction:
			input = appendToolResultInput(input, msg, text, toolIDs, customCallNames)
		default:
			if content := codexContentFromRaw(msg.Content, codexwire.ContentItemInputText, strategy, cfg); len(content) > 0 {
				input = append(input, MessageContentItems("user", content))
			}
		}
	}
	return input, systemSections
}

// appendAssistantInput emits one assistant message's tool calls,
// reasoning items, and visible content into the input slice. Reasoning
// items must precede the matching Message item per codex-rs history.rs.
func appendAssistantInput(
	input []codexwire.InputItem,
	msg adapteropenai.ChatMessage,
	rawText string,
	strategy adapterrender.MaterializationStrategy,
	cfg RequestBuilderConfig,
	toolIDs *toolIDProjection,
	customTools declaredCustomTools,
	customCallNames map[string]string,
) []codexwire.InputItem {
	for _, tc := range msg.ToolCalls {
		name := strings.TrimSpace(tc.Function.Name)
		if name == "" {
			continue
		}
		if customTools.has(name) {
			// A custom tool's payload is raw text, so replaying it as
			// function_call arguments would send the upstream a value
			// that is not JSON. Cursor echoes these back with type
			// "function", so the declared set decides, not tc.Type.
			//
			// The payload rides through verbatim. Unwrapping a JSON
			// wrapper here would corrupt a freeform tool whose own valid
			// content happens to be a JSON object.
			callID := toolCallID(tc)
			customCallNames[callID] = name
			input = append(input, CustomToolCallItem(toolIDs.project(callID), name, tc.Function.Arguments))
			continue
		}
		input = append(input, functionCallItem(tc, toolIDs))
	}
	input = emitReasoningItemsFromAssistantContent(input, rawText, cfg)
	if content := codexContentFromRaw(msg.Content, codexwire.ContentItemOutputText, strategy, cfg); len(content) > 0 {
		input = append(input, MessageContentItems("assistant", content))
	}
	return input
}

// appendToolResultInput emits one tool/function role message as the
// reply item matching the call it answers: a custom_tool_call_output
// for a client-declared custom tool, a function_call_output for an
// ordinary function tool, or a tagged user input when the message
// carries no tool_call_id.
func appendToolResultInput(
	input []codexwire.InputItem,
	msg adapteropenai.ChatMessage,
	text string,
	toolIDs *toolIDProjection,
	customCallNames map[string]string,
) []codexwire.InputItem {
	if text == "" {
		return input
	}
	callID := strings.TrimSpace(msg.ToolCallID)
	if callID == "" {
		return append(input, MessageContent("user", string(codexwire.ContentItemInputText), "tool: "+text))
	}
	// A custom tool call must be answered by a custom_tool_call_output
	// under the same tool name; a function_call_output would leave the
	// call unanswered on the upstream side.
	if name, ok := customCallNames[callID]; ok {
		return append(input, CustomToolCallOutputItem(toolIDs.project(callID), name, text))
	}
	return append(input, FunctionCallOutputItem(toolIDs.project(callID), text))
}

func chatToolCallIDs(messages []adapteropenai.ChatMessage) []string {
	toolCallIDs := make([]string, 0)
	for _, message := range messages {
		if toolCallID := strings.TrimSpace(message.ToolCallID); toolCallID != "" {
			toolCallIDs = append(toolCallIDs, toolCallID)
		}
		for _, toolCall := range message.ToolCalls {
			if toolCallID := strings.TrimSpace(toolCall.ID); toolCallID != "" {
				toolCallIDs = append(toolCallIDs, toolCallID)
			}
		}
	}
	return toolCallIDs
}
