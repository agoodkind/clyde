package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"goodkind.io/clyde/codexwire"
	adaptercontent "goodkind.io/clyde/internal/adapter/content"
	adaptercursor "goodkind.io/clyde/internal/adapter/cursor"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

// GetwdFn lets tests control workspace path rewriting.
var GetwdFn = os.Getwd

// codexResolvedModelName returns the upstream Codex model id from a
// resolved request. The Codex model identity and the resolved model
// string are the same value, so a nil-safe read of resolved.Model is
// the model name.
func codexResolvedModelName(resolved *adapterresolver.ResolvedRequest) string {
	if resolved == nil {
		return ""
	}
	return strings.TrimSpace(resolved.Model)
}

// codexResolvedInstructions returns the provider-neutral instructions
// the resolver lifted for this request, nil-safe.
func codexResolvedInstructions(resolved *adapterresolver.ResolvedRequest) string {
	if resolved == nil {
		return ""
	}
	return resolved.Instructions
}

// contentPartType is the closed enum of Codex content-part type
// strings the request builder accepts when flattening assistant and
// user messages.
type contentPartType string

const (
	contentPartTypeText       contentPartType = "text"
	contentPartTypeInputText  contentPartType = "input_text"
	contentPartTypeOutputText contentPartType = "output_text"
)

// codexChatRole is the closed enum of OpenAI chat roles the codex
// request builder recognizes.
type codexChatRole string

const (
	codexChatRoleSystem    codexChatRole = "system"
	codexChatRoleDeveloper codexChatRole = "developer"
	codexChatRoleAssistant codexChatRole = "assistant"
	codexChatRoleTool      codexChatRole = "tool"
	codexChatRoleFunction  codexChatRole = "function"
)

// reasoningEffort is the closed enum of accepted reasoning effort
// levels on the OpenAI Responses API.
type reasoningEffort string

const (
	reasoningEffortNone    reasoningEffort = "none"
	reasoningEffortMinimal reasoningEffort = "minimal"
	reasoningEffortLow     reasoningEffort = "low"
	reasoningEffortMedium  reasoningEffort = "medium"
	reasoningEffortHigh    reasoningEffort = "high"
	reasoningEffortXHigh   reasoningEffort = "xhigh"
)

// reasoningSummary is the closed enum of accepted reasoning summary
// modes on the OpenAI Responses API.
type reasoningSummary string

const (
	reasoningSummaryAuto     reasoningSummary = "auto"
	reasoningSummaryConcise  reasoningSummary = "concise"
	reasoningSummaryDetailed reasoningSummary = "detailed"
	reasoningSummaryNone     reasoningSummary = "none"
)

// MessageContent builds a typed Codex `message` input item with a
// single content part.
func MessageContent(role, textType, text string) codexwire.InputItem {
	return MessageContentItems(role, codexwire.ContentItems{{
		Type: codexwire.ContentItemType(textType),
		Text: text,
	}})
}

// MessageContentItems builds a typed Codex `message` input item
// with multiple content parts.
func MessageContentItems(role string, content codexwire.ContentItems) codexwire.InputItem {
	return codexwire.InputItem{
		Type:    codexwire.ItemTypeMessage,
		Role:    role,
		Content: content,
	}
}

func codexContentFromRaw(raw json.RawMessage, textType codexwire.ContentItemType, strategy adapterrender.MaterializationStrategy) codexwire.ContentItems {
	parts, _ := adaptercontent.NormalizeRaw(raw)
	return codexContentFromParts(parts, textType, strategy)
}

// codexContentFromContent flattens a raw content payload (string,
// array, or object) parsed from an inbound responses-input item.
func codexContentFromContent(raw inputItemContent, textType codexwire.ContentItemType, strategy adapterrender.MaterializationStrategy) codexwire.ContentItems {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	return codexContentFromRaw(encoded, textType, strategy)
}

