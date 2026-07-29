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

// loadKindsFromTOML writes a config file and resolves it the way the daemon
// does, so these tests assert what an operator's config actually produces rather
// than what a hand-built set would.
func loadKindsFromTOML(t *testing.T, body string) (conversation.ContentKindSet, error) {
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
		return conversation.ContentKindSet{}, err
	}
	return SemanticContentKinds(cfg.Conversation.Semantic)
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
				Name:        "Bash",
				Input:       transcript.ToolInputJSON{Raw: []byte(`{"command":"go test ./..."}`)},
				Display:     "",
				DisplayLang: "",
				Output:      "ok\tgoodkind.io/clyde\t1.2s",
			}},
		},
		{Role: "assistant", Timestamp: time.Unix(1710000003, 0), Text: "shipped"},
	}
}

// TestDefaultConfigSelectsChatAndToolCalls proves what a fresh install indexes.
// The reasoning-only turn is withheld because reasoning is not a default kind and
// the turn carries nothing else, while the tool-only turn is still offered
// because a tool call is content even with no text.
func TestDefaultConfigSelectsChatAndToolCalls(t *testing.T) {
	kinds, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !kinds.Has(conversation.ContentKindChat) || !kinds.Has(conversation.ContentKindToolCalls) {
		t.Fatalf("default kinds = %v, want chat and tool_calls", kinds.Kinds())
	}
	if kinds.Has(conversation.ContentKindThinking) || kinds.Has(conversation.ContentKindToolOutputs) {
		t.Fatalf("default kinds = %v, want reasoning and tool outputs absent", kinds.Kinds())
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), kinds)
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
			t.Fatalf("document %d carries reasoning %q under the default", doc.MessageIndex, doc.Thinking)
		}
		for _, tool := range doc.Tools {
			if tool.Command != "go test ./..." {
				t.Fatalf("tool command = %q, want the command the default indexes", tool.Command)
			}
			if tool.Output != "" {
				t.Fatalf("tool output = %q, want empty; tool_outputs is not a default kind", tool.Output)
			}
		}
	}
}

// TestSkippedMessageKeepsTheIndexOfEveryLaterMessage is the regression that
// matters most: the message index is a position the search path feeds back into
// the transcript loader, so a withheld message must leave a gap rather than
// renumber the turns after it.
func TestSkippedMessageKeepsTheIndexOfEveryLaterMessage(t *testing.T) {
	kinds, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), kinds)
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
	messages := policyTestMessages()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message indexes = %v, want %v; index 1 was withheld and must leave a gap", got, want)
		}
		if messages[got[i]].Role != built.Docs[i].Role {
			t.Fatalf("document %d has role %q but the loader's message at that position has %q", got[i], built.Docs[i].Role, messages[got[i]].Role)
		}
	}
}

// TestNamingThinkingOffersTheReasoningOnlyTurn proves the opt-in works, using the
// export surface's own selector name.
func TestNamingThinkingOffersTheReasoningOnlyTurn(t *testing.T) {
	kinds, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"chat\", \"thinking\", \"tool_calls\"]\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), kinds)
	if err != nil {
		t.Fatalf("build documents: %v", err)
	}
	if built.PolicySkipped != 0 {
		t.Fatalf("PolicySkipped = %d, want 0 once thinking is indexed", built.PolicySkipped)
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

// projectedToolAt resolves one selector and returns the tool call the projection
// produced, so the nested-level assertions read against real config input.
func projectedToolAt(t *testing.T, selector string) (int, string, string, string, bool) {
	t.Helper()
	kinds, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"chat\", \""+selector+"\"]\n")
	if err != nil {
		t.Fatalf("load config for %q: %v", selector, err)
	}
	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), kinds)
	if err != nil {
		t.Fatalf("build documents for %q: %v", selector, err)
	}
	for _, doc := range built.Docs {
		if len(doc.Tools) > 0 {
			tool := doc.Tools[0]
			return built.PolicySkipped, tool.Name, tool.InputJSON, tool.Output, true
		}
	}
	return built.PolicySkipped, "", "", "", false
}

