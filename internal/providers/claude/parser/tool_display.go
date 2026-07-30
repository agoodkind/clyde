package parser

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

type claudeToolDisplayInput struct {
	Command  string                    `json:"command"`
	Plan     string                    `json:"plan"`
	Question string                    `json:"question"`
	Options  []claudeToolDisplayOption `json:"options"`
	FilePath string                    `json:"file_path"`
	Pattern  string                    `json:"pattern"`
	Prompt   string                    `json:"prompt"`
	URL      string                    `json:"url"`
	Query    string                    `json:"query"`
}

// claudeToolDisplayOption is one choice offered alongside a question. Only the
// label and description are read, because those are the words on the screen.
type claudeToolDisplayOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// toolDisplayText is what the user saw for one tool call, and the language that
// text is written in. The language is "bash" when the call ran a shell and empty
// otherwise.
//
// A tool whose shape this parser does not recognize shows nothing rather than
// its serialization. An unrecognized tool is a gap to fill here, and showing
// the JSON instead would hide the gap behind text nobody wrote.
//
// A plan and a question come before the file and search fields because they are
// prose the person read and that exists nowhere else. A file path is enough for
// a call that edits or writes a file, since the file itself carries that content
// and the code index already reaches it.
func toolDisplayText(_ string, input transcript.ToolInputJSON) (string, string) {
	if input.Len() == 0 {
		return "", ""
	}
	var parsed claudeToolDisplayInput
	if err := json.Unmarshal(input.Raw, &parsed); err != nil {
		return "", ""
	}
	if command := strings.TrimSpace(parsed.Command); command != "" {
		return command, "bash"
	}
	if plan := strings.TrimSpace(parsed.Plan); plan != "" {
		return plan, "markdown"
	}
	if question := claudeQuestionText(parsed); question != "" {
		return question, ""
	}
	for _, candidate := range []string{parsed.FilePath, parsed.Pattern, parsed.Prompt, parsed.URL, parsed.Query} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed, ""
		}
	}
	return "", ""
}

// claudeQuestionText renders a question and the choices offered with it as the
// lines they appeared as on screen. It returns empty when there is no question,
// so a call carrying only options falls through to the other fields.
func claudeQuestionText(parsed claudeToolDisplayInput) string {
	question := strings.TrimSpace(parsed.Question)
	if question == "" {
		return ""
	}
	lines := make([]string, 0, len(parsed.Options)*2+1)
	lines = append(lines, question)
	for _, option := range parsed.Options {
		if label := strings.TrimSpace(option.Label); label != "" {
			lines = append(lines, label)
		}
		if description := strings.TrimSpace(option.Description); description != "" {
			lines = append(lines, description)
		}
	}
	return strings.Join(lines, "\n")
}
