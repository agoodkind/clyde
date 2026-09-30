package searchacceptance_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/conversation"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	zedparser "goodkind.io/clyde/internal/providers/zed/parser"
	"goodkind.io/clyde/internal/searchacceptance"
)

func TestMapFrozenRecordPreservesIdentityAndSelector(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "original")
	manifest := createSourcePathManifest(t, root, []string{
		"home/.claude/projects/subagents/agent.jsonl",
		"home/.codex/sessions/rollout.jsonl",
		"home/.copilot/session-state/session/events.jsonl",
		"home/.cursor/projects/project/agent-transcripts/session.jsonl",
		"home/Library/Application Support/Cursor/User/globalStorage/state.vscdb",
		"home/Library/Application Support/Zed/threads/threads.db",
		"home/Library/Application Support/Zed/db/0-stable/db.sqlite",
	})
	claude := sourcePathRecord(conversation.ProviderClaude, filepath.Join(home, ".claude/projects/subagents/agent.jsonl"))
	claude.ID = conversation.DerivedID(claude.Provider, "", claude.ArtifactPath)
	claude.NativeID = ""
	copilot := sourcePathRecord(conversation.ProviderCopilot, filepath.Join(home, ".copilot/session-state/session/events.jsonl"))
	copilot.Selector = "selected-subagent"
	cursorRoot := filepath.Join(home, "Library/Application Support/Cursor/User")
	zedRoot := filepath.Join(home, "Library/Application Support/Zed")
	records := []conversation.Record{
		claude,
		sourcePathRecord(conversation.ProviderCodex, filepath.Join(home, ".codex/sessions/rollout.jsonl")),
		copilot,
		sourcePathRecord(conversation.ProviderCursor, filepath.Join(home, ".cursor/projects/project/agent-transcripts/session.jsonl")),
		sourcePathRecord(conversation.ProviderCursor, cursorparser.BuildVirtualPath(cursorparser.RootHash(cursorRoot), cursorparser.VirtualKindComposer, "composer-id")),
		sourcePathRecord(conversation.ProviderZed, "zed://"+zedparser.RootHash(zedRoot)+"/0-stable/thread-id"),
	}
	for _, record := range records {
		mapped, err := searchacceptance.MapFrozenRecord(root, home, record, manifest)
		if err != nil {
			t.Fatal(err)
		}
		if mapped.ArtifactPath == record.ArtifactPath {
			t.Fatal("read path was not mapped")
		}
		readPath := mapped.ArtifactPath
		if filepath.IsAbs(readPath) {
			expectedPath := filepath.Join(root, "home", strings.TrimPrefix(record.ArtifactPath, home+"/"))
			if readPath != expectedPath {
				t.Fatalf("physical read path = %q, expected %q", readPath, expectedPath)
			}
			if _, err := os.Stat(readPath); err != nil {
				t.Fatal(err)
			}
		}
		mapped.ArtifactPath = record.ArtifactPath
		if !reflect.DeepEqual(mapped, record) {
			t.Fatalf("record identity or metadata changed: %+v", mapped)
		}
		if record.Provider == conversation.ProviderCursor || record.Provider == conversation.ProviderZed {
			environment, err := searchacceptance.FrozenProviderEnvironment(root, record.Provider)
			if err != nil || environment.Name == "" || environment.Value == "" {
				t.Fatalf("frozen environment = %+v, err=%v", environment, err)
			}
		}
		if record.Provider == conversation.ProviderZed {
			parsed, err := zedparser.ParseVirtualPath(readPath)
			if err != nil || parsed.Channel != "0-stable" || parsed.SessionID != "thread-id" || parsed.RootHash != zedparser.RootHash(filepath.Join(root, "home/Library/Application Support/Zed")) {
				t.Fatalf("Zed read path = %q, err=%v", readPath, err)
			}
		}
		if strings.HasPrefix(readPath, "cursor://") {
			parsed, err := cursorparser.ParseVirtualPath(readPath)
			if err != nil || parsed.Kind != cursorparser.VirtualKindComposer || parsed.ID != "composer-id" || parsed.RootHash != cursorparser.RootHash(filepath.Join(root, "home/Library/Application Support/Cursor/User")) {
				t.Fatalf("Cursor read path = %q, err=%v", readPath, err)
			}
		}
	}
}

