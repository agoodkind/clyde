//go:build live

package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

const (
	embeddedPublicSession      = "aaaaaaaa-bbbb-4ccc-8ddd-123456789abc"
	embeddedPublicConversation = "claude:" + embeddedPublicSession
	embeddedPublicChatRows     = 32
	embeddedPublicRows         = 38
	embeddedPublicQuery        = "public embedded search checkpoint"
)

type embeddedPublicHit struct {
	SourceIdentity *conversation.SearchSourceIdentity `json:"source_identity"`
	Conversation   struct {
		ID string `json:"id"`
	} `json:"conversation"`
	MessageIndex  int                             `json:"message_index"`
	Snippet       string                          `json:"snippet"`
	LoadRules     string                          `json:"load_rules"`
	Score         float64                         `json:"score"`
	ContextWindow string                          `json:"context_window"`
	ContextState  conversation.SearchContextState `json:"context_state"`
}

type embeddedPublicPage struct {
	Matches       []embeddedPublicHit `json:"matches"`
	HasMore       bool                `json:"has_more"`
	NextCursor    string              `json:"next_cursor"`
	ReturnedCount int                 `json:"returned_count"`
}

func writeEmbeddedPublicCorpus(t *testing.T) (string, string, map[string]string) {
	t.Helper()
	home := t.TempDir()
	directory := filepath.Join(home, ".claude", "projects", "-tmp-embedded-public")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 0, embeddedPublicRows)
	expected := make(map[string]string, embeddedPublicRows)
	for index := range embeddedPublicChatRows {
		text := fmt.Sprintf("%s source %03d", embeddedPublicQuery, index)
		expected[embeddedPublicIdentity(index, "chat", -1, 0, len(text))] = conversation.Snippet(text)
		lines = append(lines, pagingClaudeRecord(embeddedPublicSession, index, text))
	}
	longText := strings.Repeat("x", 8000)
	lines = append(lines, pagingClaudeRecord(embeddedPublicSession, 32, longText))
	for _, span := range [][2]int{{0, 3686}, {3686, 7372}, {7372, 8000}} {
		expected[embeddedPublicIdentity(32, "chat", -1, span[0], span[1])] = conversation.Snippet(longText[span[0]:span[1]])
	}
	command := "printf 'public embedded search checkpoint'"
	toolRecord := pagingClaudeRecord(embeddedPublicSession, 33, "")
	tools := fmt.Sprintf(`"content":[{"type":"tool_use","id":"call-0","name":"Bash","input":{"command":%q}},{"type":"tool_use","id":"call-1","name":"Bash","input":{"command":%q}}]`, command, command)
	toolRecord = strings.Replace(toolRecord, `"content":[{"type":"text","text":""}]`, tools, 1)
	lines = append(lines, toolRecord)
	for toolIndex := range 2 {
		expected[embeddedPublicIdentity(33, "tool_call", toolIndex, 0, len(command))] = conversation.Snippet(command)
	}
	closing := "public embedded search checkpoint closed"
	lines = append(lines, pagingClaudeRecord(embeddedPublicSession, 34, closing))
	expected[embeddedPublicIdentity(34, "chat", -1, 0, len(closing))] = conversation.Snippet(closing)
	path := filepath.Join(directory, embeddedPublicSession+".jsonl")
	writeAgedFixture(t, path, lines)
	return home, path, expected
}

func embeddedPublicIdentity(messageIndex int, kind string, toolIndex int, start, end int) string {
	return fmt.Sprintf("%s/m%d/%s/%d/%d:%d", embeddedPublicConversation, messageIndex, kind, toolIndex, start, end)
}

