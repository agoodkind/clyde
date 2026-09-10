package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
)

func TestIndexRefreshReusesParentPrefixAndUpdatesResumeMetadata(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	resumerRelative := "proj/agent-transcripts/" + cursorLooseConversationID + "/" + cursorLooseConversationID + ".jsonl"
	root := writeCursorProject(t, map[string]string{
		"proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl":             cursorTranscriptBody("parent"),
		"proj/agent-transcripts/" + cursorParentConversationID + "/subagents/" + cursorSubagentConversationID + ".jsonl": cursorTranscriptBody("child"),
		resumerRelative: cursorTranscriptBody("first title"),
	})
	registry := conversation.NewRegistry()
	registry.Register(New())
	index := conversation.NewIndex(registry, config.ConversationConfig{IncludeSubagentConversations: true})
	refresh := func() {
		t.Helper()
		if err := index.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	parent := func() string {
		t.Helper()
		record, ok := index.RecordByID("cursor:" + cursorSubagentConversationID)
		if !ok || record.Lineage == nil {
			t.Fatalf("missing child: %+v", record)
		}
		return record.Lineage.ParentNativeID
	}
	refresh()
	marker := time.Unix(100, 0)
	if err := os.Chtimes(conversation.CachePath(), marker, marker); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		refresh()
	}
	info, err := os.Stat(conversation.CachePath())
	if err != nil || !info.ModTime().Equal(marker) {
		t.Fatalf("unchanged refresh rewrote cache: %v", err)
	}
	resumerPath := filepath.Join(root, filepath.FromSlash(resumerRelative))
	spawn := strings.TrimPrefix(cursorSpawnTranscriptBody(cursorSubagentConversationID), cursorTranscriptBody("start the work"))
	file, err := os.OpenFile(resumerPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.TrimSuffix(spawn, "\n")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	refresh()
	if got := parent(); got != cursorParentConversationID {
		t.Fatalf("partial resume changed parent to %s", got)
	}
	record, ok := index.RecordByID("cursor:" + cursorLooseConversationID)
	if !ok || record.Title != "first title" {
		t.Fatalf("append reread parent prefix: %+v", record)
	}
	file, err = os.OpenFile(resumerPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	refresh()
	if got := parent(); got != cursorLooseConversationID {
		t.Fatalf("completed resume parent = %s", got)
	}
	if err := os.Remove(resumerPath); err != nil {
		t.Fatal(err)
	}
	refresh()
	if got := parent(); got != cursorParentConversationID {
		t.Fatalf("removed resume parent = %s", got)
	}
	if _, ok := index.RecordByID("cursor:" + cursorLooseConversationID); ok {
		t.Fatal("removed conversation remained indexed")
	}
}

func TestResumeContinuationResetsOnTruncationAndReplacement(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "truncate", true: "replace"}[replace], func(t *testing.T) {
			relative := "proj/agent-transcripts/" + cursorParentConversationID + "/" + cursorParentConversationID + ".jsonl"
			root := writeCursorProject(t, map[string]string{relative: cursorSpawnTranscriptBody(cursorSubagentConversationID)})
			path := filepath.Join(root, filepath.FromSlash(relative))
			parser := New()
			if _, err := parser.Discover(t.Context(), nil); err != nil {
				t.Fatal(err)
			}
			body := cursorTranscriptBody("replacement title")
			if replace {
				body += strings.Repeat("\n", len(cursorSpawnTranscriptBody(cursorSubagentConversationID)))
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			candidates, err := parser.Discover(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			record, ok := parser.ScanRecord(candidates[0].Path, candidates[0].Stamp)
			if !ok || record.Title != "replacement title" {
				t.Fatalf("replacement header=%+v", record)
			}
			if links := parser.resumeLinks[path].resumedIDs; len(links) != 0 {
				t.Fatalf("replacement kept links: %v", links)
			}
		})
	}
}