func TestMapFrozenRecordRejectsMissingAndEscapingSources(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(t.TempDir(), "original")
	manifest := createSourcePathManifest(t, root, []string{
		"home/.claude/projects/source.jsonl",
		"home/Library/Application Support/Cursor/User/globalStorage/state.vscdb",
		"home/Library/Application Support/Zed/threads/threads.db",
		"home/Library/Application Support/Zed/db/0-stable/db.sqlite",
		"home/Library/Application Support/Zed/db/0-preview/db.sqlite",
	})
	record := sourcePathRecord(conversation.ProviderClaude, filepath.Join(home, ".claude/projects/source.jsonl"))
	paths := []string{
		filepath.Join(home, ".claude/projects/missing.jsonl"),
		home + "/.claude/../outside.jsonl",
		home + "-other/.claude/projects/source.jsonl",
		filepath.Join(home, ".codex/sessions/source.jsonl"),
		"cursor://0000000000000000/composer/native",
		"zed://0000000000000000/0-stable/native",
	}
	for _, path := range paths {
		record.ArtifactPath = path
		if _, err := searchacceptance.MapFrozenRecord(root, home, record, manifest); err == nil {
			t.Fatalf("invalid source accepted: %q", path)
		}
	}
	virtualRecords := []conversation.Record{
		sourcePathRecord(conversation.ProviderCursor, "cursor://0000000000000000/composer/native"),
		sourcePathRecord(conversation.ProviderCursor, cursorparser.BuildVirtualPath(cursorparser.RootHash(filepath.Join(home, "Library/Application Support/Cursor/User")), cursorparser.VirtualKindLegacy, "workspace~tab")),
		sourcePathRecord(conversation.ProviderZed, "zed://0000000000000000/0-stable/native"),
		sourcePathRecord(conversation.ProviderZed, "zed://"+zedparser.RootHash(filepath.Join(home, "Library/Application Support/Zed"))+"/0-preview/native"),
	}
	for _, virtualRecord := range virtualRecords {
		if _, err := searchacceptance.MapFrozenRecord(root, home, virtualRecord, manifest); err == nil {
			t.Fatalf("unsupported virtual root, kind or channel accepted: %q", virtualRecord.ArtifactPath)
		}
	}
	missingStores := []conversation.Record{
		sourcePathRecord(conversation.ProviderCursor, cursorparser.BuildVirtualPath(cursorparser.RootHash(filepath.Join(home, "Library/Application Support/Cursor/User")), cursorparser.VirtualKindComposer, "unverified-row")),
		sourcePathRecord(conversation.ProviderZed, "zed://"+zedparser.RootHash(filepath.Join(home, "Library/Application Support/Zed"))+"/0-stable/unverified-row"),
	}
	for _, virtualRecord := range missingStores {
		if _, err := searchacceptance.MapFrozenRecord(root, home, virtualRecord, nil); err == nil {
			t.Fatalf("missing virtual store accepted: %q", virtualRecord.ArtifactPath)
		}
		if _, err := searchacceptance.MapFrozenRecord(root, home, virtualRecord, manifest); err != nil {
			t.Fatalf("mapper inferred absence of an unverified database row: %v", err)
		}
	}
	record.ArtifactPath = filepath.Join(home, ".claude/projects/source.jsonl")
	if _, err := searchacceptance.MapFrozenRecord("relative", home, record, manifest); err == nil {
		t.Fatal("relative snapshot root accepted")
	}
	if _, err := searchacceptance.MapFrozenRecord(root, "relative", record, manifest); err == nil {
		t.Fatal("relative original home accepted")
	}
}

func sourcePathRecord(provider conversation.Provider, path string) conversation.Record {
	record := new(conversation.Record)
	record.ID = provider.String() + ":native"
	record.NativeID = "native"
	record.Provider = provider
	record.ArtifactPath = path
	record.WorkspaceRoot = "/original/workspace"
	record.Archived = true
	record.Origin = conversation.OriginSubagent
	return *record
}

func createSourcePathManifest(t *testing.T, root string, files []string) map[string]bool {
	t.Helper()
	manifest := make(map[string]bool, len(files))
	for _, relative := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("frozen source"), 0o600); err != nil {
			t.Fatal(err)
		}
		manifest["./"+relative] = true
	}
	return manifest
}
