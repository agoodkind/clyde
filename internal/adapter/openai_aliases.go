package adapter

import adapteropenai "goodkind.io/clyde/internal/adapter/openai"

type (
	ChatRequest         = adapteropenai.ChatRequest
	Reasoning           = adapteropenai.Reasoning
	Tool                = adapteropenai.Tool
	ToolFunctionSchema  = adapteropenai.ToolFunctionSchema
	Function            = adapteropenai.Function
	ChatMessage         = adapteropenai.ChatMessage
	MessageAnnotation   = adapteropenai.MessageAnnotation
	URLCitation         = adapteropenai.URLCitation
	ToolCall            = adapteropenai.ToolCall
	ToolCallFunction    = adapteropenai.ToolCallFunction
	ContentPart         = adapteropenai.ContentPart
	ImageURLPart        = adapteropenai.ImageURLPart
	AudioInputRef       = adapteropenai.AudioInputRef
	StreamOptions       = adapteropenai.StreamOptions
	ChatResponse        = adapteropenai.ChatResponse
	ChatChoice          = adapteropenai.ChatChoice
	LogprobsResult      = adapteropenai.LogprobsResult
	LogprobToken        = adapteropenai.LogprobToken
	TopLogprob          = adapteropenai.TopLogprob
	StreamChunk         = adapteropenai.StreamChunk
	StreamChoice        = adapteropenai.StreamChoice
	StreamDelta         = adapteropenai.StreamDelta
	Usage               = adapteropenai.Usage
	PromptTokensDetails = adapteropenai.PromptTokensDetails
	ModelsResponse      = adapteropenai.ModelsResponse
	ModelEntry          = adapteropenai.ModelEntry
	ContentKind         = adapteropenai.ContentKind
	BodySummary         = adapteropenai.BodySummary
	MsgSummary          = adapteropenai.MsgSummary
	ToolSummary         = adapteropenai.ToolSummary
	RequestDiscovery    = adapteropenai.RequestDiscovery
)

const (
	ContentKindEmpty  = adapteropenai.ContentKindEmpty
	ContentKindString = adapteropenai.ContentKindString
	ContentKindParts  = adapteropenai.ContentKindParts
)

var (
	FlattenContent       = adapteropenai.FlattenContent
	NormalizeContent     = adapteropenai.NormalizeContent
	SummarizeChatBody    = adapteropenai.SummarizeChatBody
	SummarizeChatRequest = adapteropenai.SummarizeChatRequest
	DiscoverRequest      = adapteropenai.DiscoverRequest
	HeaderNames          = adapteropenai.HeaderNames
)
