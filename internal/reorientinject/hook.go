// Package reorientinject detects Claude compaction summarization requests and
// appends the recovered pre-compaction transcript to the summary response, so
// the client persists it in the isCompactSummary user message.
package reorientinject

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/adapter/content"
	"goodkind.io/clyde/internal/mitm"
)

const (
	// compactPromptSignature is a substring shared by every Claude Code
	// compaction prompt. The full BASE_COMPACT_PROMPT and the RECENT and
	// this-conversation partial variants all open with it.
	//
	// An exported transcript reproduces this prompt verbatim. Pasting one puts
	// this substring in the last user message of an ordinary turn.
	// MatchRequestResponse requires [mitm.RequestPurposeCompaction] first and
	// checks this substring second.
	compactPromptSignature = "Your task is to create a detailed summary of"

	messagesPathSuffix = "/v1/messages"

	eventStreamContentType = "text/event-stream"

	reorientInjectConcern   = "providers.mitm.wire"
	reorientInjectComponent = "mitm"

	anthropicBetaHeader = "Anthropic-Beta"
)

// Reorient injection sizing defaults. A zero field in [Sizing] falls back to the
// matching default here, so a caller may override only the knobs it cares about.
const (
	// DefaultMaxTokens caps the injected transcript before the context-window
	// fraction is applied.
	DefaultMaxTokens = 500_000
	// DefaultStandardContextWindow is the assumed context window, in tokens, for a
	// compaction request without the context-1m beta.
	DefaultStandardContextWindow = 200_000
	// DefaultOneMillionContextWindow is the assumed context window, in tokens, for a
	// compaction request carrying the context-1m beta.
	DefaultOneMillionContextWindow = 1_000_000
	// DefaultContextWindowFraction is the fraction of the request's context window
	// the injection may fill.
	DefaultContextWindowFraction = 0.5
	// DefaultBytesPerToken is a documented token-to-byte approximation used to keep
	// the hot path tokenizer-free.
	DefaultBytesPerToken = 4
	// DefaultRecentFraction is the fraction of conversation messages, by count,
	// reattached verbatim as the recent half in the R2 request-trim split.
	DefaultRecentFraction = 0.5
)

// ContentProvider returns the recovered pre-compaction transcript for a Claude
// session id, rendered off disk by the transcript parser with clyde's reorient
// knobs. It returns an empty string when the session cannot be resolved, which
// makes the transformer pass the response through unchanged.
type ContentProvider func(ctx context.Context, request ContentRequest) (string, error)

// ContentRequest sizes one provider render.
type ContentRequest struct {
	// SessionID is the Claude session id parsed from metadata.user_id.
	SessionID string
	// MaxBytes caps the render to its last N bytes. Zero leaves it uncapped.
	MaxBytes int
	// UncappedLines lifts the provider's configured line cap. The split path sets
	// it because MaxBytes already equals the size of the trimmed recent half, and a
	// line cap would drop part of what the trim removed from the request.
	UncappedLines bool
}

// Sizing configures the reorient injection size caps. Every zero or non-positive
// field falls back to the matching Default constant, so a caller may set only the
// knobs it wants to change and leave the rest at their defaults.
type Sizing struct {
	// MaxTokens caps the injected transcript before the context-window fraction is
	// applied. Zero uses DefaultMaxTokens.
	MaxTokens int
	// ContextWindowFraction is the fraction of the request's context window the
	// injection may fill. Zero uses DefaultContextWindowFraction.
	ContextWindowFraction float64
	// BytesPerToken is the token-to-byte approximation. Zero uses DefaultBytesPerToken.
	BytesPerToken int
	// StandardContextWindow is the assumed window, in tokens, for a request without
	// the context-1m beta. Zero uses DefaultStandardContextWindow.
	StandardContextWindow int
	// OneMillionContextWindow is the assumed window, in tokens, for a context-1m
	// request. Zero uses DefaultOneMillionContextWindow.
	OneMillionContextWindow int
	// RecentFraction is the fraction of conversation messages, by count, reattached
	// verbatim as the recent half in the R2 request-trim split. Zero uses
	// DefaultRecentFraction. The maxBytes ceiling still bounds the reattached half,
	// so a fraction near 1 reattaches as much recent history as fits MaxTokens.
	RecentFraction float64
}

