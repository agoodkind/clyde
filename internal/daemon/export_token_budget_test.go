package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/daemon"
	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/sandbox"
	"goodkind.io/clyde/internal/tokencount"
)

const (
	budgetSessionID      = "7d1f0c2a-3b4e-4f5a-8b6c-9d0e1f2a3b4c"
	budgetLargeMessages  = 6
	budgetWordsPerLine   = 6000
	budgetNewestLine     = "newest short transcript line"
	budgetTranscriptFile = budgetSessionID + ".jsonl"
	budgetAnthropicKey   = "CLYDE_ANTHROPIC_API_KEY"
	budgetOpenAIKey      = "CLYDE_OPENAI_API_KEY"
)

// TestExportTranscriptLocalMaxTokensKeepsNewestContentAcrossBudgets checks
// exports of a synthetic conversation through ExportTranscriptLocal. The
// conversation has six lines of about 6000 generated words each, followed by
// a short newest line. Each large line exceeds every token budget in the test.
// For budgets of 2000, 5000, and 10000 tokens, the test checks that each export
// stays within budget, is a suffix of the uncapped export, and keeps the newest
// line. It also checks that each larger budget returns more bytes.
func TestExportTranscriptLocalMaxTokensKeepsNewestContentAcrossBudgets(t *testing.T) {
	transcriptPath := writeBudgetTranscript(t)
	t.Setenv(budgetAnthropicKey, "")
	t.Setenv(budgetOpenAIKey, "")

	conversationID := conversation.DerivedID(providerid.ProviderClaude, budgetSessionID, transcriptPath)
	options := conversation.ExportOptions{
		Format:     conversation.ExportFormatMarkdown,
		Whitespace: conversation.WhitespaceDense,
		Content:    conversation.NewContentKindSet(conversation.ContentKindChat),
	}
	uncapped, err := daemon.ExportTranscriptLocal(t.Context(), conversationID, options)
	if err != nil {
		t.Fatalf("uncapped export: %v", err)
	}
	counter := tokencount.LocalCounter(tokencount.FamilyClaude, "", tokencount.Settings{})

	previousLength := 0
	for _, budget := range []int{2000, 5000, 10000} {
		options.MaxTokens = strconv.Itoa(budget)
		capped, err := daemon.ExportTranscriptLocal(t.Context(), conversationID, options)
		if err != nil {
			t.Fatalf("export with budget %d: %v", budget, err)
		}
		if count := counter.Estimate(string(capped)); count > budget {
			t.Fatalf("budget %d: capped token count = %d", budget, count)
		}
		if !strings.HasSuffix(string(uncapped), string(capped)) {
			t.Fatalf("budget %d: capped export is not a suffix of the uncapped export", budget)
		}
		if !strings.Contains(string(capped), budgetNewestLine) {
			t.Fatalf("budget %d: capped export does not contain the newest line", budget)
		}
		if len(capped) <= previousLength {
			t.Fatalf("budget %d: capped export has %d bytes, want more than %d bytes from the smaller budget", budget, len(capped), previousLength)
		}
		previousLength = len(capped)
	}
}

func writeBudgetTranscript(t *testing.T) string {
	t.Helper()
	roots, err := sandbox.NewRoots()
	if err != nil {
		t.Fatalf("create sandbox roots: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(roots.Base) })
	for _, variable := range sandbox.Env(roots) {
		t.Setenv(variable.Name, variable.Value)
	}
	home := filepath.Join(roots.Base, "home")
	t.Setenv("HOME", home)
	for _, name := range []string{"CODEX_HOME", "CODEX_SQLITE_HOME", "CLYDE_CURSOR_PROJECTS_DIRS", "CLYDE_CURSOR_DATA_DIRS", "CLYDE_ZED_DATA_DIRS", "COPILOT_HOME"} {
		directory := filepath.Join(roots.Base, "providers", name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
		t.Setenv(name, directory)
	}
	projectDir := filepath.Join(home, ".claude", "projects", "-repo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("create project dir: %v", err)
	}

	lines := make([]string, 0, budgetLargeMessages+1)
	for messageIndex := range budgetLargeMessages {
		var words strings.Builder
		for wordIndex := range budgetWordsPerLine {
			fmt.Fprintf(&words, "m%dw%d ", messageIndex, wordIndex)
		}
		lines = append(lines, budgetTranscriptRecord(messageIndex, "assistant", words.String()))
	}
	lines = append(lines, budgetTranscriptRecord(budgetLargeMessages, "user", budgetNewestLine))
	transcriptPath := filepath.Join(projectDir, budgetTranscriptFile)
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return transcriptPath
}

func budgetTranscriptRecord(index int, role string, body string) string {
	head := fmt.Sprintf(`"sessionId":%q,"cwd":"/repo","uuid":"00%06d-0000-4000-8000-000000000000","timestamp":"2026-07-01T12:00:%02dZ"`, budgetSessionID, index, index)
	if role == "user" {
		return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q},%s}`, body, head)
	}
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":%q}]},%s}`, body, head)
}
