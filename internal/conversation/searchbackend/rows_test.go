package searchbackend

import (
	"testing"
	"time"
)

// TestProjectFieldsPreservesNULSourceText checks original field text before
// search and embedding preparation.
func TestProjectFieldsPreservesNULSourceText(t *testing.T) {
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
	if projected.Fields[0].Text != "before\x00after" {
		t.Fatalf("chat source = %q, want original NUL", projected.Fields[0].Text)
	}
	if projected.Fields[1].DocumentPrefix != "Read\x00\n" || projected.Fields[1].Text != "notes\x00.md" {
		t.Fatalf("tool source prefix/text = %q/%q, want original NUL", projected.Fields[1].DocumentPrefix, projected.Fields[1].Text)
	}
	if projected.Fields[2].Text != "line\x00two" {
		t.Fatalf("tool output source = %q, want original NUL", projected.Fields[2].Text)
	}
}
