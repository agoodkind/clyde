package parser

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

type cursorToolDisplayInput struct {
	Command               string `json:"command"`
	Cmd                   string `json:"cmd"`
	RelativeWorkspacePath string `json:"relative_workspace_path"`
	TargetFile            string `json:"target_file"`
	Path                  string `json:"path"`
	Prompt                string `json:"prompt"`
	Description           string `json:"description"`
	Query                 string `json:"query"`
	Pattern               string `json:"pattern"`
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
	var parsed cursorToolDisplayInput
	if err := json.Unmarshal(input.Raw, &parsed); err != nil {
		return "", ""
	}
	if command := strings.TrimSpace(parsed.Command); command != "" {
		return command, "bash"
	}
	if command := strings.TrimSpace(parsed.Cmd); command != "" {
		return command, "bash"
	}
	for _, candidate := range []string{
		parsed.RelativeWorkspacePath,
		parsed.TargetFile,
		parsed.Path,
		parsed.Prompt,
		parsed.Description,
		parsed.Query,
		parsed.Pattern,
	} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed, ""
		}
	}
	return "", ""
}
