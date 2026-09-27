package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// CompletionObjectType is the object discriminator of a legacy
// Completions response and stream chunk.
const CompletionObjectType = "text_completion"

// completionIDPrefix replaces the Chat Completions id prefix on a
// converted legacy Completions id.
const completionIDPrefix = "cmpl-"

// ErrCompletionPromptUnsupported reports a prompt shape Clyde cannot run,
// such as token arrays or several prompts in one request.
var ErrCompletionPromptUnsupported = errors.New("prompt must be a string or an array with one string")

// CompletionRequest is the legacy POST /v1/completions request body.
type CompletionRequest struct {
	Model            string          `json:"model"`
	Prompt           json.RawMessage `json:"prompt"`
	Suffix           *string         `json:"suffix,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	N                *int            `json:"n,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	Logprobs         *int            `json:"logprobs,omitempty"`
	Echo             *bool           `json:"echo,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	BestOf           *int            `json:"best_of,omitempty"`
	LogitBias        json.RawMessage `json:"logit_bias,omitempty"`
	User             string          `json:"user,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
}

// knownCompletionRequestKeys mirrors the JSON tags on CompletionRequest.
// TestCompletionRequestJSONTagsMatchKnownKeys pins the two sets together.
var knownCompletionRequestKeys = map[string]bool{
	"model":             true,
	"prompt":            true,
	"suffix":            true,
	"max_tokens":        true,
	"temperature":       true,
	"top_p":             true,
	"n":                 true,
	"stream":            true,
	"stream_options":    true,
	"logprobs":          true,
	"echo":              true,
	"stop":              true,
	"presence_penalty":  true,
	"frequency_penalty": true,
	"best_of":           true,
	"logit_bias":        true,
	"user":              true,
	"seed":              true,
	"reasoning_effort":  true,
}

// UnknownCompletionKeys returns the sorted top-level keys that the typed
// CompletionRequest does not model.
func (s ResponsesFieldSet) UnknownCompletionKeys() []string {
	unknown := make([]string, 0)
	for _, key := range sortedKeys(s.fields) {
		if !knownCompletionRequestKeys[key] {
			unknown = append(unknown, key)
		}
	}
	return unknown
}

// PromptText returns the single prompt string. It accepts a JSON string
// or an array that contains exactly one string.
func (r CompletionRequest) PromptText() (string, error) {
	var single string
	if err := json.Unmarshal(r.Prompt, &single); err == nil {
		return single, nil
	}
	var list []string
	if err := json.Unmarshal(r.Prompt, &list); err == nil && len(list) == 1 {
		return list[0], nil
	}
	return "", ErrCompletionPromptUnsupported
}

// CompletionChoice is one legacy Completions choice. Logprobs is always
// JSON null because Clyde rejects a request that asks for log
// probabilities. FinishReason is null on stream chunks before the end.
type CompletionChoice struct {
	Text         string          `json:"text"`
	Index        int             `json:"index"`
	Logprobs     json.RawMessage `json:"logprobs"`
	FinishReason *string         `json:"finish_reason"`
}

// CompletionResponse is the legacy Completions response object and
// stream chunk.
type CompletionResponse struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []CompletionChoice `json:"choices"`
	Usage             *Usage             `json:"usage,omitempty"`
	SystemFingerprint string             `json:"system_fingerprint,omitempty"`
}

// completionChunkNullUsageWire is the chunk shape with a usage member
// that is never omitted.
type completionChunkNullUsageWire struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []CompletionChoice `json:"choices"`
	Usage             *Usage             `json:"usage"`
	SystemFingerprint string             `json:"system_fingerprint,omitempty"`
}

// completionID converts a Chat Completions id into a Completions id.
func completionID(chatID string) string {
	return completionIDPrefix + strings.TrimPrefix(chatID, "chatcmpl-")
}

// CompletionFromChat converts a nonstreaming Chat Completions response
// into the legacy Completions response. Only literal text content
// becomes completion text.
func CompletionFromChat(chat ChatResponse) CompletionResponse {
	choices := make([]CompletionChoice, 0, len(chat.Choices))
	for _, choice := range chat.Choices {
		finishReason := choice.FinishReason
		choices = append(choices, CompletionChoice{
			Text:         completionText(choice.Message.Content),
			Index:        choice.Index,
			Logprobs:     json.RawMessage(`null`),
			FinishReason: &finishReason,
		})
	}
	return CompletionResponse{
		ID:                completionID(chat.ID),
		Object:            CompletionObjectType,
		Created:           chat.Created,
		Model:             chat.Model,
		Choices:           choices,
		Usage:             chat.Usage,
		SystemFingerprint: chat.SystemFingerprint,
	}
}

