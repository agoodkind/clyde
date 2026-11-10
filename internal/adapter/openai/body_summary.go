package openai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"
)

const (
	chatBodySummaryMessageLimit = 3
	maxContentChars             = 2048
	maxToolCallSummaryItems     = 4
)

// toolCallPathArgKey enumerates the OpenAI tool-call argument key
// names the body summary harvests as file/directory paths.
type toolCallPathArgKey string

const (
	toolCallPathKeyPath             toolCallPathArgKey = "path"
	toolCallPathKeyFile             toolCallPathArgKey = "file"
	toolCallPathKeyFilepath         toolCallPathArgKey = "filepath"
	toolCallPathKeyTargetFile       toolCallPathArgKey = "target_file"
	toolCallPathKeyTargetDirectory  toolCallPathArgKey = "target_directory"
	toolCallPathKeyCwd              toolCallPathArgKey = "cwd"
	toolCallPathKeyWorkdir          toolCallPathArgKey = "workdir"
	toolCallPathKeyWorkingDirectory toolCallPathArgKey = "working_directory"
)

// BodySummary is part of Clyde's typed adapter surface.
type BodySummary struct {
	Model                string          `json:"model,omitempty"`
	Stream               bool            `json:"stream"`
	MessageCount         int             `json:"message_count"`
	MessagesChars        int             `json:"messages_chars"`
	Messages             []MsgSummary    `json:"messages"`
	Tools                []ToolSummary   `json:"tools"`
	ToolCount            int             `json:"tool_count"`
	ToolChoice           json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls    *bool           `json:"parallel_tool_calls,omitempty"`
	Logprobs             *bool           `json:"logprobs,omitempty"`
	Temperature          *float64        `json:"temperature,omitempty"`
	TopP                 *float64        `json:"top_p,omitempty"`
	MaxTokens            *int            `json:"max_tokens,omitempty"`
	MaxCompletion        *int            `json:"max_completion_tokens,omitempty"`
	MaxOutputTokens      *int            `json:"max_output_tokens,omitempty"`
	ServiceTier          string          `json:"service_tier,omitempty"`
	HasTextControls      bool            `json:"has_text_controls,omitempty"`
	Truncation           string          `json:"truncation,omitempty"`
	PromptCacheRetention string          `json:"prompt_cache_retention,omitempty"`
}

// MsgSummary is part of Clyde's typed adapter surface.
type MsgSummary struct {
	Role             string   `json:"role"`
	ContentChars     int      `json:"content_chars"`
	Name             string   `json:"name,omitempty"`
	ToolCallID       string   `json:"tool_call_id,omitempty"`
	HasToolCalls     bool     `json:"has_tool_calls,omitempty"`
	ToolCallCount    int      `json:"tool_call_count,omitempty"`
	ToolCallIDs      []string `json:"tool_call_ids,omitempty"`
	ToolCallNames    []string `json:"tool_call_names,omitempty"`
	ToolCallArgChars int      `json:"tool_call_arg_chars,omitempty"`
	ToolCallArgKeys  []string `json:"tool_call_arg_keys,omitempty"`
	ToolCallPaths    []string `json:"tool_call_paths,omitempty"`
}

// ToolSummary is part of Clyde's typed adapter surface.
type ToolSummary struct {
	Name        string `json:"name"`
	ParamsChars int    `json:"params_chars"`
}

// SummarizeChatBody is part of Clyde's typed adapter surface.
func SummarizeChatBody(raw []byte) (BodySummary, error) {
	log := slog.Default()
	var req ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		log.Warn("adapter.openai.body_summary.decode_failed", "concern", "adapter.chat.preflight", "subcomponent", "openai",
			"body_bytes", len(raw),
			"err", err.Error(),
		)
		return BodySummary{}, fmt.Errorf("invalid chat request: %w", err)
	}
	return SummarizeChatRequest(req), nil
}

