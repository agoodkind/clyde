package adapter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	adaptercompat "goodkind.io/clyde/internal/adapter/compat"
	"goodkind.io/clyde/internal/adapter/content"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func (s *Server) handleLegacy(ctx context.Context, hctx *handlerCtx) error {
	request := hctx.Request
	if request.Method != http.MethodPost {
		return newAdapterError(adapterErrorMethodNotAllowed, "POST required")
	}

	if listenerFollowsDocumentedContract(ctx) {
		return s.handleDocumentedLegacy(ctx, hctx)
	}

	var legacy struct {
		Model           string `json:"model"`
		Prompt          string `json:"prompt"`
		Stream          bool   `json:"stream,omitempty"`
		ReasoningEffort string `json:"reasoning_effort,omitempty"`
	}
	if err := json.NewDecoder(request.Body).Decode(&legacy); err != nil {
		return adapterErrInvalidJSON(err.Error(), err)
	}

	synthetic := ChatRequest{
		Model:           legacy.Model,
		Stream:          legacy.Stream,
		ReasoningEffort: legacy.ReasoningEffort,
		Messages: []ChatMessage{
			{
				Role:             string(content.RoleUser),
				Content:          json.RawMessage(strconv.Quote(legacy.Prompt)),
				Name:             "",
				ToolCalls:        nil,
				ToolCallID:       "",
				Reasoning:        "",
				ReasoningContent: "",
				Refusal:          "",
				Annotations:      nil,
				ReplayParts:      nil,
			},
		},
		Input:            nil,
		StreamOptions:    nil,
		Reasoning:        nil,
		Tools:            nil,
		ToolChoice:       nil,
		Functions:        nil,
		FunctionCall:     nil,
		N:                0,
		User:             "",
		Temperature:      nil,
		TopP:             nil,
		MaxTokens:        nil,
		MaxComplTokens:   nil,
		MaxOutputTokens:  nil,
		PresencePenalty:  nil,
		FrequencyPenalty: nil,
		LogitBias:

		// forceStreamUsageOptIn applies the generic OpenAI route family policy that
		// every streaming completion emits the trailing usage chunk regardless of what
		// the client requested via `stream_options.include_usage`.
		nil,
		Logprobs:             nil,
		TopLogprobs:          nil,
		Stop:                 nil,
		Seed:                 nil,
		ResponseFormat:       nil,
		Audio:                nil,
		Modalities:           nil,
		ParallelTools:        nil,
		Store:                nil,
		Metadata:             nil,
		Include:              nil,
		ServiceTier:          "",
		Text:                 nil,
		Truncation:           "",
		PromptCacheRetention: "",
	}
	body, err := json.Marshal(synthetic)
	if err != nil {
		return adapterErrInternal("serialize legacy completion request", err)
	}

	request.Body = io.NopCloser(strings.NewReader(string(body)))
	request.ContentLength = int64(len(body))
	request.Header.Set("Content-Type", "application/json")
	hctx.Request = request

	return s.handleChat(ctx, hctx)
}

