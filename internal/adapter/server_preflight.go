package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"goodkind.io/clyde/internal/slogger"
)

func toolChoiceRequestsTools(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str != "" && str != "none"
	}
	return true
}

func newPreflightError(class adapterErrorClass, message, code string) *adapterError {
	err := newAdapterError(class, message)
	err.HTTPStatus = http.StatusBadRequest
	err.OpenAIType = "invalid_request_error"
	err.OpenAICode = code
	err.OpenAIParam = ""
	return err
}

func errAudioUnsupported() *adapterError {
	return newPreflightError(
		adapterErrorUnsupportedContent,
		"audio content parts are not supported by this adapter",
		"audio_unsupported",
	)
}

func errVisionAnthropicUnsupported(modelAlias string) *adapterError {
	return newPreflightError(
		adapterErrorUnsupportedContent,
		fmt.Sprintf("model %q does not support vision input", modelAlias),
		"unsupported_content",
	)
}

func errToolNameEmpty(kind string, index int) *adapterError {
	var msg string
	if kind == "tools" {
		msg = fmt.Sprintf("tools[%d].function.name is required and must be non-empty", index)
	} else {
		msg = fmt.Sprintf("functions[%d].name is required and must be non-empty", index)
	}
	return newPreflightError(
		adapterErrorInvalidRequest,
		msg,
		"invalid_tool_name",
	)
}

func errToolsNotEnabledForAlias() *adapterError {
	return newPreflightError(
		adapterErrorUnsupportedContent,
		"tools are not enabled for this model alias",
		"unsupported_content",
	)
}

func errLogprobsUnsupported() *adapterError {
	return newPreflightError(
		adapterErrorInvalidRequest,
		"logprobs are not supported for this backend",
		"unsupported_param",
	)
}

func (s *Server) validateAudio(ctx context.Context, req *ChatRequest, reqID string) *adapterError {
	for msgIdx := range req.Messages {
		parts, _ := NormalizeContent(req.Messages[msgIdx].Content)
		for _, p := range parts {
			if p.Type != "input_audio" {
				continue
			}
			slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.preflight.audio_rejected",
				slog.String("request_id", reqID),
				slog.String("model", req.Model),
				slog.Int("message_index", msgIdx),
			)
			return errAudioUnsupported()
		}
	}
	return nil
}

func requestHasImageContent(req *ChatRequest) bool {
	for _, msg := range req.Messages {
		parts, _ := NormalizeContent(msg.Content)
		for _, p := range parts {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

func (s *Server) validateVision(ctx context.Context, req *ChatRequest, model ResolvedModel, reqID string) *adapterError {
	if model.Backend == BackendPassthroughOverride {
		return nil
	}
	if !requestHasImageContent(req) {
		return nil
	}
	if model.Backend == BackendAnthropic && !model.SupportsVision {
		slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.preflight.vision_rejected",
			slog.String("request_id", reqID),
			slog.String("model", req.Model),
		)
		return errVisionAnthropicUnsupported(req.Model)
	}
	return nil
}

func (s *Server) validateTools(ctx context.Context, req *ChatRequest, reqID string) *adapterError {
	for tIdx, t := range req.Tools {
		if t.Function.Name == "" {
			slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.preflight.tools_invalid_name",
				slog.String("request_id", reqID),
				slog.String("model", req.Model),
				slog.Int("tool_index", tIdx),
				slog.String("reason", "empty function.name"),
			)
			return errToolNameEmpty("tools", tIdx)
		}
	}
	for fIdx, f := range req.Functions {
		if f.Name == "" {
			slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.preflight.tools_invalid_name",
				slog.String("request_id", reqID),
				slog.String("model", req.Model),
				slog.Int("function_index", fIdx),
				slog.String("reason", "empty functions[].name"),
			)
			return errToolNameEmpty("functions", fIdx)
		}
	}
	return nil
}

func (s *Server) validateToolChoice(ctx context.Context, req *ChatRequest, model ResolvedModel, reqID string) *adapterError {
	if model.Backend != BackendAnthropic {
		return nil
	}
	wantsTools := len(req.Tools) > 0 || len(req.Functions) > 0 || toolChoiceRequestsTools(req.ToolChoice)
	if wantsTools && !model.SupportsTools {
		slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.preflight.tools_rejected",
			slog.String("request_id", reqID),
			slog.String("model", req.Model),
		)
		return errToolsNotEnabledForAlias()
	}
	return nil
}

func (s *Server) validateLogprobs(ctx context.Context, req *ChatRequest, model ResolvedModel, reqID string) *adapterError {
	wantsLogprobs := (req.Logprobs != nil && *req.Logprobs) || req.TopLogprobs != nil
	if !wantsLogprobs {
		return nil
	}
	switch model.Backend {
	case BackendAnthropic:
		switch s.logprobs.Anthropic {
		case "reject":
			slogger.WithConcern(s.log, slogger.ConcernAdapterChatPreflight).LogAttrs(ctx, slog.LevelWarn, "adapter.preflight.logprobs_rejected",
				slog.String("request_id", reqID),
				slog.String("model", req.Model),
				slog.String("backend", "anthropic"),
			)
			return errLogprobsUnsupported()
		case "drop":
			req.Logprobs = nil
			req.TopLogprobs = nil
		}
	}
	return nil
}

// preflightChat enforces adapter capability gates after alias resolution.
// It may mutate req when logprobs policy is "drop".
func (s *Server) preflightChat(ctx context.Context, req *ChatRequest, model ResolvedModel, reqID string) *adapterError {
	if err := s.validateAudio(ctx, req, reqID); err != nil {
		return err
	}
	if err := s.validateVision(ctx, req, model, reqID); err != nil {
		return err
	}
	if err := s.validateTools(ctx, req, reqID); err != nil {
		return err
	}
	if err := s.validateToolChoice(ctx, req, model, reqID); err != nil {
		return err
	}
	return s.validateLogprobs(ctx, req, model, reqID)
}
