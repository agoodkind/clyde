//go:build milvusintegration

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/cli"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation/semsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
	"goodkind.io/clyde/internal/conversation/vectorsearch/milvustest"
)

const (
	progressSourceCollectionID = "integration-progress-single"
	progressTargetCollectionID = "integration-progress-shared"
	progressConversations      = 4
	progressRepairRounds       = 3
	wantRateUnknown            = "rate unknown"
	wantEstimateUnknown        = "time remaining unknown"
	wantFinished               = "complete"
)

// progressLinePattern matches one progress line that copy-shared prints.
var progressLinePattern = regexp.MustCompile(`^(\S+) round (\d+): (\d+) of (\d+) rows \(([0-9.]+)%\), (.+?), (\S+) elapsed, (.+)$`)

// progressDocuments returns three messages for each of four conversations.
// The last message of every conversation uses the same text.
func progressDocuments() ([]semsearch.SemDoc, []semsearch.Fingerprint, int) {
	docs := make([]semsearch.SemDoc, 0)
	manifest := make([]semsearch.Fingerprint, 0)
	for conversationIndex := range progressConversations {
		conversationID := fmt.Sprintf("codex:progress-%02d", conversationIndex)
		texts := []string{
			fmt.Sprintf("The etcd member %d reports a slow disk.", conversationIndex),
			fmt.Sprintf("The etcd member %d needs a faster volume.", conversationIndex),
			"The cluster health check passes.",
		}
		for messageIndex, text := range texts {
			doc := repairDoc(conversationID, text, 1_790_000_000+int64(conversationIndex*10+messageIndex))
			doc.MessageIndex = int32(messageIndex)
			docs = append(docs, doc)
		}
		manifest = append(manifest, semsearch.Fingerprint{ConversationID: conversationID, Value: "v1"})
	}
	distinctTexts := progressConversations*2 + 1
	return docs, manifest, distinctTexts
}

type progressLine struct {
	text      string
	round     int
	done      int64
	total     int64
	rate      string
	remaining string
}

// parseProgressLines groups printed progress lines by phase and preserves their print order.
func parseProgressLines(t *testing.T, output string) map[string][]progressLine {
	t.Helper()
	phases := map[string][]progressLine{}
	for _, text := range strings.Split(strings.TrimSpace(output), "\n") {
		match := progressLinePattern.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		round, roundErr := strconv.Atoi(match[2])
		done, doneErr := strconv.ParseInt(match[3], 10, 64)
		total, totalErr := strconv.ParseInt(match[4], 10, 64)
		if roundErr != nil || doneErr != nil || totalErr != nil {
			t.Fatalf("progress line %q contains an invalid number", text)
		}
		phases[match[1]] = append(phases[match[1]], progressLine{text: text, round: round, done: done, total: total, rate: match[6], remaining: match[8]})
	}
	return phases
}

// TestCopySharedPrintsPhaseProgress runs copy-shared against a throwaway Milvus and the real embedding endpoint.
// The test checks that each copy and verification phase starts with Done 0, the phase total, an unknown rate, and an unknown estimate in the expected round.
// The test checks that each phase ends with Done equal to the phase total and the finished text in the expected round.
func TestCopySharedPrintsPhaseProgress(t *testing.T) {
	milvustest.RequireEnabled(t)
	address := milvustest.Start(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	ctx := context.Background()
	source := openRepairClient(t, address, vectorsearch.LayoutSingle)
	docs, manifest, distinctTexts := progressDocuments()
	if _, err := source.UpsertConversationDocuments(ctx, progressSourceCollectionID, docs, manifest); err != nil {
		t.Fatalf("write single layout: %v", err)
	}
	time.Sleep(repairVisibilityWait)

	baseURL := os.Getenv(repairEmbeddingBaseURLEnv)
	if baseURL == "" {
		baseURL = repairDefaultEmbeddingBaseURL
	}
	cfg := config.NewConfigWithDefaults()
	cfg.Conversation.Semantic.Backend = config.ConversationSemanticBackendMilvus
	cfg.Conversation.Semantic.IngestionEnabled = false
	cfg.Conversation.Semantic.MilvusAddress = address
	cfg.Conversation.Semantic.MilvusDatabase = "default"
	cfg.Conversation.Semantic.EmbeddingBaseURL = baseURL
	cfg.Conversation.Semantic.EmbeddingModel = repairEmbeddingModel
	cfg.Conversation.Semantic.EmbeddingDimension = repairEmbeddingDimension
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	factory := &cli.Factory{
		IOStreams: &cli.IOStreams{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
		Logger:    nil,
		Build:     cli.BuildInfo{Version: "test", Commit: "", Date: ""},
		Verbose:   func() bool { return false },
		Copy:      func() bool { return false },
		Config:    func() (*config.Config, error) { return cfg, nil },
	}
	options := copySharedOptions{
		FromCollectionID: progressSourceCollectionID,
		ToCollectionID:   progressTargetCollectionID,
		VerifyOnly:       false,
		Resume:           false,
		RepairRounds:     progressRepairRounds,
	}
	if err := runCopySharedIndex(ctx, factory, options); err != nil {
		t.Fatalf("copy-shared failed: %v\nstderr:\n%s", err, stderr.String())
	}
	rows := int64(len(docs))
	wantSummary := fmt.Sprintf("Copied %d rows. Verified %d source rows", rows, rows)
	if !strings.Contains(stdout.String(), wantSummary) {
		t.Fatalf("copy-shared printed %q; expected it to contain %q", stdout.String(), wantSummary)
	}

	phases := parseProgressLines(t, stderr.String())
	want := []struct {
		phase string
		round int
		total int64
	}{
		{phase: "copy_occurrences", round: 0, total: rows},
		{phase: "copy_vectors", round: 0, total: rows},
		{phase: "verify_rows", round: 1, total: rows},
		{phase: "verify_extra_rows", round: 1, total: rows},
		{phase: "verify_vector_references", round: 1, total: rows},
		{phase: "verify_vectors", round: 1, total: int64(distinctTexts)},
	}
	if len(phases) != len(want) {
		t.Fatalf("copy-shared printed progress for %d phases; expected %d:\n%s", len(phases), len(want), stderr.String())
	}
	for _, expected := range want {
		lines := phases[expected.phase]
		if len(lines) < 2 {
			t.Fatalf("phase %s printed %d progress lines; expected a start line and a final line:\n%s", expected.phase, len(lines), stderr.String())
		}
		first := lines[0]
		if first.done != 0 || first.total != expected.total || first.round != expected.round || first.rate != wantRateUnknown || first.remaining != wantEstimateUnknown {
			t.Fatalf("phase %s first line is %q; expected 0 of %d rows in round %d with an unknown rate and estimate", expected.phase, first.text, expected.total, expected.round)
		}
		last := lines[len(lines)-1]
		if last.done != expected.total || last.total != expected.total || last.round != expected.round || last.remaining != wantFinished {
			t.Fatalf("phase %s last line is %q; expected %d of %d rows in round %d with the finished text", expected.phase, last.text, expected.total, expected.total, expected.round)
		}
	}
}