// completionText returns the literal text parts of a Chat message
// content value.
func completionText(content json.RawMessage) string {
	parts, kind := NormalizeContent(content)
	if kind == ContentKindEmpty {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		if part.Type == string(openAIChatContentPartText) {
			builder.WriteString(part.Text)
		}
	}
	return builder.String()
}

// completionChunkFromChat converts one Chat Completions stream chunk. It
// drops choices with no text and no finish reason, and it reports false
// when nothing remains to send.
func completionChunkFromChat(chunk StreamChunk) (CompletionResponse, bool) {
	choices := make([]CompletionChoice, 0, len(chunk.Choices))
	for _, choice := range chunk.Choices {
		if choice.Delta.Content == "" && choice.FinishReason == nil {
			continue
		}
		choices = append(choices, CompletionChoice{
			Text:         choice.Delta.Content,
			Index:        choice.Index,
			Logprobs:     json.RawMessage(`null`),
			FinishReason: choice.FinishReason,
		})
	}
	usageChunk := len(chunk.Choices) == 0 && chunk.Usage != nil
	if len(choices) == 0 && !usageChunk {
		return CompletionResponse{ID: "", Object: "", Created: 0, Model: "", Choices: nil, Usage: nil, SystemFingerprint: ""}, false
	}
	return CompletionResponse{
		ID:                completionID(chunk.ID),
		Object:            CompletionObjectType,
		Created:           chunk.Created,
		Model:             chunk.Model,
		Choices:           choices,
		Usage:             chunk.Usage,
		SystemFingerprint: chunk.SystemFingerprint,
	}, true
}

// LegacyCompletionWriter wraps the response writer of a Chat Completions
// handler and rewrites its successful output into the legacy Completions
// shape. A 2xx JSON body becomes a text_completion object. Each SSE chat
// chunk becomes a text_completion chunk. Error bodies, error stream
// frames, and the [DONE] frame keep their bytes.
type LegacyCompletionWriter struct {
	inner         http.ResponseWriter
	flusher       http.Flusher
	includeUsage  bool
	status        int
	statusChosen  bool
	streaming     bool
	passthrough   bool
	pending       bytes.Buffer
	finishedWrite bool
}

// NewLegacyCompletionWriter wraps inner. includeUsage reports whether the
// client set stream_options.include_usage, which requires an explicit
// null usage member on ordinary stream chunks.
func NewLegacyCompletionWriter(inner http.ResponseWriter, includeUsage bool) *LegacyCompletionWriter {
	flusher, _ := inner.(http.Flusher)
	return &LegacyCompletionWriter{
		inner:         inner,
		flusher:       flusher,
		includeUsage:  includeUsage,
		status:        http.StatusOK,
		statusChosen:  false,
		streaming:     false,
		passthrough:   false,
		pending:       bytes.Buffer{},
		finishedWrite: false,
	}
}

// Header returns the wrapped writer's header map.
func (w *LegacyCompletionWriter) Header() http.Header {
	return w.inner.Header()
}

// WriteHeader records the status. A streaming or non-2xx response writes
// the status immediately. A 2xx JSON response defers the status until
// Finish writes the converted body.
func (w *LegacyCompletionWriter) WriteHeader(status int) {
	if w.statusChosen {
		return
	}
	w.statusChosen = true
	w.status = status
	contentType := strings.ToLower(w.inner.Header().Get("Content-Type"))
	w.streaming = strings.Contains(contentType, "text/event-stream")
	w.passthrough = status < http.StatusOK || status >= http.StatusMultipleChoices
	if w.streaming || w.passthrough {
		w.inner.WriteHeader(status)
	}
}

// Write buffers or converts body bytes according to the response kind.
func (w *LegacyCompletionWriter) Write(body []byte) (int, error) {
	if !w.statusChosen {
		w.WriteHeader(http.StatusOK)
	}
	if w.passthrough {
		written, err := w.inner.Write(body)
		if err != nil {
			return written, fmt.Errorf("write legacy completion passthrough: %w", err)
		}
		return written, nil
	}
	w.pending.Write(body)
	if w.streaming {
		if err := w.writeCompleteFrames(); err != nil {
			return 0, err
		}
	}
	return len(body), nil
}