func (s Sizing) normalized() Sizing {
	if s.MaxTokens <= 0 {
		s.MaxTokens = DefaultMaxTokens
	}
	if s.ContextWindowFraction <= 0 {
		s.ContextWindowFraction = DefaultContextWindowFraction
	}
	if s.BytesPerToken <= 0 {
		s.BytesPerToken = DefaultBytesPerToken
	}
	if s.StandardContextWindow <= 0 {
		s.StandardContextWindow = DefaultStandardContextWindow
	}
	if s.OneMillionContextWindow <= 0 {
		s.OneMillionContextWindow = DefaultOneMillionContextWindow
	}
	if s.RecentFraction <= 0 {
		s.RecentFraction = DefaultRecentFraction
	}
	return s
}

// Hook detects Claude compaction summarization requests and drives the summary
// append. It implements [mitm.RequestResponseHook].
type Hook struct {
	provider ContentProvider
	sizing   Sizing
}

// New constructs a reorient summary injection hook. A nil provider yields a hook
// that always passes responses through unchanged. A zero Sizing field falls back
// to its Default constant.
func New(provider ContentProvider, sizing Sizing) *Hook {
	if provider == nil {
		provider = emptyContentProvider
	}
	return &Hook{
		provider: provider,
		sizing:   sizing.normalized(),
	}
}

// MatchRequestResponse matches a request the client declared to be its own
// compaction turn. It pairs the response with a transformer that stores the
// session id read from the request.
func (h *Hook) MatchRequestResponse(
	req mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	if req.Method != http.MethodPost {
		return unmatchedRequestResponseHookMatch(), nil
	}
	if !strings.HasSuffix(req.Path, messagesPathSuffix) {
		return unmatchedRequestResponseHookMatch(), nil
	}
	// Claude Code sets the declaration on the request. A pasted transcript
	// does not set it. This check precedes the body read.
	if req.Purpose != mitm.RequestPurposeCompaction {
		return unmatchedRequestResponseHookMatch(), nil
	}
	body, err := req.Body.Bytes()
	if err != nil {
		slog.Warn(
			"mitm.reorient_inject.request_body_read_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"method", req.Method,
			"path", req.Path,
			"err", err,
		)
		// Fail open: a body the hook cannot read just means no injection. Returning
		// an error would surface through the seam and must never break the request.
		return unmatchedRequestResponseHookMatch(), nil
	}
	var request anthropicSummaryRequest
	if err := json.Unmarshal(body, &request); err != nil {
		slog.Warn(
			"mitm.reorient_inject.request_body_decode_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"method", req.Method,
			"path", req.Path,
			"err", err,
		)
		// Fail open: a body the hook cannot decode just means no injection.
		return unmatchedRequestResponseHookMatch(), nil
	}
	if !requestIsCompactionSummary(request) {
		return unmatchedRequestResponseHookMatch(), nil
	}
	sessionID := request.sessionID()
	if sessionID == "" {
		// Without a session id there is no transcript to correlate, so the hook
		// cannot inject. Report unmatched rather than pairing a transformer that
		// could never produce content (and would log a misleading "matched").
		return unmatchedRequestResponseHookMatch(), nil
	}
	maxBytes := h.maxBytes(req.Header)
	fallback := mitm.RequestResponseHookMatch{
		Matched: true,
		Transformer: responseAppendTransformer{
			provider:  h.provider,
			sessionID: sessionID,
			maxBytes:  maxBytes,
			split:     nil,
		},
		RequestTransformer: nil,
	}
	promptIndex, _ := compactionPromptIndex(request)
	plan, split := planSplit(request.Messages, promptIndex, maxBytes, h.sizing.RecentFraction)
	if !split {
		logSplitFallback(splitFallbackNoSplit, len(request.Messages))
		return fallback, nil
	}
	keep, droppedSystem := dropOlderHalfSystemMessages(
		request.Messages,
		trimKeepIndexes(plan.recentStart, plan.instructionStart, len(request.Messages)),
		plan.recentStart,
	)
	// Hard gate: only trim when the kept messages are Anthropic-valid, so a
	// boundary edge case can never forward a request that 400s /compact.
	if !validateTrim(selectMessages(request.Messages, keep)) {
		logSplitFallback(splitFallbackInvalidTrim, len(request.Messages))
		return fallback, nil
	}
	slog.Info(
		"mitm.reorient_inject.split_planned",
		"component", reorientInjectComponent,
		"concern", reorientInjectConcern,
		"message_count", len(request.Messages),
		"recent_start", plan.recentStart,
		"instruction_start", plan.instructionStart,
		"dropped_system_count", droppedSystem,
	)
	state := &splitState{content: ""}
	return mitm.RequestResponseHookMatch{
		Matched: true,
		Transformer: responseAppendTransformer{
			provider:  h.provider,
			sessionID: sessionID,
			maxBytes:  maxBytes,
			split:     state,
		},
		RequestTransformer: messageTrimTransformer{
			keep:     keep,
			provider: h.provider,
			request: ContentRequest{
				SessionID:     sessionID,
				MaxBytes:      recentBytes(request.Messages[plan.recentStart:plan.instructionStart]),
				UncappedLines: true,
			},
			state: state,
		},
	}, nil
}

