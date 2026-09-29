package searchbackend

import (
	"slices"
	"strings"

	"goodkind.io/gksyntax/shelldecomp"
)

// shellDisplayLanguage is the display language hint a provider parser sets
// for a shell command display.
const shellDisplayLanguage = "bash"

// toolCallTokens returns the deduplicated searchable tokens of one tool call:
// the tool name, then for a shell display the program names and the read and
// write targets that shelldecomp finds, then the display text. The token order
// and deduplication match the lm-semantic-search conversation tool content
// that this projection replaces.
func toolCallTokens(tool Tool) []string {
	tokens := make([]string, 0)
	appendToolToken(&tokens, tool.Name)
	display := strings.TrimSpace(tool.Display)
	if display != "" && tool.DisplayLang == shellDisplayLanguage {
		appendShellTokens(&tokens, display)
	}
	appendToolToken(&tokens, tool.Display)
	return tokens
}

// appendShellTokens parses a shell command from the root directory and appends
// its program names and file targets. An opaque parse, or a parse that yields
// no token, appends the raw command.
func appendShellTokens(tokens *[]string, command string) {
	decomposition := shelldecomp.Parse(command, "/", "")
	if decomposition == nil || decomposition.IsOpaque() {
		appendToolToken(tokens, command)
		return
	}
	tokenCount := len(*tokens)
	for _, shellCommand := range decomposition.Commands() {
		appendToolToken(tokens, shellCommand.Argv0)
	}
	for _, readTarget := range decomposition.ReadTargets() {
		appendShellTarget(tokens, readTarget.Resolvable, readTarget.Path, readTarget.Raw)
	}
	for _, writeTarget := range decomposition.WriteTargets() {
		appendShellTarget(tokens, writeTarget.Resolvable, writeTarget.Path, writeTarget.Raw)
	}
	if len(*tokens) == tokenCount {
		appendToolToken(tokens, command)
	}
}

// appendShellTarget appends the resolved path of a resolvable target and also
// its raw token when the two differ. The parse runs from the root directory,
// and a relative raw token resolves to a different absolute path.
func appendShellTarget(tokens *[]string, resolvable bool, path string, raw string) {
	if resolvable {
		appendToolToken(tokens, path)
		if strings.TrimSpace(raw) != "" && strings.TrimSpace(raw) != strings.TrimSpace(path) {
			appendToolToken(tokens, raw)
		}
		return
	}
	appendToolToken(tokens, raw)
}

// appendToolToken appends one trimmed token unless it is empty or already
// present. A one-word command and an opaque command produce the same string as
// the display text, and the token list keeps it once.
func appendToolToken(tokens *[]string, value string) {
	trimmedValue := strings.TrimSpace(value)
	if trimmedValue == "" {
		return
	}
	if slices.Contains(*tokens, trimmedValue) {
		return
	}
	*tokens = append(*tokens, trimmedValue)
}
