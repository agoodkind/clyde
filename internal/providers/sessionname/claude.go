package sessionname

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"goodkind.io/clyde/internal/session"
	"goodkind.io/clyde/internal/slogger"
)

func renameClaude(sess *session.Session, title string) error {
	transcriptPath := strings.TrimSpace(sess.Metadata.ProviderTranscriptPath())
	if transcriptPath == "" {
		return fmt.Errorf("missing transcript path")
	}
	sessionID := strings.TrimSpace(sess.Metadata.ProviderSessionID())
	if sessionID == "" {
		return fmt.Errorf("missing provider session id")
	}
	return appendClaudeRename(transcriptPath, sessionID, title)
}

func appendClaudeRename(transcriptPath, sessionID, title string) error {
	file, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		slog.Warn("sessionname.claude.open_failed",
			"component", "claude",
			"subcomponent", "sessionname",
			"concern", slogger.ConcernProviderClaudeLifecycle,
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("open claude transcript for rename: %w", err)
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(map[string]string{
		"type":        "custom-title",
		"customTitle": title,
		"sessionId":   sessionID,
	}); err != nil {
		_ = file.Close()
		slog.Warn("sessionname.claude.custom_title_failed",
			"component", "claude",
			"subcomponent", "sessionname",
			"concern", slogger.ConcernProviderClaudeLifecycle,
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("append claude custom title rename: %w", err)
	}
	if err := encoder.Encode(map[string]string{
		"type":      "agent-name",
		"agentName": title,
		"sessionId": sessionID,
	}); err != nil {
		_ = file.Close()
		slog.Warn("sessionname.claude.agent_name_failed",
			"component", "claude",
			"subcomponent", "sessionname",
			"concern", slogger.ConcernProviderClaudeLifecycle,
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("append claude agent name rename: %w", err)
	}
	if err := file.Close(); err != nil {
		slog.Warn("sessionname.claude.close_failed",
			"component", "claude",
			"subcomponent", "sessionname",
			"concern", slogger.ConcernProviderClaudeLifecycle,
			"session_id", sessionID,
			"transcript_path", transcriptPath,
			"err", err,
		)
		return fmt.Errorf("close claude transcript after rename: %w", err)
	}
	return nil
}