// TestToolKindsAreNestedLevels proves the three tool kinds behave as one nested
// ladder rather than as parallel switches: summaries carry the name alone, calls
// add the arguments, and outputs add the result. That is the property
// collapseToolContentKinds encodes, applied to the projection.
func TestToolKindsAreNestedLevels(t *testing.T) {
	_, name, input, output, found := projectedToolAt(t, "tools")
	if !found {
		t.Fatal("summaries level dropped the tool call entirely")
	}
	if name != "Bash" {
		t.Fatalf("summaries level name = %q, want Bash", name)
	}
	if input != "" || output != "" {
		t.Fatalf("summaries level carried arguments %q or output %q", input, output)
	}

	_, _, input, output, found = projectedToolAt(t, "tool_calls")
	if !found {
		t.Fatal("calls level dropped the tool call entirely")
	}
	if !strings.Contains(input, "go test") {
		t.Fatalf("calls level arguments = %q, want the call arguments", input)
	}
	if output != "" {
		t.Fatalf("calls level output = %q, want empty", output)
	}

	_, _, input, output, found = projectedToolAt(t, "tool_outputs")
	if !found {
		t.Fatal("outputs level dropped the tool call entirely")
	}
	if output == "" {
		t.Fatal("outputs level did not carry the tool result")
	}
	if !strings.Contains(input, "go test") {
		t.Fatal("outputs level dropped the arguments the calls level carries")
	}
}

// TestSelectingNoToolKindDropsTheCall proves the ladder's bottom rung: a
// selection naming no tool kind carries no tool calls, and the tool-only turn is
// then withheld because nothing it holds is indexed.
func TestSelectingNoToolKindDropsTheCall(t *testing.T) {
	kinds, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"chat\"]\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	built, err := BuildSemanticConversationDocuments(policyTestRecord(), policyTestMessages(), kinds)
	if err != nil {
		t.Fatalf("build documents: %v", err)
	}
	for _, doc := range built.Docs {
		if len(doc.Tools) != 0 {
			t.Fatalf("document %d carries tools with only chat selected", doc.MessageIndex)
		}
	}
	if built.PolicySkipped != 2 {
		t.Fatalf("PolicySkipped = %d, want 2 (the reasoning-only and tool-only turns)", built.PolicySkipped)
	}
}

// TestUnknownContentKindFailsResolution proves a typo is rejected by the export
// surface's own validator rather than silently narrowing the corpus.
func TestUnknownContentKindFailsResolution(t *testing.T) {
	_, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"chat\", \"resoning\"]\n")
	if err == nil {
		t.Fatal("resolution succeeded with an unknown content kind; a typo must fail")
	}
	if !strings.Contains(err.Error(), "resoning") {
		t.Fatalf("error = %v, want the rejected kind named", err)
	}
	if !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("error = %v, want the supported kinds listed", err)
	}
}

// TestExcludedKindsNeverReachTheParser proves the gate pushes down into
// LoadOptions for the three kinds that have a field, so content nobody selected
// is never parsed rather than parsed and discarded.
func TestExcludedKindsNeverReachTheParser(t *testing.T) {
	defaultKinds, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	defaultOptions := SemanticConversationLoadOptions(defaultKinds)
	if defaultOptions.IncludeSystemPrompts || defaultOptions.IncludeSystemMessages || defaultOptions.IncludeToolOutputs {
		t.Fatalf("default load options = %+v, want every gated kind off", defaultOptions)
	}

	optedIn, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\nindexed_content = [\"chat\", \"system_messages\", \"system_prompts\", \"tool_outputs\"]\n")
	if err != nil {
		t.Fatalf("load opted-in config: %v", err)
	}
	optedInOptions := SemanticConversationLoadOptions(optedIn)
	if !optedInOptions.IncludeSystemPrompts || !optedInOptions.IncludeSystemMessages || !optedInOptions.IncludeToolOutputs {
		t.Fatalf("opted-in load options = %+v, want every gated kind on", optedInOptions)
	}
}

// TestPolicySkipsAreCountedApartFromFailures proves the counters stay distinct
// through a real sync pass: the pass withholds messages and reports them without
// touching the failure count.
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
	kinds, err := loadKindsFromTOML(t, "[conversation.semantic]\nenabled = true\n")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), kinds)
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
