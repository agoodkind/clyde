package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
)

const (
	originTestUserSessionID    = "user-session-origin"
	originTestUserFilename     = "user-session.jsonl"
	originTestSubagentFilename = "agent-7f3a1c.jsonl"
)

// writeOriginFixtures lays down a real Claude project directory holding one
// ordinary transcript and one sidechain transcript, and points the process at it
// through HOME and XDG_CACHE_HOME so the index discovers and caches them exactly
// as it does in production.
func writeOriginFixtures(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}
	// The sidechain fixture sits beside the ordinary transcript rather than under
	// a subagents/ directory, so it reaches discovery and exercises the index
	// filter itself. It borrows the user session's id, matching what Claude Code
	// writes, so a record that keyed on that id would collide with the real
	// conversation.
	fixtures := map[string]string{
		originTestUserFilename:     `{"sessionId":"` + originTestUserSessionID + `","cwd":"/repo","type":"user","timestamp":"2026-07-01T10:00:00Z","content":"ship the feature"}` + "\n",
		originTestSubagentFilename: `{"sessionId":"` + originTestUserSessionID + `","cwd":"/repo","isSidechain":true,"type":"user","timestamp":"2026-07-01T10:05:00Z","content":"find the callers"}` + "\n",
	}
	for name, body := range fixtures {
		if err := os.WriteFile(filepath.Join(projectDir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// claudeOnlyRegistry wires just the Claude parser so the scan reads the fixtures
// under the temporary HOME and nothing else.
func claudeOnlyRegistry() *conversation.Registry {
	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	return registry
}

func refreshedIndexRecords(t *testing.T, includeSubagents bool) []conversation.Record {
	t.Helper()
	index := conversation.NewIndex(claudeOnlyRegistry(), config.ConversationConfig{
		IncludeSubagentConversations: includeSubagents,
		Semantic:                     config.ConversationSemanticConfig{},
	})
	ctx := context.Background()
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}
	records, err := index.List(ctx)
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	return records
}

func nativeIDs(records []conversation.Record) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.NativeID)
	}
	return out
}

func containsNativeID(records []conversation.Record, nativeID string) bool {
	for _, record := range records {
		if record.NativeID == nativeID {
			return true
		}
	}
	return false
}

// artifactFilenames names each record by the file it came from. A sidechain
// record claims no native session id, so its artifact is the only stable way to
// identify it.
func artifactFilenames(records []conversation.Record) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, filepath.Base(record.ArtifactPath))
	}
	return out
}

func containsArtifactFilename(records []conversation.Record, filename string) bool {
	for _, record := range records {
		if filepath.Base(record.ArtifactPath) == filename {
			return true
		}
	}
	return false
}

// TestIndexHidesSubagentConversationsByDefault proves the default setting keeps a
// dispatched agent's transcript out of what a caller lists, while the user's own
// conversation stays.
func TestIndexHidesSubagentConversationsByDefault(t *testing.T) {
	writeOriginFixtures(t)

	records := refreshedIndexRecords(t, false)

	if !containsArtifactFilename(records, originTestUserFilename) {
		t.Fatalf("listed artifacts = %v, want the user conversation present", artifactFilenames(records))
	}
	if containsArtifactFilename(records, originTestSubagentFilename) {
		t.Fatalf("listed artifacts = %v, want the subagent conversation absent", artifactFilenames(records))
	}
	// The user conversation keeps its own session id even though a sidechain
	// transcript borrowed it.
	if !containsNativeID(records, originTestUserSessionID) {
		t.Fatalf("listed native ids = %v, want the user session id intact", nativeIDs(records))
	}
}

// TestIndexExposesSubagentConversationsWhenEnabled proves the same corpus lists
// both conversations once the setting includes subagent conversations.
func TestIndexExposesSubagentConversationsWhenEnabled(t *testing.T) {
	writeOriginFixtures(t)

	records := refreshedIndexRecords(t, true)

	if !containsArtifactFilename(records, originTestUserFilename) {
		t.Fatalf("listed artifacts = %v, want the user conversation present", artifactFilenames(records))
	}
	if !containsArtifactFilename(records, originTestSubagentFilename) {
		t.Fatalf("listed artifacts = %v, want the subagent conversation present", artifactFilenames(records))
	}
}