func codexContentFromParts(parts []adaptercontent.Part, textType codexwire.ContentItemType, strategy adapterrender.MaterializationStrategy) codexwire.ContentItems {
	content := make(codexwire.ContentItems, 0, len(parts))
	for _, part := range parts {
		switch part.Kind {
		case adaptercontent.PartText:
			text := strings.TrimSpace(SanitizeForUpstreamCacheWithStrategy(part.Text, strategy))
			if text == "" {
				continue
			}
			content = append(content, codexwire.ContentItem{
				Type: textType,
				Text: text,
			})
		case adaptercontent.PartImage:
			if part.Image == nil || strings.TrimSpace(part.Image.URL) == "" {
				continue
			}
			item := codexwire.ContentItem{
				Type:     codexwire.ContentItemInputImage,
				ImageURL: strings.TrimSpace(part.Image.URL),
			}
			if detail := strings.TrimSpace(part.Image.Detail); detail != "" {
				item.Detail = detail
			}
			content = append(content, item)
		case adaptercontent.PartRefusal:
			text := strings.TrimSpace(SanitizeForUpstreamCacheWithStrategy(part.Refusal, strategy))
			if text == "" {
				continue
			}
			content = append(content, codexwire.ContentItem{
				Type: textType,
				Text: text,
			})
		case adaptercontent.PartAudio, adaptercontent.PartToolResult, adaptercontent.PartToolUse, adaptercontent.PartUnsupported:
			continue
		}
	}
	return content
}

// RequestBuilderConfig is part of Clyde's typed adapter surface.
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

// reasoningInputItem is the internal staging shape for a Codex
// Responses `reasoning` input item before it lands in the input
// slice as a [codexwire.InputItem]. Summary may be empty when the
// round-trip summary lever is `drop`. EncryptedContent may be empty
// when the round-trip encrypted lever is `drop` or the inbound
// marker did not carry a `data-encrypted` attribute (legacy spans
// and Anthropic).
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

// toInputItem renders the typed Reasoning item into the wire shape
// the input slice uses for Codex items. The summary field is always
// emitted, even as an empty array, because Codex Responses rejects
// the request with [ObjectParam] [input[i].summary]
// [missing_required_parameter] when summary is absent.
// encrypted_content stays optional and is dropped when the
// round-trip strategy did not provide one.
func (r reasoningInputItem) toInputItem() codexwire.InputItem {
	summary := make([]codexwire.ReasoningSummary, 0, len(r.Summary))
	for _, entry := range r.Summary {
		summary = append(summary, codexwire.ReasoningSummary{
			Type: "summary_text",
			Text: entry.Text,
		})
	}
	out := codexwire.InputItem{
		Type:    codexwire.ItemTypeReasoning,
		ID:      r.ID,
		Summary: summary,
	}
	if r.EncryptedContent != "" {
		out.EncryptedContent = r.EncryptedContent
	}
	return out
}

// emitReasoningItemsFromAssistantContent extracts synthetic Reasoning
// envelopes from a single assistant content payload and appends a
// Codex-native `reasoning` input item per the round-trip strategy
// table to out. Items are appended in turn-relative order so they
// can be inserted BEFORE the matching assistant Message item by the
// caller.
//
// The encrypted_content blob is read straight off
// [adapterrender.SyntheticPart.Encrypted] (the `data-encrypted`
// attribute on the close marker). There is no out-of-band store
// lookup: Cursor's transcript carries the blob inline, so this
// function is pure.
//
// Legacy markers without a data-ref AND without an encrypted blob
// are skipped silently when the round-trip mode would emit a stub
// with no useful payload (matches pre-rewrite drop behavior). For
// `plain_text_concat` summary mode the body is left to the existing
// message-text materializer; only the encrypted_content half (if
// any) is emitted as a separate Reasoning item to preserve codex-rs
// continuity.
func emitReasoningItemsFromAssistantContent(
	out []codexwire.InputItem,
	contentText string,
	cfg RequestBuilderConfig,
) []codexwire.InputItem {
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
		out = append(out, item.toInputItem())
	}
	return out
}

