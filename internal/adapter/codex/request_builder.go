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
	// RoundTripSummary picks the inbound shape for the summary half of a
	// round-tripped synthetic thinking envelope. Empty resolves to
	// [RoundTripSummaryNative] per codex-rs.
	RoundTripSummary RoundTripSummary
	// RoundTripEncrypted picks the inbound shape for the encrypted_content
	// half of a round-tripped synthetic thinking envelope. Empty resolves
	// to [RoundTripEncryptedRoundTrip] per codex-rs. The encrypted blob
	// itself rides inline on the synthetic close marker
	// (`data-encrypted="..."`) so persistence is owned by Cursor's
	// transcript; this lever only decides whether to forward what is
	// already on the marker.
	RoundTripEncrypted RoundTripEncrypted
}

// reasoningInputItem is the typed wire shape for a Codex Responses
// `reasoning` input item, mirroring codex-rs's ResponseItem::Reasoning
// (research/codex/codex-rs/protocol/src/models.rs:740-780). Summary may be
// empty when the round-trip summary lever is `drop`. EncryptedContent may
// be empty when the round-trip encrypted lever is `drop` or the inbound
// marker did not carry a `data-encrypted` attribute (legacy spans and
// Anthropic).
type reasoningInputItem struct {
	ID               string
	Summary          []reasoningSummaryText
	EncryptedContent string
}

// reasoningSummaryText is one entry in [reasoningInputItem.Summary]. The
// upstream type discriminator is fixed to "summary_text"; only the body
// text varies per entry (codex-rs ReasoningItemReasoningSummary).
type reasoningSummaryText struct {
	Text string
}

// asMap renders the typed Reasoning item into the map[string]any wire shape
// the input slice uses for opaque-shape Codex items. Only the fields the
// upstream cares about are emitted; absent optional fields are omitted to
// match codex-rs's serde(skip_serializing_if = "Option::is_none").
func (r reasoningInputItem) asMap() map[string]any {
	out := map[string]any{
		"type": "reasoning",
		"id":   r.ID,
	}
	if len(r.Summary) > 0 {
		summary := make([]map[string]any, 0, len(r.Summary))
		for _, entry := range r.Summary {
			summary = append(summary, map[string]any{
				"type": "summary_text",
				"text": entry.Text,
			})
		}
		out["summary"] = summary
	}
	if r.EncryptedContent != "" {
		out["encrypted_content"] = r.EncryptedContent
	}
	return out
}

// emitReasoningItemsFromAssistantContent extracts synthetic Reasoning
// envelopes from a single assistant content payload and appends a
// Codex-native `reasoning` input item per the round-trip strategy table
// to out. Items are appended in turn-relative order so they can be
// inserted BEFORE the matching assistant Message item by the caller.
//
// The encrypted_content blob is read straight off
// [adapterrender.SyntheticPart.Encrypted] (the `data-encrypted` attribute
// on the close marker). There is no out-of-band store lookup: Cursor's
// transcript carries the blob inline, so this function is pure.
//
// Legacy markers without a data-ref AND without an encrypted blob are
// skipped silently when the round-trip mode would emit a stub with no
// useful payload (matches pre-rewrite drop behavior). For
// `plain_text_concat` summary mode the body is left to the existing
// message-text materializer; only the encrypted_content half (if any)
// is emitted as a separate Reasoning item to preserve codex-rs
// continuity.
func emitReasoningItemsFromAssistantContent(
	out []map[string]any,
	contentText string,
	cfg RequestBuilderConfig,
) []map[string]any {
	parts := adapterrender.ExtractSyntheticParts(contentText)
	if len(parts) == 0 {
		return out
	}
	summaryMode := cfg.RoundTripSummary
	if summaryMode == "" {
		summaryMode = RoundTripSummaryNative
	}
	encryptedMode := cfg.RoundTripEncrypted
	if encryptedMode == "" {
		encryptedMode = RoundTripEncryptedRoundTrip
	}
	for _, part := range parts {
		if part.Kind != adapterrender.SyntheticReasoning {
			continue
		}
		item, emit := buildReasoningItem(part, summaryMode, encryptedMode)
		if !emit {
			continue
		}
		out = append(out, item.asMap())
	}
	return out
}