// TestSkipKeepsTheParentOfASidechainTranscript is the guard against the skip
// eating the user's own work. A Claude subagent transcript records the session id
// of the conversation that dispatched it, not one of its own, so the sidechain
// marker sits in a file that names a real user conversation. The parent must stay
// present, stay user origin, and stay listed no matter how many sibling
// subagents/agent-*.jsonl files share its session id.
func TestSkipKeepsTheParentOfASidechainTranscript(t *testing.T) {
	const parentSessionID = "3f7a1c92-parent-session"

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	subagentDir := filepath.Join(projectDir, parentSessionID, "subagents")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("create subagent dir: %v", err)
	}
	parentBody := `{"sessionId":"` + parentSessionID + `","cwd":"/repo","isSidechain":false,"type":"user","timestamp":"2026-07-01T10:00:00Z","content":"the user's own work"}` + "\n"
	if err := os.WriteFile(filepath.Join(projectDir, parentSessionID+".jsonl"), []byte(parentBody), 0o600); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}
	// The sidechain siblings borrow the parent's session id, which is exactly the
	// collision this test exists to rule out.
	for _, agentFile := range []string{"agent-abc.jsonl", "agent-def.jsonl"} {
		body := `{"sessionId":"` + parentSessionID + `","cwd":"/repo","isSidechain":true,"agentId":"explorer","type":"user","timestamp":"2026-07-01T10:05:00Z","content":"dispatched work"}` + "\n"
		if err := os.WriteFile(filepath.Join(subagentDir, agentFile), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", agentFile, err)
		}
	}

	records := refreshedIndexRecords(t, false)

	if !containsNativeID(records, parentSessionID) {
		t.Fatalf("listed native ids = %v, want the parent conversation still present", nativeIDs(records))
	}
	for _, record := range records {
		if record.NativeID != parentSessionID {
			continue
		}
		if record.Origin != conversation.OriginUser {
			t.Fatalf("parent origin = %q, want %q; a sibling sidechain file must not reclassify it", record.Origin, conversation.OriginUser)
		}
		if record.IsSubagent() {
			t.Fatalf("parent IsSubagent = true, want false")
		}
	}
}