// Flush sends converted stream frames to the network.
func (w *LegacyCompletionWriter) Flush() {
	if w.streaming && w.flusher != nil {
		w.flusher.Flush()
	}
}

// Unwrap returns the wrapped writer.
func (w *LegacyCompletionWriter) Unwrap() http.ResponseWriter {
	return w.inner
}

// Finish writes a deferred JSON body and any partial stream frame. The
// legacy Completions handler runs Finish after the Chat Completions
// handler returns.
func (w *LegacyCompletionWriter) Finish() error {
	if w.finishedWrite || !w.statusChosen || w.passthrough {
		return nil
	}
	w.finishedWrite = true
	if w.streaming {
		if w.pending.Len() == 0 {
			return nil
		}
		if _, err := w.inner.Write(w.pending.Bytes()); err != nil {
			slog.Warn("adapter.openai.legacy_completion_tail_write_failed", "concern", "adapter.chat.render", "err", err)
			return fmt.Errorf("write legacy completion stream tail: %w", err)
		}
		return nil
	}
	var chat ChatResponse
	if err := json.Unmarshal(w.pending.Bytes(), &chat); err != nil {
		slog.Warn("adapter.openai.legacy_completion_decode_failed", "concern", "adapter.chat.render", "err", err)
		return fmt.Errorf("decode chat response for legacy completion: %w", err)
	}
	converted, err := json.Marshal(CompletionFromChat(chat))
	if err != nil {
		slog.Warn("adapter.openai.legacy_completion_encode_failed", "concern", "adapter.chat.render", "err", err)
		return fmt.Errorf("encode legacy completion: %w", err)
	}
	w.inner.Header().Del("Content-Length")
	w.inner.WriteHeader(w.status)
	if _, err := w.inner.Write(converted); err != nil {
		slog.Warn("adapter.openai.legacy_completion_write_failed", "concern", "adapter.chat.render", "err", err)
		return fmt.Errorf("write legacy completion: %w", err)
	}
	return nil
}

// writeCompleteFrames converts and writes every complete SSE frame in the
// pending buffer and keeps a trailing partial frame.
func (w *LegacyCompletionWriter) writeCompleteFrames() error {
	for {
		buffered := w.pending.Bytes()
		end := bytes.Index(buffered, []byte("\n\n"))
		if end < 0 {
			return nil
		}
		frame := string(buffered[:end])
		w.pending.Next(end + 2)
		converted, keep := w.convertFrame(frame)
		if !keep {
			continue
		}
		if _, err := w.inner.Write([]byte(converted + "\n\n")); err != nil {
			slog.Warn("adapter.openai.legacy_completion_frame_write_failed", "concern", "adapter.chat.render", "err", err)
			return fmt.Errorf("write legacy completion frame: %w", err)
		}
	}
}

// convertFrame rewrites one SSE frame. It reports false for a chat chunk
// that has nothing to send in the legacy shape.
func (w *LegacyCompletionWriter) convertFrame(frame string) (string, bool) {
	payload, isData := strings.CutPrefix(frame, "data: ")
	if !isData || payload == "[DONE]" {
		return frame, true
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &probe); err == nil && len(probe.Error) > 0 {
		return frame, true
	}
	var chunk StreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return frame, true
	}
	converted, keep := completionChunkFromChat(chunk)
	if !keep {
		return "", false
	}
	encoded, err := w.marshalChunk(converted)
	if err != nil {
		return frame, true
	}
	return "data: " + string(encoded), true
}

func (w *LegacyCompletionWriter) marshalChunk(chunk CompletionResponse) ([]byte, error) {
	if w.includeUsage {
		encoded, err := json.Marshal(completionChunkNullUsageWire(chunk))
		if err != nil {
			slog.Warn("adapter.openai.legacy_completion_chunk_encode_failed", "concern", "adapter.chat.render", "err", err)
			return nil, fmt.Errorf("encode legacy completion chunk: %w", err)
		}
		return encoded, nil
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		slog.Warn("adapter.openai.legacy_completion_chunk_encode_failed", "concern", "adapter.chat.render", "err", err)
		return nil, fmt.Errorf("encode legacy completion chunk: %w", err)
	}
	return encoded, nil
}
