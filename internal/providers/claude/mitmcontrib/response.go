package mitmcontrib

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"goodkind.io/clyde/internal/adapter/anthropic"
	"goodkind.io/clyde/internal/mitm"
)

const messagesPath = "/v1/messages"

func (routeProvider) MatchesResponse(request mitm.RequestResponseHookRequest) bool {
	return request.Method == http.MethodPost && request.Path == messagesPath
}

func (routeProvider) TransformResponse(
	ctx context.Context,
	request mitm.RequestResponseHookRequest,
	response mitm.ResponseHookResponse,
	action mitm.ResponseAction,
) (mitm.ResponseHookResponse, error) {
	if !isAnthropicEventStream(response) {
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		slog.WarnContext(ctx, "mitm.claude.response_adapter_failed", "concern", "providers.mitm.wire", "operation", "read_failed", "err", err)
		return mitm.ReplaceResponseBody(response, body), fmt.Errorf("read Anthropic response: %w", err)
	}
	response = mitm.ReplaceResponseBody(response, body)
	if !bytes.Contains(body, []byte("event: message_stop")) {
		return response, nil
	}
	text := anthropic.ResponseText(body)
	if strings.TrimSpace(text) == "" {
		return response, nil
	}
	result, err := action.EvaluateResponse(ctx, mitm.ResponseActionInput{
		Text:      text,
		SessionID: extractIdentity(request.Header).SessionID,
	})
	if err != nil {
		slog.WarnContext(ctx, "mitm.claude.response_adapter_failed", "concern", "providers.mitm.wire", "operation", "action_failed", "err", err)
		return response, fmt.Errorf("evaluate Anthropic response: %w", err)
	}
	feedback := strings.TrimSpace(result.Feedback)
	if feedback == "" {
		return response, nil
	}
	message := "Agent Gate rejected the previous response. Rewrite the response before continuing.\n\n" + feedback
	augmented, err := anthropic.AppendTextBlock(body, message)
	if err != nil {
		slog.WarnContext(ctx, "mitm.claude.response_adapter_failed", "concern", "providers.mitm.wire", "operation", "append_failed", "err", err)
		return response, fmt.Errorf("append Anthropic response feedback: %w", err)
	}
	return mitm.ReplaceResponseBody(response, augmented), nil
}

func isAnthropicEventStream(response mitm.ResponseHookResponse) bool {
	if response.StatusCode != http.StatusOK {
		return false
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}