// buildReasoningItem applies the round-trip strategy table for one
// reasoning synthetic part. The encrypted blob is now an inline
// property of the part rather than the result of a store lookup,
// which collapses the legacy 9-case table down to 6 cases (one per
// (summaryMode, encryptedMode) pair). The boolean return reports
// whether the caller should emit anything at all; markers with
// nothing to round-trip under the chosen levers produce (zero,
// false).
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
	// Cross-provider rule: a foreign or unknown origin reasoning piece
	// (Anthropic, pre-upgrade transcript) cannot reproduce a Codex
	// Reasoning input item. The encrypted blob (when present) is
	// provider-specific and not interchangeable; the upstream rs_* id
	// would also be a fabrication. Drop the item; the body is injected
	// into the message text by sanitizeReasoningPartForCodex instead.
	if part.Origin != adapterrender.OriginCodex {
		return zero, false
	}
	// Codex rejects Reasoning input items with an empty id
	// (ApiIdParam.invalid_id). The id is the upstream rs_* identifier
	// captured on data-ref. A marker without ref cannot round-trip
	// continuity (codex-rs always carries the original id), so drop
	// the entire item rather than emit an empty id.
	if ref == "" {
		return zero, false
	}
	switch summaryMode {
	case RoundTripSummaryNative:
		summary := []reasoningSummaryText(nil)
		if body != "" {
			summary = []reasoningSummaryText{{Text: body}}
		}
		return reasoningInputItem{ID: ref, Summary: summary, EncryptedContent: encrypted}, true
	case RoundTripSummaryDrop:
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

// inputItemContent is the JSON union the codex Responses input item
// uses for the `content` field. It accepts a JSON string, a JSON
// array of content parts, or a JSON object.
type inputItemContent struct {
	Raw json.RawMessage
}

// UnmarshalJSON stores the raw bytes verbatim so the downstream
// flattener can decode whichever shape arrived.
func (c *inputItemContent) UnmarshalJSON(data []byte) error {
	c.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// MarshalJSON emits the raw bytes verbatim; null when empty.
func (c inputItemContent) MarshalJSON() ([]byte, error) {
	if len(c.Raw) == 0 {
		return []byte("null"), nil
	}
	return c.Raw, nil
}

// responsesInputItem is the typed view of one entry in the inbound
// `req.Input` array. The flexible fields (content as any-of-string-
// array-or-object, output as string-or-array) are deferred to
// inputItemContent.Raw for downstream flattening.
type responsesInputItem struct {
	Type      string           `json:"type,omitempty"`
	Role      string           `json:"role,omitempty"`
	Content   inputItemContent `json:"content"`
	CallID    string           `json:"call_id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Arguments string           `json:"arguments,omitempty"`
	Input     string           `json:"input,omitempty"`
	Output    json.RawMessage  `json:"output,omitempty"`
}

// assistantRawText flattens an assistant content payload from the
// responses-input item shape into a single string suitable for
// synthetic envelope extraction. Unlike [responsesContentText] this
// preserves envelope markers verbatim (no SanitizeForUpstreamCache);
// marker parsing runs over the raw text.
func assistantRawText(raw inputItemContent) string {
	if len(raw.Raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw.Raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw.Raw, &s); err != nil {
			return ""
		}
		return s
	}
	if trimmed[0] == '[' {
		var parts []responsesContentPart
		if err := json.Unmarshal(raw.Raw, &parts); err != nil {
			return ""
		}
		var out []string
		for _, part := range parts {
			switch contentPartType(strings.TrimSpace(part.Type)) {
			case contentPartTypeText, contentPartTypeInputText, contentPartTypeOutputText:
				if part.Text != "" {
					out = append(out, part.Text)
				}
			}
		}
		return strings.Join(out, "\n")
	}
	if trimmed[0] == '{' {
		var part responsesContentPart
		if err := json.Unmarshal(raw.Raw, &part); err != nil {
			return ""
		}
		switch contentPartType(strings.TrimSpace(part.Type)) {
		case contentPartTypeText, contentPartTypeInputText, contentPartTypeOutputText:
			return part.Text
		}
	}
	return ""
}

// responsesContentPart is one entry inside a responses-input item's
// content array.
type responsesContentPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

// BuildRequestWithConfig builds the HTTP transport request from a
// ChatRequest plus the typed RequestBuilderConfig. The build is now pure
// (no I/O): the encrypted_content blob rides inline on each synthetic
// thinking close marker, so no store lookup is needed and no context
// parameter is required.
func BuildRequestWithConfig(
	req adapteropenai.ChatRequest,
	resolved *adapterresolver.ResolvedRequest,
	effort string,
	cfg RequestBuilderConfig,
) HTTPTransportRequest {
	strategy := cfg.InboundThinkingMaterialization
	if strategy == "" {
		strategy = adapterrender.MaterializeDrop
	}
	cursorReq := adaptercursor.TranslateRequest(req)
	input := make([]codexwire.InputItem, 0, len(req.Messages))
	systemSections := make([]string, 0, 8)
	modelName := codexResolvedModelName(resolved)
	workspacePath := cursorReq.WorkspacePath
	if rawInput, ok := inputFromResponsesInput(req.Input, workspacePath, &systemSections, strategy, cfg); ok {
		input = rawInput
	} else {
		input, systemSections = appendChatMessageInputs(input, systemSections, req.Messages, strategy, cfg)
	}
	instructions := strings.TrimSpace(strings.Join(systemSections, "\n\n"))
	if base := strings.TrimSpace(codexResolvedInstructions(resolved)); base != "" {
		if instructions == "" {
			instructions = base
		} else {
			instructions = base + "\n\n" + instructions
		}
	}
	if len(input) == 0 {
		input = append(input, MessageContent("user", string(codexwire.ContentItemInputText), " "))
	}
	reasoning := EffectiveReasoningWithDefaultSummary(req, effort, cfg.ReasoningSummary)
	// codex-rs serializes `tools` and `include` as Vec<...> with no
	// skip_serializing_if, so they appear as `[]` when empty rather than
	// being omitted or null. Go marshals a nil slice as `null`, so the
	// empty case is coerced to a non-nil empty slice to match codex-cli on
	// the wire (research/codex/codex-rs/codex-api/src/common.rs).
	include := RequestInclude(req.Include, reasoning != nil)
	if include == nil {
		include = []string{}
	}
	tools := toolSpecs(req)
	if tools == nil {
		tools = []codexwire.ToolSpec{}
	}
	outputControls := BuildOutputControls(req)
	identity := requestContextIdentity(cursorReq, codexResolvedModelName(resolved))
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
		// WARNING: prompt_cache_key and websocket session identity
		// are intentionally not the same field. Codex upstream uses
		// the real conversation/thread id for websocket headers and
		// previous_response_id chaining, while prompt_cache_key is
		// only a cache partition and may be content-derived. Reusing
		// a websocket session from a cache key can cross-wire
		// unrelated Cursor chats that share the same account, first
		// prompt, or cache partition.
		WebsocketSessionKey: identity.WebsocketSessionKey,
		PromptCache:         identity.PromptCacheKey,
		ServiceTier:         ServiceTierFromRequest(req),
		Reasoning:           reasoning,
		Text:                outputControls.Text,
		Input:               input,
		Tools:               tools,
		ToolChoice:          "auto",
		ParallelToolCalls:   parallelToolCalls(req),
		ClientMetadata:      nil,
	}
}

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
) ([]codexwire.InputItem, []string) {
	for _, msg := range messages {
		rawText := adaptercontent.FlattenRaw(msg.Content)
		text := strings.TrimSpace(SanitizeForUpstreamCacheWithStrategy(rawText, strategy))
		switch codexChatRole(strings.ToLower(msg.Role)) {
		case codexChatRoleSystem, codexChatRoleDeveloper:
			if text != "" {
				systemSections = append(systemSections, text)
			}
		case codexChatRoleAssistant:
			input = appendAssistantInput(input, msg, rawText, strategy, cfg)
		case codexChatRoleTool, codexChatRoleFunction:
			input = appendToolResultInput(input, msg, text)
		default:
			if content := codexContentFromRaw(msg.Content, codexwire.ContentItemInputText, strategy); len(content) > 0 {
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
) []codexwire.InputItem {
	for _, tc := range msg.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) == "" {
			continue
		}
		input = append(input, FunctionCallItem(tc))
	}
	input = emitReasoningItemsFromAssistantContent(input, rawText, cfg)
	if content := codexContentFromRaw(msg.Content, codexwire.ContentItemOutputText, strategy); len(content) > 0 {
		input = append(input, MessageContentItems("assistant", content))
	}
	return input
}

// appendToolResultInput emits one tool/function role message either as a
// FunctionCallOutput item (when it has a tool_call_id) or as a tagged
// user input fallback.
func appendToolResultInput(
	input []codexwire.InputItem,
	msg adapteropenai.ChatMessage,
	text string,
) []codexwire.InputItem {
	if text == "" {
		return input
	}
	if strings.TrimSpace(msg.ToolCallID) != "" {
		return append(input, FunctionCallOutputItem(msg.ToolCallID, text))
	}
	return append(input, MessageContent("user", string(codexwire.ContentItemInputText), "tool: "+text))
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
					Parameters:  fn.Parameters, Strict: nil,
				},
			})
		}
	}
	return tools
}

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
		out = append(out, FunctionToolSpec(strings.TrimSpace(tool.Function.Name), tool.Function.Description, tool.Function.Parameters, tool.Function.Strict))
	}
	return out
}

// responsesContentText extracts the plain-text body from one
// inbound responses-input item's content. Returns sanitized text.
func responsesContentText(raw inputItemContent) string {
	if len(raw.Raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw.Raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw.Raw, &s); err != nil {
			return ""
		}
		return SanitizeForUpstreamCache(s)
	}
	if trimmed[0] == '[' {
		var parts []responsesContentPart
		if err := json.Unmarshal(raw.Raw, &parts); err != nil {
			return ""
		}
		var out []string
		for _, part := range parts {
			switch contentPartType(strings.TrimSpace(part.Type)) {
			case contentPartTypeText, contentPartTypeInputText, contentPartTypeOutputText:
				if part.Text != "" {
					out = append(out, part.Text)
				}
			}
		}
		return SanitizeForUpstreamCache(strings.Join(out, "\n"))
	}
	if trimmed[0] == '{' {
		var part responsesContentPart
		if err := json.Unmarshal(raw.Raw, &part); err != nil {
			return ""
		}
		switch contentPartType(strings.TrimSpace(part.Type)) {
		case contentPartTypeText, contentPartTypeInputText, contentPartTypeOutputText:
			return SanitizeForUpstreamCache(part.Text)
		}
	}
	return ""
}

// responsesOutputText flattens the inbound `output` field of a
// function_call_output item to plain text. Sanitizes for cache.
func responsesOutputText(raw json.RawMessage) string {
	text := codexwire.OutputText(raw)
	if text != "" {
		return SanitizeForUpstreamCache(text)
	}
	return ""
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

func functionCallItem(tc adapteropenai.ToolCall) codexwire.InputItem {
	callID := strings.TrimSpace(tc.ID)
	if callID == "" {
		callID = fmt.Sprintf("call_%d", tc.Index)
	}
	return codexwire.InputItem{
		Type:   codexwire.ItemTypeFunctionCall,
		CallID: callID,
		// The tool name and arguments pass through verbatim as the
		// opaque content the client declared and the model emitted.
		Name:      strings.TrimSpace(tc.Function.Name),
		Arguments: tc.Function.Arguments,
	}
}

// FunctionCallItem is part of Clyde's typed adapter surface.
func FunctionCallItem(tc adapteropenai.ToolCall) codexwire.InputItem {
	return functionCallItem(tc)
}

// FunctionCallOutputItem is part of Clyde's typed adapter surface.
func FunctionCallOutputItem(callID, text string) codexwire.InputItem {
	return codexwire.InputItem{
		Type:   codexwire.ItemTypeFunctionCallOutput,
		CallID: strings.TrimSpace(callID),
		Output: codexwire.RawOutput(text),
	}
}

func functionCallFromResponsesItem(item responsesInputItem, workspacePath string) codexwire.InputItem {
	args := rewriteWorkspacePath(item.Arguments, workspacePath)
	tc := adapteropenai.ToolCall{
		ID:   item.CallID,
		Type: "function",
		Function: adapteropenai.ToolCallFunction{
			Name:      strings.TrimSpace(item.Name),
			Arguments: args,
		}, Index: 0,
	}
	return functionCallItem(tc)
}

func inputFromResponsesInput(
	raw json.RawMessage,
	workspacePath string,
	developerSections *[]string,
	strategy adapterrender.MaterializationStrategy,
	cfg RequestBuilderConfig,
) ([]codexwire.InputItem, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var items []responsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return nil, false
	}
	input := make([]codexwire.InputItem, 0, len(items))
	customToolCallIDs := make(map[string]bool)
	for _, item := range items {
		role := strings.ToLower(item.Role)
		itemType := strings.TrimSpace(item.Type)
		switch {
		case role == string(codexChatRoleSystem) || role == string(codexChatRoleDeveloper):
			if text := strings.TrimSpace(responsesContentText(item.Content)); text != "" {
				*developerSections = append(*developerSections, text)
			}
		case role == "user":
			if content := codexContentFromContent(item.Content, codexwire.ContentItemInputText, strategy); len(content) > 0 {
				input = append(input, MessageContentItems("user", content))
			}
		case role == string(codexChatRoleAssistant):
			// Reasoning items must precede the assistant Message
			// they belong to in the input array; codex-rs
			// history.rs preserves that order.
			input = emitReasoningItemsFromAssistantContent(input, assistantRawText(item.Content), cfg)
			if content := codexContentFromContent(item.Content, codexwire.ContentItemOutputText, strategy); len(content) > 0 {
				input = append(input, MessageContentItems("assistant", content))
			}
		case itemType == string(codexwire.ItemTypeFunctionCall):
			input = append(input, functionCallFromResponsesItem(item, workspacePath))
		case itemType == string(codexwire.ItemTypeFunctionCallOutput):
			output := strings.TrimSpace(rewriteWorkspacePath(responsesOutputText(item.Output), workspacePath))
			if output == "" {
				continue
			}
			if customToolCallIDs[item.CallID] {
				input = append(input, CustomToolCallOutputItem(item.CallID, output))
			} else {
				input = append(input, FunctionCallOutputItem(item.CallID, output))
			}
		case itemType == string(codexwire.ItemTypeCustomToolCall):
			inputText := rewriteWorkspacePath(UnwrapApplyPatchInput(item.Input), workspacePath)
			if item.CallID != "" {
				customToolCallIDs[item.CallID] = true
			}
			input = append(input, codexwire.InputItem{
				Type:   codexwire.ItemTypeCustomToolCall,
				CallID: item.CallID,
				Name:   item.Name,
				Input:  inputText,
			})
		case itemType == string(codexwire.ItemTypeCustomToolCallOutput):
			output := strings.TrimSpace(rewriteWorkspacePath(responsesOutputText(item.Output), workspacePath))
			if output != "" {
				input = append(input, CustomToolCallOutputItem(item.CallID, output))
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
			return codexRequestContextIdentity{PromptCacheKey: "user:" + v, WebsocketSessionKey: ""}
		}
		return codexRequestContextIdentity{PromptCacheKey: "", WebsocketSessionKey: ""}
	}
	sum := sha256.Sum256([]byte(modelAlias + "\n" + firstUser))
	// A content fingerprint is useful as an upstream prompt-cache
	// partition, but it is not proof that two requests are the same
	// live chat. Do not use it as WebsocketSessionKey: repeated fresh
	// Cursor chats can start with identical text and must not inherit
	// each other's previous_response_id.
	return codexRequestContextIdentity{PromptCacheKey: "fingerprint:" + hex.EncodeToString(sum[:16]), WebsocketSessionKey: ""}
}

// metadataString is the JSON view of a request `metadata` field.
type metadataString map[string]json.RawMessage

func requestMetadataString(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var m metadataString
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, key := range keys {
		v, ok := m[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			continue
		}
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
