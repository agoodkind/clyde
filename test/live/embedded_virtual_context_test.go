//go:build live && frozen_context

package live

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	_ "github.com/mattn/go-sqlite3"
	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/sandbox"
	"goodkind.io/lm-semantic-search/library/observation"
)

const virtualContextOwner = "cursor:33333333-3333-4333-8333-333333333333"

type virtualContextHit struct {
	SourceIdentity *conversation.SearchSourceIdentity `json:"source_identity"`
	Conversation   conversation.Record                `json:"conversation"`
	MessageIndex   int                                `json:"message_index"`
	Role           string                             `json:"role"`
	Timestamp      time.Time                          `json:"timestamp"`
	Snippet        string                             `json:"snippet"`
	Score          float64                            `json:"score"`
	ContextWindow  string                             `json:"context_window"`
	ContextState   conversation.SearchContextState    `json:"context_state"`
	LoadRules      string                             `json:"load_rules"`
}

type virtualContextPage struct {
	Matches []virtualContextHit `json:"matches"`
}

func TestLiveEmbeddedVirtualCursorContext(t *testing.T) {
	artifactPath := os.Getenv("CLYDE_VIRTUAL_CONTEXT_ARTIFACT")
	if !filepath.IsAbs(artifactPath) {
		t.Fatal("the guarded parent must supply an absolute completion artifact path")
	}
	// The parent owns creation and exact cleanup before this child starts.
	database := os.Getenv("CLYDE_VIRTUAL_CONTEXT_DATABASE")
	if !regexp.MustCompile(`^clyde_frozen_fixture_[0-9]+$`).MatchString(database) {
		t.Fatal("the guarded parent must supply its registered isolated database")
	}
	home := t.TempDir()
	path := filepath.Join(home, "cursor-data", "User", "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	executeVirtualContextSQL(t, db, "virtual-context-cursor.sql")
	h := newVirtualContextHarness(t)
	verifyVirtualContextArtifactPath(t, artifactPath, home, h)
	h.extraEnv = []string{"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"), "CODEX_SQLITE_HOME=" + filepath.Join(home, ".codex"), "COPILOT_HOME=" + filepath.Join(home, ".copilot"), "CLYDE_CURSOR_PROJECTS_DIRS=" + filepath.Join(home, "cursor-projects"), "CLYDE_CURSOR_DATA_DIRS=" + filepath.Join(home, "cursor-data", "User"), "CLYDE_ZED_DATA_DIRS=" + filepath.Join(home, "zed-data")}
	h.writeConversationOnlyConfig(t, nil, "")
	contents, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var configuration config.Config
	if err := toml.Unmarshal(contents, &configuration); err != nil {
		t.Fatal(err)
	}
	semantic := &configuration.Conversation.Semantic
	semantic.ProjectionProfile = config.ConversationProjectionProfileSourceSpan
	semantic.IndexedContent = []string{"chat", "tool_calls"}
	semantic.IndexedProviders = []string{"cursor"}
	semantic.IngestionEnabled, semantic.SearchEnabled = true, false
	semantic.CollectionID, semantic.PoolID = "virtual_context", "virtual_context"
	semantic.CatalogPath = filepath.Join(h.stateRoot, "catalog.sqlite")
	semantic.LockPath = filepath.Join(h.stateRoot, "catalog.lock")
	semantic.MilvusAddress, semantic.MilvusDatabase, semantic.MilvusCollection = "localhost:39630", database, "vectors"
	semantic.EmbeddingBaseURL = "http://[::1]:5400/v1"
	semantic.EmbeddingModel, semantic.EmbeddingRevision = "nvidia/NV-EmbedCode-7b-v1", "2a97ba03aee57d4c2b146fbd74ee84b3a219ddfac13e5d4ea32f373638d2d97f"
	semantic.VectorDimension, semantic.Normalization = 4096, "l2"
	writeEmbeddedLifecycleConfig(t, h, configuration)
	h.boot(t)
	t.Cleanup(func() { h.teardown(t) })
	deadline := time.Now().Add(90 * time.Second)
	for !h.logContains(`"searchable_rows":3`) {
		if time.Now().After(deadline) {
			t.Fatalf("virtual source did not publish: %s", h.dumpLogsOnFailure(t))
		}
		time.Sleep(100 * time.Millisecond)
	}
	h.teardown(t)
	ingestion := readEmbeddedOperationMeasurements(t, h.stateRoot)
	semantic.IngestionEnabled, semantic.SearchEnabled = false, true
	writeEmbeddedLifecycleConfig(t, h, configuration)
	h.boot(t)
	connection := embeddedPublicMCP(t, h)
	read := func(radius int) virtualContextPage {
		cli := virtualContextCLI(t, h, radius)
		arguments := struct {
			ConversationID string `json:"conversation_id"`
			Query          string `json:"query"`
			Limit          int    `json:"limit"`
			Window         int    `json:"window"`
		}{virtualContextOwner, "virtual context checkpoint", 10, radius}
		body, marshalErr := json.Marshal(arguments)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		page := virtualContextMCP(t, connection, body)
		if !reflect.DeepEqual(cli, page) {
			t.Fatal("CLI/MCP context, excerpts, identities or scores differ")
		}
		if len(cli.Matches) != 3 {
			t.Fatalf("public search returned %d occurrences, want 3", len(cli.Matches))
		}
		return cli
	}
	initial := read(2)
	assertVirtualContextPageReads(t, readEmbeddedOperationMeasurements(t, h.stateRoot), 2)
	expectedContext := "Message 0 (chat):\nvirtual context checkpoint question\n\nMessage 2 (chat):\nvirtual context checkpoint answer\n\nMessage 2 (tool_call):\nread_file\nREADME.md"
	for _, hit := range initial.Matches {
		if hit.ContextState != conversation.SearchContextStateAvailable || hit.ContextWindow != expectedContext {
			t.Fatalf("public virtual context did not verify selected content: %+v", hit)
		}
		if hit.SourceIdentity == nil || hit.SourceIdentity.ConversationID != virtualContextOwner {
			t.Fatal("public source identity differs")
		}
		if hit.Conversation.ID != virtualContextOwner || hit.Conversation.NativeID != "33333333-3333-4333-8333-333333333333" || !strings.HasPrefix(hit.Conversation.ArtifactPath, "cursor://") {
			t.Fatal("public accepted provenance differs")
		}
	}
	narrow := read(1)
	assertVirtualNarrowContexts(t, narrow, initial, false)
	assertVirtualContextPageReads(t, readEmbeddedOperationMeasurements(t, h.stateRoot)[len(ingestion):], 4)
	executeVirtualContextSQL(t, db, "virtual-context-cursor-edit.sql")
	assertUnavailable := func(page virtualContextPage) {
		for index, hit := range page.Matches {
			if hit.ContextState != conversation.SearchContextStateUnavailable || hit.Snippet != initial.Matches[index].Snippet || !reflect.DeepEqual(hit.SourceIdentity, initial.Matches[index].SourceIdentity) {
				t.Fatalf("changed source did not retain unavailable context and committed excerpt: %+v", hit)
			}
		}
	}
	assertUnavailable(read(2))
	assertVirtualNarrowContexts(t, read(1), narrow, true)
	assertVirtualContextPageReads(t, readEmbeddedOperationMeasurements(t, h.stateRoot)[len(ingestion):], 8)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	assertUnavailable(read(2))
	h.teardown(t)
	retainVirtualContextProof(t, artifactPath, database, ingestion, readEmbeddedOperationMeasurements(t, h.stateRoot))
}