// splitFallbackReason names why a compaction request kept the whole
// conversation instead of trimming its recent half.
type splitFallbackReason string

const (
	splitFallbackNoSplit     splitFallbackReason = "no_split"
	splitFallbackInvalidTrim splitFallbackReason = "invalid_trim"
	splitFallbackEmptyRecent splitFallbackReason = "empty_recent"
)

func logSplitFallback(reason splitFallbackReason, messageCount int) {
	slog.Warn(
		"mitm.reorient_inject.split_fallback",
		"component", reorientInjectComponent,
		"concern", reorientInjectConcern,
		"reason", string(reason),
		"message_count", messageCount,
	)
}

// splitState holds the recent-half render the request transformer produced for
// the response transformer of the same exchange. The proxy runs the request
// transform before it reads the response, so no lock is needed. An empty
// content means the request went upstream untrimmed.
type splitState struct {
	content string
}

func (h *Hook) maxBytes(header http.Header) int {
	contextWindow := h.sizing.StandardContextWindow
	for _, beta := range anthropicBetaValues(header) {
		if strings.Contains(strings.ToLower(beta), "context-1m") {
			contextWindow = h.sizing.OneMillionContextWindow
			break
		}
	}
	windowTokens := int(float64(contextWindow) * h.sizing.ContextWindowFraction)
	effectiveTokens := min(h.sizing.MaxTokens, windowTokens)
	return effectiveTokens * h.sizing.BytesPerToken
}

func anthropicBetaValues(header http.Header) []string {
	values := header.Values(anthropicBetaHeader)
	for key, keyValues := range header {
		if key == anthropicBetaHeader {
			continue
		}
		if strings.EqualFold(key, anthropicBetaHeader) {
			values = append(values, keyValues...)
		}
	}
	return values
}

func unmatchedRequestResponseHookMatch() mitm.RequestResponseHookMatch {
	return mitm.RequestResponseHookMatch{
		Matched:            false,
		Transformer:        nil,
		RequestTransformer: nil,
	}
}

// requestIsCompactionSummary reports whether the request's last user message
// carries Claude Code's compaction prompt. The prompt is the last user message
// of the summarization request, but interactive Claude Code appends a trailing
// system-reminder message after it, so the final message is not always the
// prompt. Scanning back to the last user message matches both the headless case
// (the prompt is the final message) and the interactive case (a system-reminder
// follows the prompt). Matching the last user message still keeps a normal turn
// (whose last user message is the user's own input) from matching, because the
// signature is Claude Code's own distinctive control string.
func requestIsCompactionSummary(request anthropicSummaryRequest) bool {
	_, ok := compactionPromptIndex(request)
	return ok
}

// compactionPromptIndex returns the index of the last user message and whether it
// carries Claude Code's compaction prompt. The messages before that index are the
// conversation being summarized; the prompt message and any trailing
// system-reminder after it are the instruction region that must stay in the
// request. Returns ok=false when the last user message is not the compaction
// prompt (a normal turn).
func compactionPromptIndex(request anthropicSummaryRequest) (int, bool) {
	for index, message := range slices.Backward(request.Messages) {
		if message.Role != "user" {
			continue
		}
		if strings.Contains(message.text(), compactPromptSignature) {
			return index, true
		}
		return 0, false
	}
	return 0, false
}

