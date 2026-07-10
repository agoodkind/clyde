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

	"goodkind.io/clyde/internal/mitm"
	"goodkind.io/clyde/internal/reorienttag"
)

const (
	// compactPromptSignature is a stable, distinctive substring shared by every
	// Claude Code compaction prompt (the full BASE_COMPACT_PROMPT and the RECENT /
	// this-conversation partial variants all open with it). The compaction
	// summarization request is structurally identical to a normal turn on the wire
	// (it carries the full tool schema and the same top-level shape), so this
	// first-party control string in the request's last user message is the only
	// reliable discriminator. The imperative "Your task is to ..." framing keeps a
	// user who merely asks for a summary from matching. If Claude Code changes the
	// prompt this stops matching, so detection fails safe to no injection.
	compactPromptSignature = "Your task is to create a detailed summary of"

	messagesPathSuffix = "/v1/messages"

	eventStreamContentType = "text/event-stream"

	reorientInjectConcern           = "providers.mitm.wire"
	reorientInjectComponent         = "mitm"
	defaultMissingContentBlockIndex = -1

	anthropicBetaHeader = "Anthropic-Beta"

	defaultReorientInjectMaxTokens  = 500_000
	reorientStandardContextWindow   = 200_000
	reorientOneMillionContextWindow = 1_000_000
	reorientContextWindowFraction   = 0.5
	// reorientBytesPerToken is a documented approximation used to keep the hot
	// path tokenizer-free.
	reorientBytesPerToken = 4
)

// ContentProvider returns the recovered pre-compaction transcript for a Claude
// session id, rendered off disk with clyde's reorient knobs. It returns an empty
// string when the session cannot be resolved, which makes the transformer pass
// the response through unchanged.
type ContentProvider func(ctx context.Context, sessionID string, maxBytes int) (string, error)

// Hook detects Claude compaction summarization requests and drives the summary
// append. It implements [mitm.RequestResponseHook].
type Hook struct {
	provider  ContentProvider
	maxTokens int
}

// New constructs a reorient summary injection hook. A nil provider yields a hook
// that always passes responses through unchanged.
func New(provider ContentProvider, maxTokens int) *Hook {
	if provider == nil {
		provider = emptyContentProvider
	}
	return &Hook{
		provider:  provider,
		maxTokens: normalizeMaxTokens(maxTokens),
	}
}

