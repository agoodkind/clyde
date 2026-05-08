package hook

import (
	"fmt"
	"os"
	"strings"
)

const (
	envSessionName       = "CLYDE_SESSION_NAME"
	envSessionID         = "CLYDE_SESSION_ID"
	envLegacySessionName = "CLYDE_SESSION"
)

func appendToEnvFile(key, value string) error {
	claudeEnvFile := os.Getenv("CLAUDE_ENV_FILE")
	if claudeEnvFile == "" {
		return nil
	}
	if !isValidEnvKey(key) {
		return fmt.Errorf("invalid env file key %q", key)
	}

	f, err := os.OpenFile(claudeEnvFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		hookLog.Warn("hook.env_file.open_failed",
			"component", "hook",
			"path", claudeEnvFile,
			"err", err,
		)
		return fmt.Errorf("failed to open CLAUDE_ENV_FILE: %w", err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintf(f, "%s=%s\n", key, shellQuoteEnvValue(value)); err != nil {
		hookLog.Warn("hook.env_file.write_failed",
			"component", "hook",
			"path", claudeEnvFile,
			"key", key,
			"err", err,
		)
		return fmt.Errorf("failed to write to CLAUDE_ENV_FILE: %w", err)
	}
	return nil
}

func isValidEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for index, char := range key {
		if char >= 'A' && char <= 'Z' {
			continue
		}
		if char >= 'a' && char <= 'z' {
			continue
		}
		if char == '_' {
			continue
		}
		if index > 0 && char >= '0' && char <= '9' {
			continue
		}
		return false
	}
	return true
}

func shellQuoteEnvValue(value string) string {
	var builder strings.Builder
	builder.Grow(len(value) + 3)
	builder.WriteString("$'")
	for _, char := range []byte(value) {
		switch char {
		case '\'':
			builder.WriteString("\\'")
		case '\\':
			builder.WriteString("\\\\")
		case '\n':
			builder.WriteString("\\n")
		case '\r':
			builder.WriteString("\\r")
		case '\t':
			builder.WriteString("\\t")
		default:
			if char >= 0x20 && char <= 0x7e {
				builder.WriteByte(char)
				continue
			}
			_, _ = fmt.Fprintf(&builder, "\\%03o", char)
		}
	}
	builder.WriteByte('\'')
	return builder.String()
}

func writeSessionNameToEnv(sessionName string) error {
	return appendToEnvFile(envLegacySessionName, sessionName)
}

func writeSessionIdentityToEnv(sessionName string, sessionID string) error {
	if err := writeSessionNameToEnv(sessionName); err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	return appendToEnvFile(envSessionID, sessionID)
}
