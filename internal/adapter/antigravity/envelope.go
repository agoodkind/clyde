package antigravity

import (
	"fmt"
	"runtime"
	"strings"

	"goodkind.io/clyde/internal/adapter/replay"
	"goodkind.io/clyde/internal/clock"
)

// EnvelopeMetadata supplies identity values that the Code Assist API requires.
type EnvelopeMetadata struct {
	Model          string
	Project        string
	ConversationID string
	SessionID      string
	UnixMillis     int64
	UUID           string
	Step           int
}

// NewCodeAssistEnvelope adds Code Assist identity fields to a Gemini request.
//
// It rejects non-Gemini wire models before building the request identifier.
//
//	envelope, err := NewCodeAssistEnvelope(body, metadata)
func NewCodeAssistEnvelope(
	body GenerateContentRequest,
	metadata EnvelopeMetadata,
) (CodeAssistEnvelope, error) {
	if !strings.HasPrefix(metadata.Model, "gemini-") {
		return CodeAssistEnvelope{}, invalid("Antigravity requires a Gemini model")
	}

	return CodeAssistEnvelope{
		Model:       metadata.Model,
		Project:     metadata.Project,
		UserAgent:   "antigravity",
		RequestType: "agent",
		RequestID:   newRequestID(metadata),
		Request: CodeAssistRequest{
			SessionID:              metadata.SessionID,
			GenerateContentRequest: body,
		},
	}, nil
}

func newRequestID(metadata EnvelopeMetadata) string {
	requestUUID := metadata.UUID
	if requestUUID == "" {
		requestUUID = replay.NewID()
	}

	unixMillis := metadata.UnixMillis
	if unixMillis == 0 {
		unixMillis = clock.Now().UnixMilli()
	}

	return fmt.Sprintf(
		"agent/%s/%d/%s/%d",
		metadata.ConversationID,
		unixMillis,
		requestUUID,
		metadata.Step,
	)
}

// ClientUserAgent builds the only extra HTTP header required by Code Assist.
//
// It includes the version, operating system, and architecture.
//
//	userAgent := ClientUserAgent(version)
func ClientUserAgent(version string) string {
	return fmt.Sprintf(
		"antigravity/hub/%s (aidev_client; os_type=%s; arch=%s)",
		version,
		runtime.GOOS,
		runtime.GOARCH,
	)
}
