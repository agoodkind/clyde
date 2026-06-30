package hookspec

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

// claudeSettingsDocument preserves unknown Claude Code settings as raw JSON
// because the settings schema is owned by Claude Code.
type claudeSettingsDocument struct {
	fields map[string]json.RawMessage
}

type rawClaudeHookGroup struct {
	fields map[string]json.RawMessage
}

type rawClaudeHookHandler struct {
	fields map[string]json.RawMessage
}

func unmarshalClaudeSettingsDocument(body []byte) (claudeSettingsDocument, error) {
	fields := map[string]json.RawMessage{}
	if strings.TrimSpace(string(body)) == "" {
		return claudeSettingsDocument{fields: fields}, nil
	}
	if err := json.Unmarshal(body, &fields); err != nil {
		wrapped := fmt.Errorf("unmarshal Claude Code settings: %w", err)
		slog.Warn("Claude Code settings unmarshal failed", "err", wrapped)
		return claudeSettingsDocument{}, wrapped
	}
	return claudeSettingsDocument{fields: fields}, nil
}

// MarshalJSON renders the complete Claude Code settings document.
func (document *claudeSettingsDocument) MarshalJSON() ([]byte, error) {
	fields := document.fields
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	body, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal Claude Code settings: %w", err)
	}
	body = append(body, '\n')
	return body, nil
}

func (document *claudeSettingsDocument) marshalClaudeCodeHookInstalls(installs []RegisteredInstall, signatures [][]string, clydeBin string) error {
	if document.fields == nil {
		document.fields = map[string]json.RawMessage{}
	}
	hooksByEvent, err := document.unmarshalClaudeCodeHooks()
	if err != nil {
		return err
	}
	for eventName, groups := range hooksByEvent {
		hooksByEvent[eventName] = removeClaudeHookHandlers(groups, signatures)
	}
	for _, install := range installs {
		eventName := install.Spec.Event
		groups := hooksByEvent[eventName]
		handler := newClaudeCommandHookHandler(install.Spec, clydeBin)
		groups = addClaudeHookHandler(groups, install.Spec.Matcher, handler)
		hooksByEvent[eventName] = groups
	}
	body, err := json.Marshal(hooksByEvent)
	if err != nil {
		wrapped := fmt.Errorf("marshal Claude Code hooks: %w", err)
		slog.Warn("Claude Code hooks marshal failed", "err", wrapped)
		return wrapped
	}
	document.fields["hooks"] = body
	return nil
}

func (document *claudeSettingsDocument) unmarshalClaudeCodeHooks() (map[string][]rawClaudeHookGroup, error) {
	hooksByEvent := map[string][]rawClaudeHookGroup{}
	raw, ok := document.fields["hooks"]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return hooksByEvent, nil
	}
	if err := json.Unmarshal(raw, &hooksByEvent); err != nil {
		wrapped := fmt.Errorf("unmarshal Claude Code hooks: %w", err)
		slog.Warn("Claude Code hooks unmarshal failed", "err", wrapped)
		return nil, wrapped
	}
	return hooksByEvent, nil
}

func removeClaudeHookHandlers(groups []rawClaudeHookGroup, signatures [][]string) []rawClaudeHookGroup {
	out := make([]rawClaudeHookGroup, 0, len(groups))
	for _, group := range groups {
		handlers, ok := group.handlers()
		if !ok {
			out = append(out, group)
			continue
		}
		filtered := make([]rawClaudeHookHandler, 0, len(handlers))
		removed := false
		for _, handler := range handlers {
			if handler.matchesHookSignature(signatures) {
				removed = true
				continue
			}
			filtered = append(filtered, handler)
		}
		if removed {
			group.setHandlers(filtered)
		}
		if removed && len(filtered) == 0 {
			continue
		}
		out = append(out, group)
	}
	return out
}

