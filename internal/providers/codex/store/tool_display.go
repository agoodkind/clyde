package codexstore

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

type codexToolDisplayInput struct {
	Command json.RawMessage `json:"command"`
	Cmd     string          `json:"cmd"`
	Path    string          `json:"path"`
	Pattern string          `json:"pattern"`
	Query   string          `json:"query"`
}

// toolDisplayText is what the user saw for one tool call, and the language that
// text is written in. The language is "bash" when the call ran a shell and empty
// otherwise.
//
// A tool whose shape this parser does not recognize shows nothing rather than
// its serialization. An unrecognized tool is a gap to fill here, and showing
// the JSON instead would hide the gap behind text nobody wrote.
func toolDisplayText(_ string, input transcript.ToolInputJSON) (string, string) {
	if input.Len() == 0 {
		return "", ""
	}
	var rawText string
	if err := json.Unmarshal(input.Raw, &rawText); err == nil {
		return rawText, ""
	}
	var parsed codexToolDisplayInput
	if err := json.Unmarshal(input.Raw, &parsed); err != nil {
		return "", ""
	}
	if command := codexCommandText(parsed.Command); command != "" {
		return command, "bash"
	}
	if command := strings.TrimSpace(parsed.Cmd); command != "" {
		return command, "bash"
	}
	for _, candidate := range []string{parsed.Path, parsed.Pattern, parsed.Query} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed, ""
		}
	}
	return "", ""
}

func codexCommandText(raw json.RawMessage) string {
	var command string
	if err := json.Unmarshal(raw, &command); err == nil {
		return strings.TrimSpace(command)
	}
	var arguments []string
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Join(arguments, " "))
}