func newEmbeddedPublicHarness(t *testing.T, home string) (*harness, config.Config) {
	t.Helper()
	h := newHarness(t)
	h.extraEnv = []string{
		"HOME=" + home, "CODEX_HOME=" + filepath.Join(home, ".codex"),
		"CODEX_SQLITE_HOME=" + filepath.Join(home, ".codex"), "COPILOT_HOME=" + filepath.Join(home, ".copilot"),
		"CLYDE_CURSOR_PROJECTS_DIRS=" + filepath.Join(home, "cursor-projects"),
		"CLYDE_CURSOR_DATA_DIRS=" + filepath.Join(home, "cursor-data"),
		"CLYDE_ZED_DATA_DIRS=" + filepath.Join(home, "zed-data"),
	}
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
	semantic.Backend = config.ConversationSemanticBackendEmbedded
	semantic.ProjectionProfile = config.ConversationProjectionProfileSourceSpan
	semantic.IngestionEnabled = true
	semantic.SearchEnabled = false
	semantic.CollectionID = "public"
	semantic.PoolID = "public"
	semantic.CatalogPath = filepath.Join(h.stateRoot, "catalog.sqlite")
	semantic.LockPath = filepath.Join(h.stateRoot, "catalog.lock")
	semantic.MilvusAddress = "localhost:39530"
	semantic.MilvusDatabase = createEmbeddedLifecycleDatabase(t)
	semantic.MilvusCollection = "vectors"
	semantic.EmbeddingBaseURL = "http://localhost:5400/v1"
	semantic.EmbeddingModel = "nvidia/NV-EmbedCode-7b-v1"
	semantic.EmbeddingRevision = "public"
	semantic.VectorDimension = 4096
	semantic.Normalization = "l2"
	writeEmbeddedLifecycleConfig(t, h, configuration)
	return h, configuration
}

func embeddedPublicCLI(t *testing.T, h *harness, home, cursor string, limit int) (embeddedPublicPage, error) {
	t.Helper()
	args := []string{"conversation", "search", embeddedPublicConversation, "--query", embeddedPublicQuery, "--limit", fmt.Sprint(limit), "--output-format", "json"}
	if cursor != "" {
		args = append(args, "--cursor", cursor)
	}
	return embeddedPublicCLIArguments(t, h, args)
}

func embeddedPublicCLIArguments(t *testing.T, h *harness, args []string) (embeddedPublicPage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, h.binPath, args...)
	command.Env = h.env()
	var stderr strings.Builder
	command.Stderr = &stderr
	body, err := command.Output()
	if err != nil {
		return embeddedPublicPage{}, fmt.Errorf("public CLI: %w: %s", err, stderr.String())
	}
	var page embeddedPublicPage
	if err := json.Unmarshal(body, &page); err != nil {
		return page, err
	}
	return page, nil
}

func embeddedPublicMCP(t *testing.T, h *harness) *client.Client {
	t.Helper()
	connection, err := client.NewStdioMCPClient(h.binPath, h.env(), "mcp", "serve")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeEmbeddedPublicMCP(t, connection)
	})
	var request mcp.InitializeRequest
	if err := json.Unmarshal([]byte(`{"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"embedded-public-test","version":"1"}}}`), &request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := connection.Initialize(ctx, request); err != nil {
		t.Fatal(err)
	}
	return connection
}

func closeEmbeddedPublicMCP(t *testing.T, connection *client.Client) {
	t.Helper()
	if err := connection.Close(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signal() == syscall.SIGPIPE {
				t.Log("MCP server exited after its stdio transport closed")
				return
			}
		}
		t.Error(err)
	}
}

func embeddedPublicMCPPage(t *testing.T, connection *client.Client, cursor string, limit int) embeddedPublicPage {
	t.Helper()
	arguments := struct {
		Conversation string `json:"conversation_id"`
		Query        string `json:"query"`
		Cursor       string `json:"cursor"`
		Limit        int    `json:"limit"`
	}{embeddedPublicConversation, embeddedPublicQuery, cursor, limit}
	body, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return embeddedPublicMCPArguments(t, connection, body)
}

func embeddedPublicMCPArguments(t *testing.T, connection *client.Client, body []byte) embeddedPublicPage {
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
		t.Fatalf("MCP search error: %+v", result.Content)
	}
	var page embeddedPublicPage
	if err := json.Unmarshal(result.RawStructuredContent, &page); err != nil {
		t.Fatalf("MCP structured result: %v", err)
	}
	return page
}