// anthropicSummaryRequest is the minimal decode of the /v1/messages request the
// hook needs: the messages (to find the compaction prompt in the last user message)
// and metadata.user_id (to correlate to the on-disk transcript). The Anthropic
// Messages API keeps content as string-or-array and metadata.user_id as a
// double-encoded JSON string, so both stay opaque here and are narrowed by the
// helpers below.
type anthropicSummaryRequest struct {
	Messages []anthropicMessage `json:"messages"`
	Metadata *anthropicMetadata `json:"metadata"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicMetadata struct {
	UserID string `json:"user_id"`
}

type anthropicUserID struct {
	SessionID string `json:"session_id"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// text returns the concatenated text of a message's content blocks, handling
// both the plain-string form and the array-of-blocks form of the wire contract.
func (m anthropicMessage) text() string {
	trimmed := strings.TrimSpace(string(m.Content))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err != nil {
			return ""
		}
		return plain
	}
	var blocks []anthropicTextBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return ""
	}
	var builder strings.Builder
	for _, block := range blocks {
		if block.Text == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(block.Text)
	}
	return builder.String()
}

// sessionID parses metadata.user_id (a double-encoded JSON string) and returns
// the Claude session id, or empty when the field is absent or unparseable.
func (r anthropicSummaryRequest) sessionID() string {
	if r.Metadata == nil || r.Metadata.UserID == "" {
		return ""
	}
	var uid anthropicUserID
	if err := json.Unmarshal([]byte(r.Metadata.UserID), &uid); err != nil {
		return ""
	}
	return strings.TrimSpace(uid.SessionID)
}

// parts maps this message's content into the neutral content parts the adapter
// boundary defines. The provider package owns the Anthropic block shapes; every
// split and render step below reads only the neutral parts, so a second
// provider supplies its own mapping and reuses the same steps.
func (m anthropicMessage) parts() []content.Part {
	parts, _ := anthropic.NormalizeContent(m.Content)
	return parts
}

// toolIDs returns the tool_use ids this message declares and the tool_use ids
// its tool results answer. Used to pair tool calls across wire messages so the
// split never orphans one side.
func (m anthropicMessage) toolIDs() (uses []string, results []string) {
	return toolIDsOfParts(m.parts())
}

func toolIDsOfParts(parts []content.Part) (uses []string, results []string) {
	for _, part := range parts {
		switch part.Kind {
		case content.PartToolUse:
			if part.ID != "" {
				uses = append(uses, part.ID)
			}
		case content.PartToolResult:
			if part.ToolUseID != "" {
				results = append(results, part.ToolUseID)
			}
		case content.PartText,
			content.PartThinking,
			content.PartImage,
			content.PartAudio,
			content.PartRefusal,
			content.PartUnsupported:
		}
	}
	return uses, results
}

// recentBytes is the wire size of the recent half the trim removes. The provider
// render uses it as its byte cap, so the injection covers about the same span.
func recentBytes(messages []anthropicMessage) int {
	total := 0
	for _, message := range messages {
		total += len(message.Content)
	}
	return total
}

// messageTrimTransformer renders the recent half through the transcript parser
// and, only when that render is non-empty, rewrites the summarization request to
// keep the message indexes in keep (the older half without its system messages,
// plus the instruction region). Rendering before trimming means the request is
// never trimmed unless the removed messages have a replacement to inject.
type messageTrimTransformer struct {
	keep     []int
	provider ContentProvider
	request  ContentRequest
	state    *splitState
}

func (t messageTrimTransformer) TransformRequest(ctx context.Context, body []byte) ([]byte, bool, error) {
	recent, err := t.provider(ctx, t.request)
	if err != nil {
		// The proxy forwards the original body on a transform error, so the
		// request goes upstream untrimmed and the response takes the disk fallback.
		slog.WarnContext(ctx, "mitm.reorient_inject.content_provider_failed",
			"component", reorientInjectComponent, "concern", reorientInjectConcern, "err", err)
		logSplitFallback(splitFallbackEmptyRecent, len(t.keep))
		return body, false, fmt.Errorf("render reorient recent half: %w", err)
	}
	if strings.TrimSpace(recent) == "" {
		logSplitFallback(splitFallbackEmptyRecent, len(t.keep))
		return body, false, nil
	}
	trimmed, err := marshalTrimmedRequest(ctx, body, t.keep)
	if err != nil {
		// The proxy treats an error as fail-open: it forwards the original request
		// body unchanged, so a decode or encode failure never breaks /compact. The
		// state stays empty, so the response takes the disk fallback.
		return body, false, err
	}
	t.state.content = recent
	return trimmed, true, nil
}