type virtualContextProof struct {
	Complete         bool                           `json:"complete"`
	Database         string                         `json:"database"`
	PublicationRows  int                            `json:"publication_rows"`
	InitialStageRows int                            `json:"initial_stage_rows"`
	QueryRequests    int                            `json:"query_requested"`
	SearchStageCalls int                            `json:"search_stage_calls"`
	SearchUpserts    int                            `json:"search_upsert_calls"`
	SearchIngestion  int                            `json:"search_ingestion_operations"`
	Operations       []embeddedOperationMeasurement `json:"operations"`
	ContextPages     []embeddedOperationMeasurement `json:"context_pages"`
}

func retainVirtualContextProof(t *testing.T, path, database string, initial, final []embeddedOperationMeasurement) {
	t.Helper()
	if len(final) < len(initial) || !reflect.DeepEqual(initial, final[:len(initial)]) {
		t.Fatal("search-only operation log changed the initial ingestion prefix")
	}
	proof := virtualContextProof{Complete: false, Database: database, PublicationRows: 0, InitialStageRows: 0, QueryRequests: 0, SearchStageCalls: 0, SearchUpserts: 0, SearchIngestion: 0, Operations: nil, ContextPages: nil}
	for index, event := range final {
		if event.Message == "daemon.conversation_embedded_search.context_page_completed" {
			proof.ContextPages = append(proof.ContextPages, event)
		}
		if event.Message == "daemon.conversation_semantic_sync.pass_completed" && index < len(initial) && event.ProjectionRows == 3 {
			proof.PublicationRows = event.ProjectionRows
		}
		if event.Message != "daemon.conversation_semantic_embedded.operation_completed" {
			continue
		}
		if index >= len(initial) {
			if event.Purpose == observation.Ingestion {
				proof.SearchIngestion++
			}
			if event.Operation == observation.Stage {
				proof.SearchStageCalls++
			}
			if event.Operation == observation.UpsertCall {
				proof.SearchUpserts++
			}
		}
		// Startup catalog transactions have no caller run scope.
		if event.Purpose != observation.Ingestion && event.Purpose != observation.Query {
			continue
		}
		if event.OperationID == 0 || event.RunID == "" || event.PID <= 0 || event.Duration < 0 || event.Outcome != observation.Success {
			t.Fatalf("operation identity or outcome is invalid: %+v", event)
		}
		proof.Operations = append(proof.Operations, event)
		if index < len(initial) {
			if event.Operation == observation.Stage {
				proof.InitialStageRows += event.StageRows
			}
			continue
		}
		if event.Purpose == observation.Query && event.Operation == observation.EmbeddingAttempt {
			proof.QueryRequests += event.EmbeddingInputs
		}
	}
	if proof.PublicationRows != 3 || proof.InitialStageRows != 3 || proof.QueryRequests == 0 || proof.SearchStageCalls != 0 || proof.SearchUpserts != 0 || proof.SearchIngestion != 0 {
		t.Fatalf("virtual context operation proof differs: %+v", proof)
	}
	proof.Complete = true
	body, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(body, '\n')); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertVirtualContextPageReads(t *testing.T, measurements []embeddedOperationMeasurement, expectedPages int) {
	t.Helper()
	seen := make(map[string]bool)
	for _, event := range measurements {
		if event.Message != "daemon.conversation_embedded_search.context_page_completed" {
			continue
		}
		if event.RunID == "" || seen[event.RunID] || event.PID <= 0 || event.Purpose != observation.Query || event.Outcome != observation.Success {
			t.Fatalf("page context observation scope differs: %+v", event)
		}
		seen[event.RunID] = true
		if event.PageHits != 3 || event.SourceGroups != 1 || event.SourceReads != 1 || event.MessagesVisited != 3 || event.MessagesRetained != 3 || event.Windows != 3 {
			t.Fatalf("public page repeated source reads or retained messages outside the union: %+v", event)
		}
	}
	if len(seen) != expectedPages {
		t.Fatalf("actual CLI and MCP pages emitted %d context observations", len(seen))
	}
}

