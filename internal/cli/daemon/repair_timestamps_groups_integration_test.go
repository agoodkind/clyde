//go:build milvusintegration

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"

	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const (
	groupedRepairSharedText   = "The nightly retention job finished without errors."
	groupedRepairConversation = "a0a0a0a0-0000-4000-8000-%012d"
	groupedRepairRowsEvent    = "collection.shared_row_scalars_updated"
	groupedRepairCommandTime  = 5 * time.Minute
	groupedRepairConfigMode   = 0o600
	groupedRepairDirMode      = 0o755
)

// groupedRepairCLI runs the built clyde command in one temporary home.
type groupedRepairCLI struct {
	binary string
	home   string
	env    []string
}

func newGroupedRepairCLI(t *testing.T, address string, dataRoots []string) groupedRepairCLI {
	t.Helper()
	home := t.TempDir()
	baseURL := os.Getenv(repairEmbeddingBaseURLEnv)
	if baseURL == "" {
		baseURL = repairDefaultEmbeddingBaseURL
	}
	configDir := filepath.Join(home, "config", "clyde")
	if err := os.MkdirAll(configDir, groupedRepairDirMode); err != nil {
		t.Fatalf("The test could not create the config directory: %v", err)
	}
	settings := fmt.Sprintf("[conversation.semantic]\ncollection_id = %q\nmilvus_address = %q\nmilvus_database = \"default\"\nmilvus_layout = \"shared\"\nembedding_base_url = %q\nembedding_api_key_env = \"CLYDE_GROUPED_REPAIR_KEY\"\nembedding_model = %q\nembedding_dimension = %d\n", repairCollectionID, address, baseURL, repairEmbeddingModel, repairEmbeddingDimension)
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(settings), groupedRepairConfigMode); err != nil {
		t.Fatalf("The test could not write the config file: %v", err)
	}
	_, thisFile, _, found := runtime.Caller(0)
	if !found {
		t.Fatal("The Go runtime could not report the test file path.")
	}
	binary := filepath.Join(t.TempDir(), "clyde")
	build := exec.Command("go", "build", "-o", binary, "./cmd/clyde")
	build.Dir = filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("The test could not build the clyde command: %v\n%s", err, output)
	}
	return groupedRepairCLI{binary: binary, home: home, env: []string{
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(home, "state"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"CLYDE_GROUPED_REPAIR_KEY=" + repairEmbeddingKey,
		"CLYDE_CURSOR_DATA_DIRS=" + strings.Join(dataRoots, string(os.PathListSeparator)),
		"CLYDE_CURSOR_PROJECTS_DIRS=" + filepath.Join(home, "cursor-projects"),
	}}
}

func (cli groupedRepairCLI) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), groupedRepairCommandTime)
	defer cancel()
	command := exec.CommandContext(ctx, cli.binary, append([]string{"conversation", "index", "repair-timestamps", "--provider", "cursor", "--source-store", "--execute", "--limit", "100", "--max-rows", "10000"}, args...)...)
	command.Env = cli.env
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	return output.String(), err
}

// rowsEvents returns the shared update log lines that the command wrote.
func (cli groupedRepairCLI) rowsEvents(t *testing.T) []string {
	t.Helper()
	events := make([]string, 0)
	walkErr := filepath.WalkDir(filepath.Join(cli.home, "state"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for line := range strings.Lines(string(contents)) {
			if strings.Contains(line, groupedRepairRowsEvent) {
				events = append(events, line)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("The test could not read the command logs: %v", walkErr)
	}
	return events
}

func groupedVectorRange(t *testing.T, address string, key string) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: "default"})
	if err != nil {
		t.Fatalf("The test could not connect to Milvus: %v", err)
	}
	defer func() { _ = client.Close(ctx) }()
	vectors := milvusstore.VectorCollectionName(vectorsearch.CollectionName(repairCollectionID))
	resultSet, err := client.Query(ctx, milvusclient.NewQueryOption(vectors).
		WithIDs(column.NewColumnVarChar(milvusstore.VectorKeyField, []string{key})).
		WithOutputFields("minimum_timestampUnix", "maximum_timestampUnix").
		WithConsistencyLevel(entity.ClStrong))
	if err != nil || resultSet.ResultCount != 1 {
		t.Fatalf("The test could not read the vector row, as shown by the vector key, result count, and error: %s, %d, %v", key, resultSet.ResultCount, err)
	}
	minimum, err := resultSet.GetColumn("minimum_timestampUnix").GetAsInt64(0)
	if err != nil {
		t.Fatalf("The test could not read a vector range bound: %v", err)
	}
	maximum, err := resultSet.GetColumn("maximum_timestampUnix").GetAsInt64(0)
	if err != nil {
		t.Fatalf("The test could not read a vector range bound: %v", err)
	}
	return minimum, maximum
}

