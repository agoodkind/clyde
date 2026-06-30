package hookspec

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

type testReorientCall struct {
	ConversationID string
	Workspace      string
	Cursor         string
}

func TestRunnerReorientsCompactSessionStartWithTranscriptPath(t *testing.T) {
	t.Parallel()

	calls := make([]testReorientCall, 0, 2)
	pages := []conversation.ReorientPage{
		{
			Items:      []conversation.ReorientItem{{Title: "First", Body: "one"}},
			NextCursor: "cursor-two",
			Remaining:  1,
			Offset:     0,
			TotalItems: 2,
		},
		{
			Items:      []conversation.ReorientItem{{Title: "Second", Body: "two"}},
			NextCursor: "",
			Remaining:  0,
			Offset:     1,
			TotalItems: 2,
		},
	}
	reorient := func(
		_ context.Context,
		conversationID string,
		workspace string,
		_ string,
		cursor string,
		_ int,
		_ int,
		_ int,
	) (conversation.ReorientPage, error) {
		calls = append(calls, testReorientCall{
			ConversationID: conversationID,
			Workspace:      workspace,
			Cursor:         cursor,
		})
		return pages[len(calls)-1], nil
	}

	var output strings.Builder
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/Users/me/.claude/projects/proj/session.jsonl",
			"cwd": "/Users/me/project"
		}`),
		Output:   &output,
		Reorient: reorient,
	}
	err := runner.Run(context.Background(), HookIDClaudeCodeReorientAfterCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	expectedCalls := []testReorientCall{
		{
			ConversationID: "/Users/me/.claude/projects/proj/session.jsonl",
			Workspace:      "/Users/me/project",
			Cursor:         "",
		},
		{
			ConversationID: "/Users/me/.claude/projects/proj/session.jsonl",
			Workspace:      "/Users/me/project",
			Cursor:         "cursor-two",
		},
	}
	if !slices.Equal(calls, expectedCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, expectedCalls)
	}
	if !strings.Contains(output.String(), "## First") {
		t.Fatalf("output missing first page:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "## Second") {
		t.Fatalf("output missing second page:\n%s", output.String())
	}
}

func TestRunnerIgnoresNonCompactSessionStart(t *testing.T) {
	t.Parallel()

	var output strings.Builder
	called := false
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "startup",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output: &output,
		Reorient: func(
			context.Context,
			string,
			string,
			string,
			string,
			int,
			int,
			int,
		) (conversation.ReorientPage, error) {
			called = true
			return conversation.ReorientPage{}, nil
		},
	}

	err := runner.Run(context.Background(), HookIDClaudeCodeReorientAfterCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Fatal("reorient was called for non-compact source")
	}
	if output.String() != "" {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestRunnerErrorsWhenReorientCursorStalls(t *testing.T) {
	t.Parallel()

	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output: &strings.Builder{},
		Reorient: func(
			context.Context,
			string,
			string,
			string,
			string,
			int,
			int,
			int,
		) (conversation.ReorientPage, error) {
			return conversation.ReorientPage{Remaining: 1, NextCursor: ""}, nil
		},
	}

	err := runner.Run(context.Background(), HookIDClaudeCodeReorientAfterCompact)
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if !strings.Contains(err.Error(), "remaining 1 but next cursor is empty") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunnerReturnsDaemonErrors(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("daemon unavailable")
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output: &strings.Builder{},
		Reorient: func(
			context.Context,
			string,
			string,
			string,
			string,
			int,
			int,
			int,
		) (conversation.ReorientPage, error) {
			return conversation.ReorientPage{}, expectedErr
		},
	}

	err := runner.Run(context.Background(), HookIDClaudeCodeReorientAfterCompact)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("Run error = %v, want %v", err, expectedErr)
	}
}

func TestRunnerFallsBackToWorkspaceWhenTranscriptNotFound(t *testing.T) {
	t.Parallel()

	calls := make([]testReorientCall, 0, 2)
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/tmp/missing-session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output: &strings.Builder{},
		Reorient: func(
			_ context.Context,
			conversationID string,
			workspace string,
			_ string,
			cursor string,
			_ int,
			_ int,
			_ int,
		) (conversation.ReorientPage, error) {
			calls = append(calls, testReorientCall{
				ConversationID: conversationID,
				Workspace:      workspace,
				Cursor:         cursor,
			})
			if len(calls) == 1 {
				return conversation.ReorientPage{}, errors.New("conversation not found")
			}
			return conversation.ReorientPage{Remaining: 0}, nil
		},
	}

	err := runner.Run(context.Background(), HookIDClaudeCodeReorientAfterCompact)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	expectedCalls := []testReorientCall{
		{
			ConversationID: "/tmp/missing-session.jsonl",
			Workspace:      "/tmp/project",
			Cursor:         "",
		},
		{
			ConversationID: "",
			Workspace:      "/tmp/project",
			Cursor:         "",
		},
	}
	if !slices.Equal(calls, expectedCalls) {
		t.Fatalf("calls = %#v, want %#v", calls, expectedCalls)
	}
}

func TestRunnerStopsWhenContextIsCanceledBetweenPages(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	callCount := 0
	runner := Runner{
		Registry: NewRegistry(),
		Input: strings.NewReader(`{
			"hook_event_name": "SessionStart",
			"source": "compact",
			"transcript_path": "/tmp/session.jsonl",
			"cwd": "/tmp/project"
		}`),
		Output: &strings.Builder{},
		Reorient: func(
			context.Context,
			string,
			string,
			string,
			string,
			int,
			int,
			int,
		) (conversation.ReorientPage, error) {
			callCount++
			cancel()
			return conversation.ReorientPage{Remaining: 1, NextCursor: "next"}, nil
		},
	}

	err := runner.Run(ctx, HookIDClaudeCodeReorientAfterCompact)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if callCount != 1 {
		t.Fatalf("reorient calls = %d, want 1", callCount)
	}
}
