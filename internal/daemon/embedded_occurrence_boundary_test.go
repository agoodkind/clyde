package daemon

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/searchbackend"
)

// embeddedOccurrenceInputLimitBytes is the largest embedding input, document
// prefix included, that PrepareText returns for a 4,096-token model without a
// tokenizer.
const embeddedOccurrenceInputLimitBytes = 3686

const (
	embeddedOccurrenceToolName     = "exec_command"
	embeddedOccurrenceCollectionID = "clyde-conversations"
)

// TestEmbeddedOccurrenceBoundary projects fields at the embedding input limit
// and one byte above it, converts them into occurrences, and validates every
// occurrence against the declared namespace. It also validates occurrences
// with a workspace_root at the declared maximum length and one byte above it.
func TestEmbeddedOccurrenceBoundary(t *testing.T) {
	spec := embeddedConversationNamespace(embeddedOccurrenceCollectionID)
	if err := spec.Validate(); err != nil {
		t.Fatalf("validate namespace declaration: %v", err)
	}
	toolPrefixBytes := len(embeddedOccurrenceToolName + "\n")
	for _, testCase := range []struct {
		name      string
		message   searchbackend.Message
		wantParts int
	}{
		{name: "chat_at_limit", message: embeddedOccurrenceChatMessage(strings.Repeat("a", embeddedOccurrenceInputLimitBytes)), wantParts: 1},
		{name: "chat_above_limit", message: embeddedOccurrenceChatMessage(strings.Repeat("a", embeddedOccurrenceInputLimitBytes+1)), wantParts: 2},
		{name: "multibyte_chat_at_limit", message: embeddedOccurrenceChatMessage(strings.Repeat("é", embeddedOccurrenceInputLimitBytes/2)), wantParts: 1},
		{name: "multibyte_chat_above_limit", message: embeddedOccurrenceChatMessage(strings.Repeat("é", embeddedOccurrenceInputLimitBytes/2+1)), wantParts: 2},
		{name: "tool_call_at_limit", message: embeddedOccurrenceToolMessage(strings.Repeat("b", embeddedOccurrenceInputLimitBytes-toolPrefixBytes)), wantParts: 1},
		{name: "tool_call_above_limit", message: embeddedOccurrenceToolMessage(strings.Repeat("b", embeddedOccurrenceInputLimitBytes-toolPrefixBytes+1)), wantParts: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			field := embeddedOccurrenceOnlyField(t, testCase.message)
			owner := embeddedOccurrenceOwner(t, "/repo")
			occurrences, err := embeddedFieldOccurrences(t.Context(), owner, field)
			if err != nil {
				t.Fatalf("build occurrences: %v", err)
			}
			assertEmbeddedOccurrences(t, spec, field, occurrences, testCase.wantParts)
		})
	}

	t.Run("workspace_root_length", func(t *testing.T) {
		field := embeddedOccurrenceOnlyField(t, embeddedOccurrenceChatMessage("hello"))
		atLimit := embeddedOccurrenceOwner(t, "/"+strings.Repeat("w", embeddedWorkspaceRootMaxBytes-1))
		occurrences, err := embeddedFieldOccurrences(t.Context(), atLimit, field)
		if err != nil {
			t.Fatalf("build occurrences with a %d-byte workspace: %v", embeddedWorkspaceRootMaxBytes, err)
		}
		assertEmbeddedOccurrences(t, spec, field, occurrences, 1)

		aboveLimit := embeddedOccurrenceOwner(t, "/"+strings.Repeat("w", embeddedWorkspaceRootMaxBytes))
		occurrences, err = embeddedFieldOccurrences(t.Context(), aboveLimit, field)
		if err != nil {
			t.Fatalf("build occurrences with a %d-byte workspace: %v", embeddedWorkspaceRootMaxBytes+1, err)
		}
		validateErr := spec.ValidateOccurrence(occurrences[0])
		if !errors.Is(validateErr, library.ErrInvalidRequest) || !strings.Contains(validateErr.Error(), embeddedScalarWorkspaceRoot) {
			t.Fatalf("validate %d-byte workspace_root error = %v, want an invalid request for %s", embeddedWorkspaceRootMaxBytes+1, validateErr, embeddedScalarWorkspaceRoot)
		}
	})
}

