package antigravity

import (
	"context"
	"slices"

	"goodkind.io/clyde/internal/adapter/finishreason"
	"goodkind.io/clyde/internal/adapter/openai"
	"goodkind.io/clyde/internal/adapter/provider"
	"goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/adapter/replay"
)

type streamTranslator struct {
	result provider.Result
}

func (state *streamTranslator) translate(
	ctx context.Context,
	chunk streamChunk,
	writer provider.EventWriter,
) error {
	response := chunk.Response
	if response.Usage != nil {
		state.result.Usage = response.Usage.openAIUsage()
	}

	if chunk.TraceID != "" {
		state.result.UpstreamResponseID = chunk.TraceID
	}

	for _, candidate := range response.Candidates {
		if candidate.Index != 0 {
			continue
		}

		for _, part := range candidate.Content.Parts {
			if err := state.writePart(ctx, part, writer); err != nil {
				return err
			}
		}

		if candidate.FinishReason != "" {
			state.result.FinishReason = finishreason.FromGemini(candidate.FinishReason)
			if candidate.FinishReason == "STOP" && state.result.ToolCallCount > 0 {
				state.result.FinishReason = "tool_calls"
			}
		}
	}

	return nil
}

func (usage streamUsage) openAIUsage() openai.Usage {
	var result openai.Usage
	result.PromptTokens = usage.Prompt
	result.CompletionTokens = usage.Candidates
	result.TotalTokens = usage.Total
	result.PromptTokensDetails = &openai.PromptTokensDetails{
		CachedTokens:               usage.Cached,
		CacheWriteTokens:           new(0),
		CompatibilityOmitsUncached: true,
	}

	reasoning := 0
	if usage.Thoughts != nil {
		reasoning = *usage.Thoughts
	}

	result.CompletionTokens += reasoning
	result.CompletionTokensDetails = &openai.CompletionTokensDetails{ReasoningTokens: reasoning}

	return result
}

func (state *streamTranslator) writePart(
	ctx context.Context,
	part streamPart,
	writer provider.EventWriter,
) error {
	var event render.Event
	var metadata *render.ReplayMetadata
	switch {
	case part.FunctionCall != nil:
		call := part.FunctionCall
		callID := call.ID
		if callID == "" {
			callID = replay.NewID()
		}

		arguments := string(call.Args)
		if arguments == "" {
			arguments = "{}"
		}

		event = render.ToolCallDelta{
			ToolCalls: []openai.ToolCall{
				{
					Index:    state.result.ToolCallCount,
					Type:     functionToolType,
					ID:       callID,
					Function: openai.ToolCallFunction{Name: call.Name, Arguments: arguments},
				},
			},
			NativePatchInput: nil,
		}
		if part.ThoughtSignature != "" {
			metadata = &render.ReplayMetadata{
				Part: toolSignaturePart(callID, part.ThoughtSignature),
			}
		}

		state.result.ToolCallCount++
		if !slices.Contains(state.result.ToolCallNames, call.Name) {
			state.result.ToolCallNames = append(state.result.ToolCallNames, call.Name)
		}
	case part.Thought:
		state.result.ReasoningSignaled = true
		var delta render.ReasoningDelta
		delta.Text = part.Text
		event = delta
	case part.Text != "":
		event = render.TextDelta{Text: part.Text}
	default:
		return nil
	}

	if err := writer.WriteEvent(event); err != nil {
		return geminiStreamError(ctx, "write event for", err)
	}

	if metadata != nil {
		if err := writer.WriteEvent(*metadata); err != nil {
			return geminiStreamError(ctx, "write replay metadata for", err)
		}
	}

	return nil
}

func toolSignaturePart(id, signature string) render.SyntheticPart {
	var part render.SyntheticPart
	part.Kind = render.SyntheticToolSignature
	part.Ref = id
	part.Signature = signature
	part.Origin = render.OriginAntigravity

	return part
}