// buildReasoningItem applies the round-trip strategy table for one
// reasoning synthetic part. The encrypted blob is now an inline property
// of the part rather than the result of a store lookup, which collapses
// the legacy 9-case table down to 6 cases (one per (summaryMode,
// encryptedMode) pair). The boolean return reports whether the caller
// should emit anything at all; markers with nothing to round-trip under
// the chosen levers produce (zero, false).
func buildReasoningItem(
	part adapterrender.SyntheticPart,
	summaryMode RoundTripSummary,
	encryptedMode RoundTripEncrypted,
) (reasoningInputItem, bool) {
	body := strings.TrimSpace(part.Body)
	ref := strings.TrimSpace(part.Ref)
	encrypted := ""
	if encryptedMode == RoundTripEncryptedRoundTrip {
		encrypted = strings.TrimSpace(part.Encrypted)
	}
	zero := reasoningInputItem{ID: "", Summary: nil, EncryptedContent: ""}
	switch summaryMode {
	case RoundTripSummaryNative:
		// Empty Ref + no encrypted_content + no body to emit means the
		// envelope is a legacy attribute-less marker with nothing
		// useful to round-trip; drop it silently.
		if ref == "" && encrypted == "" && body == "" {
			return zero, false
		}
		summary := []reasoningSummaryText(nil)
		if body != "" {
			summary = []reasoningSummaryText{{Text: body}}
		}
		return reasoningInputItem{ID: ref, Summary: summary, EncryptedContent: encrypted}, true
	case RoundTripSummaryDrop:
		if ref == "" && encrypted == "" {
			return zero, false
		}
		return reasoningInputItem{ID: ref, Summary: nil, EncryptedContent: encrypted}, true
	case RoundTripSummaryPlainText:
		// The summary body is folded into the assistant message body
		// by MaterializePlainTextConcat; only the encrypted_content
		// half is emitted as a Reasoning item, and only when the
		// marker carried one.
		if encrypted == "" {
			return zero, false
		}
		return reasoningInputItem{ID: ref, Summary: nil, EncryptedContent: encrypted}, true
	}
	return zero, false
}

// assistantRawText flattens an assistant content payload from the
// responses-input item shape into a single string suitable for synthetic
// envelope extraction. Unlike [responsesContentText] this preserves
// envelope markers verbatim (no SanitizeForUpstreamCache); marker parsing
// runs over the raw text.
func assistantRawText(raw any) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var parts []string
		for _, entry := range v {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			switch strings.TrimSpace(mapString(m, "type")) {
			case "text", "input_text", "output_text":
				if text := rawString(m, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		switch strings.TrimSpace(mapString(v, "type")) {
		case "text", "input_text", "output_text":
			return rawString(v, "text")
		}
	}
	return ""
}

// BuildRequestWithConfig builds the HTTP transport request from a
// ChatRequest plus the typed RequestBuilderConfig. The build is now pure
// (no I/O): the encrypted_content blob rides inline on each synthetic
// thinking close marker, so no store lookup is needed and no context
// parameter is required.
func BuildRequestWithConfig(
	req adapteropenai.ChatRequest,
	model adaptermodel.ResolvedModel,
	effort string,
	cfg RequestBuilderConfig,
) HTTPTransportRequest {
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
	if rawInput, ok := inputFromResponsesInput(req.Input, workspacePath, &systemSections, strategy, cfg); ok {
		input = rawInput
	} else {
		for _, msg := range req.Messages {
			rawText := adaptercontent.FlattenRaw(msg.Content)
			text := strings.TrimSpace(SanitizeForUpstreamCacheWithStrategy(rawText, strategy))
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
				// Reasoning items must precede the assistant Message
				// they belong to in the input array; codex-rs's
				// history.rs preserves that order.
				input = emitReasoningItemsFromAssistantContent(input, rawText, cfg)
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
	cfg RequestBuilderConfig,
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
			// Reasoning items must precede the assistant Message they
			// belong to in the input array; codex-rs history.rs
			// preserves that order.
			input = emitReasoningItemsFromAssistantContent(input, assistantRawText(item["content"]), cfg)
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