// marshalTrimmedRequest returns body with its messages array reduced to the keep
// indexes, in order. It preserves every other top-level field's value verbatim via
// [json.RawMessage], so model, system, tools, metadata, and max_tokens are
// unchanged. Any failure is logged once and returned wrapped; the proxy fail-opens
// on it and forwards the original request unchanged.
func marshalTrimmedRequest(ctx context.Context, body []byte, keep []int) (out []byte, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "mitm.reorient_inject.request_trim_failed",
				"component", reorientInjectComponent, "concern", reorientInjectConcern, "err", err)
		}
	}()
	var top map[string]json.RawMessage
	if err = json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	rawMessages, ok := top["messages"]
	if !ok {
		return nil, fmt.Errorf("request body has no messages field")
	}
	var messages []json.RawMessage
	if err = json.Unmarshal(rawMessages, &messages); err != nil {
		return nil, fmt.Errorf("decode request messages: %w", err)
	}
	kept := make([]json.RawMessage, 0, len(keep))
	for _, index := range keep {
		if index < 0 || index >= len(messages) {
			return nil, fmt.Errorf("keep index %d out of range %d", index, len(messages))
		}
		kept = append(kept, messages[index])
	}
	encodedMessages, marshalErr := json.Marshal(kept)
	if marshalErr != nil {
		return nil, fmt.Errorf("encode trimmed messages: %w", marshalErr)
	}
	top["messages"] = encodedMessages
	out, marshalErr = json.Marshal(top)
	if marshalErr != nil {
		return nil, fmt.Errorf("encode trimmed request: %w", marshalErr)
	}
	return out, nil
}

type responseAppendTransformer struct {
	provider  ContentProvider
	sessionID string
	maxBytes  int
	// split, when non-nil, holds the recent half the request transformer rendered
	// for a trimmed request. An empty split content means the request went
	// upstream untrimmed, so the response takes the capped disk fallback.
	split *splitState
}

func (t responseAppendTransformer) TransformResponse(
	ctx context.Context,
	resp mitm.ResponseHookResponse,
) (mitm.ResponseHookResponse, error) {
	if !responseIsStreamingSuccess(resp) {
		// Never rewrite a non-200 or non-SSE response: an upstream error body must
		// reach the client intact rather than be replaced by injected events.
		return resp, nil
	}
	slog.InfoContext(
		ctx,
		"mitm.reorient_inject.matched",
		"component", reorientInjectComponent,
		"concern", reorientInjectConcern,
	)
	content := ""
	if t.split != nil {
		content = t.split.content
	}
	if content == "" {
		provided, err := t.provider(ctx, ContentRequest{
			SessionID:     t.sessionID,
			MaxBytes:      t.maxBytes,
			UncappedLines: false,
		})
		if err != nil {
			slog.WarnContext(
				ctx,
				"mitm.reorient_inject.content_provider_failed",
				"component", reorientInjectComponent,
				"concern", reorientInjectConcern,
				"err", err,
			)
			return resp, nil
		}
		content = provided
	}
	if content == "" {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// Fail open: a read failure means the upstream stream itself broke, so
		// return the bytes read so far uninjected rather than an error, which the
		// seam would turn into a 502 and break the client's /compact.
		slog.WarnContext(
			ctx,
			"mitm.reorient_inject.response_body_read_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	output, err := anthropic.InjectIntoSummary(body, content)
	if err != nil {
		// Fail open: a rewrite failure must not break /compact. Return the
		// original (fully read) summary response unchanged.
		slog.WarnContext(
			ctx,
			"mitm.reorient_inject.sse_append_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return responseWithBody(resp, body), nil
	}
	return responseWithBody(resp, output), nil
}

// responseIsStreamingSuccess reports whether the response is a 200 Anthropic SSE
// stream, the only shape the summary append is valid for.
func responseIsStreamingSuccess(resp mitm.ResponseHookResponse) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(contentType, eventStreamContentType)
}

func responseWithBody(
	resp mitm.ResponseHookResponse,
	body []byte,
) mitm.ResponseHookResponse {
	header := resp.Header.Clone()
	header.Del("Content-Length")
	return mitm.ResponseHookResponse{
		StatusCode:    resp.StatusCode,
		Status:        resp.Status,
		Proto:         resp.Proto,
		Header:        header,
		Body:          bytes.NewReader(body),
		ContentLength: -1,
	}
}

func emptyContentProvider(context.Context, ContentRequest) (string, error) {
	return "", nil
}