func assertVirtualNarrowContexts(t *testing.T, page, baseline virtualContextPage, edited bool) {
	t.Helper()
	questionCount, assistantCount := 0, 0
	for index, hit := range page.Matches {
		prior := baseline.Matches[index]
		if hit.Conversation.ID != prior.Conversation.ID || hit.Conversation.Provider != prior.Conversation.Provider || hit.Snippet != prior.Snippet || hit.Score != prior.Score || hit.MessageIndex != prior.MessageIndex || hit.Role != prior.Role || !hit.Timestamp.Equal(prior.Timestamp) || hit.LoadRules != prior.LoadRules || !reflect.DeepEqual(hit.SourceIdentity, prior.SourceIdentity) {
			t.Fatalf("public window selection changed occurrence order or committed metadata: %+v", hit)
		}
		expected := ""
		state := conversation.SearchContextStateAvailable
		switch hit.MessageIndex {
		case 0:
			questionCount++
			expected = "Message 0 (chat):\nvirtual context checkpoint question"
			if edited {
				state = conversation.SearchContextStateUnavailable
				expected = "virtual context checkpoint question"
			}
		case 2:
			assistantCount++
			expected = "Message 2 (chat):\nvirtual context checkpoint answer\n\nMessage 2 (tool_call):\nread_file\nREADME.md"
			if edited && !reflect.DeepEqual(hit, prior) {
				t.Fatalf("an edit outside the requested window changed available context: %+v", hit)
			}
		default:
			t.Fatalf("public narrow context returned unexpected message index %d", hit.MessageIndex)
		}
		if hit.ContextState != state || hit.ContextWindow != expected {
			t.Fatalf("public narrow context returned incorrect content or availability: %+v", hit)
		}
	}
	if questionCount != 1 || assistantCount != 2 {
		t.Fatalf("public narrow context returned %d question and %d assistant occurrences", questionCount, assistantCount)
	}
}

