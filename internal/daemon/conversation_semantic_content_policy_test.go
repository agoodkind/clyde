package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

// loadPolicyFromTOML writes a config file and loads it the way the daemon does,
// so these tests assert what an operator's config actually produces rather than
// what a hand-built policy struct would.
func loadPolicyFromTOML(t *testing.T, body string) (SemanticContentPolicy, error) {
	t.Helper()
	configRoot := t.TempDir()
	configDir := filepath.Join(configRoot, "clyde")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	cfg, err := config.LoadGlobalOrDefault()
	if err != nil {
		return SemanticContentPolicy{}, err
	}
	return SemanticContentPolicyFromConfig(cfg.Conversation.Semantic), nil
}

func policyTestRecord() conversation.Record {
	return conversation.Record{
		ID:            "claude:policy",
		Provider:      conversation.ProviderClaude,
		NativeID:      "policy",
		Lineage:       nil,
		Origin:        conversation.OriginUser,
		Title:         "policy",
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/clyde-policy-test.jsonl",
		ArtifactKind:  "jsonl",
		Model:         "",
		CreatedAt:     time.Time{},
		UpdatedAt:     time.Time{},
		SizeBytes:     0,
		Archived:      false,
	}
}

func policyTestMessages() []transcript.Message {
	return []transcript.Message{
		{Role: "user", Timestamp: time.Unix(1710000000, 0), Text: "how do I ship this"},
		{
			Role:      "assistant",
			Timestamp: time.Unix(1710000001, 0),
			Text:      "",
			Thinking:  "weighing the two approaches",
		},
		{
			Role:      "assistant",
			Timestamp: time.Unix(1710000002, 0),
			Text:      "",
			HasTools:  true,
			Tools: []transcript.ToolCall{{
				Name:   "Bash",
				Input:  transcript.ToolInputJSON{Raw: []byte(`{"command":"go test ./..."}`)},
				Output: "ok\tgoodkind.io/clyde\t1.2s",
			}},
		},
		{Role: "assistant", Timestamp: time.Unix(1710000003, 0), Text: "shipped"},
	}
}

// TestDefaultConfigWithholdsReasoningOnly proves what a fresh install indexes:
// the reasoning-only turn is withheld because reasoning is not a default class
// and the turn carries nothing else, while the tool-only turn is still offered
// because a tool call is content even with no text.
func TestDefaultConfigWithholdsReasoningOnly(t *testing.T) {
	policy, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), policy)
	if err != nil {
		t.Fatalf("build documents: %v", err)
	}

	if built.PolicySkipped != 1 {
		t.Fatalf("PolicySkipped = %d, want 1 (the reasoning-only turn)", built.PolicySkipped)
	}
	if len(built.Docs) != 3 {
		t.Fatalf("documents = %d, want 3", len(built.Docs))
	}
	for _, doc := range built.Docs {
		if doc.Thinking != "" {
			t.Fatalf("document at index %d carries reasoning %q under the default policy", doc.MessageIndex, doc.Thinking)
		}
	}
	toolDocs := 0
	for _, doc := range built.Docs {
		if len(doc.Tools) > 0 {
			toolDocs++
			if doc.Tools[0].Output == "" {
				t.Fatalf("tool output missing from document %d; the default policy indexes it", doc.MessageIndex)
			}
		}
	}
	if toolDocs != 1 {
		t.Fatalf("documents carrying tools = %d, want 1", toolDocs)
	}
}

// TestSkippedMessageKeepsTheIndexOfEveryLaterMessage is the regression that
// matters most: the message index is a position the search path feeds back into
// the transcript loader, so a withheld message must leave a gap rather than
// renumber the turns after it.
func TestSkippedMessageKeepsTheIndexOfEveryLaterMessage(t *testing.T) {
	policy, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), policy)
	if err != nil {
		t.Fatalf("build documents: %v", err)
	}

	got := make([]int32, 0, len(built.Docs))
	for _, doc := range built.Docs {
		got = append(got, doc.MessageIndex)
	}
	want := []int32{0, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("message indexes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message indexes = %v, want %v; index 1 was withheld and must leave a gap", got, want)
		}
	}
	messages := policyTestMessages()
	for _, doc := range built.Docs {
		if messages[doc.MessageIndex].Role != doc.Role {
			t.Fatalf("document %d has role %q but the loader's message at that position has %q", doc.MessageIndex, doc.Role, messages[doc.MessageIndex].Role)
		}
	}
}

// TestNamingReasoningOffersTheReasoningOnlyTurn proves the opt-in works: naming
// reasoning makes the previously withheld turn deliverable, carrying its
// reasoning text.
func TestNamingReasoningOffersTheReasoningOnlyTurn(t *testing.T) {
	policy, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"text\", \"reasoning\", \"tool_call\", \"tool_input\", \"tool_output\"]\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), policy)
	if err != nil {
		t.Fatalf("build documents: %v", err)
	}

	if built.PolicySkipped != 0 {
		t.Fatalf("PolicySkipped = %d, want 0 once reasoning is indexed", built.PolicySkipped)
	}
	if len(built.Docs) != 4 {
		t.Fatalf("documents = %d, want 4", len(built.Docs))
	}
	reasoning := ""
	for _, doc := range built.Docs {
		if doc.MessageIndex == 1 {
			reasoning = doc.Thinking
		}
	}
	if reasoning != "weighing the two approaches" {
		t.Fatalf("reasoning at index 1 = %q, want the turn's reasoning text", reasoning)
	}
}

