//go:build live

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/frozensource"
	claudeparser "goodkind.io/clyde/internal/providers/claude/parser"
	codexparser "goodkind.io/clyde/internal/providers/codex/parser"
	copilotparser "goodkind.io/clyde/internal/providers/copilot/parser"
	cursorparser "goodkind.io/clyde/internal/providers/cursor/parser"
	zedparser "goodkind.io/clyde/internal/providers/zed/parser"
	"goodkind.io/clyde/internal/transcript"
)

type frozenCorpusModel struct {
	Name          string `json:"name"`
	Revision      string `json:"revision"`
	Dimension     int    `json:"dimension"`
	Normalization string `json:"normalization"`
}

type frozenCorpusRequest struct {
	ExpectedOwners           int                               `json:"expected_eligible_owners"`
	ExpectedOccurrences      int                               `json:"expected_occurrences"`
	CompletionTimeoutSeconds int                               `json:"completion_timeout_seconds"`
	SnapshotRoot             string                            `json:"snapshot_root"`
	ReadRoot                 string                            `json:"read_root"`
	OriginalHome             string                            `json:"original_home"`
	ManifestPath             string                            `json:"manifest_path"`
	ManifestDigest           string                            `json:"manifest_sha256"`
	IndexDigest              string                            `json:"index_sha256"`
	RuntimeRoot              string                            `json:"runtime_root"`
	OutputPath               string                            `json:"output_path"`
	Semantic                 config.ConversationSemanticConfig `json:"semantic"`
	Model                    frozenCorpusModel                 `json:"model"`
	RetainOnSuccess          bool                              `json:"retain_on_success"`
}

type frozenCorpusCache struct {
	Version int                               `json:"version"`
	Records []conversation.Record             `json:"records"`
	Stamps  map[string]conversation.FileStamp `json:"stamps"`
}

// frozenCorpusIndex adapts immutable original records to the production loader.
// Only mapped read paths differ from the original cache.
type frozenCorpusIndex struct {
	request      frozenCorpusRequest
	manifest     map[string]bool
	records      []conversation.StampedRecord
	reader       *conversation.Index
	verification frozensource.SnapshotVerification
}

var _ embeddedRecordIndex = (*frozenCorpusIndex)(nil)

func openFrozenCorpusIndex(ctx context.Context, request frozenCorpusRequest) (*frozenCorpusIndex, error) {
	for _, path := range []string{request.SnapshotRoot, request.ReadRoot, request.OriginalHome, request.ManifestPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
			return nil, errors.New("frozen source roots and manifest must be explicit clean absolute paths")
		}
	}
	semantic := request.Semantic
	model := request.Model
	if semantic.ProjectionProfile != config.ConversationProjectionProfileSourceSpan || !semantic.IngestionEnabled || semantic.SearchEnabled {
		return nil, errors.New("frozen driver requires explicit embedded p3 ingestion with search disabled")
	}
	if model.Name == "" || model.Revision == "" || model.Dimension <= 0 || model.Normalization == "" || model.Name != semantic.EmbeddingModel || model.Revision != semantic.EmbeddingRevision || model.Dimension != semantic.VectorDimension || model.Normalization != semantic.Normalization {
		return nil, errors.New("complete supplied model must match semantic descriptor")
	}
	environments, err := frozensource.Environments(request.ReadRoot)
	if err != nil {
		return nil, err
	}
	environments = append(environments,
		frozensource.Environment{Name: "HOME", Value: filepath.Join(request.ReadRoot, "home")},
		frozensource.Environment{Name: "CODEX_HOME", Value: filepath.Join(request.ReadRoot, "home/.codex")},
		frozensource.Environment{Name: "CODEX_SQLITE_HOME", Value: filepath.Join(request.ReadRoot, "home/.codex")},
		frozensource.Environment{Name: "COPILOT_HOME", Value: filepath.Join(request.ReadRoot, "home/.copilot")})
	for _, environment := range environments {
		if os.Getenv(environment.Name) != environment.Value {
			return nil, fmt.Errorf("frozen driver requires bound %s", environment.Name)
		}
	}
	manifest, err := frozensource.SnapshotManifestFiles(request.SnapshotRoot, request.ManifestPath, request.ManifestDigest)
	if err != nil {
		return nil, err
	}
	indexPath, err := frozensource.ManifestPath(request.SnapshotRoot, "clyde-cache/conversation-index.json", manifest)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != request.IndexDigest {
		return nil, errors.New("frozen index digest differs from supplied identity")
	}
	var cache frozenCorpusCache
	if err = json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	if cache.Version != 5 || len(cache.Records) == 0 {
		return nil, errors.New("nonempty version 5 frozen index required")
	}
	seen := make(map[string]bool, len(cache.Records))
	resolvedSnapshot, err := filepath.EvalSymlinks(request.SnapshotRoot)
	if err != nil {
		return nil, err
	}
	resolvedReadRoot, err := filepath.EvalSymlinks(request.ReadRoot)
	if err != nil {
		return nil, err
	}
	records := make([]conversation.StampedRecord, 0, len(cache.Records))
	for _, record := range cache.Records {
		if record.ID == "" || seen[record.ID] {
			return nil, fmt.Errorf("frozen index has empty or repeated owner ID %q", record.ID)
		}
		seen[record.ID] = true
		if record.ID != conversation.DerivedID(record.Provider, record.NativeID, record.ArtifactPath) {
			return nil, fmt.Errorf("frozen record identity differs from original source: %s", record.ID)
		}
		if strings.HasPrefix(record.ArtifactPath, "cursor://") && resolvedReadRoot == resolvedSnapshot {
			return nil, errors.New("Cursor SQLite source requires a separate verified read copy")
		}
		if _, err = frozensource.MapFrozenRecord(request.SnapshotRoot, request.OriginalHome, record, manifest); err != nil {
			return nil, err
		}
		stamp, ok := cache.Stamps[record.ArtifactPath]
		if !ok || stamp.Mtime.IsZero() || stamp.Size < 0 {
			return nil, fmt.Errorf("original artifact stamp is absent or invalid: %s", record.ID)
		}
		records = append(records, conversation.StampedRecord{Record: record, Stamp: stamp})
	}
	verification, err := frozensource.VerifySnapshot(ctx, request.SnapshotRoot, request.ManifestPath, request.ManifestDigest)
	if err != nil {
		return nil, err
	}
	if request.ReadRoot != request.SnapshotRoot {
		if _, err = frozensource.VerifySnapshot(ctx, request.ReadRoot, request.ManifestPath, request.ManifestDigest); err != nil {
			return nil, fmt.Errorf("verify isolated source copy: %w", err)
		}
	}
	registry := conversation.NewRegistry()
	registry.Register(claudeparser.New())
	registry.Register(codexparser.New())
	registry.Register(copilotparser.New())
	registry.Register(cursorparser.New())
	registry.Register(zedparser.New())
	return &frozenCorpusIndex{request: request, manifest: manifest, records: records, reader: conversation.NewIndex(registry, config.NewConfigWithDefaults().Conversation), verification: verification}, nil
}

