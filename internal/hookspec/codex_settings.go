package hookspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

const (
	codexManagedBegin = "# BEGIN clyde managed hooks"
	codexManagedEnd   = "# END clyde managed hooks"
)

type codexHookIdentity struct {
	EventName string                 `json:"event_name"`
	Group     codexHookIdentityGroup `json:"group"`
}

type codexHookIdentityGroup struct {
	Matcher string                     `json:"matcher,omitempty"`
	Hooks   []codexHookIdentityHandler `json:"hooks"`
}

type codexHookIdentityHandler struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout,omitempty"`
	StatusMessage string `json:"statusMessage,omitempty"`
}

func marshalCodexHookInstalls(existing []byte, installs []RegisteredInstall, signatures [][]string, clydeBin string, settingsPath string) ([]byte, error) {
	base := removeCodexManagedBlock(string(existing))
	base = removeCodexCommandHookGroups(base, signatures)
	existingHookGroups := countExistingCodexHookGroups(base)
	base = ensureCodexHooksFeature(base)
	base = strings.TrimRight(base, "\n")
	block, err := renderCodexManagedBlock(installs, clydeBin, settingsPath, existingHookGroups)
	if err != nil {
		return nil, err
	}
	if base == "" {
		return []byte(block + "\n"), nil
	}
	return []byte(base + "\n\n" + block + "\n"), nil
}

func removeCodexCommandHookGroups(input string, signatures [][]string) string {
	if len(signatures) == 0 || strings.TrimSpace(input) == "" {
		return input
	}
	lines := strings.Split(input, "\n")
	out := make([]string, 0, len(lines))
	for index := 0; index < len(lines); {
		if !isCodexHookGroupHeader(lines[index]) {
			out = append(out, lines[index])
			index++
			continue
		}
		end := index + 1
		for end < len(lines) && !isCodexHookGroupHeader(lines[end]) && !isNonHookTableHeader(lines[end]) {
			end++
		}
		if codexGroupContainsCommand(lines[index:end], signatures) {
			index = end
			continue
		}
		out = append(out, lines[index:end]...)
		index = end
	}
	return strings.Join(out, "\n")
}

func isCodexHookGroupHeader(line string) bool {
	trimmed := tomlHeaderText(line)
	return strings.HasPrefix(trimmed, "[[hooks.") && strings.HasSuffix(trimmed, "]]") && !strings.Contains(trimmed, ".hooks")
}

func isNonHookTableHeader(line string) bool {
	trimmed := tomlHeaderText(line)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return false
	}
	if strings.HasPrefix(trimmed, "[[hooks.") {
		return false
	}
	return !strings.HasPrefix(trimmed, "[hooks.") && trimmed != "[hooks]"
}

func codexGroupContainsCommand(lines []string, signatures [][]string) bool {
	for _, line := range lines {
		key, value, ok := parseTomlAssignment(line)
		if !ok || key != "command" {
			continue
		}
		if commandHasManagedArgs(value, signatures) {
			return true
		}
	}
	return false
}

func parseTomlAssignment(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "=", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	key := strings.TrimSpace(parts[0])
	value := strings.TrimSpace(trimTomlInlineComment(parts[1]))
	decoded, err := strconv.Unquote(value)
	if err == nil {
		value = decoded
	} else if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "'"), "'")
	}
	return key, value, true
}

func trimTomlInlineComment(value string) string {
	inSingleQuote := false
	inDoubleQuote := false
	escaped := false
	for index, char := range value {
		if escaped {
			escaped = false
			continue
		}
		if inDoubleQuote && char == '\\' {
			escaped = true
			continue
		}
		switch char {
		case '\'':
			if !inDoubleQuote {
				inSingleQuote = !inSingleQuote
			}
		case '"':
			if !inSingleQuote {
				inDoubleQuote = !inDoubleQuote
			}
		case '#':
			if !inSingleQuote && !inDoubleQuote {
				return value[:index]
			}
		}
	}
	return value
}

func removeCodexManagedBlock(input string) string {
	start := strings.Index(input, codexManagedBegin)
	end := strings.Index(input, codexManagedEnd)
	if start < 0 || end < start {
		return input
	}
	end += len(codexManagedEnd)
	return input[:start] + input[end:]
}

func ensureCodexHooksFeature(input string) string {
	lines := strings.Split(input, "\n")
	featuresStart := -1
	featuresEnd := len(lines)
	for index, line := range lines {
		if strings.TrimSpace(line) == "[features]" {
			featuresStart = index
			continue
		}
		if featuresStart >= 0 && index > featuresStart && isTomlTableHeader(line) {
			featuresEnd = index
			break
		}
	}
	if featuresStart < 0 {
		trimmed := strings.TrimLeft(input, "\n")
		if strings.TrimSpace(trimmed) == "" {
			return "[features]\nhooks = true\n"
		}
		return "[features]\nhooks = true\n\n" + trimmed
	}
	hooksIndex := -1
	for index := featuresStart + 1; index < featuresEnd; index++ {
		if strings.HasPrefix(strings.TrimSpace(lines[index]), "hooks") {
			parts := strings.SplitN(strings.TrimSpace(lines[index]), "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "hooks" {
				hooksIndex = index
				break
			}
		}
	}
	if hooksIndex >= 0 {
		lines[hooksIndex] = "hooks = true"
		return strings.Join(lines, "\n")
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:featuresStart+1]...)
	out = append(out, "hooks = true")
	out = append(out, lines[featuresStart+1:]...)
	return strings.Join(out, "\n")
}

