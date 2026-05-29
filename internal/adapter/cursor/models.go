package cursor

import (
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

// RequestPathKind is part of Clyde's typed adapter surface.
type RequestPathKind string

const (
	// RequestPathForeground is part of Clyde's typed adapter surface.
	RequestPathForeground RequestPathKind = "foreground"
	// RequestPathBackground is part of Clyde's typed adapter surface.
	RequestPathBackground RequestPathKind = "background"
	// RequestPathResume is part of Clyde's typed adapter surface.
	RequestPathResume RequestPathKind = "resume"
	// RequestPathSubagent is part of Clyde's typed adapter surface.
	RequestPathSubagent RequestPathKind = "subagent"
)

// NormalizeModelAlias trims Cursor's raw model alias for stable log attributes.
func NormalizeModelAlias(rawModel string) string {
	return strings.TrimSpace(rawModel)
}

// RequestPath is part of Clyde's typed adapter surface.
func RequestPath(req Request) RequestPathKind {
	if metadataHasAny(req.Metadata, "cursorResumeTaskId", "resumeTaskId", "resume", "isResume") {
		return RequestPathResume
	}
	if metadataHasAny(req.Metadata, "cursorSubagentId", "subagentId", "subagent", "isSubagent") {
		return RequestPathSubagent
	}
	if metadataHasAny(req.Metadata, "cursorBackgroundTaskId", "backgroundTaskId", "background", "isBackground", "runInBackground") {
		return RequestPathBackground
	}
	if requestTextContains(req.OpenAI, "you are the forked subagent") {
		return RequestPathSubagent
	}
	if requestTextContains(req.OpenAI, "resume after background task", "background task completed") {
		return RequestPathResume
	}
	if requestTextContains(req.OpenAI, "background task") {
		return RequestPathBackground
	}
	return RequestPathForeground
}

func hasRawToolName(toolNames []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, name := range toolNames {
		if strings.TrimSpace(name) == want {
			return true
		}
	}
	return false
}

func requestTextContains(req adapteropenai.ChatRequest, needles ...string) bool {
	if len(needles) == 0 {
		return false
	}
	haystack := strings.Builder{}
	for _, msg := range req.Messages {
		if text := adapteropenai.FlattenContent(msg.Content); text != "" {
			haystack.WriteString(text)
			haystack.WriteByte('\n')
		}
	}
	if len(req.Input) > 0 {
		haystack.Write(req.Input)
	}
	text := strings.ToLower(haystack.String())
	if text == "" {
		return false
	}
	for _, needle := range needles {
		if strings.Contains(text, strings.ToLower(needle)) {
			return true
		}
	}
	return false
}
