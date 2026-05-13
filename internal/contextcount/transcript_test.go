package contextcount

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestTranscriptZeroValue(t *testing.T) {
	var transcript Transcript

	if transcript.Model != "" {
		t.Fatalf("Model = %q, want empty string", transcript.Model)
	}
	if transcript.System != nil {
		t.Fatalf("System = %#v, want nil", transcript.System)
	}
	if transcript.Tools != nil {
		t.Fatalf("Tools = %#v, want nil", transcript.Tools)
	}
	if transcript.Messages != nil {
		t.Fatalf("Messages = %#v, want nil", transcript.Messages)
	}

	cloned := cloneTranscriptForTest(t, transcript)
	if !reflect.DeepEqual(cloned, transcript) {
		t.Fatalf("clone = %#v, want %#v", cloned, transcript)
	}
}

func TestTranscriptCloneCopiesNestedSlices(t *testing.T) {
	original := Transcript{
		Model: "claude-opus-4-1m",
		System: []TextBlock{
			{Text: "system"},
		},
		Tools: []ToolDef{
			{
				Name:        "Read",
				Description: "read a file",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
		Messages: []Message{
			{
				Role: "assistant",
				Content: []ContentBlock{
					{
						Type:      "thinking",
						Thinking:  "private chain",
						Signature: "sig",
						ToolInput: json.RawMessage(`null`),
					},
					{
						Type:        "tool_use",
						ToolUseID:   "tool-1",
						ToolName:    "Read",
						ToolInput:   json.RawMessage(`{"file_path":"README.md"}`),
						ImageSource: ImageSource{Type: "url", URL: "https://example.test"},
					},
				},
			},
			{
				Role: "user",
				Content: []ContentBlock{
					{
						Type:      "tool_result",
						ToolInput: json.RawMessage(`null`),
						ToolResult: []ContentBlock{
							{
								Type:      "text",
								Text:      "result",
								ToolInput: json.RawMessage(`null`),
							},
						},
					},
				},
			},
		},
	}

	cloned := cloneTranscriptForTest(t, original)
	if !reflect.DeepEqual(cloned, original) {
		t.Fatalf("clone = %#v, want %#v", cloned, original)
	}

	cloned.System[0].Text = "changed"
	cloned.Tools[0].Name = "Write"
	cloned.Messages[0].Role = "user"
	cloned.Messages[0].Content[0].Thinking = "changed"
	cloned.Messages[1].Content[0].ToolResult[0].Text = "changed"

	if original.System[0].Text != "system" {
		t.Fatalf("original.System[0].Text = %q, want system", original.System[0].Text)
	}
	if original.Tools[0].Name != "Read" {
		t.Fatalf("original.Tools[0].Name = %q, want Read", original.Tools[0].Name)
	}
	if original.Messages[0].Role != "assistant" {
		t.Fatalf("original.Messages[0].Role = %q, want assistant", original.Messages[0].Role)
	}
	if original.Messages[0].Content[0].Thinking != "private chain" {
		t.Fatalf(
			"original.Messages[0].Content[0].Thinking = %q, want private chain",
			original.Messages[0].Content[0].Thinking,
		)
	}
	if original.Messages[1].Content[0].ToolResult[0].Text != "result" {
		t.Fatalf(
			"original.Messages[1].Content[0].ToolResult[0].Text = %q, want result",
			original.Messages[1].Content[0].ToolResult[0].Text,
		)
	}
}

func cloneTranscriptForTest(t *testing.T, transcript Transcript) Transcript {
	t.Helper()

	data, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("json.Marshal(Transcript) error = %v", err)
	}

	var cloned Transcript
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatalf("json.Unmarshal(Transcript) error = %v", err)
	}
	return cloned
}
