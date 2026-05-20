package hook

// SessionStartInput is the JSON body Claude Code sends to SessionStart hooks.
type SessionStartInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Source         string `json:"source"`
}

// SessionStartConfig optional hooks for tests and embedding. Nil fields use defaults.
type SessionStartConfig struct {
	LogRawEvent func(rawJSON []byte, sessionID string) error
}

// Result summarizes what ProcessSessionStart did for the operator and telemetry.
type Result struct {
	SkippedDuplicate bool
	Source           string
	SessionName      string
}

type sessionStartDeps struct {
	logRawEvent func(rawJSON []byte, sessionID string) error
}
