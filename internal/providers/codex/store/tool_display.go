package codexstore

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"

	"goodkind.io/clyde/internal/transcript"
)

type codexToolDisplayInput struct {
	Command codexShellCommand `json:"command"`
	Cmd     string            `json:"cmd"`
	Path    string            `json:"path"`
	Pattern string            `json:"pattern"`
	Query   string            `json:"query"`
}

// codexShellCommand is a shell command as Codex writes it, which is either the
// command line as one string or the argument vector it was split into. Text
// holds the command line either way, so a caller reads one string rather than
// deciding the shape again.
type codexShellCommand struct {
	Text string
}

// UnmarshalJSON reads whichever of the two forms Codex wrote, and reads a third
// shape as no command.
//
// It returns no error in any case. A custom unmarshaler that returns one aborts
// the surrounding decode at that key, so every later key of the tool input would
// stay zero and a command this parser cannot read would cost the path, the
// pattern, and the query beside it.
func (command *codexShellCommand) UnmarshalJSON(data []byte) error {
	*command = codexShellCommand{Text: ""}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	switch trimmed[0] {
	case '"':
		var line string
		if err := json.Unmarshal(trimmed, &line); err != nil {
			slog.Debug("codex.store.tool_display.command_line_failed", "concern", "providers.codex.store", "err", err)
		} else {
			command.Text = strings.TrimSpace(line)
		}
		return nil
	case '[':
		var arguments []string
		if err := json.Unmarshal(trimmed, &arguments); err != nil {
			slog.Debug("codex.store.tool_display.command_arguments_failed", "concern", "providers.codex.store", "err", err)
		} else {
			command.Text = strings.TrimSpace(strings.Join(arguments, " "))
		}
		return nil
	default:
		slog.Debug("codex.store.tool_display.command_shape_unsupported", "concern", "providers.codex.store", "shape", string(trimmed[:1]))
		return nil
	}
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
	if parsed.Command.Text != "" {
		return parsed.Command.Text, "bash"
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
