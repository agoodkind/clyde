package mitmcontrib

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/mitm"
)

const (
	codexResponsesPath  = "/backend-api/codex/responses"
	openAIResponsesPath = "/v1/responses"
)

func (routeProvider) MatchesResponse(request mitm.RequestResponseHookRequest) bool {
	if request.Method != http.MethodPost {
		return false
	}
	return request.Path == codexResponsesPath || request.Path == openAIResponsesPath
}

func (routeProvider) TransformResponse(
	ctx context.Context,
	request mitm.RequestResponseHookRequest,
	response mitm.ResponseHookResponse,
	action mitm.ResponseAction,
) (mitm.ResponseHookResponse, error) {
	if !isResponsesEventStream(response) {
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		slog.WarnContext(ctx, "mitm.codex.response_adapter_failed", "concern", "providers.mitm.wire", "operation", "read_failed", "err", err)
		return mitm.ReplaceResponseBody(response, body), fmt.Errorf("read Responses stream: %w", err)
	}
	response = mitm.ReplaceResponseBody(response, body)
	if !bytes.Contains(body, []byte(adapteropenai.ResponsesEventCompleted)) {
		return response, nil
	}
	text := adapteropenai.ResponseText(body)
	if strings.TrimSpace(text) == "" {
		return response, nil
	}
	result, err := action.EvaluateResponse(ctx, mitm.ResponseActionInput{
		Text:      text,
		SessionID: extractIdentity(request.Header).SessionID,
	})
	if err != nil {
		slog.WarnContext(ctx, "mitm.codex.response_adapter_failed", "concern", "providers.mitm.wire", "operation", "action_failed", "err", err)
		return response, fmt.Errorf("evaluate Responses output: %w", err)
	}
	feedback := strings.TrimSpace(result.Feedback)
	if feedback == "" {
		return response, nil
	}
	message := "Agent Gate rejected the previous response. Rewrite the response before continuing.\n\n" + feedback
	augmented, err := adapteropenai.AppendTextItem(body, message)
	if err != nil {
		slog.WarnContext(ctx, "mitm.codex.response_adapter_failed", "concern", "providers.mitm.wire", "operation", "append_failed", "err", err)
		return response, fmt.Errorf("append Responses feedback: %w", err)
	}
	return mitm.ReplaceResponseBody(response, augmented), nil
}

func isResponsesEventStream(response mitm.ResponseHookResponse) bool {
	if response.StatusCode != http.StatusOK {
		return false
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}
