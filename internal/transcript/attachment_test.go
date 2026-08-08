package transcript

import (
	"strings"
	"testing"
)

func TestAttachmentMetadataRendersWithoutPayload(t *testing.T) {
	message := Message{
		Role: "user",
		Attachments: []Attachment{{
			MIMEType: "image/png", SizeBytes: 42, Description: "diagram",
			AssetReference: "asset-1",
		}},
	}
	plain := RenderPlainTextWithOptions([]Message{message}, DefaultShapeOptions())
	if !strings.Contains(plain, "[attachment: image/png, 42 bytes, diagram, asset-1]") {
		t.Fatalf("plain attachment placeholder = %q", plain)
	}
	markdown := RenderMarkdownWithOptions([]Message{message}, DefaultShapeOptions())
	if !strings.Contains(markdown, "[attachment: image/png, 42 bytes, diagram, asset-1]") {
		t.Fatalf("markdown attachment placeholder = %q", markdown)
	}
	html := RenderHTMLConversation(ShapeConversation([]Message{message}, DefaultShapeOptions()))
	if !strings.Contains(html, "attachment: image/png, 42 bytes, diagram, asset-1") {
		t.Fatalf("HTML attachment placeholder = %q", html)
	}
	body, err := RenderJSONWithOptions([]Message{message}, DefaultShapeOptions())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "base64") {
		t.Fatalf("JSON retained payload bytes: %s", body)
	}
}
