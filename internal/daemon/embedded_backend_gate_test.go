package daemon

import (
	"os"
	"testing"
	"time"
)

func ageLiveArtifactForGate(t *testing.T, path string) {
	t.Helper()
	aged := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatalf("age %s: %v", path, err)
	}
}

func embeddedGateMessages(capture *semanticLogCapture) []string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	messages := make([]string, 0, len(capture.records))
	for _, record := range capture.records {
		messages = append(messages, record.Message)
	}
	return messages
}
