package searchbackend

import (
	"strings"
	"testing"
	"time"
)

// TestProjectFieldsReplacesNULBytes projects a message with NUL bytes in its
// text, tool name, tool display, and tool output. Every field text and document
// prefix must contain spaces in place of the NUL bytes, because the shared
// search library rejects lexical text that contains a NUL byte.
func TestProjectFieldsReplacesNULBytes(t *testing.T) {
	t.Parallel()

	projected := ProjectFields(Conversation{
		ID:                     "claude:nul",
		LoadRules:              "v1;tool_outputs",
		MessageCount:           1,
		TrailingMessageMayGrow: false,
		ArtifactSettled:        true,
		ToolDetail:             ToolDetailOutput,
	}, []Message{{
		Index:             0,
		ProviderMessageID: "entry-0",
		Role:              "assistant",
		Timestamp:         time.Unix(1710000000, 0),
		Text:              "before\x00after",
		Thinking:          "",
		Tools: []Tool{{
			Name:        "Read\x00",
			Display:     "notes\x00.md",
			DisplayLang: "",
			Output:      "line\x00two",
		}},
	}})

	if len(projected.Fields) != 3 {
		t.Fatalf("fields = %d, want chat, tool call, and tool output", len(projected.Fields))
	}
	for _, field := range projected.Fields {
		if strings.ContainsRune(field.Text, 0) || strings.ContainsRune(field.DocumentPrefix, 0) {
			t.Fatalf("field %s text %q prefix %q contains a NUL byte", field.Key, field.Text, field.DocumentPrefix)
		}
	}
	if projected.Fields[0].Text != "before after" {
		t.Fatalf("chat text = %q, want %q", projected.Fields[0].Text, "before after")
	}
	if projected.Fields[1].DocumentPrefix != "Read\n" || projected.Fields[1].Text != "notes .md" {
		t.Fatalf("tool call prefix %q text %q, want %q and %q", projected.Fields[1].DocumentPrefix, projected.Fields[1].Text, "Read\n", "notes .md")
	}
	if projected.Fields[2].Text != "line two" {
		t.Fatalf("tool output text = %q, want %q", projected.Fields[2].Text, "line two")
	}
}
