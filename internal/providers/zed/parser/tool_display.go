package parser

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

type zedToolDisplayInput struct {
	Command string `json:"command"`
	Path    string `json:"path"`
	Regex   string `json:"regex"`
	Query   string `json:"query"`
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
	var parsed zedToolDisplayInput
	if err := json.Unmarshal(input.Raw, &parsed); err != nil {
		return "", ""
	}
	if command := strings.TrimSpace(parsed.Command); command != "" {
		return command, "bash"
	}
	for _, candidate := range []string{parsed.Path, parsed.Regex, parsed.Query} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed, ""
		}
	}
	return "", ""
}
