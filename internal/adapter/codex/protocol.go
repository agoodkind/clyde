package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"goodkind.io/clyde/codexwire"
	"goodkind.io/clyde/internal/adapter/finishreason"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
)

// Reasoning is part of Clyde's typed adapter surface.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// RunResult is part of Clyde's typed adapter surface.
type RunResult struct {
	Usage                      adapteropenai.Usage
	UsageTelemetry             UsageTelemetry
	FinishReason               string
	ReasoningSignaled          bool
	ReasoningVisible           bool
	DerivedCacheCreationTokens int
	ResponseID                 string
	OutputItems                []codexwire.InputItem
	ToolCallCount              int
	ToolCallNames              []string
	HasSubagentToolCall        bool
}

// ContextWindowError reports an upstream Codex over-context rejection.
// The adapter maps this to OpenAI's context_length_exceeded shape so
// Cursor can run its normal retry flow.
type ContextWindowError struct {
	Message string
}

// Error is part of Clyde's typed adapter surface.
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

// Error is part of Clyde's typed adapter surface.
func (e *UnsupportedModelError) Error() string {
	if e == nil || strings.TrimSpace(e.Message) == "" {
		return "codex model is not supported"
	}
	return e.Message
}

// NewRunResult is part of Clyde's typed adapter surface.
func NewRunResult(finishReason string) RunResult {
	return RunResult{
		FinishReason: finishreason.FromCodex(finishReason), Usage: adapteropenai.
				Usage{PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0},

		UsageTelemetry: UsageTelemetry{UsagePresent: false, InputTokens: 0, OutputTokens: 0, TotalTokens: 0, InputTokensDetailsPresent: false, CachedTokens: 0, OutputTokensDetailsPresent: false, ReasoningOutputTokens: 0},

		ReasoningSignaled: false, ReasoningVisible: false, DerivedCacheCreationTokens: 0, ResponseID: "", OutputItems: nil, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false,
	}
}

// SetFinishReason is part of Clyde's typed adapter surface.
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

// UsageTelemetry is the provider-owned diagnostic view of Codex
// Responses usage. The boolean fields intentionally distinguish an
// explicitly present zero from omitted/null details.
type UsageTelemetry struct {
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

// codexItemType is the closed enum of Codex history item type
// strings observed on response output items. It is kept as a thin
// alias over [codexwire.ItemType] so the codex package can spell
// the item-type switches without an import-path stutter.
type codexItemType = codexwire.ItemType

const (
	codexItemTypeMessage              = codexwire.ItemTypeMessage
	codexItemTypeFunctionCall         = codexwire.ItemTypeFunctionCall
	codexItemTypeLocalShellCall       = codexwire.ItemTypeLocalShellCall
	codexItemTypeCustomToolCall       = codexwire.ItemTypeCustomToolCall
	codexItemTypeFunctionCallOutput   = codexwire.ItemTypeFunctionCallOutput
	codexItemTypeCustomToolCallOutput = codexwire.ItemTypeCustomToolCallOutput
	codexItemTypeReasoning            = codexwire.ItemTypeReasoning
)

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
	Type              string
	IdentityEmitted   bool
	ArgumentDeltaSeen bool
	ArgumentsEmitted  bool
	NativeParseLogged bool
	Arguments         strings.Builder
	Input             strings.Builder
}

// ClientMetadataWithTurn builds the typed codex-cli client_metadata
// carrying the installation id, window id, and `x-codex-turn-metadata`
// JSON blob. codex-rs build_ws_client_metadata
// (research/codex/codex-rs/core/src/client.rs) populates the same keys;
// Clyde mirrors that subset. turnMetadataJSON should be the
// already-marshaled JSON string from TurnMetadata.MarshalCompact.
// Returns nil when no key is populated so the caller omits the whole
// `client_metadata` field.
func ClientMetadataWithTurn(installationID, windowID, turnMetadataJSON string) *ClientMetadata {
	out := ClientMetadata{
		InstallationID:  strings.TrimSpace(installationID),
		WindowID:        strings.TrimSpace(windowID),
		Subagent:        "",
		ParentThreadID:  "",
		TurnMetadata:    strings.TrimSpace(turnMetadataJSON),
		WSStreamStartMS: "",
		Traceparent:     "",
		Tracestate:      "",
	}
	if out.IsEmpty() {
		return nil
	}
	return &out
}

