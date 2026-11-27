package finishreason

type geminiFinishReason string

const (
	geminiFinishReasonUnset                 geminiFinishReason = ""
	geminiFinishReasonStop                  geminiFinishReason = "STOP"
	geminiFinishReasonMaxTokens             geminiFinishReason = "MAX_TOKENS"
	geminiFinishReasonSafety                geminiFinishReason = "SAFETY"
	geminiFinishReasonRecitation            geminiFinishReason = "RECITATION"
	geminiFinishReasonOther                 geminiFinishReason = "OTHER"
	geminiFinishReasonMalformedFunctionCall geminiFinishReason = "MALFORMED_FUNCTION_CALL"
	geminiFinishReasonUnexpectedToolCall    geminiFinishReason = "UNEXPECTED_TOOL_CALL"
)

// FromGemini maps a Gemini candidate's finish reason to an OpenAI value.
//
// Safety and recitation stop content delivery. Invalid tool calls end the
// turn without asking the client to execute a tool.
//
//	finish := FromGemini(candidate.FinishReason)
func FromGemini(reason string) string {
	switch geminiFinishReason(reason) {
	case geminiFinishReasonMaxTokens:
		return string(codexFinishReasonLength)
	case geminiFinishReasonSafety, geminiFinishReasonRecitation:
		return string(codexFinishReasonContentFilter)
	case geminiFinishReasonUnset,
		geminiFinishReasonStop,
		geminiFinishReasonOther,
		geminiFinishReasonMalformedFunctionCall,
		geminiFinishReasonUnexpectedToolCall:
		return string(codexFinishReasonStop)
	default:
		return string(codexFinishReasonStop)
	}
}
