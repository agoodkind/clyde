package transcript

import (
	"strings"
	"testing"
)

func TestAttachmentMetadataRendersWithoutPayload(t *testing.T) {
	tests := []struct {
		name       string
		attachment Attachment
		want       string
	}{
		{
			name: "directory",
			attachment: Attachment{
				Kind: "directory", DisplayName: "source", Path: "/repo/source",
			},
			want: "[attachment: directory, source, /repo/source]",
		},
		{
			name: "file",
			attachment: Attachment{
				Kind: "file", DisplayName: "notes.txt", Path: "/tmp/notes.txt",
				MIMEType: "text/plain", SizeBytes: 5,
			},
			want: "[attachment: file, notes.txt, /tmp/notes.txt, text/plain, 5 bytes]",
		},
		{
			name: "reference",
			attachment: Attachment{
				Kind: "reference", DisplayName: "issue", URL: "https://example.com/1",
				OmittedReason: "remote content",
			},
			want: "[attachment: reference, issue, https://example.com/1, remote content]",
		},
		{
			name: "binary metadata",
			attachment: Attachment{
				MIMEType: "image/png", SizeBytes: 42, Description: "diagram",
				AssetReference: "asset-1",
			},
			want: "[attachment: image/png, 42 bytes, diagram, asset-1]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := Message{Role: "user", Attachments: []Attachment{test.attachment}}
			plain := RenderPlainTextWithOptions([]Message{message}, DefaultShapeOptions())
			if !strings.Contains(plain, test.want) {
				t.Fatalf("plain attachment placeholder = %q, want %q", plain, test.want)
			}
			markdown := RenderMarkdownWithOptions([]Message{message}, DefaultShapeOptions())
			if !strings.Contains(markdown, test.want) {
				t.Fatalf("markdown attachment placeholder = %q, want %q", markdown, test.want)
			}
			html := RenderHTMLConversation(ShapeConversation([]Message{message}, DefaultShapeOptions()))
			wantHTML := strings.TrimSuffix(strings.TrimPrefix(test.want, "["), "]")
			if !strings.Contains(html, wantHTML) {
				t.Fatalf("HTML attachment placeholder = %q, want %q", html, wantHTML)
			}
			body, err := RenderJSONWithOptions([]Message{message}, DefaultShapeOptions())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "base64") {
				t.Fatalf("JSON retained payload bytes: %s", body)
			}
		})
	}
}
