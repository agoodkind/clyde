package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/transcript"
)

const (
	positionRuleComposerID     = "d1d1d1d1-1111-4111-8111-d1d1d1d1d1d1"
	positionRuleConversationID = "d2d2d2d2-2222-4222-8222-d2d2d2d2d2d2"
	positionRuleWorkspaceHash  = "workspace-hash"
	positionRuleLegacyTabID    = "tab-a"
)

// positionRuleStream runs Discover on the given parser, selects the candidate
// path that contains needle, and collects the messages Stream returns for it.
func positionRuleStream(t *testing.T, parser *Parser, needle string) []transcript.Message {
	t.Helper()

	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatalf("Discover returned error: %v", err)
	}
	path := findPathContaining(t, candidatePaths(candidates), needle)
	messages, err := conversation.CollectMessages(parser.Stream(path, conversation.LoadOptions{
		IncludeSystemPrompts:  false,
		IncludeSystemMessages: false,
		IncludeToolOutputs:    false,
		IncludeInjected:       false,
		HarnessTally:          nil,
	}))
	if err != nil {
		t.Fatalf("CollectMessages(%q) returned error: %v", path, err)
	}
	return messages
}

// assertAppendKeepsPositions checks that the second parse returns every message
// of the first parse at the same index with the same role and text, followed by
// exactly one new message with the appended text.
func assertAppendKeepsPositions(
	t *testing.T,
	before []transcript.Message,
	after []transcript.Message,
	appendedText string,
) {
	t.Helper()

	if len(before) < 2 {
		t.Fatalf("first parse returned %d messages, want at least 2: %#v", len(before), before)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("second parse returned %d messages, want %d: %#v", len(after), len(before)+1, after)
	}
	for index, earlier := range before {
		later := after[index]
		if later.Role != earlier.Role || later.Text != earlier.Text {
			t.Fatalf("message %d changed after append: first parse %s %q, second parse %s %q",
				index, earlier.Role, earlier.Text, later.Role, later.Text)
		}
	}
	if last := after[len(before)]; last.Text != appendedText {
		t.Fatalf("appended message text = %q, want %q", last.Text, appendedText)
	}
}

// TestComposerAppendKeepsEarlierMessagePositions writes a composer chat with two
// bubbles to a real global store. The test then adds a third bubble row and
// rewrites the composer header with the longer bubble list. The second parse
// must return the first two messages at their original indexes.
func TestComposerAppendKeepsEarlierMessagePositions(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())
	globalDBPath := filepath.Join(rootDir, "globalStorage", "state.vscdb")

	createCursorParserGlobalDBWithStatements(t, globalDBPath, []string{
		composerDataStatement(positionRuleComposerID, "Position", "", []string{"p-1", "p-2"}),
		bubbleStatement(positionRuleComposerID, "p-1", 1, "composer first question", ""),
		bubbleStatement(positionRuleComposerID, "p-2", 2, "composer first answer", ""),
	})
	parser := New()
	before := positionRuleStream(t, parser, positionRuleComposerID)

	appendCursorRequestStatements(t, globalDBPath, []string{
		bubbleStatement(positionRuleComposerID, "p-3", 1, "composer second question", ""),
		`DELETE FROM cursorDiskKV WHERE key = 'composerData:` + positionRuleComposerID + `'`,
		composerDataStatement(positionRuleComposerID, "Position", "", []string{"p-1", "p-2", "p-3"}),
	})
	after := positionRuleStream(t, parser, positionRuleComposerID)

	assertAppendKeepsPositions(t, before, after, "composer second question")
}

// TestJSONLAppendKeepsEarlierMessagePositions writes a Cursor agent transcript
// with a user turn and an assistant turn. The test then appends one user line,
// which starts a new turn after the trailing assistant turn. The second parse
// must return the first two messages at their original indexes.
func TestJSONLAppendKeepsEarlierMessagePositions(t *testing.T) {
	projectsDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", t.TempDir())
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", projectsDir)

	path := writeCursorJSONLTranscript(t, projectsDir, "Users-alice-source-cursor-repo", positionRuleConversationID, []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"jsonl first question"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"jsonl first answer"}]}}`,
	})
	parser := New()
	before := positionRuleStream(t, parser, positionRuleConversationID)

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open transcript for append: %v", err)
	}
	appendedLine := `{"role":"user","message":{"content":[{"type":"text","text":"jsonl second question"}]}}` + "\n"
	if _, err := file.WriteString(appendedLine); err != nil {
		_ = file.Close()
		t.Fatalf("append transcript line: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close transcript: %v", err)
	}
	after := positionRuleStream(t, parser, positionRuleConversationID)

	assertAppendKeepsPositions(t, before, after, "jsonl second question")
}

// TestLegacyAppendKeepsEarlierMessagePositions writes a legacy chat tab with two
// bubbles to a real workspace store. The test then rewrites the stored chat data
// with one more bubble at the end of the tab. The second parse must return the
// first two messages at their original indexes.
func TestLegacyAppendKeepsEarlierMessagePositions(t *testing.T) {
	rootDir := t.TempDir()
	t.Setenv("CLYDE_CURSOR_DATA_DIRS", rootDir)
	t.Setenv("CLYDE_CURSOR_PROJECTS_DIRS", t.TempDir())

	createCursorParserWorkspaceDB(t, rootDir, positionRuleWorkspaceHash)
	workspaceDBPath := filepath.Join(rootDir, "workspaceStorage", positionRuleWorkspaceHash, "state.vscdb")
	needle := legacyID(positionRuleWorkspaceHash, positionRuleLegacyTabID)
	parser := New()
	before := positionRuleStream(t, parser, needle)

	execCursorParserStatements(t, workspaceDBPath,
		`UPDATE ItemTable SET value = json_insert(value, '$.tabs[0].bubbles[#]', json('{"type":"user","text":"legacy second question"}')) `+
			`WHERE key = 'workbench.panel.aichat.view.aichat.chatdata'`,
	)
	after := positionRuleStream(t, parser, needle)

	assertAppendKeepsPositions(t, before, after, "legacy second question")
}
