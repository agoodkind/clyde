package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"goodkind.io/clyde/internal/adapter/finishreason"
	"goodkind.io/clyde/internal/adapter/provider"
)

// Code Assist can include a complete function call in one SSE data line.
const (
	maxStreamLineBytes     = 8 << 20
	initialStreamLineBytes = 4 << 10
)

var errMissingResponse = errors.New("stream data omitted the Gemini response")

type streamChunk struct {
	Response *streamResponse `json:"response"`
	TraceID  string          `json:"traceId"`
}

type streamResponse struct {
	Candidates   []streamCandidate `json:"candidates"`
	Usage        *streamUsage      `json:"usageMetadata"`
	ModelVersion string            `json:"modelVersion"`
}

type streamCandidate struct {
	Index   int `json:"index"`
	Content struct {
		Parts []streamPart `json:"parts"`
	} `json:"content"`
	FinishReason string `json:"finishReason"`
}

type streamPart struct {
	Text             string        `json:"text"`
	Thought          bool          `json:"thought"`
	FunctionCall     *functionCall `json:"functionCall"`
	ThoughtSignature string        `json:"thoughtSignature"`
}

type streamUsage struct {
	Prompt     int  `json:"promptTokenCount"`
	Candidates int  `json:"candidatesTokenCount"`
	Thoughts   *int `json:"thoughtsTokenCount"`
	Total      int  `json:"totalTokenCount"`
	Cached     int  `json:"cachedContentTokenCount"`
}

// ReadStream translates each Gemini SSE data line into adapter events.
//
// Callers supply a response body that honors ctx while waiting for data.
// It returns accumulated usage and tool metadata with the first read error.
//
//	result, err := ReadStream(ctx, response.Body, writer)
func ReadStream(
	ctx context.Context,
	body io.Reader,
	writer provider.EventWriter,
) (provider.Result, error) {
	var state streamTranslator
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, initialStreamLineBytes), maxStreamLineBytes)

	for {
		if err := ctx.Err(); err != nil {
			return state.result, geminiStreamError(ctx, "read", err)
		}

		if !scanner.Scan() {
			break
		}

		data, ok := bytes.CutPrefix(scanner.Bytes(), []byte("data:"))
		data = bytes.TrimSpace(data)
		if !ok || len(data) == 0 {
			continue
		}

		var chunk streamChunk
		if err := json.Unmarshal(data, &chunk); err != nil {
			return state.result, geminiStreamError(ctx, "decode", err)
		}

		if chunk.Response == nil {
			return state.result, errMissingResponse
		}

		if err := state.translate(ctx, chunk, writer); err != nil {
			return state.result, err
		}
	}

	if err := scanner.Err(); err != nil {
		return state.result, geminiStreamError(ctx, "scan", err)
	}

	if state.result.FinishReason == "" {
		state.result.FinishReason = finishreason.FromGemini("")
	}

	return state.result, nil
}

func geminiStreamError(ctx context.Context, operation string, err error) error {
	slog.WarnContext(ctx, "adapter.antigravity.stream_failed", "operation", operation)

	return fmt.Errorf("%s Gemini stream: %w", operation, err)
}
