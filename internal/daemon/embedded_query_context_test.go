//go:build live

package daemon

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/conversation"
)

// TestEmbeddedQueryContextBoundary ingests a real provider artifact and checks
// the source's verified context after append, incompatible edit, and loss.
func TestEmbeddedQueryContextBoundary(t *testing.T) {
	stores := isolateEmbeddedProjectionStores(t)
	artifact := writeEmbeddedProjectionCodexRollout(t, stores)
	ageLiveArtifact(t, artifact)
	index := newEmbeddedProjectionIndex()
	refreshLiveIndex(t, index)
	store, semantic := openEmbeddedQueryTestStore(t)
	if err := store.outbox.releaseLock(); err != nil {
		t.Fatal(err)
	}
	worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
	worker.embedded = newEmbeddedConversationSync(semantic, store.outbox.path, newEmbeddedSemanticStatus(), index)
	worker.embedded.store = store
	if err := worker.runPass(t.Context()); err != nil {
		t.Fatalf("ingest isolated context artifact: %v", err)
	}
	source := &embeddedConversationSearchSource{library: store.library, semantic: semantic, gate: nil, index: index, outbox: store.outbox}
	options := conversation.SearchConversationsOptions{Query: "run the test suite", Roles: []string{"user"}, Limit: 10, ContextWindow: 1}
	first, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(first.Matches) != 1 || first.Matches[0].ContextState != conversation.SearchContextStateAvailable {
		t.Fatalf("initial context page = %+v, %v, want verified message zero", first, err)
	}
	if strings.Contains(first.Matches[0].ContextWindow, embeddedProjectionCodexThinking) || strings.Contains(first.Matches[0].ContextWindow, embeddedProjectionCodexToolOutput) {
		t.Fatal("context rendered excluded content")
	}
	original, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	removed := strings.Replace(string(original), "The suite passed.", "", 1)
	if err := os.WriteFile(artifact, []byte(removed), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	options.ContextWindow = 4
	missingNeighbor, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(missingNeighbor.Matches) != 1 || missingNeighbor.Matches[0].ContextState != conversation.SearchContextStateUnavailable {
		t.Fatalf("removed committed neighbor context = %+v, %v, want unavailable", missingNeighbor, err)
	}
	if err := os.WriteFile(artifact, original, 0o600); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	options.ContextWindow = 1
	options.Query, options.Roles = "The suite passed.", []string{"assistant"}
	assistant, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(assistant.Matches) != 2 {
		t.Fatalf("assistant page = %+v, %v, want both eligible assistant occurrences", assistant, err)
	}
	assistantMatch := embeddedContextMatchAt(t, assistant, 3)
	if assistantMatch.ContextState != conversation.SearchContextStateAvailable || !strings.Contains(assistantMatch.ContextWindow, "Message 2") {
		t.Fatalf("nonzero window context = %+v, %v, want verified absolute message positions", assistant, err)
	}
	options.Query, options.Roles = "run the test suite", []string{"user"}
	appendEmbeddedProjectionLines(t, artifact, embeddedProjectionCodexAppendedLines)
	refreshLiveIndex(t, index)
	appended, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(appended.Matches) != 1 || appended.Matches[0].ContextState != conversation.SearchContextStateAvailable || appended.Matches[0].ContextWindow != first.Matches[0].ContextWindow {
		t.Fatalf("context after append = %+v, %v, want unchanged verified earlier window", appended, err)
	}
	options.Query, options.Roles = "The suite passed.", []string{"assistant"}
	beforeEdit, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(beforeEdit.Matches) != 2 {
		t.Fatalf("assistant context query = %+v, %v", beforeEdit, err)
	}
	// The appended, uncommitted neighbor makes the larger window unavailable.
	beforeEditMatch := embeddedContextMatchAt(t, beforeEdit, 3)
	if beforeEditMatch.ContextState != conversation.SearchContextStateUnavailable {
		t.Fatalf("uncommitted neighbor context = %s, want unavailable", beforeEditMatch.ContextState)
	}
	contents, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(contents), "run the test suite", "run a different test suite", 1)
	if err := os.WriteFile(artifact, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	options.Query, options.Roles = "run the test suite", []string{"user"}
	edited, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(edited.Matches) != 1 || edited.Matches[0].ContextState != conversation.SearchContextStateUnavailable || edited.Matches[0].Snippet != first.Matches[0].Snippet {
		t.Fatalf("context after incompatible edit = %+v, %v, want stored excerpt and unavailable context", edited, err)
	}
	if err := os.Remove(artifact); err != nil {
		t.Fatal(err)
	}
	refreshLiveIndex(t, index)
	missing, err := source.SearchConversations(t.Context(), options)
	if err != nil || len(missing.Matches) != 1 || missing.Matches[0].ContextState != conversation.SearchContextStateUnavailable || missing.Matches[0].Snippet != first.Matches[0].Snippet {
		t.Fatalf("context after artifact loss = %+v, %v, want retained stored excerpt", missing, err)
	}
}

func TestEmbeddedOriginalSourceIdentity(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		oldText      string
		newText      string
		messageIndex int
		role         string
	}{
		{name: "NUL_to_space", oldText: "run\\u0000the test suite", newText: "run the test suite", messageIndex: 0, role: "user"},
		{name: "normalized_tool_display", oldText: "cd /repo && make\\\\u0000test", newText: embeddedProjectionShellCommand, messageIndex: 2, role: "assistant"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			stores := isolateEmbeddedProjectionStores(t)
			artifact := writeEmbeddedProjectionCodexRollout(t, stores)
			contents, err := os.ReadFile(artifact)
			if err != nil {
				t.Fatal(err)
			}
			original := strings.Replace(string(contents), testCase.newText, testCase.oldText, 1)
			if err := os.WriteFile(artifact, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			ageLiveArtifact(t, artifact)
			index := newEmbeddedProjectionIndex()
			refreshLiveIndex(t, index)
			store, semantic := openEmbeddedQueryTestStore(t)
			if err := store.outbox.releaseLock(); err != nil {
				t.Fatal(err)
			}
			worker := newConversationSemanticSyncWorker(index, nil, semantic.CollectionID, slog.Default(), defaultSemanticContentKinds())
			worker.embedded = newEmbeddedConversationSync(semantic, store.outbox.path, newEmbeddedSemanticStatus(), index)
			worker.embedded.store = store
			if err := worker.runPass(t.Context()); err != nil {
				t.Fatal(err)
			}
			source := &embeddedConversationSearchSource{library: store.library, semantic: semantic, index: index, outbox: store.outbox}
			options := conversation.SearchConversationsOptions{Query: testCase.newText, Roles: []string{testCase.role}, Limit: 10, ContextWindow: 1}
			first, err := source.SearchConversations(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			match := embeddedContextMatchAt(t, first, testCase.messageIndex)
			if match.ContextState != conversation.SearchContextStateAvailable {
				t.Fatalf("original source context = %s", match.ContextState)
			}
			filter, err := embeddedConversationFilter(semantic, options)
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.library.Search(t.Context(), library.SearchRequest{Namespace: semantic.CollectionID, Query: testCase.newText, Filter: filter, PageSize: 10})
			if err != nil {
				t.Fatal(err)
			}
			wantSource := strings.ReplaceAll(testCase.oldText, "\\\\u0000", "\x00")
			wantSource = strings.ReplaceAll(wantSource, "\\u0000", "\x00")
			found := false
			for _, hit := range page.Hits {
				if hit.Scalars[embeddedScalarMessageIndex].Int64 == int64(testCase.messageIndex) {
					if hit.SourceText != wantSource {
						t.Fatalf("stored source = %q, want %q", hit.SourceText, wantSource)
					}
					found = true
				}
			}
			if !found {
				t.Fatal("original source occurrence is absent")
			}
			changed := strings.Replace(original, testCase.oldText, testCase.newText, 1)
			if err := os.WriteFile(artifact, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			ageLiveArtifact(t, artifact)
			refreshLiveIndex(t, index)
			if err := worker.runPass(t.Context()); err != nil {
				t.Fatal(err)
			}
			second, err := source.SearchConversations(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			edited := embeddedContextMatchAt(t, second, testCase.messageIndex)
			if edited.ContextState != conversation.SearchContextStateUnavailable || edited.Snippet != match.Snippet {
				t.Fatalf("edited source = %+v, want stored excerpt and unavailable context", edited)
			}
		})
	}
}

func embeddedContextMatchAt(t *testing.T, result conversation.SearchConversationsResult, index int) conversation.SearchMatch {
	t.Helper()
	for _, match := range result.Matches {
		if match.MessageIndex == index {
			return match
		}
	}
	t.Fatalf("no match for message %d in %+v", index, result)
	return conversation.SearchMatch{}
}