func verifyVirtualContextArtifactPath(t *testing.T, path, home string, h *harness) {
	t.Helper()
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{home, h.stateRoot, h.configRoot, h.cacheRoot, h.runtimeRoot} {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(resolved, parent)
		if err != nil {
			t.Fatal(err)
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatal("completion artifact must be outside the fixture sandbox")
		}
	}
}

func newVirtualContextHarness(t *testing.T) *harness {
	t.Helper()
	path := os.Getenv("CLYDE_VIRTUAL_CONTEXT_BINARY")
	if !filepath.IsAbs(path) {
		t.Fatal("the parent must supply an absolute retained CLI binary")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("the retained CLI is not executable: %v", err)
	}
	roots, err := sandbox.NewRoots()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(roots.Base); err != nil {
			t.Error(err)
		}
	})
	h := &harness{
		stateRoot: roots.State, configRoot: roots.Config, cacheRoot: roots.Cache, runtimeRoot: roots.Runtime,
		cfg: resolveFakePorts(), conversationSemantic: resolveFakeConversationSemanticConfig(t),
		extraEnv: nil, binPath: path, prodPidsPre: snapshotProductionPids(), cmd: nil,
	}
	h.configPath = filepath.Join(h.configRoot, "clyde", "config.toml")
	h.daemonLog = filepath.Join(h.stateRoot, "daemon-run.out")
	if err := os.MkdirAll(filepath.Dir(h.configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	return h
}

func virtualContextCLI(t *testing.T, h *harness, radius int) virtualContextPage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, h.binPath, "conversation", "search", virtualContextOwner, "--query", "virtual context checkpoint", "--limit", "10", "--window", strconv.Itoa(radius), "--output-format", "json")
	command.Env = h.env()
	var stderr strings.Builder
	command.Stderr = &stderr
	body, err := command.Output()
	if err != nil {
		t.Fatalf("public CLI failed: %v: %s", err, stderr.String())
	}
	var page virtualContextPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func virtualContextMCP(t *testing.T, connection *client.Client, body []byte) virtualContextPage {
	t.Helper()
	var request mcp.CallToolRequest
	if err := json.Unmarshal([]byte(`{"params":{"name":"clyde_search","arguments":`+string(body)+`}}`), &request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := connection.CallTool(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("public MCP failed: %+v", result.Content)
	}
	var page virtualContextPage
	if err := json.Unmarshal(result.RawStructuredContent, &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func executeVirtualContextSQL(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), string(content)); err != nil {
		t.Fatal(err)
	}
}