// SummarizeChatRequest is part of Clyde's typed adapter surface.
func SummarizeChatRequest(req ChatRequest) BodySummary {
	toolChoice := req.ToolChoice
	if string(toolChoice) == "null" {
		toolChoice = nil
	}

	summary := BodySummary{
		Model:                req.Model,
		Stream:               req.Stream,
		MessageCount:         len(req.Messages),
		ToolChoice:           toolChoice,
		ParallelToolCalls:    req.ParallelTools,
		Logprobs:             req.Logprobs,
		Temperature:          req.Temperature,
		TopP:                 req.TopP,
		MaxTokens:            req.MaxTokens,
		MaxCompletion:        req.MaxComplTokens,
		MaxOutputTokens:      req.MaxOutputTokens,
		ServiceTier:          req.ServiceTier,
		HasTextControls:      len(req.Text) > 0 && string(req.Text) != "null",
		Truncation:           req.Truncation,
		PromptCacheRetention: req.PromptCacheRetention, MessagesChars: 0, Messages: nil, Tools: nil, ToolCount: 0,
	}

	summary.ToolCount = len(req.Tools) + len(req.Functions)
	summary.Tools = make([]ToolSummary, 0, summary.ToolCount)
	for _, tool := range req.Tools {
		summary.Tools = append(summary.Tools, ToolSummary{Name: tool.Function.Name, ParamsChars: len(tool.Function.Parameters)})
	}
	for _, fn := range req.Functions {
		summary.Tools = append(summary.Tools, ToolSummary{Name: fn.Name, ParamsChars: len(fn.Parameters)})
	}

	msgSummaries := summarizeMessages(req.Messages)
	summary.Messages = msgSummaries
	for _, msg := range msgSummaries {
		summary.MessagesChars += msg.ContentChars
	}
	return summary
}

func summarizeMessages(messages []ChatMessage) []MsgSummary {
	samples := make([]MsgSummary, 0, min(chatBodySummaryMessageLimit*2, len(messages)))
	for _, msg := range sampleMessages(messages) {
		samples = append(samples, summarizeMessage(msg))
	}
	return samples
}

func sampleMessages(messages []ChatMessage) []ChatMessage {
	if len(messages) <= chatBodySummaryMessageLimit*2 {
		return messages
	}
	out := make([]ChatMessage, 0, chatBodySummaryMessageLimit*2)
	out = append(out, messages[:chatBodySummaryMessageLimit]...)
	out = append(out, messages[len(messages)-chatBodySummaryMessageLimit:]...)
	return out
}

func summarizeMessage(msg ChatMessage) MsgSummary {
	content := FlattenContent(msg.Content)
	if len(content) > maxContentChars {
		content = content[:maxContentChars]
	}
	summary := MsgSummary{
		Role:         msg.Role,
		ContentChars: len(content),
		Name:         msg.Name,
		ToolCallID:   msg.ToolCallID, HasToolCalls: false, ToolCallCount: 0, ToolCallIDs: nil, ToolCallNames: nil, ToolCallArgChars: 0, ToolCallArgKeys: nil, ToolCallPaths: nil,
	}
	if len(msg.ToolCalls) > 0 {
		summary.HasToolCalls = true
		summary.ToolCallCount = len(msg.ToolCalls)
		for _, tc := range msg.ToolCalls {
			if summary.ToolCallID == "" {
				summary.ToolCallID = tc.ID
			}
			appendUniqueString(&summary.ToolCallIDs, tc.ID)
			appendUniqueString(&summary.ToolCallNames, tc.Function.Name)
			summary.ToolCallArgChars += len(tc.Function.Arguments)
			summarizeToolCallArguments(tc.Function.Arguments, &summary)
		}
	}
	return summary
}

func summarizeToolCallArguments(raw string, summary *MsgSummary) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return
	}
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		appendUniqueString(&summary.ToolCallArgKeys, key)
		switch toolCallPathArgKey(key) {
		case toolCallPathKeyPath, toolCallPathKeyFile, toolCallPathKeyFilepath, toolCallPathKeyTargetFile, toolCallPathKeyTargetDirectory, toolCallPathKeyCwd, toolCallPathKeyWorkdir, toolCallPathKeyWorkingDirectory:
			var value string
			if json.Unmarshal(args[key], &value) == nil {
				appendUniqueString(&summary.ToolCallPaths, value)
			}
		}
	}
}

func appendUniqueString(values *[]string, value string) {
	if len(*values) >= maxToolCallSummaryItems {
		return
	}
	if value == "" {
		return
	}
	if slices.Contains(*values, value) {
		return
	}
	*values = append(*values, value)
}
