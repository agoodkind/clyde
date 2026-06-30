package hookspec

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

type hookSpecificOutputEnvelope struct {
	HookSpecificOutput hookSpecificAdditionalContext `json:"hookSpecificOutput"`
}

type hookSpecificAdditionalContext struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type cursorFollowupOutput struct {
	FollowupMessage string `json:"followup_message"`
}

func writeAdditionalContext(writer io.Writer, client Client, eventName string, body string) error {
	switch client {
	case ClientCodex:
		envelope := hookSpecificOutputEnvelope{
			HookSpecificOutput: hookSpecificAdditionalContext{
				HookEventName:     eventName,
				AdditionalContext: body,
			},
		}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			wrapped := fmt.Errorf("marshal Codex additional context output: %w", err)
			slog.Warn("hook output marshal failed", "client", client, "event", eventName, "err", wrapped)
			return wrapped
		}
		encoded = append(encoded, '\n')
		if _, err := writer.Write(encoded); err != nil {
			wrapped := fmt.Errorf("write Codex additional context output: %w", err)
			slog.Warn("hook output write failed", "client", client, "event", eventName, "err", wrapped)
			return wrapped
		}
		return nil
	case ClientClaudeCode:
		if _, err := io.WriteString(writer, body); err != nil {
			wrapped := fmt.Errorf("write Claude additional context output: %w", err)
			slog.Warn("hook output write failed", "client", client, "event", eventName, "err", wrapped)
			return wrapped
		}
		return nil
	case ClientAll, ClientCursor:
		err := fmt.Errorf("client %q does not support additional context output", client)
		wrapped := fmt.Errorf("write additional context output: %w", err)
		slog.Warn("hook output write failed", "client", client, "event", eventName, "err", wrapped)
		return wrapped
	default:
		err := fmt.Errorf("client %q does not support additional context output", client)
		wrapped := fmt.Errorf("write additional context output: %w", err)
		slog.Warn("hook output write failed", "client", client, "event", eventName, "err", wrapped)
		return wrapped
	}
}

func writeCursorFollowup(writer io.Writer, body string) error {
	encoded, err := json.Marshal(cursorFollowupOutput{FollowupMessage: body})
	if err != nil {
		wrapped := fmt.Errorf("marshal Cursor followup output: %w", err)
		slog.Warn("hook output marshal failed", "client", ClientCursor, "event", EventCursorStop, "err", wrapped)
		return wrapped
	}
	encoded = append(encoded, '\n')
	if _, err := writer.Write(encoded); err != nil {
		wrapped := fmt.Errorf("write Cursor followup output: %w", err)
		slog.Warn("hook output write failed", "client", ClientCursor, "event", EventCursorStop, "err", wrapped)
		return wrapped
	}
	return nil
}