func embeddedOccurrenceChatMessage(text string) searchbackend.Message {
	return searchbackend.Message{
		Index:             0,
		ProviderMessageID: "message-0",
		Role:              "user",
		Timestamp:         time.Date(2026, time.September, 28, 12, 0, 0, 0, time.UTC),
		Text:              text,
		Thinking:          "",
		Tools:             nil,
	}
}

func embeddedOccurrenceToolMessage(display string) searchbackend.Message {
	message := embeddedOccurrenceChatMessage("")
	message.Role = "assistant"
	message.Tools = []searchbackend.Tool{{Name: embeddedOccurrenceToolName, Display: display, DisplayLang: "", Output: ""}}
	return message
}

// embeddedOccurrenceOnlyField projects one message under the default content
// kinds and returns its only field.
func embeddedOccurrenceOnlyField(t *testing.T, message searchbackend.Message) searchbackend.Field {
	t.Helper()
	projected := searchbackend.ProjectFields(searchbackend.Conversation{
		ID:                     "codex:occurrence-boundary",
		LoadRules:              conversation.LoadRulesTag(defaultSemanticContentKinds()),
		MessageCount:           1,
		TrailingMessageMayGrow: false,
		ArtifactSettled:        true,
		ToolDetail:             embeddedToolDetail(defaultSemanticContentKinds()),
	}, []searchbackend.Message{message})
	if len(projected.Fields) != 1 {
		t.Fatalf("projected fields = %d, want 1", len(projected.Fields))
	}
	return projected.Fields[0]
}

func embeddedOccurrenceOwner(t *testing.T, workspaceRoot string) embeddedConversationOwner {
	t.Helper()
	record := conversation.Record{ID: "codex:occurrence-boundary", Provider: conversation.ProviderCodex, WorkspaceRoot: workspaceRoot}
	return newEmbeddedConversationOwner(record, defaultSemanticContentKinds())
}

// assertEmbeddedOccurrences checks the part count, the byte limit of every
// embedding input, the prefix and span of every text, the row key suffixes,
// the sort key order, the namespace validation, and that the parts cover the
// field text in order.
func assertEmbeddedOccurrences(
	t *testing.T,
	spec library.NamespaceSpec,
	field searchbackend.Field,
	occurrences []library.Occurrence,
	wantParts int,
) {
	t.Helper()
	if len(occurrences) != wantParts {
		t.Fatalf("occurrences = %d, want %d", len(occurrences), wantParts)
	}
	var covered strings.Builder
	for partIndex, occurrence := range occurrences {
		if err := spec.ValidateOccurrence(occurrence); err != nil {
			t.Fatalf("part %d fails namespace validation: %v", partIndex, err)
		}
		if len(occurrence.EmbeddingInput) > embeddedOccurrenceInputLimitBytes {
			t.Fatalf("part %d embedding input is %d bytes, over %d", partIndex, len(occurrence.EmbeddingInput), embeddedOccurrenceInputLimitBytes)
		}
		if occurrence.EmbeddingInput != field.DocumentPrefix+occurrence.SourceText || occurrence.SearchText != occurrence.EmbeddingInput {
			t.Fatalf("part %d embedding input or search text is not the document prefix followed by the source text", partIndex)
		}
		if wantKey := field.Key + "/" + strconv.Itoa(partIndex); occurrence.RowKey != wantKey {
			t.Fatalf("part %d row key = %q, want %q", partIndex, occurrence.RowKey, wantKey)
		}
		if partIndex > 0 && occurrences[partIndex-1].SortKey >= occurrence.SortKey {
			t.Fatalf("part %d sort key %q does not follow %q", partIndex, occurrence.SortKey, occurrences[partIndex-1].SortKey)
		}
		covered.WriteString(occurrence.SourceText)
	}
	if wantParts > 1 && len(occurrences[0].EmbeddingInput) < embeddedOccurrenceInputLimitBytes-1 {
		t.Fatalf("first of %d parts is %d bytes, want the part cut at the %d-byte limit", wantParts, len(occurrences[0].EmbeddingInput), embeddedOccurrenceInputLimitBytes)
	}
	if covered.String() != field.Text {
		t.Fatalf("parts cover %d bytes of the %d-byte field text", covered.Len(), len(field.Text))
	}
}
