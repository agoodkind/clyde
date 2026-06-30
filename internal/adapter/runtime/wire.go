package runtime

import adapteropenai "goodkind.io/clyde/internal/adapter/openai"

type (
	// ChatResponse is part of Clyde's typed adapter surface.
	ChatResponse = adapteropenai.ChatResponse
	// ChatChoice is part of Clyde's typed adapter surface.
	ChatChoice = adapteropenai.ChatChoice
	// ChatMessage is part of Clyde's typed adapter surface.
	ChatMessage = adapteropenai.ChatMessage
	// MessageAnnotation is part of Clyde's typed adapter surface.
	MessageAnnotation = adapteropenai.MessageAnnotation
	// URLCitation is part of Clyde's typed adapter surface.
	URLCitation = adapteropenai.URLCitation
	// ToolCall is part of Clyde's typed adapter surface.
	ToolCall = adapteropenai.ToolCall
	// ToolCallFunction is part of Clyde's typed adapter surface.
	ToolCallFunction = adapteropenai.ToolCallFunction
	// LogprobsResult is part of Clyde's typed adapter surface.
	LogprobsResult = adapteropenai.LogprobsResult
	// LogprobToken is part of Clyde's typed adapter surface.
	LogprobToken = adapteropenai.LogprobToken
	// TopLogprob is part of Clyde's typed adapter surface.
	TopLogprob = adapteropenai.TopLogprob
	// Usage is part of Clyde's typed adapter surface.
	Usage = adapteropenai.Usage
	// PromptTokensDetails is part of Clyde's typed adapter surface.
	PromptTokensDetails = adapteropenai.PromptTokensDetails
	// StreamChunk is part of Clyde's typed adapter surface.
	StreamChunk = adapteropenai.StreamChunk
	// StreamChoice is part of Clyde's typed adapter surface.
	StreamChoice = adapteropenai.StreamChoice
	// StreamDelta is part of Clyde's typed adapter surface.
	StreamDelta = adapteropenai.StreamDelta
)
