//go:build live

package live

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/lm-semantic-search/library/observation"
)

type embeddedOperationMeasurement struct {
	Message         string                `json:"msg"`
	RunID           string                `json:"run_id"`
	PID             int                   `json:"pid"`
	Purpose         observation.Purpose   `json:"purpose"`
	Operation       observation.Operation `json:"operation"`
	Outcome         observation.Outcome   `json:"outcome"`
	Duration        int64                 `json:"duration_ns"`
	StageRows       int                   `json:"stage_rows"`
	EmbeddingInputs int                   `json:"embedding_requested"`
	Acknowledged    int64                 `json:"vector_acknowledged"`
	DuplicateInputs int                   `json:"identity_duplicate_inputs"`
	SecondLookup    bool                  `json:"identity_second_lookup"`
	ProjectionRows  int                   `json:"projection_rows"`
	SourceRead      int                   `json:"source_read"`
	NewFields       int                   `json:"projection_new_fields"`
	UnchangedFields int                   `json:"projection_unchanged_fields"`
}

func readEmbeddedOperationMeasurements(t *testing.T, stateRoot string) []embeddedOperationMeasurement {
	t.Helper()
	root, err := os.OpenRoot(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var measurements []embeddedOperationMeasurement
	err = filepath.WalkDir(stateRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, filepath.Join("conversation", "semantic.jsonl")) {
			return nil
		}
		relative, err := filepath.Rel(stateRoot, path)
		if err != nil {
			return fmt.Errorf("resolve measurement path: %w", err)
		}
		file, err := root.Open(relative)
		if err != nil {
			return fmt.Errorf("open measurement log: %w", err)
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			var measurement embeddedOperationMeasurement
			if err := json.Unmarshal(scanner.Bytes(), &measurement); err != nil {
				return fmt.Errorf("decode measurement log: %w", err)
			}
			measurements = append(measurements, measurement)
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return measurements
}

func assertEmbeddedIngestionOperations(t *testing.T, stateRoot string) string {
	t.Helper()
	measurements := readEmbeddedOperationMeasurements(t, stateRoot)
	runID := ""
	for _, measurement := range measurements {
		if measurement.Message == "daemon.conversation_semantic_sync.pass_completed" && measurement.ProjectionRows == embeddedPublicRows {
			runID = measurement.RunID
			break
		}
	}
	if runID == "" {
		t.Fatal("completed initial ingestion has no explicit run identity")
	}
	var staged, requested, duplicates int
	var acknowledged int64
	for _, measurement := range measurements {
		if measurement.RunID != runID || measurement.Message != "daemon.conversation_semantic_embedded.operation_completed" {
			continue
		}
		if measurement.Purpose != observation.Ingestion || measurement.PID <= 0 || measurement.Duration < 0 || measurement.Outcome != observation.Success {
			t.Fatalf("initial ingestion operation is invalid: %+v", measurement)
		}
		switch measurement.Operation {
		case observation.Stage:
			staged += measurement.StageRows
		case observation.EmbeddingAttempt:
			requested += measurement.EmbeddingInputs
		case observation.UpsertCall:
			acknowledged += measurement.Acknowledged
		case observation.IdentitySelection:
			if !measurement.SecondLookup {
				duplicates += measurement.DuplicateInputs
			}
		case observation.EmbeddingValidation, observation.WriterAdmission, observation.CatalogTransaction, observation.StrongVerification:
		}
	}
	if staged != embeddedPublicRows || requested != embeddedPublicRows-2 || acknowledged != embeddedPublicRows-2 || duplicates != 2 {
		t.Fatalf("actual vector reuse measurements differ: staged=%d requested=%d acknowledged=%d duplicate_inputs=%d", staged, requested, acknowledged, duplicates)
	}
	return runID
}

func assertEmbeddedUnchangedOperations(t *testing.T, stateRoot, initialRunID string) {
	t.Helper()
	measurements := readEmbeddedOperationMeasurements(t, stateRoot)
	runID := ""
	for _, measurement := range measurements {
		if measurement.Message == "daemon.conversation_semantic_sync.pass_completed" && measurement.RunID != initialRunID && measurement.SourceRead == 1 && measurement.NewFields == 0 && measurement.UnchangedFields == 36 {
			runID = measurement.RunID
			break
		}
	}
	if runID == "" {
		t.Fatal("unchanged source has no completed independently identified ingestion pass")
	}
	for _, measurement := range measurements {
		if measurement.RunID == runID && measurement.Message == "daemon.conversation_semantic_embedded.operation_completed" && (measurement.Operation == observation.EmbeddingAttempt || measurement.Operation == observation.UpsertCall) {
			t.Fatalf("unchanged ingestion executed a model or vector-write operation: %+v", measurement)
		}
	}
}

func assertEmbeddedQueryOperations(t *testing.T, stateRoot string) {
	t.Helper()
	requested := 0
	for _, measurement := range readEmbeddedOperationMeasurements(t, stateRoot) {
		if measurement.Message != "daemon.conversation_semantic_embedded.operation_completed" || measurement.Purpose != observation.Query {
			continue
		}
		if measurement.RunID == "" || measurement.PID <= 0 || measurement.Duration < 0 || measurement.Outcome != observation.Success {
			t.Fatalf("public query operation is invalid: %+v", measurement)
		}
		if measurement.Operation == observation.UpsertCall {
			t.Fatalf("public query executed a vector write: %+v", measurement)
		}
		if measurement.Operation == observation.EmbeddingAttempt {
			requested += measurement.EmbeddingInputs
		}
	}
	if requested == 0 {
		t.Fatal("public queries recorded no actual query embedding request")
	}
}
