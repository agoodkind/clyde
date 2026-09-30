package main_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/searchacceptance"
)

type sourceCLISettings struct {
	Semantic        config.ConversationSemanticConfig `json:"semantic"`
	Model           searchacceptance.Model            `json:"model"`
	ArtifactSettled bool                              `json:"artifact_settled"`
}

type sourceCLIIndex struct {
	Version int                   `json:"version"`
	Records []conversation.Record `json:"records"`
}

type sourceCLIFixture struct {
	root         string
	originalHome string
	manifest     string
	digest       string
	settings     string
	output       string
	source       string
	identity     searchacceptance.SourceIdentity
	text         string
}

func TestSourceExportCLIUsesVerifiedFrozenSources(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "embedded-search-acceptance")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real source exporter: %v\n%s", err, output)
	}
	t.Run("complete original identity and isolated provider roots", func(t *testing.T) {
		fixture := createSourceCLIFixture(t)
		if output, err := runSourceCLI(t, binary, fixture); err != nil {
			t.Fatalf("export frozen source: %v\n%s", err, output)
		}
		rows, summary := readSourceCLIArtifact(t, fixture.output)
		if !summary.Complete || summary.Records != 1 || summary.Occurrences != 1 || len(rows) != 1 || rows[0].Identity != fixture.identity || rows[0].SourceDigest != searchacceptance.Digest([]byte(fixture.text)) {
			t.Fatalf("source identity or completion differs: %+v, %+v", rows, summary)
		}
	})
	t.Run("existing output remains unchanged", func(t *testing.T) {
		fixture := createSourceCLIFixture(t)
		original := []byte("existing source artifact\n")
		writeSourceCLIFile(t, fixture.output, original)
		if output, err := runSourceCLI(t, binary, fixture); err == nil {
			t.Fatalf("existing artifact replaced: %s", output)
		}
		if data := readSourceCLIFile(t, fixture.output); !bytes.Equal(data, original) {
			t.Fatalf("existing artifact changed: %q", data)
		}
	})
	t.Run("tampered source cannot create output", func(t *testing.T) {
		fixture := createSourceCLIFixture(t)
		writeSourceCLIFile(t, fixture.source, []byte("changed after approval"))
		if output, err := runSourceCLI(t, binary, fixture); err == nil {
			t.Fatalf("tampered source accepted: %s", output)
		}
		assertSourceCLIOutputAbsent(t, fixture.output)
	})
	t.Run("failed projection emits no complete marker", func(t *testing.T) {
		fixture := createSourceCLIFixture(t)
		settings := sourceCLISettingsForFixture()
		settings.Semantic.ProjectionProfile = config.ConversationProjectionProfileOriginal
		writeSourceCLIJSON(t, fixture.settings, settings)
		if output, err := runSourceCLI(t, binary, fixture); err == nil {
			t.Fatalf("legacy ingestion projection accepted: %s", output)
		}
		rows, summary := readSourceCLIArtifact(t, fixture.output)
		if summary.Complete || summary.Error == "" || len(rows) != 0 {
			t.Fatalf("failed projection emitted completion: %+v", summary)
		}
	})
	for _, symlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("output inside source root symlink=%t", symlink), func(t *testing.T) {
			fixture := createSourceCLIFixture(t)
			parent := fixture.root
			if symlink {
				parent = filepath.Join(t.TempDir(), "frozen-root-link")
				if err := os.Symlink(fixture.root, parent); err != nil {
					t.Fatal(err)
				}
			}
			fixture.output = filepath.Join(parent, "source-output.jsonl")
			originalManifest := readSourceCLIFile(t, fixture.manifest)
			originalSource := readSourceCLIFile(t, fixture.source)
			if output, err := runSourceCLI(t, binary, fixture); err == nil {
				t.Fatalf("output inside frozen snapshot accepted: %s", output)
			}
			assertSourceCLIOutputAbsent(t, fixture.output)
			if !bytes.Equal(originalManifest, readSourceCLIFile(t, fixture.manifest)) || !bytes.Equal(originalSource, readSourceCLIFile(t, fixture.source)) {
				t.Fatal("frozen inputs changed")
			}
		})
	}
}