// TestSidechainTranscriptNeverDerivesTheParentConversationID pins the identity
// half of the same hazard: even reached directly, a sidechain transcript must not
// derive claude:<session-id> from the session id it borrowed from its parent.
func TestSidechainTranscriptNeverDerivesTheParentConversationID(t *testing.T) {
	t.Parallel()
	const parentSessionID = "9c2b48e1-parent-session"

	path := filepath.Join(t.TempDir(), "agent-abc.jsonl")
	body := `{"sessionId":"` + parentSessionID + `","cwd":"/repo","isSidechain":true,"type":"user","timestamp":"2026-07-01T10:05:00Z","content":"dispatched work"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write sidechain transcript: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat sidechain transcript: %v", err)
	}

	record, ok := claudeparser.New().ScanRecord(path, conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()})
	if !ok {
		t.Fatalf("ScanRecord returned ok=false")
	}
	collidingID := conversation.DerivedID(conversation.ProviderClaude, parentSessionID, path)
	if record.ID == collidingID {
		t.Fatalf("sidechain id = %q, which is the parent conversation's derived id", record.ID)
	}
	if record.NativeID == parentSessionID {
		t.Fatalf("sidechain native id = %q, which belongs to the parent conversation", record.NativeID)
	}
	if record.Origin != conversation.OriginSubagent {
		t.Fatalf("origin = %q, want %q", record.Origin, conversation.OriginSubagent)
	}
}

// writeClaudeParentAndSidechain lays down a parent transcript plus one sidechain
// transcript under the parent's subagents/ directory, borrowing the parent's
// session id the way Claude Code does, and returns both paths.
func writeClaudeParentAndSidechain(t *testing.T, parentSessionID string) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	subagentDir := filepath.Join(projectDir, parentSessionID, "subagents")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("create subagent dir: %v", err)
	}
	parentPath := filepath.Join(projectDir, parentSessionID+".jsonl")
	parentBody := `{"sessionId":"` + parentSessionID + `","cwd":"/repo","isSidechain":false,"type":"user","timestamp":"2026-07-01T10:00:00Z","content":"the user's own work"}` + "\n"
	if err := os.WriteFile(parentPath, []byte(parentBody), 0o600); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}
	agentPath := filepath.Join(subagentDir, "agent-abc.jsonl")
	agentBody := `{"sessionId":"` + parentSessionID + `","cwd":"/repo","isSidechain":true,"agentId":"explorer","type":"user","timestamp":"2026-07-01T10:05:00Z","content":"dispatched work"}` + "\n"
	if err := os.WriteFile(agentPath, []byte(agentBody), 0o600); err != nil {
		t.Fatalf("write agent transcript: %v", err)
	}
	return parentPath, agentPath
}

// TestResolveOfAParentSessionIDNeverReturnsASidechainSibling covers the opt-in
// path, where hidden records do reach resolution. Resolve matches on title as
// well as id and native id, and a sidechain transcript records its parent's
// session id, so a title that falls back to that id would let a lookup for the
// user's own conversation return the agent's transcript instead.
//
// Both fixtures carry their user text at message.content, which is where real
// Claude Code transcripts put it, so neither produces a title and both take the
// fallback. The sidechain file is stamped later so it sorts first and wins the
// walk if it matches at all.
func TestResolveOfAParentSessionIDNeverReturnsASidechainSibling(t *testing.T) {
	const parentSessionID = "8b4d2f60-parent-session"

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	subagentDir := filepath.Join(projectDir, parentSessionID, "subagents")
	if err := os.MkdirAll(subagentDir, 0o755); err != nil {
		t.Fatalf("create subagent dir: %v", err)
	}
	parentPath := filepath.Join(projectDir, parentSessionID+".jsonl")
	parentBody := `{"sessionId":"` + parentSessionID + `","cwd":"/repo","isSidechain":false,"type":"user","message":{"role":"user","content":[{"type":"text","text":"the user's own work"}]}}` + "\n"
	if err := os.WriteFile(parentPath, []byte(parentBody), 0o600); err != nil {
		t.Fatalf("write parent transcript: %v", err)
	}
	agentPath := filepath.Join(subagentDir, "agent-abc.jsonl")
	agentBody := `{"sessionId":"` + parentSessionID + `","cwd":"/repo","isSidechain":true,"agentId":"explorer","type":"user","message":{"role":"user","content":[{"type":"text","text":"dispatched work"}]}}` + "\n"
	if err := os.WriteFile(agentPath, []byte(agentBody), 0o600); err != nil {
		t.Fatalf("write agent transcript: %v", err)
	}
	parentTime := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(parentPath, parentTime, parentTime); err != nil {
		t.Fatalf("stamp parent transcript: %v", err)
	}
	agentTime := parentTime.Add(time.Hour)
	if err := os.Chtimes(agentPath, agentTime, agentTime); err != nil {
		t.Fatalf("stamp agent transcript: %v", err)
	}

	index := conversation.NewIndex(claudeOnlyRegistry(), config.ConversationConfig{
		IncludeSubagentConversations: true,
		Semantic:                     config.ConversationSemanticConfig{},
	})
	ctx := context.Background()
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}

	record, err := index.Resolve(ctx, parentSessionID)
	if err != nil {
		t.Fatalf("resolve %q: %v", parentSessionID, err)
	}
	if record.ArtifactPath != parentPath {
		t.Fatalf("resolved artifact = %q, want the user's own transcript %q", record.ArtifactPath, parentPath)
	}
	if record.IsSubagent() {
		t.Fatalf("resolved a subagent record for the user's own session id")
	}
}

// TestClaudeDiscoverAdmitsSubagentTranscriptsForClassification proves the Claude
// path skip folded into the setting: discovery now surfaces a subagents/
// transcript so ScanRecord can classify it, instead of dropping it in a second
// always-on skip the setting could not reach.
func TestClaudeDiscoverAdmitsSubagentTranscriptsForClassification(t *testing.T) {
	parentPath, agentPath := writeClaudeParentAndSidechain(t, "5d1e7b03-parent-session")

	candidates, err := claudeparser.New().Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("discover Claude transcripts: %v", err)
	}
	discovered := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		discovered[candidate.Path] = true
	}
	if !discovered[parentPath] {
		t.Fatalf("discovered %v, want the parent transcript %q", candidates, parentPath)
	}
	if !discovered[agentPath] {
		t.Fatalf("discovered %v, want the sidechain transcript %q admitted for classification", candidates, agentPath)
	}
}

// TestClaudeSubagentTranscriptFollowsTheSetting proves the fold-in end to end: the
// same sidechain transcript is hidden by default and served once the operator
// opts in, and the parent survives both ways.
func TestClaudeSubagentTranscriptFollowsTheSetting(t *testing.T) {
	const parentSessionID = "6e2f9a45-parent-session"
	_, agentPath := writeClaudeParentAndSidechain(t, parentSessionID)
	agentFilename := filepath.Base(agentPath)

	hidden := refreshedIndexRecords(t, false)
	if containsArtifactFilename(hidden, agentFilename) {
		t.Fatalf("listed artifacts = %v, want the sidechain transcript hidden by default", artifactFilenames(hidden))
	}
	if !containsNativeID(hidden, parentSessionID) {
		t.Fatalf("listed native ids = %v, want the parent conversation present", nativeIDs(hidden))
	}

	shown := refreshedIndexRecords(t, true)
	if !containsArtifactFilename(shown, agentFilename) {
		t.Fatalf("listed artifacts = %v, want the sidechain transcript served when opted in", artifactFilenames(shown))
	}
	if !containsNativeID(shown, parentSessionID) {
		t.Fatalf("listed native ids = %v, want the parent conversation still present", nativeIDs(shown))
	}
}

// TestHiddenSubagentConversationIsOmittedFromTheEngineManifest proves the only
// effect the setting has on the vector store: the feeder stops offering a hidden
// conversation. It sends a shorter manifest under the retain reconcile mode that
// clyde hardcodes, so whatever the engine already stores is left alone. Nothing in
// clyde issues a delete for a conversation, a chunk, or a store row.
func TestHiddenSubagentConversationIsOmittedFromTheEngineManifest(t *testing.T) {
	writeOriginFixtures(t)

	index := conversation.NewIndex(claudeOnlyRegistry(), config.ConversationConfig{
		IncludeSubagentConversations: false,
		Semantic:                     config.ConversationSemanticConfig{},
	})
	ctx := context.Background()
	if err := index.Refresh(ctx); err != nil {
		t.Fatalf("refresh conversation index: %v", err)
	}

	client := &fakeConversationSemanticClient{needed: nil}
	worker := newConversationSemanticSyncWorker(index, staticSemanticSyncClient(client), "collection-test", semanticTestLogger(), semanticTestContentKinds())
	if err := worker.runPass(ctx); err != nil {
		t.Fatalf("runPass returned error: %v", err)
	}

	if len(client.syncCalls) != 1 {
		t.Fatalf("manifest syncs = %d, want 1", len(client.syncCalls))
	}
	manifest := client.syncCalls[0].Manifest
	manifestIDs := make([]string, 0, len(manifest))
	for _, fingerprint := range manifest {
		manifestIDs = append(manifestIDs, fingerprint.ConversationID)
	}
	if len(manifest) != 1 {
		t.Fatalf("manifest ids = %v, want only the user conversation offered", manifestIDs)
	}
	userConversationID := conversation.DerivedID(conversation.ProviderClaude, originTestUserSessionID, "")
	if manifestIDs[0] != userConversationID {
		t.Fatalf("manifest ids = %v, want %q", manifestIDs, userConversationID)
	}
}

// TestSkippedSubagentConversationStaysInTheCache pins the behavior that makes the
// setting reversible: an index that hides subagent conversations still caches
// them with their origin, so turning the setting on later needs no cache deletion
// and no re-parse.
func TestSkippedSubagentConversationStaysInTheCache(t *testing.T) {
	writeOriginFixtures(t)

	hidden := refreshedIndexRecords(t, false)
	if containsArtifactFilename(hidden, originTestSubagentFilename) {
		t.Fatalf("listed artifacts = %v, want the subagent conversation absent", artifactFilenames(hidden))
	}

	cachePath := filepath.Join(config.GlobalCacheDir(), "conversation-index.json")
	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read conversation cache: %v", err)
	}
	var cache struct {
		Records []conversation.Record `json:"records"`
	}
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("decode conversation cache: %v", err)
	}
	if !containsArtifactFilename(cache.Records, originTestSubagentFilename) {
		t.Fatalf("cached artifacts = %v, want the hidden subagent conversation retained", artifactFilenames(cache.Records))
	}
	for _, record := range cache.Records {
		if filepath.Base(record.ArtifactPath) != originTestSubagentFilename {
			continue
		}
		if record.Origin != conversation.OriginSubagent {
			t.Fatalf("cached origin = %q, want %q", record.Origin, conversation.OriginSubagent)
		}
	}
}