func (index *frozenCorpusIndex) ListAllWithStamps(ctx context.Context) ([]conversation.StampedRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return slices.Clone(index.records), nil
}

func (index *frozenCorpusIndex) ListWithStamps(ctx context.Context) ([]conversation.StampedRecord, error) {
	return index.ListAllWithStamps(ctx)
}

func (index *frozenCorpusIndex) LoadMessagesWithOptions(record conversation.Record, options conversation.LoadOptions) ([]transcript.Message, error) {
	found := false
	for _, stamped := range index.records {
		if stamped.Record.ID == record.ID && stamped.Record.ArtifactPath == record.ArtifactPath && stamped.Record.Selector == record.Selector {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("record is absent from approved frozen index")
	}
	mapped, err := frozensource.MapFrozenRecord(index.request.ReadRoot, index.request.OriginalHome, record, index.manifest)
	if err != nil {
		return nil, err
	}
	return index.reader.LoadMessagesWithOptions(mapped, options)
}

func TestFrozenCorpusSourceBoundary(t *testing.T) {
	request := createFrozenCorpusFixture(t)
	index, err := openFrozenCorpusIndex(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := SemanticContentKinds(request.Semantic)
	if err != nil {
		t.Fatal(err)
	}
	var archived, subagent, derived, selectors int
	for _, stamped := range index.records {
		record := stamped.Record
		if record.Archived {
			archived++
		}
		if record.IsSubagent() {
			subagent++
		}
		if strings.HasPrefix(record.ID, "artifact:") {
			derived++
		}
		if record.Selector != "" {
			selectors++
		}
		messages, err := index.LoadMessagesWithOptions(record, SemanticConversationLoadOptions(kinds))
		if err != nil {
			t.Fatal(err)
		}
		projected, err := ProjectEmbeddedConversationOccurrences(t.Context(), request.Semantic, record, messages, true)
		if err != nil {
			t.Fatal(err)
		}
		if !projected.Admitted || len(projected.Occurrences) == 0 {
			t.Fatalf("selected source has no occurrences: %s", record.ID)
		}
		for _, occurrence := range projected.Occurrences {
			if occurrence.Scalars["conversation_id"].String != record.ID || occurrence.Scalars["source_byte_start"].Int64 < 0 || occurrence.Scalars["source_byte_end"].Int64-occurrence.Scalars["source_byte_start"].Int64 != int64(len(occurrence.SourceText)) {
				t.Fatalf("original identity or source span changed: %s", record.ID)
			}
		}
	}
	if archived != 1 || subagent < 2 || derived != 1 || selectors != 1 {
		t.Fatalf("fixture archive/subagent/derived/selector coverage=%d/%d/%d/%d", archived, subagent, derived, selectors)
	}
	record := index.records[0].Record
	mapped, err := frozensource.MapFrozenRecord(request.SnapshotRoot, request.OriginalHome, record, index.manifest)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(request.SnapshotRoot, mapped.ArtifactPath)
	if err != nil {
		t.Fatal(err)
	}
	delete(index.manifest, "./"+filepath.ToSlash(relative))
	if _, err = index.LoadMessagesWithOptions(record, SemanticConversationLoadOptions(kinds)); err == nil {
		t.Fatal("unmanifested source read accepted")
	}
}

func TestFrozenCorpusSourceRejectsRepeatedOwner(t *testing.T) {
	request := createFrozenCorpusFixture(t)
	path := filepath.Join(request.SnapshotRoot, "clyde-cache/conversation-index.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cache frozenCorpusCache
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatal(err)
	}
	cache.Records = append(cache.Records, cache.Records[0])
	data, err = json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	writeFrozenCorpusBytes(t, path, data)
	newDigest := sha256.Sum256(data)
	manifest, err := os.ReadFile(request.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	oldLine := request.IndexDigest + "  ./clyde-cache/conversation-index.json"
	if !strings.Contains(string(manifest), oldLine) {
		t.Fatal("fixture manifest omits cache identity")
	}
	updated := strings.Replace(string(manifest), oldLine, hex.EncodeToString(newDigest[:])+"  ./clyde-cache/conversation-index.json", 1)
	writeFrozenCorpusBytes(t, request.ManifestPath, []byte(updated))
	manifestDigest := sha256.Sum256([]byte(updated))
	request.ManifestDigest = hex.EncodeToString(manifestDigest[:])
	request.IndexDigest = hex.EncodeToString(newDigest[:])
	if _, err := openFrozenCorpusIndex(t.Context(), request); err == nil || !strings.Contains(err.Error(), "repeated owner ID") {
		t.Fatalf("repeated original owner result=%v", err)
	}
}

func createFrozenCorpusFixture(t *testing.T) frozenCorpusRequest {
	t.Helper()
	return createFrozenCorpusFixtureWithPadding(t, 0)
}

func TestFrozenCorpusPaddedSourcePreflight(t *testing.T) {
	request := createFrozenCorpusFixtureWithPadding(t, conversationSemanticBatchBytes)
	index, err := openFrozenCorpusIndex(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := preflightFrozenOwners(t.Context(), index)
	if err != nil {
		t.Fatal(err)
	}
	oversized := 0
	for _, owner := range expected {
		if owner.Seal.RowCount != 1 {
			t.Fatalf("padded source changed original occurrence count: %+v", owner.Seal)
		}
		if owner.Source.Stamp.Size > conversationSemanticBatchBytes {
			oversized++
		}
	}
	if len(expected) != 4 || oversized != 1 {
		t.Fatalf("padded source inventory owners=%d oversized=%d, want 4 and 1", len(expected), oversized)
	}
	t.Log("actual padded source preserves four original owners and occurrences; one physical source exceeds the unchanged production pass budget")
}

func createFrozenCorpusFixtureWithPadding(t *testing.T, padding int) frozenCorpusRequest {
	t.Helper()
	root := t.TempDir()
	original := filepath.Join(t.TempDir(), "original")
	files := []string{"home/.claude/projects/subagents/agent.jsonl", "home/.codex/archived_sessions/rollout.jsonl", "home/.copilot/session-state/frozen/events.jsonl"}
	texts := []string{
		`{"sessionId":"borrowed-parent","isSidechain":true,"uuid":"source","type":"user","timestamp":"2026-09-28T07:00:00Z","message":{"role":"user","content":"frozen artifact source"}}` + "\n",
		`{"timestamp":"2026-09-28T07:00:00Z","type":"session_meta","payload":{"id":"frozen-archive","timestamp":"2026-09-28T07:00:00Z","cwd":"/source","source":"cli"}}` + "\n" + `{"timestamp":"2026-09-28T07:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"frozen archived source"}}` + "\n",
		`{"id":"1","timestamp":"2026-09-28T07:00:00Z","type":"session.start","data":{"sessionId":"frozen","version":1,"context":{"cwd":"/source"}}}` + "\n" + `{"id":"2","timestamp":"2026-09-28T07:00:01Z","type":"user.message","data":{"content":"frozen root source"}}` + "\n" + `{"id":"3","timestamp":"2026-09-28T07:00:02Z","agentId":"agent-1","type":"subagent.started","data":{"agentDisplayName":"Researcher","toolCallId":"call-agent"}}` + "\n" + `{"id":"4","timestamp":"2026-09-28T07:00:03Z","agentId":"agent-1","type":"user.message","data":{"content":"frozen selected source","source":"agent-agent-1"}}` + "\n",
	}
	bindFrozenFixture(t, root)
	texts[0] += strings.Repeat("\n", padding)
	parsers := []conversation.Parser{claudeparser.New(), codexparser.New(), copilotparser.New()}
	cache := frozenCorpusCache{Version: 5, Stamps: make(map[string]conversation.FileStamp)}
	for position, relative := range files {
		path := filepath.Join(root, relative)
		writeFrozenCorpusBytes(t, path, []byte(texts[position]))
		age := time.Now().Add(-liveArtifactAge)
		if err := os.Chtimes(path, age, age); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stamp := conversation.FileStamp{Size: info.Size(), Mtime: info.ModTime()}
		if _, err := parsers[position].Discover(t.Context(), map[string]conversation.Record{}); err != nil {
			t.Fatal(err)
		}
		var records []conversation.Record
		if multi, ok := parsers[position].(conversation.MultiConversationParser); ok {
			scanned, valid := multi.ScanRecords(t.Context(), conversation.MultiConversationScan{Candidate: conversation.ScanCandidate{Path: path, Stamp: stamp}})
			if !valid {
				t.Fatal("real selector scan rejected fixture")
			}
			records = scanned.Records
		} else {
			record, valid := parsers[position].ScanRecord(path, stamp)
			if !valid {
				t.Fatal("real source scan rejected fixture")
			}
			records = []conversation.Record{record}
		}
		for _, record := range records {
			record.ArtifactPath = filepath.Join(original, strings.TrimPrefix(relative, "home/"))
			record.ID = conversation.DerivedID(record.Provider, record.NativeID, record.ArtifactPath)
			cache.Records = append(cache.Records, record)
			cache.Stamps[record.ArtifactPath] = stamp
		}
	}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	writeFrozenCorpusBytes(t, filepath.Join(root, "clyde-cache/conversation-index.json"), data)
	files = append(files, "clyde-cache/conversation-index.json")
	var manifest strings.Builder
	for _, relative := range files {
		bytes, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(bytes)
		fmt.Fprintf(&manifest, "%x  ./%s\n", hash, relative)
	}
	manifestPath := filepath.Join(root, "MANIFEST.sha256")
	writeFrozenCorpusBytes(t, manifestPath, []byte(manifest.String()))
	manifestHash := sha256.Sum256([]byte(manifest.String()))
	indexHash := sha256.Sum256(data)
	semantic := config.NewConfigWithDefaults().Conversation.Semantic

	semantic.IngestionEnabled = true
	semantic.SearchEnabled = false
	semantic.ProjectionProfile = config.ConversationProjectionProfileSourceSpan
	semantic.IncludeArchived = true
	semantic.IncludeSubagents = true
	semantic.EmbeddingModel = liveEmbeddingModel
	semantic.EmbeddingRevision = "frozen-fixture"
	semantic.VectorDimension = liveEmbeddingDimension
	semantic.Normalization = "l2"
	return frozenCorpusRequest{ExpectedOwners: 4, ExpectedOccurrences: 4, CompletionTimeoutSeconds: 180, SnapshotRoot: root, ReadRoot: root, OriginalHome: original, ManifestPath: manifestPath, ManifestDigest: hex.EncodeToString(manifestHash[:]), IndexDigest: hex.EncodeToString(indexHash[:]), Semantic: semantic, Model: frozenCorpusModel{Name: semantic.EmbeddingModel, Revision: semantic.EmbeddingRevision, Dimension: semantic.VectorDimension, Normalization: semantic.Normalization}}
}

func bindFrozenFixture(t *testing.T, root string) {
	t.Helper()
	environments, err := frozensource.Environments(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, environment := range environments {
		t.Setenv(environment.Name, environment.Value)
	}
	for _, name := range []string{"HOME", "CODEX_HOME", "CODEX_SQLITE_HOME", "COPILOT_HOME"} {
		path := filepath.Join(root, "home")
		if name == "CODEX_HOME" || name == "CODEX_SQLITE_HOME" {
			path = filepath.Join(path, ".codex")
		}
		if name == "COPILOT_HOME" {
			path = filepath.Join(path, ".copilot")
		}
		t.Setenv(name, path)
	}
}

func writeFrozenCorpusBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