// MatchRequestResponse matches the compaction summarization request by the
// compact-prompt signature in its final user message, and pairs the response
// with a transformer carrying the session id parsed from metadata.user_id.
func (h *Hook) MatchRequestResponse(
	req mitm.RequestResponseHookRequest,
) (mitm.RequestResponseHookMatch, error) {
	if req.Method != http.MethodPost {
		return unmatchedRequestResponseHookMatch(), nil
	}
	if !strings.HasSuffix(req.Path, messagesPathSuffix) {
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
	return mitm.RequestResponseHookMatch{
		Matched: true,
		Transformer: responseAppendTransformer{
			provider:  h.provider,
			sessionID: sessionID,
			maxBytes:  h.maxBytes(req.Header),
		},
	}, nil
}

func normalizeMaxTokens(maxTokens int) int {
	if maxTokens <= 0 {
		return defaultReorientInjectMaxTokens
	}
	return maxTokens
}

func (h *Hook) maxBytes(header http.Header) int {
	contextWindow := reorientStandardContextWindow
	for _, beta := range anthropicBetaValues(header) {
		if strings.Contains(strings.ToLower(beta), "context-1m") {
			contextWindow = reorientOneMillionContextWindow
			break
		}
	}
	windowTokens := int(float64(contextWindow) * reorientContextWindowFraction)
	effectiveTokens := min(h.maxTokens, windowTokens)
	return effectiveTokens * reorientBytesPerToken
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
		Matched:     false,
		Transformer: nil,
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
	for _, message := range slices.Backward(request.Messages) {
		if message.Role != "user" {
			continue
		}
		return strings.Contains(message.text(), compactPromptSignature)
	}
	return false
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

type responseAppendTransformer struct {
	provider  ContentProvider
	sessionID string
	maxBytes  int
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
	content, err := t.provider(ctx, t.sessionID, t.maxBytes)
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
	output, err := appendSSEContent(body, content)
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

func appendSSEContent(body []byte, content string) ([]byte, error) {
	events := parseSSEEvents(string(body))
	// The client's formatCompactSummary keeps the text between <summary> and
	// </summary> and drops a separately-appended trailing content block, so the
	// transcript must go INSIDE the summary span (before </summary>) to survive
	// into the persisted isCompactSummary message.
	injected, ok, err := injectIntoSummaryBlock(events, content)
	if err != nil {
		return nil, err
	}
	if ok {
		return buildSSEBody(injected), nil
	}
	// Fallback: no </summary> in any text block (a malformed or empty summary).
	// With no summary span the client keeps the whole assistant text, so a
	// trailing appended block survives.
	blockIndex := maxSeenContentBlockIndex(events) + 1
	appendEvents, err := marshalAppendEvents(blockIndex, content)
	if err != nil {
		return nil, err
	}
	events = insertAppendEvents(events, appendEvents)
	return buildSSEBody(events), nil
}

const summaryCloseTag = "</summary>"

// injectIntoSummaryBlock inserts the wrapped transcript just before the first
// </summary> in the assistant's summary text block, so it lands inside the span
// the client extracts. It rebuilds that block's streamed text deltas into a
// single delta carrying the modified text and leaves every other event intact.
// The second return is false when no text block contains </summary>, so the
// caller falls back to appending a trailing block.
func injectIntoSummaryBlock(events []sseEvent, content string) ([]sseEvent, bool, error) {
	textByIndex := map[int]string{}
	order := make([]int, 0)
	for _, event := range events {
		index, text, ok := textDeltaOf(event)
		if !ok {
			continue
		}
		if _, seen := textByIndex[index]; !seen {
			order = append(order, index)
		}
		textByIndex[index] += text
	}
	target := -1
	for _, index := range order {
		if strings.Contains(textByIndex[index], summaryCloseTag) {
			target = index
			break
		}
	}
	if target == -1 {
		return nil, false, nil
	}
	original := textByIndex[target]
	cut := strings.Index(original, summaryCloseTag)
	injection := sanitizeForSummarySpan(wrappedTranscriptContent(content))
	modified := original[:cut] + injection + original[cut:]

	output := make([]sseEvent, 0, len(events))
	replaced := false
	for _, event := range events {
		index, _, ok := textDeltaOf(event)
		if ok && index == target {
			if replaced {
				continue
			}
			data, err := marshalSSEData(newContentBlockDeltaPayload(target, modified))
			if err != nil {
				return nil, false, err
			}
			output = append(output, sseEvent{Name: "content_block_delta", Data: data})
			replaced = true
			continue
		}
		output = append(output, event)
	}
	return output, true, nil
}

// textDeltaOf returns the block index and text of a text_delta content block
// event. The final bool is false for any other event.
func textDeltaOf(event sseEvent) (int, string, bool) {
	if event.Name != "content_block_delta" {
		return 0, "", false
	}
	var payload deltaTextPayload
	if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
		return 0, "", false
	}
	if payload.Index == nil || payload.Delta.Type != "text_delta" {
		return 0, "", false
	}
	return *payload.Index, payload.Delta.Text, true
}

// sanitizeForSummarySpan neutralizes any literal </summary> inside the injected
// transcript so a conversation that discussed the tag cannot prematurely close
// the summary span the client matches.
func sanitizeForSummarySpan(s string) string {
	return strings.ReplaceAll(s, summaryCloseTag, `<\/summary>`)
}

func parseSSEEvents(body string) []sseEvent {
	records := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	events := make([]sseEvent, 0, len(records))
	for _, record := range records {
		event := parseSSERecord(record)
		if event.Name == "" && event.Data == "" {
			continue
		}
		events = append(events, event)
	}
	return events
}

func parseSSERecord(record string) sseEvent {
	lines := strings.Split(record, "\n")
	dataLines := make([]string, 0, len(lines))
	event := sseEvent{Name: "", Data: ""}
	for _, line := range lines {
		if value, ok := strings.CutPrefix(line, "event:"); ok {
			event.Name = strings.TrimSpace(value)
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		}
	}
	event.Data = strings.Join(dataLines, "\n")
	return event
}

func maxSeenContentBlockIndex(events []sseEvent) int {
	maxIndex := defaultMissingContentBlockIndex
	for _, event := range events {
		var payload indexedSSEPayload
		if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
			continue
		}
		if payload.Index == nil {
			continue
		}
		if *payload.Index > maxIndex {
			maxIndex = *payload.Index
		}
	}
	return maxIndex
}

func marshalAppendEvents(blockIndex int, content string) ([]sseEvent, error) {
	wrappedContent := wrappedTranscriptContent(content)
	start, err := marshalSSEData(newContentBlockStartPayload(blockIndex))
	if err != nil {
		return nil, err
	}
	delta, err := marshalSSEData(newContentBlockDeltaPayload(blockIndex, wrappedContent))
	if err != nil {
		return nil, err
	}
	stop, err := marshalSSEData(newContentBlockStopPayload(blockIndex))
	if err != nil {
		return nil, err
	}
	return []sseEvent{
		{Name: "content_block_start", Data: start},
		{Name: "content_block_delta", Data: delta},
		{Name: "content_block_stop", Data: stop},
	}, nil
}

func insertAppendEvents(events []sseEvent, appendEvents []sseEvent) []sseEvent {
	insertAt := len(events)
	for index, event := range events {
		if event.Name == "message_delta" || event.Name == "message_stop" {
			insertAt = index
			break
		}
	}
	output := make([]sseEvent, 0, len(events)+len(appendEvents))
	output = append(output, events[:insertAt]...)
	output = append(output, appendEvents...)
	output = append(output, events[insertAt:]...)
	return output
}

func buildSSEBody(events []sseEvent) []byte {
	var builder strings.Builder
	for _, event := range events {
		if event.Name != "" {
			builder.WriteString("event: ")
			builder.WriteString(event.Name)
			builder.WriteByte('\n')
		}
		writeSSEDataLines(&builder, event.Data)
		builder.WriteByte('\n')
	}
	return []byte(builder.String())
}

func writeSSEDataLines(builder *strings.Builder, data string) {
	if data == "" {
		return
	}
	for line := range strings.SplitSeq(data, "\n") {
		builder.WriteString("data: ")
		builder.WriteString(line)
		builder.WriteByte('\n')
	}
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

func marshalSSEData[T ssePayload](payload T) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		slog.Warn(
			"mitm.reorient_inject.sse_json_encode_failed",
			"component", reorientInjectComponent,
			"concern", reorientInjectConcern,
			"err", err,
		)
		return "", fmt.Errorf("encode reorient inject SSE JSON: %w", err)
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

func wrappedTranscriptContent(content string) string {
	var builder strings.Builder
	builder.WriteString("\n\n")
	builder.WriteString(reorienttag.PreCompactionTranscriptOpen)
	builder.WriteByte('\n')
	builder.WriteString(content)
	builder.WriteByte('\n')
	builder.WriteString(reorienttag.PreCompactionTranscriptClose)
	builder.WriteByte('\n')
	return builder.String()
}

func emptyContentProvider(context.Context, string, int) (string, error) {
	return "", nil
}

type sseEvent struct {
	Name string
	Data string
}

type indexedSSEPayload struct {
	Index *int `json:"index"`
}

// deltaTextPayload decodes a content_block_delta event enough to read its block
// index and streamed text, so the summary-span injection can reassemble a text
// block's content.
type deltaTextPayload struct {
	Index *int `json:"index"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type ssePayload interface {
	contentBlockStartPayload | contentBlockDeltaPayload | contentBlockStopPayload
}

type textContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type contentBlockStartPayload struct {
	Type         string           `json:"type"`
	Index        int              `json:"index"`
	ContentBlock textContentBlock `json:"content_block"`
}

type contentBlockDeltaPayload struct {
	Type  string    `json:"type"`
	Index int       `json:"index"`
	Delta textDelta `json:"delta"`
}

type contentBlockStopPayload struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

func newContentBlockStartPayload(blockIndex int) contentBlockStartPayload {
	return contentBlockStartPayload{
		Type:  "content_block_start",
		Index: blockIndex,
		ContentBlock: textContentBlock{
			Type: "text",
			Text: "",
		},
	}
}

func newContentBlockDeltaPayload(
	blockIndex int,
	content string,
) contentBlockDeltaPayload {
	return contentBlockDeltaPayload{
		Type:  "content_block_delta",
		Index: blockIndex,
		Delta: textDelta{
			Type: "text_delta",
			Text: content,
		},
	}
}

func newContentBlockStopPayload(blockIndex int) contentBlockStopPayload {
	return contentBlockStopPayload{
		Type:  "content_block_stop",
		Index: blockIndex,
	}
}
