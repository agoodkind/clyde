package parser

import (
	"encoding/json"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

type claudeToolDisplayInput struct {
	Command   string                      `json:"command"`
	Plan      string                      `json:"plan"`
	Questions []claudeToolDisplayQuestion `json:"questions"`
	FilePath  string                      `json:"file_path"`
	Pattern   string                      `json:"pattern"`
	Prompt    string                      `json:"prompt"`
	URL       string                      `json:"url"`
	Query     string                      `json:"query"`
}

// claudeToolDisplayQuestion is one question put to the person, with the choices
// offered under it. A call carries a list of these, because one prompt can ask
// several things at once.
type claudeToolDisplayQuestion struct {
	Question string                    `json:"question"`
	Options  []claudeToolDisplayOption `json:"options"`
}

// claudeToolDisplayOption is one choice offered under a question. Only the label
// and the description are read, because those are the words on the screen.
type claudeToolDisplayOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// foreignToolPrefix marks a tool some other server defined. Claude namespaces
// every tool it loads from an MCP server as mcp__<server>__<tool>, so a name
// carrying this prefix belongs to that server and its keys mean whatever that
// server decided.
//
// Testing for the foreign marker rather than listing Claude's own tools is what
// keeps this right as Claude grows. Claude adds tools outside this repository,
// so a list of its own names would misread every tool added after the list was
// written and degrade a shape this parser handles into raw JSON. The marker runs
// the other way: a new tool of Claude's is read, and a new server's tool is kept
// whole.
const foreignToolPrefix = "mcp__"

// toolDisplayText is what the user saw for one tool call, and the language that
// text is written in. The language is "bash" when the call ran a shell and empty
// otherwise.
//
// A tool another server defined keeps its input exactly as it arrived. Those keys
// belong to whoever defined the tool, so reading them through Claude's vocabulary
// drops whatever does not match, and a question or an answer the person read
// stops being searchable. Keeping the input keeps those words reachable. A tool
// of Claude's whose input will not decode keeps its input for the same reason.
//
// A tool of Claude's that decodes but carries none of these keys stores its name
// alone. Its shape is known and holds nothing a person read, so a gap here is a
// key to add rather than a payload to keep.
//
// A plan and a question come before the file and search fields because they are
// prose the person read and that exists nowhere else. A file path is enough for
// a call that edits or writes a file, since the file itself carries that content
// and the code index already reaches it.
func toolDisplayText(name string, input transcript.ToolInputJSON) (string, string) {
	if input.Len() == 0 {
		return "", ""
	}
	if strings.HasPrefix(strings.TrimSpace(name), foreignToolPrefix) {
		return string(input.Raw), ""
	}
	var parsed claudeToolDisplayInput
	if err := json.Unmarshal(input.Raw, &parsed); err != nil {
		return string(input.Raw), ""
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

// claudeQuestionText renders every question a call asked, with the choices
// offered under each, as the lines they appeared as on screen. It returns empty
// when no question carries text, so a call whose shape does not match falls
// through to the other fields.
func claudeQuestionText(parsed claudeToolDisplayInput) string {
	lines := make([]string, 0, len(parsed.Questions)*4)
	for _, question := range parsed.Questions {
		if asked := strings.TrimSpace(question.Question); asked != "" {
			lines = append(lines, asked)
		}
		for _, option := range question.Options {
			if label := strings.TrimSpace(option.Label); label != "" {
				lines = append(lines, label)
			}
			if description := strings.TrimSpace(option.Description); description != "" {
				lines = append(lines, description)
			}
		}
	}
	return strings.Join(lines, "\n")
}