func countExistingCodexHookGroups(base string) map[string]int {
	counts := map[string]int{}
	if strings.TrimSpace(base) == "" {
		return counts
	}
	for line := range strings.SplitSeq(base, "\n") {
		eventName, ok := codexHookGroupEventName(line)
		if !ok {
			continue
		}
		normalizedEventName := normalizeCodexEventName(eventName)
		if normalizedEventName == "state" {
			continue
		}
		counts[normalizedEventName]++
	}
	return counts
}

func codexHookGroupEventName(line string) (string, bool) {
	trimmed := tomlHeaderText(line)
	if !strings.HasPrefix(trimmed, "[[hooks.") || !strings.HasSuffix(trimmed, "]]") {
		return "", false
	}
	trimmed = strings.TrimSuffix(strings.TrimPrefix(trimmed, "[[hooks."), "]]")
	if strings.Contains(trimmed, ".") {
		return "", false
	}
	decoded, err := strconv.Unquote(trimmed)
	if err == nil {
		return decoded, true
	}
	if strings.HasPrefix(trimmed, "'") && strings.HasSuffix(trimmed, "'") && len(trimmed) >= 2 {
		return strings.TrimSuffix(strings.TrimPrefix(trimmed, "'"), "'"), true
	}
	return trimmed, true
}

func normalizeCodexEventName(eventName string) string {
	return strings.ReplaceAll(strings.ToLower(eventName), "_", "")
}

func isTomlTableHeader(line string) bool {
	trimmed := tomlHeaderText(line)
	return strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")
}

func tomlHeaderText(line string) string {
	return strings.TrimSpace(trimTomlInlineComment(line))
}

func renderCodexManagedBlock(installs []RegisteredInstall, clydeBin string, settingsPath string, existingHookGroups map[string]int) (string, error) {
	var builder strings.Builder
	builder.WriteString(codexManagedBegin)
	builder.WriteString("\n")
	builder.WriteString("# This block is managed by clyde install hooks.\n")
	builder.WriteString("\n")

	eventCounters := map[string]int{}
	for _, install := range installs {
		eventName := codexEventName(install.Spec.Event)
		counterKey := normalizeCodexEventName(eventName)
		groupIndex, ok := eventCounters[counterKey]
		if !ok {
			groupIndex = existingHookGroups[counterKey]
		}
		eventCounters[counterKey] = groupIndex + 1
		command := shellCommand(clydeBin, install.Spec.Args)
		hash, err := marshalCodexTrustedHash(eventName, install.Spec, command)
		if err != nil {
			return "", err
		}
		stateKey := fmt.Sprintf("%s:%s:%d:0", settingsPath, eventName, groupIndex)
		fmt.Fprintf(&builder, "[hooks.state.%s]\n", strconv.Quote(stateKey))
		fmt.Fprintf(&builder, "trusted_hash = %s\n\n", strconv.Quote(hash))

		fmt.Fprintf(&builder, "[[hooks.%s]]\n", eventName)
		if install.Spec.Matcher != "" {
			fmt.Fprintf(&builder, "matcher = %s\n", strconv.Quote(install.Spec.Matcher))
		}
		builder.WriteString("\n")
		fmt.Fprintf(&builder, "[[hooks.%s.hooks]]\n", eventName)
		builder.WriteString("type = \"command\"\n")
		fmt.Fprintf(&builder, "command = %s\n", strconv.Quote(command))
		if install.Spec.TimeoutSeconds > 0 {
			fmt.Fprintf(&builder, "timeout = %d\n", install.Spec.TimeoutSeconds)
		}
		if install.Spec.StatusMessage != "" {
			fmt.Fprintf(&builder, "statusMessage = %s\n", strconv.Quote(install.Spec.StatusMessage))
		}
		builder.WriteString("\n")
	}
	builder.WriteString(codexManagedEnd)
	return builder.String(), nil
}

func codexEventName(event string) string {
	switch event {
	case EventPreCompact:
		return "pre_compact"
	case EventSessionStart:
		return "session_start"
	default:
		return strings.ToLower(event)
	}
}

func marshalCodexTrustedHash(eventName string, install InstallSpec, command string) (string, error) {
	identity := codexHookIdentity{
		EventName: eventName,
		Group: codexHookIdentityGroup{
			Matcher: install.Matcher,
			Hooks: []codexHookIdentityHandler{
				{
					Type:          "command",
					Command:       command,
					Timeout:       install.TimeoutSeconds,
					StatusMessage: install.StatusMessage,
				},
			},
		},
	}
	body, err := json.Marshal(identity)
	if err != nil {
		wrapped := fmt.Errorf("marshal Codex hook trust identity: %w", err)
		slog.Warn("Codex hook trust identity marshal failed", "event", eventName, "err", wrapped)
		return "", wrapped
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
