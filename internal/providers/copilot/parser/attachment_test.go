package parser

import (
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

func TestStreamSelectedMapsAttachmentMetadataWithoutPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	body := `{"id":"1","timestamp":"2026-08-07T10:00:00Z","type":"user.message","data":{"content":"inspect","attachments":[{"mimeType":"image/png","sizeBytes":42,"description":"diagram","assetReference":"asset-1","data":"base64-payload"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	messages, err := conversation.CollectMessages(New().StreamSelected(path, "", conversation.LoadOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || len(messages[0].Attachments) != 1 {
		t.Fatalf("messages = %+v", messages)
	}
	attachment := messages[0].Attachments[0]
	if attachment.MIMEType != "image/png" || attachment.SizeBytes != 42 ||
		attachment.Description != "diagram" || attachment.AssetReference != "asset-1" ||
		attachment.Text != "" {
		t.Fatalf("attachment = %+v", attachment)
	}
}
