package cursor

import (
	"strings"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

type RequestPathKind string

const (
	RequestPathForeground RequestPathKind = "foreground"
	RequestPathBackground RequestPathKind = "background"
	RequestPathResume     RequestPathKind = "resume"
	RequestPathSubagent   RequestPathKind = "subagent"
)

// NormalizeModelAlias is the legacy whitespace-trim helper used by
// daemon and TUI logging to produce a stable `cursor_normalized_model`
// attribute. New adapter code should resolve full model identity via
// internal/adapter/resolver.Resolve, which returns the typed
// ResolvedRequest with provider, family, effort, and budget. This
// helper stays as a slim shim until the remaining daemon log call
// sites migrate.
func NormalizeModelAlias(rawModel string) string {
	return strings.TrimSpace(rawModel)
}

// NormalizeSessionSettingsModel trims the selected model while preserving the
// full declarative alias. Effort is part of the canonical clyde-* model name.
func NormalizeSessionSettingsModel(rawModel string) string {
	return NormalizeModelAlias(rawModel)
}

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
