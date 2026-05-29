package adapter

import adapteropenai "goodkind.io/clyde/internal/adapter/openai"

type (
	// ChatRequest is part of Clyde's typed adapter surface.
	ChatRequest = adapteropenai.ChatRequest
	// Reasoning is part of Clyde's typed adapter surface.
	Reasoning = adapteropenai.Reasoning
	// Tool is part of Clyde's typed adapter surface.
	Tool = adapteropenai.Tool
	// ToolFunctionSchema is part of Clyde's typed adapter surface.
	ToolFunctionSchema = adapteropenai.ToolFunctionSchema
	// Function is part of Clyde's typed adapter surface.
	Function = adapteropenai.Function
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
	// ContentPart is part of Clyde's typed adapter surface.
	ContentPart = adapteropenai.ContentPart
	// ImageURLPart is part of Clyde's typed adapter surface.
	ImageURLPart = adapteropenai.ImageURLPart
	// AudioInputRef is part of Clyde's typed adapter surface.
	AudioInputRef = adapteropenai.AudioInputRef
	// StreamOptions is part of Clyde's typed adapter surface.
	StreamOptions = adapteropenai.StreamOptions
	// ChatResponse is part of Clyde's typed adapter surface.
	ChatResponse = adapteropenai.ChatResponse
	// ChatChoice is part of Clyde's typed adapter surface.
	ChatChoice = adapteropenai.ChatChoice
	// LogprobsResult is part of Clyde's typed adapter surface.
	LogprobsResult = adapteropenai.LogprobsResult
	// LogprobToken is part of Clyde's typed adapter surface.
	LogprobToken = adapteropenai.LogprobToken
	// TopLogprob is part of Clyde's typed adapter surface.
	TopLogprob = adapteropenai.TopLogprob
	// StreamChunk is part of Clyde's typed adapter surface.
	StreamChunk = adapteropenai.StreamChunk
	// StreamChoice is part of Clyde's typed adapter surface.
	StreamChoice = adapteropenai.StreamChoice
	// StreamDelta is part of Clyde's typed adapter surface.
	StreamDelta = adapteropenai.StreamDelta
	// Usage is part of Clyde's typed adapter surface.
	Usage = adapteropenai.Usage
	// PromptTokensDetails is part of Clyde's typed adapter surface.
	PromptTokensDetails = adapteropenai.PromptTokensDetails
	// ModelsResponse is part of Clyde's typed adapter surface.
	ModelsResponse = adapteropenai.ModelsResponse
	// ModelEntry is part of Clyde's typed adapter surface.
	ModelEntry = adapteropenai.ModelEntry
	// ContentKind is part of Clyde's typed adapter surface.
	ContentKind = adapteropenai.ContentKind
	// BodySummary is part of Clyde's typed adapter surface.
	BodySummary = adapteropenai.BodySummary
	// MsgSummary is part of Clyde's typed adapter surface.
	MsgSummary = adapteropenai.MsgSummary
	// ToolSummary is part of Clyde's typed adapter surface.
	ToolSummary = adapteropenai.ToolSummary
	// RequestDiscovery is part of Clyde's typed adapter surface.
	RequestDiscovery = adapteropenai.RequestDiscovery
)

const (
	// ContentKindEmpty is part of Clyde's typed adapter surface.
	ContentKindEmpty = adapteropenai.ContentKindEmpty
	// ContentKindString is part of Clyde's typed adapter surface.
	ContentKindString = adapteropenai.ContentKindString
	// ContentKindParts is part of Clyde's typed adapter surface.
	ContentKindParts = adapteropenai.ContentKindParts
)

var (
	// FlattenContent is part of Clyde's typed adapter surface.
	FlattenContent = adapteropenai.FlattenContent
	// NormalizeContent is part of Clyde's typed adapter surface.
	NormalizeContent = adapteropenai.NormalizeContent
	// SummarizeChatBody is part of Clyde's typed adapter surface.
	SummarizeChatBody = adapteropenai.SummarizeChatBody
	// SummarizeChatRequest is part of Clyde's typed adapter surface.
	SummarizeChatRequest = adapteropenai.SummarizeChatRequest
	// DiscoverRequest is part of Clyde's typed adapter surface.
	DiscoverRequest = adapteropenai.DiscoverRequest
	// HeaderNames is part of Clyde's typed adapter surface.
	HeaderNames = adapteropenai.HeaderNames
)