// TestRepairTimestampsCommandGroupsSharedUpdates checks one shared vector rebuild per group and the exact stored date range.
func TestRepairTimestampsCommandGroupsSharedUpdates(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	conversations := repairSourceGroupConversations + 2
	roots := make([]string, 0, conversations)
	docs := make([]semsearch.SemDoc, 0, conversations)
	manifest := make([]semsearch.Fingerprint, 0, conversations)
	storeRoot := t.TempDir()
	zero := time.Time{}.Unix()
	for position := range conversations {
		composer := fmt.Sprintf(groupedRepairConversation, position)
		root := filepath.Join(storeRoot, fmt.Sprintf("cursor-%d", position))
		roots = append(roots, root)
		writeComposerStore(t, root, composer, []sourceBubble{{id: "g1", kind: 1, text: groupedRepairSharedText, created: partialMessageTime(position).Format("2006-01-02T15:04:05.000Z"), tool: ""}}, nil)
		doc := repairDoc("cursor:"+composer, groupedRepairSharedText, zero)
		docs = append(docs, doc)
		manifest = append(manifest, semsearch.Fingerprint{ConversationID: "cursor:" + composer, Value: "old-parser"})
	}
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	if _, err := client.UpsertConversationDocuments(context.Background(), repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("The test could not seed the conversation rows: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, seededVectors := sharedSnapshot(t, address)
	if len(seeded) != conversations || len(seededVectors) != 1 {
		t.Fatalf("The seeded row and vector counts differ from the expected counts: %d, %d", len(seeded), len(seededVectors))
	}

	cli := newGroupedRepairCLI(t, address, roots)
	progressPath := filepath.Join(cli.home, "progress.json")
	output, err := cli.run(t, "--progress-file", progressPath)
	if err != nil {
		t.Fatalf("The repair command failed: %v\n%s", err, output)
	}
	progress := readProgress(t, progressPath)
	if !progress.Complete || progress.Totals.RowsWritten != conversations || progress.Totals.Conversations != conversations {
		t.Fatalf("The saved progress differs from the completed run: %+v", progress)
	}
	events := cli.rowsEvents(t)
	if len(events) != 2 || !strings.Contains(events[0], fmt.Sprintf(`"rows":%d,`, repairSourceGroupConversations)) || !strings.Contains(events[1], `"rows":2,`) {
		t.Fatalf("The shared update log lines do not show one update per group: %q", events)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	if !maps.EqualFunc(seededVectors, repairedVectors, slices.Equal[[]uint32]) {
		t.Fatal("The repair changed the vector bytes.")
	}
	for id, row := range repaired {
		before := seeded[id]
		if row.timestampNull || row.timestampUnix == zero || row.content != before.content || row.others != before.others || row.vectorKey != before.vectorKey {
			t.Fatalf("A repaired row has the wrong date or changed fields, as shown by its ID and contents: %s: %+v", id, row)
		}
	}
	key := slices.Collect(maps.Keys(seededVectors))[0]
	minimum, maximum := groupedVectorRange(t, address, key)
	if minimum != partialMessageTime(0).Unix() || maximum != partialMessageTime(conversations-1).Unix() {
		t.Fatalf("The stored vector minimum and maximum differ from the source dates: %d, %d", minimum, maximum)
	}
}

// seedGroupedConversations stores one row with the same text for each new composer.
func seedGroupedConversations(t *testing.T, address string, count int) ([]string, []string) {
	t.Helper()
	roots := make([]string, 0, count)
	conversationIDs := make([]string, 0, count)
	docs := make([]semsearch.SemDoc, 0, count)
	manifest := make([]semsearch.Fingerprint, 0, count)
	storeRoot := t.TempDir()
	for position := range count {
		composer := fmt.Sprintf(groupedRepairConversation, position)
		root := filepath.Join(storeRoot, fmt.Sprintf("cursor-%d", position))
		roots = append(roots, root)
		writeComposerStore(t, root, composer, []sourceBubble{{id: "g1", kind: 1, text: groupedRepairSharedText, created: partialMessageTime(position).Format("2006-01-02T15:04:05.000Z"), tool: ""}}, nil)
		docs = append(docs, repairDoc("cursor:"+composer, groupedRepairSharedText, time.Time{}.Unix()))
		manifest = append(manifest, semsearch.Fingerprint{ConversationID: "cursor:" + composer, Value: "old-parser"})
		conversationIDs = append(conversationIDs, "cursor:"+composer)
	}
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	if _, err := client.UpsertConversationDocuments(context.Background(), repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("The test could not seed the conversation rows: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	return roots, conversationIDs
}

// TestRepairTimestampsCommandSavesAGroupAfterAnEvidenceFailure checks that the command saves progress for a written group when writing evidence fails.
func TestRepairTimestampsCommandSavesAGroupAfterAnEvidenceFailure(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	conversations := 2
	roots, conversationIDs := seedGroupedConversations(t, address, conversations)
	cli := newGroupedRepairCLI(t, address, roots)
	progressPath := filepath.Join(cli.home, "progress.json")
	evidencePath := filepath.Join(cli.home, "evidence.pipe")
	if err := syscall.Mkfifo(evidencePath, groupedRepairConfigMode); err != nil {
		t.Fatalf("The test could not create the evidence pipe: %v", err)
	}
	readerOpened := make(chan error, 1)
	go func() {
		reader, err := os.OpenFile(evidencePath, os.O_RDONLY, 0)
		if err != nil {
			readerOpened <- err
			return
		}
		readerOpened <- reader.Close()
	}()
	output, err := cli.run(t, "--progress-file", progressPath, "--evidence-file", evidencePath)
	if err == nil {
		t.Fatalf("The repair command accepted the failed evidence write and produced this output: %s", output)
	}
	if readerErr := <-readerOpened; readerErr != nil {
		t.Fatalf("The evidence pipe reader failed: %v", readerErr)
	}
	progress := readProgress(t, progressPath)
	if progress.Cursor != conversationIDs[conversations-1] || progress.Totals.RowsWritten != conversations || progress.Totals.Conversations != conversations {
		t.Fatalf("The saved progress after the evidence failure is wrong: %+v", progress)
	}
	time.Sleep(repairVisibilityWait)
	repaired, _ := sharedSnapshot(t, address)
	zero := time.Time{}.Unix()
	for id, row := range repaired {
		if row.timestampNull || row.timestampUnix == zero {
			t.Fatalf("A repaired row has the wrong date or changed fields, as shown by its ID and contents: %s: %+v", id, row)
		}
	}
}

// TestRepairTimestampsCommandKeepsQueuedRetriesAcrossGroups checks that checkpoints after completed groups keep unattempted retries before the saved cursor.
func TestRepairTimestampsCommandKeepsQueuedRetriesAcrossGroups(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	conversations := repairSourceGroupConversations + 3
	roots, conversationIDs := seedGroupedConversations(t, address, conversations)
	_, seededVectors := sharedSnapshot(t, address)

	cli := newGroupedRepairCLI(t, address, roots)
	progressPath := filepath.Join(cli.home, "progress.json")
	savedCursor := conversationIDs[conversations-1]
	queued := conversationIDs[:conversations-1]
	emptyTotals := vectorsearch.TimestampRepairReport{Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0, RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0}
	seededProgress := timestampRepairProgress{Collection: vectorsearch.CollectionName(repairCollectionID), Provider: "cursor", SourceStore: true, Cursor: savedCursor, Complete: false, UpdatedAt: time.Time{}, Totals: emptyTotals, Failed: queued, LastRunRows: 0}
	if err := writeTimestampRepairProgress(progressPath, seededProgress); err != nil {
		t.Fatalf("The test could not seed the progress file: %v", err)
	}
	output, err := cli.run(t, "--progress-file", progressPath, "--resume", "--max-rows", strconv.Itoa(repairSourceGroupConversations))
	if err != nil {
		t.Fatalf("The repair command failed before the first group checkpoint: %v\n%s", err, output)
	}
	progress := readProgress(t, progressPath)
	remaining := queued[repairSourceGroupConversations:]
	if progress.Complete || progress.Cursor != savedCursor || !slices.Equal(progress.Failed, remaining) || progress.Totals.RowsWritten != repairSourceGroupConversations {
		t.Fatalf("The first group checkpoint is wrong: %+v", progress)
	}

	output, err = cli.run(t, "--progress-file", progressPath, "--resume")
	if err != nil {
		t.Fatalf("The resumed repair command failed: %v\n%s", err, output)
	}
	progress = readProgress(t, progressPath)
	if !progress.Complete || progress.Cursor != savedCursor || len(progress.Failed) != 0 || progress.Totals.RowsWritten != len(queued) {
		t.Fatalf("The saved progress after the resume is wrong: %+v", progress)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	if !maps.EqualFunc(seededVectors, repairedVectors, slices.Equal[[]uint32]) {
		t.Fatal("The repair changed the vector bytes.")
	}
	zero := time.Time{}.Unix()
	for id, row := range repaired {
		wantDated := !strings.Contains(row.relativePath, savedCursor)
		if (row.timestampUnix != zero) != wantDated {
			t.Fatalf("A repaired row has the wrong date or changed fields, as shown by its ID and contents: %s: %+v", id, row)
		}
	}
}

// TestRepairTimestampsCommandResumesAPartiallyWrittenGroup checks that a failed group preserves the saved cursor and a resume finishes the group.
func TestRepairTimestampsCommandResumesAPartiallyWrittenGroup(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	storeRoot := filepath.Join(t.TempDir(), "cursor-data")
	bubbles := make([]sourceBubble, 0, partialRepairRows)
	docs := make([]semsearch.SemDoc, 0, partialRepairRows)
	zero := time.Time{}.Unix()
	for position := range partialRepairRows {
		message := fmt.Sprintf("Partial write message %03d asks about bucket retention.", position)
		bubbles = append(bubbles, sourceBubble{id: fmt.Sprintf("p%03d", position), kind: 1, text: message, created: partialMessageTime(position).Format("2006-01-02T15:04:05.000Z"), tool: ""})
		doc := repairDoc("cursor:"+partialComposerID, message, zero)
		doc.MessageIndex = int32(position)
		docs = append(docs, doc)
	}
	writeComposerStore(t, storeRoot, partialComposerID, bubbles, nil)
	ctx := context.Background()
	client := openRepairClient(t, address, vectorsearch.LayoutShared)
	manifest := []semsearch.Fingerprint{{ConversationID: "cursor:" + partialComposerID, Value: "old-parser"}}
	if _, err := client.UpsertConversationDocuments(ctx, repairCollectionID, docs, manifest); err != nil {
		t.Fatalf("The test could not seed the conversation rows: %v", err)
	}
	time.Sleep(repairVisibilityWait)
	seeded, seededVectors := sharedSnapshot(t, address)
	ids := slices.Sorted(maps.Keys(seeded))
	lastID := ids[len(ids)-1]
	lastPath := seeded[lastID].relativePath
	lastIndex := lastPath[strings.LastIndexByte(lastPath, '/')+1:]
	setRowMetadata(t, ctx, address, lastID, partialMetadata(lastIndex, partialMetadataLength))
	time.Sleep(repairVisibilityWait)

	cli := newGroupedRepairCLI(t, address, []string{storeRoot})
	progressPath := filepath.Join(cli.home, "progress.json")
	output, err := cli.run(t, "--progress-file", progressPath)
	if err == nil {
		t.Fatalf("The repair command accepted the oversized metadata update and produced this output: %s", output)
	}
	progress := readProgress(t, progressPath)
	if progress.Cursor != "" || progress.Complete || progress.Totals.RowsWritten == 0 || progress.Totals.RowsWritten >= partialRepairRows {
		t.Fatalf("The saved progress after the failed write is wrong: %+v", progress)
	}
	if events := cli.rowsEvents(t); len(events) != 0 {
		t.Fatalf("The command logged a completed shared update for the failed group in these log lines: %q", events)
	}
	time.Sleep(repairVisibilityWait)
	lastKey := seeded[lastID].vectorKey
	var lastMessage int
	if _, err := fmt.Sscanf(lastIndex, "%d", &lastMessage); err != nil {
		t.Fatalf("The test could not parse this message index: %q", lastIndex)
	}
	_, widenedMaximum := groupedVectorRange(t, address, lastKey)
	if widenedMaximum != partialMessageTime(lastMessage).Unix() {
		t.Fatalf("The stored vector maximum does not cover the planned date of the failed row: %d", widenedMaximum)
	}

	setRowMetadata(t, ctx, address, lastID, partialMetadata(lastIndex, 0))
	time.Sleep(repairVisibilityWait)
	output, err = cli.run(t, "--progress-file", progressPath, "--resume")
	if err != nil {
		t.Fatalf("The resumed repair command failed: %v\n%s", err, output)
	}
	progress = readProgress(t, progressPath)
	if !progress.Complete || progress.Totals.RowsWritten != partialRepairRows {
		t.Fatalf("The saved progress after the resume is wrong: %+v", progress)
	}
	time.Sleep(repairVisibilityWait)
	repaired, repairedVectors := sharedSnapshot(t, address)
	if !maps.EqualFunc(seededVectors, repairedVectors, slices.Equal[[]uint32]) {
		t.Fatal("The repair changed the vector bytes.")
	}
	for id, row := range repaired {
		if row.timestampNull || row.timestampUnix == zero || row.others != seeded[id].others {
			t.Fatalf("A repaired row has the wrong date or changed fields, as shown by its ID and contents: %s: %+v", id, row)
		}
	}
}
