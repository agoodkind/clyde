package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/adapter/finishreason"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type RunResult struct {
	Usage                      adapteropenai.Usage
	UsageTelemetry             CodexUsageTelemetry
	FinishReason               string
	ReasoningSignaled          bool
	ReasoningVisible           bool
	DerivedCacheCreationTokens int
	ResponseID                 string
	OutputItems                []map[string]any
	ToolCallCount              int
	ToolCallNames              []string
	HasSubagentToolCall        bool
}

// ContextWindowError reports an upstream Codex over-context rejection.
// The adapter maps this to OpenAI's context_length_exceeded shape so
// Cursor can run its normal compaction/retry flow.
type ContextWindowError struct {
	Message string
}

func (e *ContextWindowError) Error() string {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return "codex input exceeds context window"
	}
	return e.Message
}

// UnsupportedModelError reports an upstream Codex rejection for a model that
// the authenticated account or Codex surface does not support.
type UnsupportedModelError struct {
	Message string
}

func (e *UnsupportedModelError) Error() string {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return "codex model is not supported"
	}
	return e.Message
}

func NewRunResult(finishReason string) RunResult {
	return RunResult{FinishReason: finishreason.FromCodex(finishReason)}
}

func (r *RunResult) SetFinishReason(finishReason string) {
	r.FinishReason = finishreason.FromCodex(finishReason)
}

type completedResponse struct {
	Response completedResponseBody `json:"response"`
}

type completedResponseBody struct {
	ID                string                    `json:"id"`
	Status            string                    `json:"status"`
	IncompleteDetails completedIncompleteDetail `json:"incomplete_details"`
	Usage             *completedUsage           `json:"usage"`
}

type completedIncompleteDetail struct {
	Reason string `json:"reason"`
}

// completedUsage mirrors the OpenAI Responses `response.completed`
// usage shape parsed by Codex at
// research/codex/codex-rs/codex-api/src/sse/responses.rs.
type completedUsage struct {
	InputTokens         int                           `json:"input_tokens"`
	InputTokensDetails  *completedInputTokensDetails  `json:"input_tokens_details"`
	OutputTokens        int                           `json:"output_tokens"`
	OutputTokensDetails *completedOutputTokensDetails `json:"output_tokens_details"`
	TotalTokens         int                           `json:"total_tokens"`
}

type completedInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completedOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// CodexUsageTelemetry is the provider-owned diagnostic view of Codex
// Responses usage. The boolean fields intentionally distinguish an
// explicitly present zero from omitted/null details.
type CodexUsageTelemetry struct {
	UsagePresent               bool
	InputTokens                int
	OutputTokens               int
	TotalTokens                int
	InputTokensDetailsPresent  bool
	CachedTokens               int
	OutputTokensDetailsPresent bool
	ReasoningOutputTokens      int
}

// Mirrors the observed Responses SSE envelope from
// research/codex/codex-rs/codex-api/src/sse/responses.rs and the local
// mock websocket script. The item payload remains a named raw-object boundary
// because Codex emits a broad response-item union here.
type transportStreamEvent struct {
	Type         string              `json:"type"`
	Sequence     int                 `json:"sequence_number,omitempty"`
	Response     *transportResponse  `json:"response,omitempty"`
	Item         transportItem       `json:"item,omitempty"`
	ItemID       string              `json:"item_id,omitempty"`
	CallID       string              `json:"call_id,omitempty"`
	Delta        string              `json:"delta,omitempty"`
	SummaryIndex *int                `json:"summary_index,omitempty"`
	ContentIndex *int                `json:"content_index,omitempty"`
	Error        *transportErrorBody `json:"error,omitempty"`
}

