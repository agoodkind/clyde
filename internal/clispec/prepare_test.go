package clispec

import (
	"context"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	conv "goodkind.io/clyde/internal/conversation"
)

// requirePrepared asserts an operation declares both a Prepare and a Run. With
// Run receiving only the prepared payload, Prepare is the sole place input can be
// rejected, so a nil Prepare would leave an operation unable to reject input
// before the help boundary. This is the construction guard: it walks the actual
// operation constructors, so adding an operation that forgets Prepare fails here.
func requirePrepared[I Input, P Prepared](t *testing.T, name string, op Operation[I, P]) {
	t.Helper()
	if op.Prepare == nil {
		t.Errorf("%s: Prepare is nil; input could not be rejected before the help boundary", name)
	}
	if op.Run == nil {
		t.Errorf("%s: Run is nil", name)
	}
}

// TestEveryConversationOpDeclaresPrepare enforces that every conversation
// operation sets Prepare and Run.
func TestEveryConversationOpDeclaresPrepare(t *testing.T) {
	t.Parallel()
	requirePrepared(t, "search", searchOp())
	requirePrepared(t, "export_transcript", exportTranscriptOp())
}

// TestExportPrepareRejectsBadSelection asserts the export Prepare rejects an
// empty selection and an unknown kind, and accepts a valid one. Because Prepare
// runs in PreRunE on the terminal, these rejections render full help.
func TestExportPrepareRejectsBadSelection(t *testing.T) {
	t.Parallel()
	op := exportTranscriptOp()

	empty := op.New()
	if _, err := op.Prepare(empty); err == nil {
		t.Error("export Prepare should reject an empty content selection")
	}

	unknown := op.New()
	unknown.Kinds = []string{"bogus"}
	if _, err := op.Prepare(unknown); err == nil {
		t.Error("export Prepare should reject an unknown content kind")
	}

	valid := op.New()
	valid.Kinds = []string{"chat"}
	payload, err := op.Prepare(valid)
	if err != nil {
		t.Fatalf("export Prepare rejected a valid selection: %v", err)
	}
	if !payload.Options.Content.Has(conv.ContentKindChat) {
		t.Errorf("prepared payload missing the chat kind")
	}
}

// TestSearchPrepareRejectsBadProvider asserts the search Prepare rejects an
// unsupported provider, the validation that used to live inside Run.
func TestSearchPrepareRejectsBadProvider(t *testing.T) {
	t.Parallel()
	op := searchOp()
	in := op.New()
	in.Provider = "bogus"
	if _, err := op.Prepare(in); err == nil {
		t.Error("search Prepare should reject an unsupported provider")
	}
	if _, err := op.Prepare(op.New()); err != nil {
		t.Errorf("search Prepare rejected valid input: %v", err)
	}
}

// TestSearchPrepareRejectsBadDate asserts the search Prepare rejects an
// unparseable time bound on a query search.
func TestSearchPrepareRejectsBadDate(t *testing.T) {
	t.Parallel()
	op := searchOp()
	in := op.New()
	in.Query = "auth"
	in.After = "nope"
	if _, err := op.Prepare(in); err == nil {
		t.Error("search Prepare should reject an unparseable --after")
	}
}

// TestSearchPrepareRejectsAroundWithoutConversation asserts a read window with
// no conversation to center on is rejected in Prepare.
func TestSearchPrepareRejectsAroundWithoutConversation(t *testing.T) {
	t.Parallel()
	op := searchOp()
	in := op.New()
	in.Around = 5
	if _, err := op.Prepare(in); err == nil {
		t.Error("search Prepare should reject around with no conversation")
	}
}

// TestExportMCPHandlerRejectsEmptyOnly asserts the MCP handler surfaces the
// Prepare rejection as tool text instead of reaching the work function.
func TestExportMCPHandlerRejectsEmptyOnly(t *testing.T) {
	t.Parallel()
	_, handler := exportTranscriptOp().mcpTool()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"conversation_id": "claude:abc"}
	result, err := handler(context.Background(), req)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	text := textOf(t, result)
	if !strings.Contains(text, "no content kinds selected") {
		t.Errorf("expected the no-content-kinds error in tool text, got: %q", text)
	}
}