// TestWithholdingToolOutputKeepsTheCallAndDropsTheResult proves a finer class
// empties its own field without discarding the call it belongs to, so the tool
// stays searchable by name and arguments.
func TestWithholdingToolOutputKeepsTheCallAndDropsTheResult(t *testing.T) {
	policy, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"text\", \"tool_call\", \"tool_input\"]\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), policy)
	if err != nil {
		t.Fatalf("build documents: %v", err)
	}

	found := false
	for _, doc := range built.Docs {
		if len(doc.Tools) == 0 {
			continue
		}
		found = true
		if doc.Tools[0].Name != "Bash" {
			t.Fatalf("tool name = %q, want Bash", doc.Tools[0].Name)
		}
		if doc.Tools[0].Output != "" {
			t.Fatalf("tool output = %q, want empty when tool_output is not indexed", doc.Tools[0].Output)
		}
		if !strings.Contains(doc.Tools[0].InputJSON, "go test") {
			t.Fatalf("tool input = %q, want the call arguments", doc.Tools[0].InputJSON)
		}
	}
	if !found {
		t.Fatal("no document carried the tool call; withholding tool_output must not drop the call")
	}
}

// TestWithholdingToolCallDropsTheWholeCall proves the coarse class removes the
// call outright, because the engine derives the call's own row from the call
// being present at all.
func TestWithholdingToolCallDropsTheWholeCall(t *testing.T) {
	policy, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"text\"]\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), policy)
	if err != nil {
		t.Fatalf("build documents: %v", err)
	}

	for _, doc := range built.Docs {
		if len(doc.Tools) != 0 {
			t.Fatalf("document %d still carries tools with only text indexed", doc.MessageIndex)
		}
	}
	// The reasoning-only turn and the tool-only turn both go, leaving the two
	// turns that carry text.
	if built.PolicySkipped != 2 {
		t.Fatalf("PolicySkipped = %d, want 2", built.PolicySkipped)
	}
	if len(built.Docs) != 2 {
		t.Fatalf("documents = %d, want 2", len(built.Docs))
	}
}

// TestUnknownContentClassFailsTheLoad proves a typo is rejected rather than
// silently narrowing the corpus, which is how an operator would discover they
// had stopped indexing something.
func TestUnknownContentClassFailsTheLoad(t *testing.T) {
	_, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"text\", \"resoning\"]\n")
	if err == nil {
		t.Fatal("load succeeded with an unknown content class; a typo must fail the load")
	}
	if !strings.Contains(err.Error(), "resoning") {
		t.Fatalf("error = %v, want the rejected class named", err)
	}
	if !strings.Contains(err.Error(), "reasoning") {
		t.Fatalf("error = %v, want the supported classes listed", err)
	}
}

// TestSystemMessagesAreOptInThroughTheLoader proves the system class reaches the
// transcript loader rather than being filtered after the fact, so a class the
// operator did not name is never parsed.
func TestSystemMessagesAreOptInThroughTheLoader(t *testing.T) {
	defaultPolicy, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if SemanticConversationLoadOptions(defaultPolicy).IncludeSystemMessages {
		t.Fatal("default load options include system messages; they are opt-in")
	}

	optedIn, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"text\", \"system_messages\"]\n")
	if err != nil {
		t.Fatalf("load opted-in config: %v", err)
	}
	if !SemanticConversationLoadOptions(optedIn).IncludeSystemMessages {
		t.Fatal("naming system_messages did not reach the transcript loader")
	}
}

// TestPolicySkipsAreCountedApartFromFailures proves the counters stay distinct
// through a real sync pass: the pass withholds messages and reports them without
// touching the failure count, so deliberate policy can never fill the counter
// that means content was lost.
func TestPolicySkipsAreCountedApartFromFailures(t *testing.T) {
	conversationID := "claude:policy-counts"
	index := &fakeConversationSemanticIndex{
		records: []conversation.StampedRecord{{
			Record: policyCountsRecord(conversationID),
			Stamp:  semanticTestStamp(64, 200),
		}},
		messagesByID: map[string][]transcript.Message{conversationID: policyTestMessages()},
		loadOptions:  nil,
	}
	client := &fakeConversationSemanticClient{needed: []string{conversationID}}
	policy, err := loadPolicyFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), policy)
	freshness := newConversationSemanticFreshness()
	worker.freshness = freshness

	if err := worker.runPass(context.Background()); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.upsertCalls) != 1 {
		t.Fatalf("upsert calls = %d, want 1", len(client.upsertCalls))
	}
	if len(client.upsertCalls[0].Docs) != 3 {
		t.Fatalf("delivered documents = %d, want 3 with the reasoning-only turn withheld", len(client.upsertCalls[0].Docs))
	}
	if snapshot := freshness.snapshot(); snapshot.Manifest != 1 {
		t.Fatalf("freshness manifest = %d, want 1", snapshot.Manifest)
	}
}

func policyCountsRecord(conversationID string) conversation.Record {
	record := policyTestRecord()
	record.ID = conversationID
	record.NativeID = conversationID
	return record
}