func addClaudeHookHandler(
	groups []rawClaudeHookGroup,
	matcher string,
	handler rawClaudeHookHandler,
) []rawClaudeHookGroup {
	for index := range groups {
		if groups[index].matcher() != matcher {
			continue
		}
		handlers, ok := groups[index].handlers()
		if !ok {
			continue
		}
		handlers = append(handlers, handler)
		groups[index].setHandlers(handlers)
		return groups
	}
	group := newRawClaudeHookGroup(matcher, []rawClaudeHookHandler{handler})
	return append(groups, group)
}

func newRawClaudeHookGroup(
	matcher string,
	handlers []rawClaudeHookHandler,
) rawClaudeHookGroup {
	group := rawClaudeHookGroup{fields: map[string]json.RawMessage{}}
	group.setMatcher(matcher)
	group.setHandlers(handlers)
	return group
}

func newClaudeCommandHookHandler(install InstallSpec, clydeBin string) rawClaudeHookHandler {
	handler := rawClaudeHookHandler{fields: map[string]json.RawMessage{}}
	handler.setString("type", "command")
	handler.setString("command", clydeBin)
	handler.setStringSlice("args", install.Args)
	if install.TimeoutSeconds > 0 {
		handler.setInt("timeout", install.TimeoutSeconds)
	}
	if install.StatusMessage != "" {
		handler.setString("statusMessage", install.StatusMessage)
	}
	return handler
}

func (group *rawClaudeHookGroup) UnmarshalJSON(body []byte) error {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("unmarshal Claude Code hook group: %w", err)
	}
	group.fields = fields
	return nil
}

func (group *rawClaudeHookGroup) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(group.fields)
	if err != nil {
		return nil, fmt.Errorf("marshal Claude Code hook group: %w", err)
	}
	return body, nil
}

func (group *rawClaudeHookGroup) matcher() string {
	var matcher string
	_ = json.Unmarshal(group.fields["matcher"], &matcher)
	return matcher
}

func (group *rawClaudeHookGroup) handlers() ([]rawClaudeHookHandler, bool) {
	var handlers []rawClaudeHookHandler
	if err := json.Unmarshal(group.fields["hooks"], &handlers); err != nil {
		return nil, false
	}
	return handlers, true
}

func (group *rawClaudeHookGroup) setMatcher(matcher string) {
	group.setString("matcher", matcher)
}

func (group *rawClaudeHookGroup) setHandlers(handlers []rawClaudeHookHandler) {
	body, err := json.Marshal(handlers)
	if err == nil {
		group.fields["hooks"] = body
	}
}

func (group *rawClaudeHookGroup) setString(key string, value string) {
	body, err := json.Marshal(value)
	if err == nil {
		group.fields[key] = body
	}
}

func (handler *rawClaudeHookHandler) UnmarshalJSON(body []byte) error {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return fmt.Errorf("unmarshal Claude Code hook handler: %w", err)
	}
	handler.fields = fields
	return nil
}

func (handler *rawClaudeHookHandler) MarshalJSON() ([]byte, error) {
	body, err := json.Marshal(handler.fields)
	if err != nil {
		return nil, fmt.Errorf("marshal Claude Code hook handler: %w", err)
	}
	return body, nil
}

func (handler *rawClaudeHookHandler) args() []string {
	var args []string
	_ = json.Unmarshal(handler.fields["args"], &args)
	return args
}

func (handler *rawClaudeHookHandler) matchesHookSignature(signatures [][]string) bool {
	args := handler.args()
	for _, signature := range signatures {
		if slices.Equal(args, signature) {
			return true
		}
	}
	return false
}

func (handler *rawClaudeHookHandler) setString(key string, value string) {
	body, err := json.Marshal(value)
	if err == nil {
		handler.fields[key] = body
	}
}

func (handler *rawClaudeHookHandler) setStringSlice(key string, value []string) {
	body, err := json.Marshal(value)
	if err == nil {
		handler.fields[key] = body
	}
}

func (handler *rawClaudeHookHandler) setInt(key string, value int) {
	body, err := json.Marshal(value)
	if err == nil {
		handler.fields[key] = body
	}
}
