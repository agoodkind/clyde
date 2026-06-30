package hookspec

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// cursorHooksDocument preserves unknown Cursor hook settings as raw JSON
// because the user hook schema is owned by Cursor.
type cursorHooksDocument struct {
	fields map[string]json.RawMessage
}

type rawCursorHookHandler struct {
	fields map[string]json.RawMessage
}

func unmarshalCursorHooksDocument(body []byte) (cursorHooksDocument, error) {
	fields := map[string]json.RawMessage{}
	if strings.TrimSpace(string(body)) == "" {
		return cursorHooksDocument{fields: fields}, nil
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		wrapped := fmt.Errorf("unmarshal Cursor hooks: %w", err)
		slog.Warn("Cursor hooks unmarshal failed", "err", wrapped)
		return cursorHooksDocument{}, wrapped
	}
	return cursorHooksDocument{fields: fields}, nil
}

// MarshalJSON renders the complete Cursor hooks document.
func (document *cursorHooksDocument) MarshalJSON() ([]byte, error) {
	fields := document.fields
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	body, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal Cursor hooks: %w", err)
	}
	body = append(body, '\n')
	return body, nil
}

func (document *cursorHooksDocument) marshalCursorHookInstalls(installs []RegisteredInstall, signatures [][]string, clydeBin string) error {
	if document.fields == nil {
		document.fields = map[string]json.RawMessage{}
	}
	document.setInt("version", 1)
	hooksByEvent, err := document.unmarshalCursorHooks()
	if err != nil {
		return err
	}
	commands := cursorCommandSignatures(signatures, clydeBin)
	for eventName, handlers := range hooksByEvent {
		hooksByEvent[eventName] = removeCursorHookHandlers(handlers, commands)
	}
	for _, install := range installs {
		eventName := install.Spec.Event
		handler := newCursorCommandHookHandler(install.Spec, clydeBin)
		hooksByEvent[eventName] = append(hooksByEvent[eventName], handler)
	}
	body, err := json.Marshal(hooksByEvent)
	if err != nil {
		wrapped := fmt.Errorf("marshal Cursor hooks map: %w", err)
		slog.Warn("Cursor hooks marshal failed", "err", wrapped)
		return wrapped
	}
	document.fields["hooks"] = body
	return nil
}

func (document *cursorHooksDocument) unmarshalCursorHooks() (map[string][]rawCursorHookHandler, error) {
	hooksByEvent := map[string][]rawCursorHookHandler{}
	raw, ok := document.fields["hooks"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return hooksByEvent, nil
	}
	if err := json.Unmarshal(raw, &hooksByEvent); err != nil {
		wrapped := fmt.Errorf("unmarshal Cursor hooks map: %w", err)
		slog.Warn("Cursor hooks map unmarshal failed", "err", wrapped)
		return nil, wrapped
	}
	return hooksByEvent, nil
}

func (document *cursorHooksDocument) setInt(key string, value int) {
	body, err := json.Marshal(value)
	if err == nil {
		document.fields[key] = body
	}
}

func cursorCommandSignatures(signatures [][]string, clydeBin string) []string {
	commands := make([]string, 0, len(signatures))
	for _, signature := range signatures {
		commands = append(commands, shellCommand(clydeBin, signature))
	}
	return commands
}

func removeCursorHookHandlers(handlers []rawCursorHookHandler, commands []string) []rawCursorHookHandler {
	filtered := make([]rawCursorHookHandler, 0, len(handlers))
	for _, handler := range handlers {
		if slices.Contains(commands, handler.command()) {
			continue
		}
		filtered = append(filtered, handler)
	}
	return filtered
}

func newCursorCommandHookHandler(install InstallSpec, clydeBin string) rawCursorHookHandler {
	handler := rawCursorHookHandler{fields: map[string]json.RawMessage{}}
	handler.setString("command", shellCommand(clydeBin, install.Args))
	if install.TimeoutSeconds > 0 {
		handler.setInt("timeout", install.TimeoutSeconds)
	}
	if install.Matcher != "" {
		handler.setString("matcher", install.Matcher)
	}
	handler.setBool("failClosed", install.FailClosed)
	if install.LoopLimit > 0 {
		handler.setInt("loop_limit", install.LoopLimit)
	}
	return handler
}

func (handler *rawCursorHookHandler) UnmarshalJSON(body []byte) error {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("unmarshal Cursor hook handler: %w", err)
	}
	handler.fields = fields
	return nil
}

func (handler *rawCursorHookHandler) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(handler.fields)
	if err != nil {
		return nil, fmt.Errorf("marshal Cursor hook handler: %w", err)
	}
	return body, nil
}

func (handler *rawCursorHookHandler) command() string {
	var command string
	_ = json.Unmarshal(handler.fields["command"], &command)
	return command
}

func (handler *rawCursorHookHandler) setString(key string, value string) {
	body, err := json.Marshal(value)
	if err == nil {
		handler.fields[key] = body
	}
}

func (handler *rawCursorHookHandler) setInt(key string, value int) {
	body, err := json.Marshal(value)
	if err == nil {
		handler.fields[key] = body
	}
}

func (handler *rawCursorHookHandler) setBool(key string, value bool) {
	body, err := json.Marshal(value)
	if err == nil {
		handler.fields[key] = body
	}
}