func createSourceCLIFixture(t *testing.T) sourceCLIFixture {
	t.Helper()
	root := t.TempDir()
	originalHome := filepath.Join(t.TempDir(), "original-home")
	originalPath := filepath.Join(originalHome, ".claude/projects/subagents/agent.jsonl")
	text := "original source\x00text"
	var record conversation.Record
	record.ID = conversation.DerivedID(conversation.ProviderClaude, "", originalPath)
	record.Provider = conversation.ProviderClaude
	record.ArtifactPath = originalPath
	record.Origin = conversation.OriginSubagent
	record.ArtifactKind = "transcript"
	source := filepath.Join(root, "home/.claude/projects/subagents/agent.jsonl")
	encoded, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	writeSourceCLIFile(t, source, []byte(fmt.Sprintf(`{"sessionId":"parent-id","isSidechain":true,"uuid":"source","type":"user","timestamp":"2026-09-28T07:00:00Z","message":{"role":"user","content":%s}}
`, encoded)))
	index := sourceCLIIndex{Version: 5, Records: []conversation.Record{record}}
	writeSourceCLIJSON(t, filepath.Join(root, "clyde-cache/conversation-index.json"), index)
	var manifest strings.Builder
	for _, relative := range []string{"home/.claude/projects/subagents/agent.jsonl", "clyde-cache/conversation-index.json"} {
		data := readSourceCLIFile(t, filepath.Join(root, relative))
		fmt.Fprintf(&manifest, "%s  ./%s\n", searchacceptance.Digest(data), relative)
	}
	manifestPath := filepath.Join(root, "MANIFEST.sha256")
	writeSourceCLIFile(t, manifestPath, []byte(manifest.String()))
	settings := filepath.Join(t.TempDir(), "settings.json")
	writeSourceCLIJSON(t, settings, sourceCLISettingsForFixture())
	return sourceCLIFixture{
		root: root, originalHome: originalHome, manifest: manifestPath,
		digest: searchacceptance.Digest([]byte(manifest.String())), settings: settings,
		output: filepath.Join(t.TempDir(), "source-output.jsonl"), source: source,
		identity: searchacceptance.SourceIdentity{ConversationID: record.ID, MessageIndex: 0, ContentKind: "chat", ToolIndex: -1, SourceByteStart: 0, SourceByteEnd: int64(len(text))}, text: text,
	}
}

func sourceCLISettingsForFixture() sourceCLISettings {
	semantic := config.NewConfigWithDefaults().Conversation.Semantic
	semantic.ProjectionProfile = config.ConversationProjectionProfileSourceSpan
	semantic.IncludeSubagents = true
	semantic.EmbeddingModel = "NV-EmbedCode-7b-v1"
	semantic.EmbeddingRevision = "fixture"
	semantic.VectorDimension = 4096
	semantic.Normalization = "l2"
	return sourceCLISettings{Semantic: semantic, Model: searchacceptance.Model{Name: semantic.EmbeddingModel, Revision: semantic.EmbeddingRevision, Dimension: semantic.VectorDimension, Normalization: semantic.Normalization}, ArtifactSettled: true}
}

func runSourceCLI(t *testing.T, binary string, fixture sourceCLIFixture) ([]byte, error) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binary, "export-sources", "--root", fixture.root, "--original-home", fixture.originalHome, "--manifest", fixture.manifest, "--sha256", fixture.digest, "--settings", fixture.settings, "--output", fixture.output)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "CLYDE_CURSOR_DATA_DIRS=") || strings.HasPrefix(value, "CLYDE_ZED_DATA_DIRS=") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	unrelatedRoot := t.TempDir()
	command.Env = append(command.Env, "CLYDE_CURSOR_DATA_DIRS="+unrelatedRoot, "CLYDE_ZED_DATA_DIRS="+unrelatedRoot)
	return command.CombinedOutput()
}

func readSourceCLIArtifact(t *testing.T, path string) ([]searchacceptance.FrozenSourceOccurrence, searchacceptance.FrozenSourceSummary) {
	t.Helper()
	var rows []searchacceptance.FrozenSourceOccurrence
	var summary searchacceptance.FrozenSourceSummary
	for _, line := range bytes.Split(bytes.TrimSpace(readSourceCLIFile(t, path)), []byte("\n")) {
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			t.Fatal(err)
		}
		switch envelope.Type {
		case "header", "record":
		case "occurrence":
			var row searchacceptance.FrozenSourceOccurrence
			if err := json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		case "summary":
			if err := json.Unmarshal(line, &summary); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected source output type %q", envelope.Type)
		}
	}
	return rows, summary
}

func assertSourceCLIOutputAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source output exists or failed unexpectedly: %v", err)
	}
}

func writeSourceCLIJSON[T sourceCLISettings | sourceCLIIndex](t *testing.T, path string, value T) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeSourceCLIFile(t, path, data)
}

func writeSourceCLIFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSourceCLIFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