// handleDocumentedLegacy runs a legacy prompt as a one-message Chat
// Completions request. LegacyCompletionWriter rewrites the Chat output into
// text_completion objects and chunks.
func (s *Server) handleDocumentedLegacy(ctx context.Context, hctx *handlerCtx) error {
	request := hctx.Request
	limitedBody := http.MaxBytesReader(hctx.Writer, request.Body, maxLegacyRequestBodyBytes)
	body, err := io.ReadAll(limitedBody)
	if err != nil {
		return adapterErrInvalidRequest("failed to read body", err)
	}

	fields, err := adapteropenai.DecodeFieldSet(body)
	if err != nil {
		return adapterErrInvalidJSON("invalid JSON: "+err.Error(), err)
	}

	var legacy adapteropenai.CompletionRequest
	if err := json.Unmarshal(body, &legacy); err != nil {
		return adapterErrInvalidJSON("invalid JSON: "+err.Error(), err)
	}

	prompt, rejectErr := legacyCompletionPrompt(legacy, fields)
	if rejectErr != nil {
		return rejectErr
	}

	chatBody, err := json.Marshal(legacyChatRequest(legacy, prompt))
	if err != nil {
		return adapterErrInternal("serialize legacy completion request", err)
	}

	request.Body = io.NopCloser(strings.NewReader(string(chatBody)))
	request.ContentLength = int64(len(chatBody))
	request.Header.Set("Content-Type", "application/json")
	hctx.Request = request
	includeUsage := legacy.StreamOptions != nil && legacy.StreamOptions.IncludeUsage
	writer := adapteropenai.NewLegacyCompletionWriter(hctx.Writer, includeUsage)
	hctx.Writer = writer
	if chatErr := s.handleChat(ctx, hctx); chatErr != nil {
		return chatErr
	}

	if finishErr := writer.Finish(); finishErr != nil {
		return adapterErrInternal("write legacy completion", finishErr)
	}

	return nil
}

func legacyCompletionPrompt(
	legacy adapteropenai.CompletionRequest,
	fields adapteropenai.ResponsesFieldSet,
) (string, *adapterError) {
	if unknown := fields.UnknownCompletionKeys(); len(unknown) > 0 {
		return "", adapterErrRejectedParameter(adaptercompat.Rejection{
			Code:    adaptercompat.RejectionCodeUnknownParameter,
			Param:   unknown[0],
			Message: "Unrecognized request argument supplied: " + unknown[0],
		})
	}

	unsupported := []struct {
		param   string
		present bool
	}{
		{param: "suffix", present: legacy.Suffix != nil && *legacy.Suffix != ""},
		{param: "echo", present: legacy.Echo != nil && *legacy.Echo},
		{param: "logprobs", present: legacy.Logprobs != nil},
		{param: "best_of", present: legacy.BestOf != nil && *legacy.BestOf > 1},
		{param: "n", present: legacy.N != nil && *legacy.N > 1},
	}
	for _, field := range unsupported {
		if field.present {
			return "", adapterErrRejectedParameter(adaptercompat.Rejection{
				Code:    adaptercompat.RejectionCodeUnsupportedParameter,
				Param:   field.param,
				Message: "Unsupported parameter: '" + field.param + "' is not supported by Clyde.",
			})
		}
	}

	prompt, err := legacy.PromptText()
	if err != nil {
		return "", adapterErrRejectedParameter(adaptercompat.Rejection{
			Code:    adaptercompat.RejectionCodeUnsupportedParameter,
			Param:   "prompt",
			Message: err.Error(),
		})
	}

	return prompt, nil
}

// handleChat rejects each copied field that the resolved provider cannot
// honor.
func legacyChatRequest(legacy adapteropenai.CompletionRequest, prompt string) ChatRequest {
	var chat ChatRequest
	chat.Model = legacy.Model
	chat.Messages = []ChatMessage{
		{
			Role:             string(content.RoleUser),
			Content:          json.RawMessage(strconv.Quote(prompt)),
			Name:             "",
			ToolCalls:        nil,
			ToolCallID:       "",
			Reasoning:        "",
			ReasoningContent: "",
			Refusal:          "",
			Annotations:      nil,
			ReplayParts:      nil,
		},
	}
	chat.Stream = legacy.Stream
	chat.StreamOptions = legacy.StreamOptions
	chat.MaxTokens = legacy.MaxTokens
	chat.Temperature = legacy.Temperature
	chat.TopP = legacy.TopP
	chat.Stop = legacy.Stop
	chat.PresencePenalty = legacy.PresencePenalty
	chat.FrequencyPenalty = legacy.FrequencyPenalty
	chat.LogitBias = legacy.LogitBias
	chat.Seed = legacy.Seed
	chat.User = legacy.User
	chat.ReasoningEffort = legacy.ReasoningEffort

	return chat
}