// RequestInclude is part of Clyde's typed adapter surface.
func RequestInclude(requested []string, includeEncryptedReasoning bool) []string {
	if len(requested) == 0 && !includeEncryptedReasoning {
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
	if includeEncryptedReasoning {
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

// EffectiveReasoningWithDefaultSummary is part of Clyde's typed adapter surface.
func EffectiveReasoningWithDefaultSummary(req adapteropenai.ChatRequest, effort, defaultSummary string) *Reasoning {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	}
	if effort == "" && req.Reasoning != nil {
		effort = strings.ToLower(strings.TrimSpace(req.Reasoning.Effort))
	}
	var out Reasoning
	switch reasoningEffort(effort) {
	case reasoningEffortNone, reasoningEffortMinimal, reasoningEffortLow, reasoningEffortMedium, reasoningEffortHigh, reasoningEffortXHigh:
		out.Effort = effort
	}
	if req.Reasoning != nil {
		switch reasoningSummary(strings.ToLower(strings.TrimSpace(req.Reasoning.Summary))) {
		case reasoningSummaryAuto, reasoningSummaryConcise, reasoningSummaryDetailed, reasoningSummaryNone:
			out.Summary = strings.ToLower(strings.TrimSpace(req.Reasoning.Summary))
		}
	}
	if out.Effort != "" && out.Summary == "" {
		switch reasoningSummary(strings.ToLower(strings.TrimSpace(defaultSummary))) {
		case reasoningSummaryAuto, reasoningSummaryConcise, reasoningSummaryDetailed, reasoningSummaryNone:
			out.Summary = strings.ToLower(strings.TrimSpace(defaultSummary))
		default:
			out.Summary = string(reasoningSummaryAuto)
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
	// DeclaredTools is the tools array the client sent on this request.
	// When codex answers with one of its native tool types (a freeform
	// apply_patch custom_tool_call, or a local_shell_call), the parser
	// maps the native call back to the client-declared tool name by
	// SHAPE using these specs, so the client receives a tool name it
	// recognizes. It is never used to match by hardcoded client names.
	DeclaredTools []codexwire.ToolSpec
}

// ParseSSEEventsWithOptions is the option-aware codex SSE parser entry
// point. Callers that do not need to override defaults pass a zero
// [SSEParseOptions].
func ParseSSEEventsWithOptions(ctx context.Context, body io.Reader, emit func(adapterrender.Event) error, logCtx sseInstrumentationContext, opts SSEParseOptions) (RunResult, error) {
	parser := newSSEEventParser(ctx, emit, logCtx)
	parser.dropEncryptedContent = opts.DropEncryptedContent
	parser.tools = newToolHandlers(newDeclaredToolResolver(opts.DeclaredTools))
	return parser.parse(ctx, body)
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
	logContext                func() context.Context
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
	// tools is the provider-neutral set of reply-item handlers plus the
	// chooser that maps codex native tool item types back to the
	// client-declared tool name and field names by parameter shape.
	tools toolHandlers
}

func newSSEEventParser(ctx context.Context, emit func(adapterrender.Event) error, logCtx sseInstrumentationContext) *sseEventParser {
	return &sseEventParser{
		logContext:           func() context.Context { return ctx },
		emit:                 emit,
		logCtx:               logCtx,
		out:                  NewRunResult("stop"),
		toolCallsByItemID:    make(map[string]*toolCallState),
		tools:                newToolHandlers(newDeclaredToolResolver(nil)),
		dropEncryptedContent: false, reasoningSignaled: false, reasoningVisible: false, reasoningTextDeltaSeen: false, reasoningSummaryDeltaSeen: false, nextToolIndex: 0, aggregate: sseAggregateCollector{Assistant: assistantTextDeltaAggregate{DeltaCount: 0, CharCount: 0, FirstPreview: "", LastPreview: "", NormalizedText: strings.
					Builder{}, PendingWhitespace: false}, Items: outputItemAggregate{Added: outputItemCounts{Total: 0, ToolTotal: 0, TypeCounts: nil}, Done: outputItemCounts{Total: 0, ToolTotal: 0, TypeCounts: nil}}, Tools: toolDeltaAggregate{FunctionArgumentDeltaCount: 0, FunctionArgumentDeltaChars: 0, CustomToolInputDeltaCount: 0, CustomToolInputDeltaChars: 0}, Reasoning: reasoningAggregate{DeltaCount: 0, DeltaChars: 0, TextDeltaCount: 0, TextDeltaChars: 0, SummaryDeltaCount: 0, SummaryDeltaChars: 0}},

		upstreamEventSeq: 0,
	}
}

func (p *sseEventParser) parse(ctx context.Context, body io.Reader) (RunResult, error) {
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

		result := p.processPayload(ctx, eventNameLocal, payload)
		switch result.Action {
		case ssePayloadBreak:
			return p.finishEOF(ctx), nil
		case ssePayloadReturn:
			return result.Result, result.Err
		case ssePayloadContinue:
			continue
		}
	}
	if err := sc.Err(); err != nil {
		p.logAggregate(ctx, p.out.ResponseID, "scanner_error", err)
		slog.WarnContext(ctx, "adapter.codex.protocol.parse_scanner_error", "concern", "adapter.providers.codex.request", "request_id", p.logCtx.RequestID, "err", err)
		return p.out, fmt.Errorf("scan codex SSE events: %w", err)
	}
	return p.finishEOF(ctx), nil
}

func (p *sseEventParser) finishEOF(ctx context.Context) RunResult {
	p.out.ReasoningSignaled = p.reasoningSignaled
	p.out.ReasoningVisible = p.reasoningVisible
	p.logAggregate(ctx, p.out.ResponseID, "eof", nil)
	return p.out
}

func (p *sseEventParser) processPayload(ctx context.Context, eventName, payload string) ssePayloadResult {
	if strings.TrimSpace(payload) == "[DONE]" {
		return ssePayloadResult{Action: ssePayloadBreak, Result: p.out, Err: nil}
	}
	var raw transportStreamEvent
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
	p.upstreamEventSeq++
	p.aggregate.observe(eventName, raw)
	return p.handleEvent(ctx, eventName, payload, raw)
}

func (p *sseEventParser) handleEvent(ctx context.Context, eventName, payload string, raw transportStreamEvent) ssePayloadResult {
	switch {
	case eventName == "response.output_text.delta":
		return p.handleOutputTextDelta(raw)
	case eventName == "response.output_item.added" || eventName == "response.output_item.done":
		return p.handleOutputItemEvent(ctx, eventName, raw)
	case eventName == "response.function_call_arguments.delta":
		return p.handleFunctionCallArgumentsDelta(raw)
	case eventName == "response.custom_tool_call_input.delta":
		return p.handleCustomToolCallInputDelta(raw)
	case strings.Contains(eventName, "reasoning") && strings.HasSuffix(eventName, ".delta"):
		return p.handleReasoningDelta(eventName, raw)
	case eventName == "response.reasoning_summary_part.added":
		return p.handleReasoningSummaryPartAdded(raw)
	case eventName == "response.completed":
		return p.handleResponseCompleted(ctx, payload, raw)
	case eventName == "response.created":
		return p.handleResponseCreated(raw)
	case eventName == "response.failed":
		return p.handleResponseFailed(ctx, raw)
	default:
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
}

func (p *sseEventParser) handleOutputTextDelta(raw transportStreamEvent) ssePayloadResult {
	if delta := raw.Delta; delta != "" {
		if err := p.emitNormalized(adapterrender.TextDelta{Text: delta}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleOutputItemEvent(ctx context.Context, eventName string, raw transportStreamEvent) ssePayloadResult {
	item := raw.Item
	itemType := item.string("type")
	if codexItemType(itemType) == codexItemTypeReasoning {
		return p.handleReasoningOutputItem(eventName, item)
	}
	// A tool reply routes to the handler keyed by its reply kind. The
	// chooser inside each handler maps the native call back to the
	// client-declared tool by parameter shape.
	if kind, ok := toolReplyKindForItemType(codexItemType(itemType)); ok {
		switch kind {
		case toolReplyFunction:
			return p.handleFunctionCallOutputItem(eventName, item)
		case toolReplyShell:
			return p.handleLocalShellOutputItem(ctx, eventName, item, itemType)
		case toolReplyPatch:
			return p.handleCustomToolOutputItem(ctx, eventName, item, itemType)
		}
	}
	// Output-item messages and tool outputs are not tool replies; the
	// parser keeps the raw envelope on response.output_item.done so the
	// renderer can replay it.
	if eventName == "response.output_item.done" && item != nil {
		p.out.OutputItems = append(p.out.OutputItems, item.toInputItem())
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleReasoningOutputItem(eventName string, item transportItem) ssePayloadResult {
	if err := p.emitReasoningPresence(item.string("id")); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	if eventName != "response.output_item.done" || item == nil {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
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
	cloned := item.toInputItem()
	p.out.OutputItems = append(p.out.OutputItems, cloned)
	// Surface the encrypted_content blob on a per-item Finished event
	// when present. The renderer captures it and embeds the bytes on
	// the synthetic-thinking close marker as `data-encrypted` so
	// Cursor's transcript carries the blob into the next turn without
	// any side-channel persistence. Drop mode is honored upstream by
	// the WebsocketTransportConfig knob: when the resolver picks drop,
	// the parser config strips the blob before emit.
	encrypted := cloned.EncryptedContent
	if !p.dropEncryptedContent && encrypted != "" {
		ev := adapterrender.ReasoningFinished{
			ReasoningKind:    "",
			EncryptedContent: encrypted,
			Signature:        "",
			ItemID:           cloned.ID,
			ItemType:         "reasoning",
		}
		if err := p.emitNormalized(ev); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleFunctionCallOutputItem(eventName string, item transportItem) ssePayloadResult {
	itemID := strings.TrimSpace(item.string("id"))
	callID := strings.TrimSpace(item.string("call_id"))
	itemID, callID = normalizedToolIDs(itemID, callID)
	// The function handler relays the client-declared tool name: a
	// function_call already carries the name the client declared, so it
	// passes through; an empty name falls back to a stable item-type name.
	rawName := strings.TrimSpace(item.string("name"))
	name := ""
	if rawName != "" {
		name = p.tools.functionToolName(rawName)
	}
	args := item.string("arguments")
	state, created := p.getToolState(itemID, callID, name)
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name, Arguments: ""}); err != nil {
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
	return p.finishFunctionCallOutputItem(eventName, args, state)
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
	completed := item.toInputItem()
	if strings.TrimSpace(completed.Arguments) == "" && state.Arguments.Len() > 0 {
		completed.Arguments = state.Arguments.String()
	}
	p.out.OutputItems = append(p.out.OutputItems, completed)
}

func (p *sseEventParser) finishFunctionCallOutputItem(eventName, args string, state *toolCallState) ssePayloadResult {
	// The function handler relays the arguments string verbatim: it is
	// opaque JSON the model emitted, so clyde does not reshape it.
	relayed, ok := p.tools.handleFunctionArguments(args)
	if eventName == "response.output_item.done" && ok && !state.ArgumentDeltaSeen {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: relayed, Name: ""}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
		state.ArgumentsEmitted = true
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleLocalShellOutputItem(ctx context.Context, eventName string, item transportItem, itemType string) ssePayloadResult {
	if eventName == "response.output_item.done" && item != nil {
		p.out.OutputItems = append(p.out.OutputItems, item.toInputItem())
	}
	itemID := strings.TrimSpace(item.string("id"))
	callID := strings.TrimSpace(item.string("call_id"))
	// local_shell_call is a codex-internal item TYPE; the shell handler
	// maps it back to the client-declared command-like tool name by shape.
	// If none is declared, the chooser falls back to the codex item type
	// so the call still has a stable name.
	toolName := p.tools.shellToolName()
	state, created := p.getToolState(itemID, callID, toolName)
	p.observeActualToolCallName(state.Name)
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name, Arguments: ""}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	p.out.SetFinishReason("tool_calls")
	// The shell handler renders the action under the field names the
	// client declared on its command-like tool, not hardcoded names.
	if args, ok := p.tools.handleShellAction(item.toInputItem()); ok {
		return p.emitNativeParsedArguments(ctx, eventName, itemType, state, state.Name, args)
	}
	if eventName == "response.output_item.done" && !state.ArgumentsEmitted {
		LogToolingEvent(nil, ctx, p.logCtx.RequestID, "native_local_shell.parse_failed",
			slog.String("item_type", itemType),
			slog.String("item_id", itemID),
			slog.String("tool_name", state.Name),
		)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleCustomToolOutputItem(ctx context.Context, eventName string, item transportItem, itemType string) ssePayloadResult {
	if eventName == "response.output_item.done" && item != nil {
		p.out.OutputItems = append(p.out.OutputItems, item.toInputItem())
	}
	itemID := strings.TrimSpace(item.string("id"))
	callID := strings.TrimSpace(item.string("call_id"))
	name := strings.TrimSpace(item.string("name"))
	// custom_tool_call is codex's freeform apply_patch type; the patch
	// handler maps it back to the client-declared patch-like tool name by
	// shape rather than by the codex-internal "apply_patch" name.
	clientName := p.tools.patchToolName(name)
	state, created := p.getToolState(itemID, callID, clientName)
	if name != "" {
		p.observeActualToolCallName(name)
	} else {
		p.observeActualToolCallName(state.Name)
	}
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name, Arguments: ""}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	p.out.SetFinishReason("tool_calls")
	input := item.string("input")
	if input == "" {
		input = state.Input.String()
	}
	// The patch handler unwraps the JSON wrapper and repairs the
	// freeform body so it satisfies the vendored apply_patch grammar.
	if args, ok := p.tools.handlePatchInput(input); ok {
		return p.emitNativeParsedArguments(ctx, eventName, itemType, state, state.Name, args)
	}
	if eventName == "response.output_item.done" && !state.ArgumentsEmitted {
		LogToolingEvent(nil, ctx, p.logCtx.RequestID, "native_custom_tool.parse_failed",
			slog.String("item_type", itemType),
			slog.String("item_id", itemID),
			slog.String("tool_name", state.Name),
		)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) emitNativeParsedArguments(ctx context.Context, eventName string, itemType string, state *toolCallState, toolName, args string) ssePayloadResult {
	p.logNativeToolParsed(ctx, eventName, itemType, state, toolName)
	if state.ArgumentsEmitted {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
	if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: args, Name: ""}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	state.ArgumentsEmitted = true
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleFunctionCallArgumentsDelta(raw transportStreamEvent) ssePayloadResult {
	itemID := strings.TrimSpace(raw.ItemID)
	delta := raw.Delta
	state := p.toolCallsByItemID[itemID]
	if state == nil || delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
	state.ArgumentDeltaSeen = true
	state.Arguments.WriteString(delta)
	p.out.SetFinishReason("tool_calls")
	// Function-call argument deltas stream through verbatim: the
	// arguments are opaque JSON the model emitted, so clyde does not
	// buffer-and-reshape any tool's args.
	if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: delta, Name: ""}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleCustomToolCallInputDelta(raw transportStreamEvent) ssePayloadResult {
	itemID := strings.TrimSpace(raw.ItemID)
	callID := strings.TrimSpace(raw.CallID)
	delta := raw.Delta
	// custom_tool_call input deltas carry codex's freeform apply_patch
	// body; the patch handler maps it back to the client-declared
	// patch-like tool name by shape.
	state, created := p.getToolState(itemID, callID, p.tools.patchToolName(""))
	p.observeActualToolCallName(state.Name)
	if created {
		if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Name: state.Name, Arguments: ""}); err != nil {
			return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
		}
	}
	if delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
	}
	state.Input.WriteString(delta)
	state.ArgumentDeltaSeen = true
	p.out.SetFinishReason("tool_calls")
	if err := p.emitToolCall(state, adapteropenai.ToolCallFunction{Arguments: delta, Name: ""}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	state.ArgumentsEmitted = true
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleReasoningDelta(eventName string, raw transportStreamEvent) ssePayloadResult {
	if raw.Delta == "" {
		return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
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
	err := p.emitNormalized(adapterrender.ReasoningDelta{
		Text:          raw.Delta,
		ReasoningKind: kind,
		SummaryIndex:  summaryIdx,
		Signature:     "",
		RedactedData:  "",
		ItemID:        strings.TrimSpace(raw.ItemID),
		ItemType:      "reasoning",
	})
	if err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: RunResult{
			Usage: adapteropenai.
				Usage{PromptTokens: 0, CompletionTokens: 0, TotalTokens: 0, PromptTokensDetails: nil, InputTokens: 0, OutputTokens: 0, CacheReadTokens: 0, CacheWriteTokens: 0, MaxTokens: 0},

			UsageTelemetry: UsageTelemetry{UsagePresent: false, InputTokens: 0, OutputTokens: 0, TotalTokens: 0, InputTokensDetailsPresent: false, CachedTokens: 0, OutputTokensDetailsPresent: false, ReasoningOutputTokens: 0},

			FinishReason: "", ReasoningSignaled: false, ReasoningVisible: false, DerivedCacheCreationTokens: 0, ResponseID: "", OutputItems: nil, ToolCallCount: 0, ToolCallNames: nil, HasSubagentToolCall: false,
		}, Err: err}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleReasoningSummaryPartAdded(raw transportStreamEvent) ssePayloadResult {
	if err := p.emitReasoningPresence(raw.ItemID); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleResponseCompleted(ctx context.Context, payload string, raw transportStreamEvent) ssePayloadResult {
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
	if err := p.emitNormalized(adapterrender.ReasoningFinished{
		ReasoningKind:    "",
		EncryptedContent: "",
		Signature:        "",
		ItemID:           "",
		ItemType:         "",
	}); err != nil {
		return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
	}
	p.out.ReasoningSignaled = p.reasoningSignaled
	p.out.ReasoningVisible = p.reasoningVisible
	p.logAggregate(ctx, p.out.ResponseID, status, nil)
	return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleResponseCreated(raw transportStreamEvent) ssePayloadResult {
	if raw.Response != nil {
		p.out.ResponseID = strings.TrimSpace(raw.Response.ID)
	}
	return ssePayloadResult{Action: ssePayloadContinue, Result: p.out, Err: nil}
}

func (p *sseEventParser) handleResponseFailed(ctx context.Context, raw transportStreamEvent) ssePayloadResult {
	msg := "codex response failed"
	if raw.Error != nil && raw.Error.Message != "" {
		msg = raw.Error.Message
	}
	err := codexResponseFailedError(msg)
	if strings.TrimSpace(msg) != "" && !isContextWindowError(err) {
		_ = p.emitNormalized(adapterrender.ReasoningFinished{
			ReasoningKind:    "",
			EncryptedContent: "",
			Signature:        "",
			ItemID:           "",
			ItemType:         "",
		})
	}
	p.logAggregate(ctx, p.out.ResponseID, "failed", err)
	return ssePayloadResult{Action: ssePayloadReturn, Result: p.out, Err: err}
}

func (p *sseEventParser) logAggregate(ctx context.Context, responseID, status string, err error) {
	if !p.logCtx.Enabled() {
		return
	}
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	p.aggregate.Log(ctx, p.logCtx, responseID, status, errText)
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
	return p.emitNormalized(adapterrender.ReasoningSignaled{
		ReasoningKind: "",
		ItemID:        strings.TrimSpace(itemID),
		ItemType:      "reasoning",
	})
}

func (p *sseEventParser) emitToolCall(state *toolCallState, fn adapteropenai.ToolCallFunction) error {
	if state == nil {
		return nil
	}
	tc := adapteropenai.ToolCall{
		Index:    state.Index,
		Function: fn, ID: "", Type: "",
	}
	if !state.IdentityEmitted {
		tc.ID = state.CallID
		tc.Type = state.Type
		state.IdentityEmitted = true
	}
	return p.emitNormalized(adapterrender.ToolCallDelta{
		ToolCalls: []adapteropenai.ToolCall{tc},
	})
}

func (p *sseEventParser) logNativeToolParsed(ctx context.Context, eventName, itemType string, state *toolCallState, toolName string) {
	if state == nil || state.NativeParseLogged {
		return
	}
	state.NativeParseLogged = true
	LogToolingEvent(nil, ctx, p.logCtx.RequestID, "native_tool_item.parsed",
		slog.String("sse_event", eventName),
		slog.String("item_type", itemType),
		slog.String("item_id", state.ItemID),
		slog.String("call_id", state.CallID),
		slog.String("tool_name", toolName),
	)
}
