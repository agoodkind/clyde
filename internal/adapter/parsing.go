package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/slogger"
	"goodkind.io/gklog/correlation"
)

// logChatParseFailed records a body that did not parse as JSON.
func (s *Server) logChatParseFailed(
	ctx context.Context,
	corr correlation.Context,
	reqID string,
	bodyBytes int,
	parseErr error,
) {
	slogger.WithConcern(s.log, slogger.ConcernAdapterHTTPErrors).LogAttrs(
		ctx,
		slog.LevelWarn,
		"adapter.chat.parse_failed",
		append(
			[]slog.Attr{slog.String("concern", "adapter.http.errors")},
			correlation.AppendAttrs(
				[]slog.Attr{
					slog.String("request_id", reqID),
					slog.String("err", parseErr.Error()),
					slog.Int("body_bytes", bodyBytes),
				},
				corr,
			)...,
		)...,
	)
}

// normalizeRequestMessages folds Responses-style input into req.Messages.
// Returns a non-nil adapter error when the caller should abort.
func normalizeRequestMessages(req *ChatRequest) error {
	if len(req.Messages) != 0 || len(req.Input) == 0 {
		return nil
	}

	if _, err := parseMessagesFromInput(req); err != nil {
		return adapterErrInvalidRequest(err.Error(), err)
	}

	return nil
}

// logMessagesRequired records a request that arrived with no messages.
func (s *Server) logMessagesRequired(
	ctx context.Context,
	corr correlation.Context,
	reqID string,
	req ChatRequest,
) {
	slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(
		ctx,
		slog.LevelWarn,
		"adapter.chat.validation_failed",
		append(
			[]slog.Attr{slog.String("concern", "adapter.chat.preflight")},
			correlation.AppendAttrs(
				[]slog.Attr{
					slog.String("request_id", reqID),
					slog.String("model", req.Model),
					slog.String("reason", "messages_required"),
				},
				corr,
			)...,
		)...,
	)
}

// chatToolNames flattens tool and legacy function names from a chat request.
func chatToolNames(req ChatRequest) []string {
	toolNames := make([]string, 0, len(req.Tools)+len(req.Functions))
	for _, t := range req.Tools {
		toolNames = append(toolNames, t.Function.Name)
	}

	for _, f := range req.Functions {
		toolNames = append(toolNames, f.Name)
	}

	return toolNames
}

func parseMessagesFromInput(req *ChatRequest) (int, error) {
	var inputItems []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(req.Input, &inputItems); err != nil {
		slog.Warn(
			"adapter.server_dispatch.input_payload_invalid",
			"concern",
			"adapter.chat.dispatch",
			"err",
			err,
		)

		return 0, fmt.Errorf("invalid input payload: %w", err)
	}

	if len(inputItems) == 0 {
		return 0, nil
	}

	messages := make([]ChatMessage, 0, len(inputItems))
	for _, item := range inputItems {
		role := strings.TrimSpace(item.Role)
		if role == "" {
			continue
		}

		content, err := parseInputContent(item.Content)
		if err != nil {
			return 0, err
		}

		messages = append(
			messages,
			ChatMessage{
				Role:             role,
				Content:          content,
				Name:             "",
				ToolCalls:        nil,
				ToolCallID:       "",
				Reasoning:        "",
				ReasoningContent: "",
				Refusal:          "",
				Annotations:      nil,
			},
		)
	}

	if len(messages) == 0 {
		return 0, nil
	}

	req.Messages = messages

	return len(messages), nil
}