type transportResponse struct {
	ID                string `json:"id,omitempty"`
	Status            string `json:"status,omitempty"`
	IncompleteDetails struct {
		Reason string `json:"reason,omitempty"`
	} `json:"incomplete_details"`
	Usage struct {
		OutputTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens,omitempty"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

type transportErrorBody struct {
	Message string `json:"message,omitempty"`
}

type transportItem map[string]json.RawMessage

type reasoningItemPayload struct {
	ID      string                 `json:"id,omitempty"`
	Summary []reasoningSummaryPart `json:"summary,omitempty"`
	Content []reasoningContentPart `json:"content,omitempty"`
}

type reasoningSummaryPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type reasoningContentPart struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type toolCallState struct {
	Index             int
	ItemID            string
	CallID            string
	Name              string
	NativeName        string
	Type              string
	IdentityEmitted   bool
	ArgumentDeltaSeen bool
	ArgumentsEmitted  bool
	NativeParseLogged bool
	Arguments         strings.Builder
	Input             strings.Builder
}

// SanitizeForUpstreamCache removes every render-owned synthetic envelope
// (reasoning, notice, future kinds) from assistant text before Codex reuses
// the transcript on the next upstream turn. It is a thin wrapper over
// [SanitizeForUpstreamCacheWithStrategy] using the Codex default
// [adapterrender.MaterializeDrop] for backward compatibility with call
// sites that have no surrounding config context.
//
// Operators who want to promote thinking round-tripping to plain-text
// concat use [config.AdapterSyntheticContent.Codex.InboundThinkingMaterialization].
// The codex request builder threads that strategy through to
// [SanitizeForUpstreamCacheWithStrategy] explicitly; this wrapper retains
// the conservative default for ad-hoc callers (e.g. canonical_items).
func SanitizeForUpstreamCache(text string) string {
	return SanitizeForUpstreamCacheWithStrategy(text, adapterrender.MaterializeDrop)
}

// SanitizeForUpstreamCacheWithStrategy applies the given materialization
// strategy to the synthetic envelopes inside text and returns the
// resulting upstream-ready string. Codex has no native thinking content
// block, so [adapterrender.MaterializeNativeThinkingBlock] degrades to
// drop here (the materializer's MaterializedKindNativeThinking output is
// not representable in Codex's wire shape).
func SanitizeForUpstreamCacheWithStrategy(text string, strategy adapterrender.MaterializationStrategy) string {
	parts := adapterrender.ExtractSyntheticParts(text)
	if len(parts) == 1 && parts[0].Kind == adapterrender.SyntheticKindText {
		return parts[0].Body
	}
	materialized := adapterrender.MaterializeSyntheticParts(parts, strategy)
	var b strings.Builder
	for _, mp := range materialized {
		if mp.Kind != adapterrender.MaterializedKindText {
			// Codex cannot accept native thinking content blocks, so any
			// MaterializedKindNativeThinking is intentionally dropped
			// here. Callers that want the reasoning trace preserved
			// must select [adapterrender.MaterializePlainTextConcat] or
			// [adapterrender.MaterializePassthrough] in config.
			continue
		}
		b.WriteString(mp.Body)
	}
	return b.String()
}

// ClientMetadataWithTurn extends ClientMetadata with the
// `x-codex-turn-metadata` JSON blob. Codex CLI and Codex Desktop both
// mirror the handshake header into client_metadata; we do the same.
// turnMetadataJSON should be the already-marshaled JSON string from
// TurnMetadata.MarshalCompact.
func ClientMetadataWithTurn(installationID, windowID, turnMetadataJSON string) map[string]string {
	out := map[string]string{}
	if v := strings.TrimSpace(installationID); v != "" {
		out["x-codex-installation-id"] = v
	}
	if v := strings.TrimSpace(windowID); v != "" {
		out["x-codex-window-id"] = v
	}
	if v := strings.TrimSpace(turnMetadataJSON); v != "" {
		out["x-codex-turn-metadata"] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func RequestInclude(requested []string, reasoningEnabled bool) []string {
	if len(requested) == 0 && !reasoningEnabled {
		return nil
	}
	seen := make(map[string]struct{}, len(requested)+1)
	out := make([]string, 0, len(requested)+1)
	for _, item := range requested {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	if reasoningEnabled {
		const encryptedReasoning = "reasoning.encrypted_content"
		if _, ok := seen[encryptedReasoning]; !ok {
			out = append(out, encryptedReasoning)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func EffectiveReasoningWithDefaultSummary(req adapteropenai.ChatRequest, effort, defaultSummary string) *Reasoning {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	}
	if effort == "" && req.Reasoning != nil {
		effort = strings.ToLower(strings.TrimSpace(req.Reasoning.Effort))
	}
	var out Reasoning
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		out.Effort = effort
	}
	if req.Reasoning != nil {
		switch strings.ToLower(strings.TrimSpace(req.Reasoning.Summary)) {
		case "auto", "concise", "detailed", "none":
			out.Summary = strings.ToLower(strings.TrimSpace(req.Reasoning.Summary))
		}
	}
	if out.Effort != "" && out.Summary == "" {
		switch strings.ToLower(strings.TrimSpace(defaultSummary)) {
		case "auto", "concise", "detailed", "none":
			out.Summary = strings.ToLower(strings.TrimSpace(defaultSummary))
		default:
			out.Summary = "auto"
		}
	}
	if out.Effort == "" && out.Summary == "" {
		return nil
	}
	return &out
}

// SSEParseOptions carries the optional knobs the codex SSE parser honors
// at construction time. Today: whether to strip the encrypted_content
// blob from emitted reasoning-finished events. Drop mode lives at this
// boundary so the renderer can stay backend-neutral.
type SSEParseOptions struct {
	// DropEncryptedContent skips populating Event.EncryptedContent on
	// EventReasoningFinished even when the upstream reasoning item
	// carried a blob. Used when DirectConfig.RoundTripEncrypted is
	// RoundTripEncryptedDrop so the synthetic-thinking close marker
	// stays bare.
	DropEncryptedContent bool
}

// ParseSSEEventsWithOptions is the option-aware codex SSE parser entry
// point. Callers that do not need to override defaults pass a zero
// [SSEParseOptions].
func ParseSSEEventsWithOptions(ctx context.Context, body io.Reader, emit func(adapterrender.Event) error, logCtx sseInstrumentationContext, opts SSEParseOptions) (RunResult, error) {
	parser := newSSEEventParser(ctx, emit, logCtx)
	parser.dropEncryptedContent = opts.DropEncryptedContent
	return parser.parse(body)
}

type ssePayloadAction int

const (
	ssePayloadContinue ssePayloadAction = iota
	ssePayloadBreak
	ssePayloadReturn
)

type ssePayloadResult struct {
	Action ssePayloadAction
	Result RunResult
	Err    error
}

type sseEventParser struct {
	ctx                       context.Context
	emit                      func(adapterrender.Event) error
	logCtx                    sseInstrumentationContext
	out                       RunResult
	reasoningSignaled         bool
	reasoningVisible          bool
	reasoningTextDeltaSeen    bool
	reasoningSummaryDeltaSeen bool
	toolCallsByItemID         map[string]*toolCallState
	nextToolIndex             int
	aggregate                 sseAggregateCollector
	upstreamEventSeq          int
	// dropEncryptedContent gates whether the encrypted_content blob
	// scraped off response.output_item.done reasoning items rides on
	// the EventReasoningFinished emit. False (default) keeps the
	// codex-rs round_trip behavior; true honors a config drop.
	dropEncryptedContent bool
}

func newSSEEventParser(ctx context.Context, emit func(adapterrender.Event) error, logCtx sseInstrumentationContext) *sseEventParser {
	return &sseEventParser{
		ctx:                  ctx,
		emit:                 emit,
		logCtx:               logCtx,
		out:                  NewRunResult("stop"),
		toolCallsByItemID:    make(map[string]*toolCallState),
		dropEncryptedContent: false,
	}
}

func (p *sseEventParser) parse(body io.Reader) (RunResult, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 1024*128), 1024*1024*8)

	var eventName string
	var dataLines []string
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line != "" {
			if value, ok := strings.CutPrefix(line, "event:"); ok {
				eventName = strings.TrimSpace(value)
				continue
			}
			if value, ok := strings.CutPrefix(line, "data:"); ok {
				dataLines = append(dataLines, strings.TrimSpace(value))
			}
			continue
		}

		if eventName == "" || len(dataLines) == 0 {
			eventName = ""
			dataLines = nil
			continue
		}
		payload := strings.Join(dataLines, "\n")
		eventNameLocal := eventName
		eventName = ""
		dataLines = nil

		result := p.processPayload(eventNameLocal, payload)
		switch result.Action {
		case ssePayloadBreak:
			return p.finishEOF(), nil
		case ssePayloadReturn:
			return result.Result, result.Err
		case ssePayloadContinue:
			continue
		}
	}
	if err := sc.Err(); err != nil {
		p.logAggregate(p.out.ResponseID, "scanner_error", err)
		return p.out, err
	}
	return p.finishEOF(), nil
}

func (p *sseEventParser) finishEOF() RunResult {
	p.out.ReasoningSignaled = p.reasoningSignaled
	p.out.ReasoningVisible = p.reasoningVisible
	p.logAggregate(p.out.ResponseID, "eof", nil)
	return p.out
}

func (p *sseEventParser) processPayload(eventName, payload string) ssePayloadResult {
	if strings.TrimSpace(payload) == "[DONE]" {
		return ssePayloadResult{Action: ssePayloadBreak, Result: p.out}
	}
	var raw transportStreamEvent
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
	p.upstreamEventSeq++
	p.aggregate.observe(eventName, raw)
	return p.handleEvent(eventName, payload, raw)
}

func (p *sseEventParser) handleEvent(eventName, payload string, raw transportStreamEvent) ssePayloadResult {
	switch {
	case eventName == "response.output_text.delta":
		return p.handleOutputTextDelta(eventName, raw)
	case eventName == "response.output_item.added" || eventName == "response.output_item.done":
		return p.handleOutputItemEvent(eventName, raw)
	case eventName == "response.function_call_arguments.delta":
		return p.handleFunctionCallArgumentsDelta(eventName, raw)
	case eventName == "response.custom_tool_call_input.delta":
		return p.handleCustomToolCallInputDelta(eventName, raw)
	case strings.Contains(eventName, "reasoning") && strings.HasSuffix(eventName, ".delta"):
		return p.handleReasoningDelta(eventName, raw)
	case eventName == "response.reasoning_summary_part.added":
		return p.handleReasoningSummaryPartAdded(eventName, raw)
	case eventName == "response.completed":
		return p.handleResponseCompleted(eventName, payload, raw)
	case eventName == "response.created":
		return p.handleResponseCreated(raw)
	case eventName == "response.failed":
		return p.handleResponseFailed(eventName, raw)
	default:
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
}

func (p *sseEventParser) handleOutputTextDelta(eventName string, raw transportStreamEvent) ssePayloadResult {
	if delta := raw.Delta; delta != "" {
		err := p.emitNormalized(adapterrender.Event{Kind: adapterrender.EventAssistantTextDelta, Text: delta, EncryptedContent: "", Signature: ""})
		if err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleOutputItemEvent(eventName string, raw transportStreamEvent) ssePayloadResult {
	item := raw.Item
	itemType := item.string("type")
	switch itemType {
	case "reasoning":
		return p.handleReasoningOutputItem(eventName, raw, item)
	case "function_call":
		return p.handleFunctionCallOutputItem(eventName, raw, item, itemType)
	case "local_shell_call":
		return p.handleLocalShellOutputItem(eventName, raw, item, itemType)
	case "custom_tool_call":
		return p.handleCustomToolOutputItem(eventName, raw, item, itemType)
	default:
		if eventName == "response.output_item.done" && item != nil {
			p.out.OutputItems = append(p.out.OutputItems, item.cloneMap())
		}
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
}

func (p *sseEventParser) handleReasoningOutputItem(eventName string, raw transportStreamEvent, item transportItem) ssePayloadResult {
	if err := p.emitReasoningPresence(item.string("id")); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	if eventName != "response.output_item.done" || item == nil {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
	for _, ev := range reasoningEventsFromItem(item, p.reasoningSummaryDeltaSeen, p.reasoningTextDeltaSeen) {
		if ev.ReasoningKind == "summary" {
			p.reasoningSummaryDeltaSeen = true
		} else {
			p.reasoningTextDeltaSeen = true
		}
		if err := p.emitNormalized(ev); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	cloned := item.cloneMap()
	p.out.OutputItems = append(p.out.OutputItems, cloned)
	// Surface the encrypted_content blob on a per-item Finished event
	// when present. The renderer captures it and embeds the bytes on
	// the synthetic-thinking close marker as `data-encrypted` so
	// Cursor's transcript carries the blob into the next turn without
	// any side-channel persistence. Drop mode is honored upstream by
	// the WebsocketTransportConfig knob: when the resolver picks drop,
	// the parser config strips the blob before emit.
	encrypted := mapString(cloned, "encrypted_content")
	if !p.dropEncryptedContent && encrypted != "" {
		ev := adapterrender.Event{
			Kind:             adapterrender.EventReasoningFinished,
			Text:             "",
			ReasoningKind:    "",
			SummaryIndex:     nil,
			ToolCalls:        nil,
			ItemID:           mapString(cloned, "id"),
			ItemType:         "reasoning",
			EncryptedContent: encrypted,
			Signature:        "",
		}
		if err := p.emitNormalized(ev); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleFunctionCallOutputItem(eventName string, raw transportStreamEvent, item transportItem, itemType string) ssePayloadResult {
	itemID := strings.TrimSpace(item.string("id"))
	callID := strings.TrimSpace(item.string("call_id"))
	itemID, callID = normalizedToolIDs(itemID, callID)
	name := strings.TrimSpace(item.string("name"))
	args := item.string("arguments")
	state, created := p.getToolState(itemID, callID, name)
	if state.NativeName == "" {
		state.NativeName = name
	}
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	if state.Name == "" && name != "" {
		state.Name = name
	}
	if name != "" {
		p.observeActualToolCallName(name)
	} else {
		p.observeActualToolCallName(state.Name)
	}
	p.appendCompletedFunctionCallItem(eventName, item, state)
	p.out.SetFinishReason("tool_calls")
	return p.finishFunctionCallOutputItem(eventName, raw, itemType, itemID, args, state)
}

func normalizedToolIDs(itemID, callID string) (string, string) {
	if itemID == "" {
		itemID = callID
	}
	if callID == "" {
		callID = itemID
	}
	return itemID, callID
}

func (p *sseEventParser) appendCompletedFunctionCallItem(eventName string, item transportItem, state *toolCallState) {
	if eventName != "response.output_item.done" || item == nil {
		return
	}
	completed := item.cloneMap()
	if strings.TrimSpace(mapString(completed, "arguments")) == "" && state.Arguments.Len() > 0 {
		completed["arguments"] = state.Arguments.String()
	}
	p.out.OutputItems = append(p.out.OutputItems, completed)
}

func (p *sseEventParser) finishFunctionCallOutputItem(eventName string, raw transportStreamEvent, itemType, itemID, args string, state *toolCallState) ssePayloadResult {
	if eventName == "response.output_item.done" && state.NativeName == "shell_command" {
		return p.finishShellCommandOutputItem(eventName, raw, itemType, itemID, args, state)
	}
	if eventName == "response.output_item.done" && args != "" && !state.ArgumentDeltaSeen {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: args}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
		state.ArgumentsEmitted = true
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) finishShellCommandOutputItem(eventName string, raw transportStreamEvent, itemType, itemID, args string, state *toolCallState) ssePayloadResult {
	if args == "" {
		args = state.Arguments.String()
	}
	if converted, ok := ShellArgsFromShellCommandArguments(args); ok && !state.ArgumentsEmitted {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: converted}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
		state.ArgumentsEmitted = true
	} else if !state.ArgumentsEmitted {
		LogToolingEvent(nil, context.Background(), "", "shell_command.parse_failed",
			slog.String("item_type", itemType),
			slog.String("item_id", itemID),
			slog.String("tool_name", "Shell"),
		)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleLocalShellOutputItem(eventName string, raw transportStreamEvent, item transportItem, itemType string) ssePayloadResult {
	if eventName == "response.output_item.done" && item != nil {
		p.out.OutputItems = append(p.out.OutputItems, item.cloneMap())
	}
	itemID := strings.TrimSpace(item.string("id"))
	callID := strings.TrimSpace(item.string("call_id"))
	state, created := p.getToolState(itemID, callID, "Shell")
	p.observeActualToolCallName(state.Name)
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: "Shell"}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	p.out.SetFinishReason("tool_calls")
	if args, ok := ShellArgsFromLocalShellItem(item.cloneMap()); ok {
		return p.emitNativeParsedArguments(eventName, raw, itemType, state, "Shell", args)
	}
	if eventName == "response.output_item.done" && !state.ArgumentsEmitted {
		LogToolingEvent(nil, context.Background(), "", "native_local_shell.parse_failed",
			slog.String("item_type", itemType),
			slog.String("item_id", itemID),
			slog.String("tool_name", "Shell"),
		)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleCustomToolOutputItem(eventName string, raw transportStreamEvent, item transportItem, itemType string) ssePayloadResult {
	if eventName == "response.output_item.done" && item != nil {
		p.out.OutputItems = append(p.out.OutputItems, item.cloneMap())
	}
	itemID := strings.TrimSpace(item.string("id"))
	callID := strings.TrimSpace(item.string("call_id"))
	name := strings.TrimSpace(item.string("name"))
	cursorName := name
	if IsApplyPatchToolName(cursorName) || IsApplyPatchToolName(name) {
		cursorName = "ApplyPatch"
	}
	state, created := p.getToolState(itemID, callID, cursorName)
	if name != "" {
		p.observeActualToolCallName(name)
	} else {
		p.observeActualToolCallName(state.Name)
	}
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	p.out.SetFinishReason("tool_calls")
	input := item.string("input")
	if input == "" {
		input = state.Input.String()
	}
	if args, ok := ApplyPatchArgs(input); ok {
		return p.emitNativeParsedArguments(eventName, raw, itemType, state, cursorName, args)
	}
	if eventName == "response.output_item.done" && !state.ArgumentsEmitted {
		LogToolingEvent(nil, context.Background(), "", "native_custom_tool.parse_failed",
			slog.String("item_type", itemType),
			slog.String("item_id", itemID),
			slog.String("tool_name", cursorName),
		)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) emitNativeParsedArguments(eventName string, raw transportStreamEvent, itemType string, state *toolCallState, toolName, args string) ssePayloadResult {
	p.logNativeToolParsed(eventName, itemType, state, toolName)
	if state.ArgumentsEmitted {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
	if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: args}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	state.ArgumentsEmitted = true
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleFunctionCallArgumentsDelta(eventName string, raw transportStreamEvent) ssePayloadResult {
	itemID := strings.TrimSpace(raw.ItemID)
	delta := raw.Delta
	state := p.toolCallsByItemID[itemID]
	if state == nil || delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
	state.ArgumentDeltaSeen = true
	state.Arguments.WriteString(delta)
	p.out.SetFinishReason("tool_calls")
	if state.NativeName == "shell_command" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
	if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: delta}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleCustomToolCallInputDelta(eventName string, raw transportStreamEvent) ssePayloadResult {
	itemID := strings.TrimSpace(raw.ItemID)
	callID := strings.TrimSpace(raw.CallID)
	delta := raw.Delta
	state, created := p.getToolState(itemID, callID, "ApplyPatch")
	p.observeActualToolCallName(state.Name)
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	if delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
	state.Input.WriteString(delta)
	state.ArgumentDeltaSeen = true
	p.out.SetFinishReason("tool_calls")
	if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: delta}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	state.ArgumentsEmitted = true
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleReasoningDelta(eventName string, raw transportStreamEvent) ssePayloadResult {
	if raw.Delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
	}
	p.reasoningSignaled = true
	p.reasoningVisible = true
	kind := "text"
	var summaryIdx *int
	if strings.Contains(eventName, "summary") {
		kind = "summary"
		summaryIdx = raw.SummaryIndex
		p.reasoningSummaryDeltaSeen = true
	} else {
		p.reasoningTextDeltaSeen = true
	}
	err := p.emitNormalized(adapterrender.Event{
		Kind:             adapterrender.EventReasoningDelta,
		Text:             raw.Delta,
		ReasoningKind:    kind,
		SummaryIndex:     summaryIdx,
		ItemID:           strings.TrimSpace(raw.ItemID),
		ItemType:         "reasoning",
		EncryptedContent: "",
		Signature:        "",
	})
	if err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: RunResult{}, Err: err}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleReasoningSummaryPartAdded(eventName string, raw transportStreamEvent) ssePayloadResult {
	if err := p.emitReasoningPresence(raw.ItemID); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleResponseCompleted(eventName, payload string, raw transportStreamEvent) ssePayloadResult {
	status := ""
	var c completedResponse
	if err := json.Unmarshal([]byte(payload), &c); err == nil {
		if responseID := strings.TrimSpace(c.Response.ID); responseID != "" {
			p.out.ResponseID = responseID
		}
		status = strings.TrimSpace(c.Response.Status)
		p.out.Usage = mapUsage(c)
		p.out.UsageTelemetry = usageTelemetry(c)
		if p.out.FinishReason != "tool_calls" {
			p.out.SetFinishReason(finishreason.FromCodexResponse(c.Response.Status, c.Response.IncompleteDetails.Reason))
		}
	}
	if reasoningTokensFromEvent(raw) > 0 && !p.reasoningSignaled {
		if err := p.emitReasoningPresence(""); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	if err := p.emitNormalized(adapterrender.Event{Kind: adapterrender.EventReasoningFinished, EncryptedContent: "", Signature: ""}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	p.out.ReasoningSignaled = p.reasoningSignaled
	p.out.ReasoningVisible = p.reasoningVisible
	p.logAggregate(p.out.ResponseID, status, nil)
	return ssePayloadResult{Action: ssePayloadReturn, Result: p.out}
}

func (p *sseEventParser) handleResponseCreated(raw transportStreamEvent) ssePayloadResult {
	if raw.Response != nil {
		p.out.ResponseID = strings.TrimSpace(raw.Response.ID)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out}
}

func (p *sseEventParser) handleResponseFailed(eventName string, raw transportStreamEvent) ssePayloadResult {
	msg := "codex response failed"
	if raw.Error != nil && raw.Error.Message != "" {
		msg = raw.Error.Message
	}
	err := codexResponseFailedError(msg)
	if strings.TrimSpace(msg) != "" && !isContextWindowError(err) {
		_ = p.emitNormalized(adapterrender.Event{Kind: adapterrender.EventReasoningFinished, EncryptedContent: "", Signature: ""})
	}
	p.logAggregate(p.out.ResponseID, "failed", err)
	return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
}

func (p *sseEventParser) logAggregate(responseID, status string, err error) {
	if !p.logCtx.Enabled() {
		return
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	p.aggregate.Log(p.ctx, p.logCtx, responseID, status, errText)
}

func (p *sseEventParser) emitNormalized(ev adapterrender.Event) error {
	return p.emit(ev)
}

func (p *sseEventParser) emitReasoningPresence(itemID string) error {
	if p.reasoningSignaled {
		return nil
	}
	p.reasoningSignaled = true
	p.reasoningVisible = true
	return p.emitNormalized(adapterrender.Event{
		Kind:             adapterrender.EventReasoningSignaled,
		ItemID:           strings.TrimSpace(itemID),
		ItemType:         "reasoning",
		EncryptedContent: "",
		Signature:        "",
	})
}

func (p *sseEventParser) emitToolCall(state *toolCallState, fn adapteropenai.ToolCallFunction) error {
	if state == nil {
		return nil
	}
	tc := adapteropenai.ToolCall{
		Index:    state.Index,
		Function: fn,
	}
	if !state.IdentityEmitted {
		tc.ID = state.CallID
		tc.Type = state.Type
		state.IdentityEmitted = true
	}
	return p.emitNormalized(adapterrender.Event{
		Kind:             adapterrender.EventToolCallDelta,
		ToolCalls:        []adapteropenai.ToolCall{tc},
		EncryptedContent: "",
		Signature:        "",
	})
}

func (p *sseEventParser) logNativeToolParsed(eventName, itemType string, state *toolCallState, toolName string) {
	if state == nil || state.NativeParseLogged {
		return
	}
	state.NativeParseLogged = true
	LogToolingEvent(nil, p.ctx, p.logCtx.RequestID, "native_tool_item.parsed",
		slog.String("sse_event", eventName),
		slog.String("item_type", itemType),
		slog.String("item_id", state.ItemID),
		slog.String("call_id", state.CallID),
		slog.String("tool_name", toolName),
	)
}

func (p *sseEventParser) getToolState(itemID, callID, name string) (*toolCallState, bool) {
	itemID = strings.TrimSpace(itemID)
	callID = strings.TrimSpace(callID)
	itemID, callID = normalizedToolIDs(itemID, callID)
	if state := p.toolCallsByItemID[itemID]; state != nil {
		p.updateToolState(state, callID, name)
		return state, false
	}
	if callID != "" {
		if state := p.toolCallsByItemID[callID]; state != nil {
			if itemID != "" {
				p.toolCallsByItemID[itemID] = state
			}
			if state.Name == "" {
				state.Name = name
			}
			return state, false
		}
	}
	return p.createToolState(itemID, callID, name), true
}

func (p *sseEventParser) updateToolState(state *toolCallState, callID, name string) {
	if state.CallID == "" {
		state.CallID = callID
	}
	if state.Name == "" {
		state.Name = name
	}
	if callID != "" {
		p.toolCallsByItemID[callID] = state
	}
}

func (p *sseEventParser) createToolState(itemID, callID, name string) *toolCallState {
	state := &toolCallState{
		Index:  p.nextToolIndex,
		ItemID: itemID,
		CallID: callID,
		Name:   name,
		Type:   "function",
	}
	p.nextToolIndex++
	p.out.ToolCallCount = max(p.out.ToolCallCount, p.nextToolIndex)
	if itemID != "" {
		p.toolCallsByItemID[itemID] = state
	}
	if callID != "" {
		p.toolCallsByItemID[callID] = state
	}
	return state
}

func (p *sseEventParser) observeActualToolCallName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	p.recordToolCallName(name)
	if isSubagentToolCallName(name) {
		p.out.HasSubagentToolCall = true
	}
}

func (p *sseEventParser) recordToolCallName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	if slices.Contains(p.out.ToolCallNames, name) {
		return
	}
	p.out.ToolCallNames = append(p.out.ToolCallNames, name)
}

func isSubagentToolCallName(name string) bool {
	switch strings.TrimSpace(name) {
	case "Task", "Subagent", "spawn_agent":
		return true
	default:
		return false
	}
}

func isContextWindowError(err error) bool {
	if err == nil {
		return false
	}
	var contextErr *ContextWindowError
	return errors.As(err, &contextErr)
}

func codexResponseFailedError(message string) error {
	if isCodexContextWindowMessage(message) {
		return &ContextWindowError{Message: strings.TrimSpace(message)}
	}
	if isCodexUnsupportedModelMessage(message) {
		return &UnsupportedModelError{Message: strings.TrimSpace(message)}
	}
	return fmt.Errorf("%s", message)
}

func isCodexContextWindowMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(normalized, "exceeds the context window"):
		return true
	case strings.Contains(normalized, "context_length_exceeded"):
		return true
	case strings.Contains(normalized, "maximum context length"):
		return true
	default:
		return false
	}
}

func isCodexUnsupportedModelMessage(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	switch {
	case strings.Contains(normalized, "model is not supported"):
		return true
	case strings.Contains(normalized, "unsupported model"):
		return true
	default:
		return false
	}
}

func (item transportItem) string(key string) string {
	raw := item[key]
	if len(raw) == 0 {
		return ""
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return ""
	}
	return out
}

func (item transportItem) cloneMap() map[string]any {
	if item == nil {
		return nil
	}
	raw, _ := json.Marshal(item)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func reasoningEventsFromItem(item transportItem, skipSummary, skipText bool) []adapterrender.Event {
	if item == nil {
		return nil
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	var payload reasoningItemPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	itemID := strings.TrimSpace(payload.ID)
	var out []adapterrender.Event
	if !skipSummary {
		for i, part := range payload.Summary {
			if strings.TrimSpace(part.Type) != "summary_text" || part.Text == "" {
				continue
			}
			idx := i
			out = append(out, adapterrender.Event{
				Kind:             adapterrender.EventReasoningDelta,
				Text:             part.Text,
				ReasoningKind:    "summary",
				SummaryIndex:     &idx,
				ItemID:           itemID,
				ItemType:         "reasoning",
				EncryptedContent: "",
				Signature:        "",
			})
		}
	}
	if !skipText {
		for _, part := range payload.Content {
			switch strings.TrimSpace(part.Type) {
			case "reasoning_text", "text":
				if part.Text == "" {
					continue
				}
				out = append(out, adapterrender.Event{
					Kind:             adapterrender.EventReasoningDelta,
					Text:             part.Text,
					ReasoningKind:    "text",
					ItemID:           itemID,
					ItemType:         "reasoning",
					EncryptedContent: "",
					Signature:        "",
				})
			}
		}
	}
	return out
}

func mapUsage(c completedResponse) adapteropenai.Usage {
	if c.Response.Usage == nil {
		return adapteropenai.Usage{}
	}
	usage := c.Response.Usage
	u := adapteropenai.Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if usage.InputTokensDetails != nil {
		u.PromptTokensDetails = &adapteropenai.PromptTokensDetails{CachedTokens: usage.InputTokensDetails.CachedTokens}
	}
	return u
}

func reasoningTokensFromEvent(raw transportStreamEvent) int {
	if raw.Response == nil {
		return 0
	}
	return raw.Response.Usage.OutputTokensDetails.ReasoningTokens
}

func usageTelemetry(c completedResponse) CodexUsageTelemetry {
	if c.Response.Usage == nil {
		return CodexUsageTelemetry{}
	}
	usage := c.Response.Usage
	out := CodexUsageTelemetry{
		UsagePresent: true,
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
	}
	if usage.InputTokensDetails != nil {
		out.InputTokensDetailsPresent = true
		out.CachedTokens = usage.InputTokensDetails.CachedTokens
	}
	if usage.OutputTokensDetails != nil {
		out.OutputTokensDetailsPresent = true
		out.ReasoningOutputTokens = usage.OutputTokensDetails.ReasoningTokens
	}
	return out
}

func rawString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}
