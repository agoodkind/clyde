//go:build live

package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/milvusclient"
	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/lm-semantic-search/library"
	librarymilvus "goodkind.io/lm-semantic-search/library/milvus"
	"goodkind.io/lm-semantic-search/library/observation"
)

const frozenCorpusDriverRequestEnv = "CLYDE_FROZEN_INGESTION_REQUEST"

const frozenCorpusMilvusAddressEnv = "CLYDE_FROZEN_MILVUS_ADDRESS"

// A 256-vector batch at 4096 dimensions uses 4 MiB of raw float payload.
const frozenVectorVerificationBatchSize = 256

//go:embed frozen_corpus_published_rows.sql
var frozenPublishedRowsQuery string

//go:embed frozen_corpus_published_scalars.sql
var frozenPublishedScalarsQuery string

//go:embed frozen_corpus_catalog_versions.sql
var frozenCatalogVersionsQuery string

//go:embed frozen_corpus_catalog_calibration_create.sql
var frozenCatalogCalibrationCreate string

//go:embed frozen_corpus_catalog_calibration_update.sql
var frozenCatalogCalibrationUpdate string

type frozenCatalogVersions struct {
	Data   int64 `json:"data_version"`
	Schema int64 `json:"schema_version"`
}

type frozenCorpusPassProof struct {
	Summary             frozenPassSummary `json:"summary"`
	RunID               string            `json:"run_id"`
	Completed           bool              `json:"completed"`
	EmbeddingAttempts   int               `json:"embedding_sdk_attempts"`
	RequestedInputs     int               `json:"embedding_requested_inputs"`
	VectorCalls         int               `json:"milvus_sdk_upsert_calls"`
	CatalogTransactions int               `json:"catalog_transactions"`
	Stages              int               `json:"stages"`
	FailedOperations    int               `json:"failed_operations"`
}

type frozenPassSummary struct {
	Admitted         int `json:"admitted"`
	Needed           int `json:"needed"`
	Deferred         int `json:"deferred"`
	PendingBlocked   int `json:"pending_blocked"`
	SourceFailed     int `json:"source_failed"`
	Suppressed       int `json:"source_failed_suppressed"`
	ProjectionFailed int `json:"projection_failed"`
	DeliveryFailed   int `json:"delivery_failed"`
	MetadataFailed   int `json:"metadata_reprojection_failed"`
	ReconcileFailed  int `json:"reconcile_failed"`
	Blocked          int `json:"blocked_owners"`
	Changed          int `json:"projection_changed_committed"`
	Withheld         int `json:"projection_withheld_fields"`
	ReplayDeferred   int `json:"persistence_replay_deferred_batches"`
	Generations      int `json:"searchable_generations"`
	Rows             int `json:"searchable_rows"`
	Conversations    int `json:"searchable_conversations"`
}

type frozenExpectedOwner struct {
	Source conversation.StampedRecord
	Seal   library.GenerationSeal
}

type frozenSweepProof struct {
	Namespace            frozenNamespaceInventory `json:"actual_namespace"`
	Phase                string                   `json:"phase"`
	Pass                 frozenCorpusPassProof    `json:"pass"`
	PublishedOwners      int                      `json:"published_owners"`
	PublishedOccurrences int                      `json:"published_occurrences"`
	ProvenSources        int                      `json:"proven_sources"`
	Missing              []string                 `json:"missing_owner_ids"`
	UnprovenSources      []string                 `json:"unproven_source_owner_ids"`
	CatalogVersions      *frozenCatalogVersions   `json:"catalog_versions,omitempty"`
	ElapsedMilliseconds  int64                    `json:"elapsed_ms"`
	Error                string                   `json:"error,omitempty"`
}

type frozenNamespaceInventory struct {
	OwnerIDs    []string `json:"owner_ids"`
	Owners      int64    `json:"owners"`
	Occurrences int64    `json:"occurrences"`
}

type frozenVectorVerifier struct {
	store       *librarymilvus.Store
	description string
	catalogUUID string
	bound       bool
}

type frozenCorpusProof struct {
	ExpectedOwnerIDs                []string              `json:"expected_source_owner_ids"`
	RejectedUnexpectedOwner         string                `json:"rejected_unexpected_owner,omitempty"`
	InitialPasses                   []frozenSweepProof    `json:"initial_passes"`
	RestartPasses                   []frozenSweepProof    `json:"restart_passes"`
	Complete                        bool                  `json:"complete"`
	Database                        string                `json:"database"`
	Records                         int                   `json:"records"`
	AdmittedOwners                  int                   `json:"admitted_owners"`
	Occurrences                     int                   `json:"occurrences"`
	First                           frozenCorpusPassProof `json:"first_pass"`
	Restart                         frozenCorpusPassProof `json:"restarted_unchanged_pass"`
	CatalogBeforeRestart            frozenCatalogVersions `json:"catalog_before_restart"`
	CatalogAfterRestart             frozenCatalogVersions `json:"catalog_after_restart"`
	Retained                        bool                  `json:"retained"`
	PublicCLIContextMappingVerified bool                  `json:"public_cli_context_mapping_verified"`
	Resumed                         bool                  `json:"resumed"`
}

// TestFrozenCorpusIngestionDriver runs only with an explicit guarded request.
// A successful retained catalog permits a later search-only CLI measurement.
func TestFrozenCorpusIngestionDriver(t *testing.T) {
	path := os.Getenv(frozenCorpusDriverRequestEnv)
	if path == "" {
		return
	}
	if !filepath.IsAbs(path) {
		t.Fatal("driver request path must be absolute")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var request frozenCorpusRequest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	proof, err := runFrozenCorpusIngestion(t, request, false)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(request.OutputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(proof)
	if err = errors.Join(encodeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestFrozenCorpusIngestionPreservesOriginalOwnersAcrossRestart(t *testing.T) {
	requireLiveLocalEmbeddingModel(t)
	request := createFrozenCorpusFixtureWithPadding(t, conversationSemanticBatchBytes)
	root := t.TempDir()
	request.RuntimeRoot = root
	request.OutputPath = filepath.Join(root, "proof.json")
	bindFrozenRuntime(t, root)
	request.Semantic.CatalogPath = filepath.Join(root, "catalog.sqlite")
	request.Semantic.LockPath = filepath.Join(root, "catalog.lock")
	request.Semantic.PoolID = "frozen-fixture"
	request.Semantic.CollectionID = "frozen-fixture"
	request.Semantic.MilvusCollection = "frozen_vectors"
	address, err := frozenCorpusMilvusAddress()
	if err != nil {
		t.Fatal(err)
	}
	request.Semantic.MilvusAddress = address
	request.Semantic.MilvusDatabase = "clyde_frozen_" + fmt.Sprint(time.Now().UnixNano())
	if registeredDatabase := os.Getenv("CLYDE_FROZEN_FIXTURE_DATABASE"); registeredDatabase != "" {
		request.Semantic.MilvusDatabase = registeredDatabase
	}
	request.Semantic.EmbeddingBaseURL = liveEmbeddingBaseURL
	request.Semantic.EmbeddingRequestTimeout = config.Duration(2 * time.Minute)
	proof, err := runFrozenCorpusIngestion(t, request, true)
	if err != nil {
		t.Fatal(err)
	}
	proofPath := request.OutputPath
	if evidenceRoot := os.Getenv("CLYDE_FROZEN_FIXTURE_EVIDENCE_ROOT"); evidenceRoot != "" {
		proofPath = filepath.Join(evidenceRoot, "frozen-fixture-proof.json")
	}
	file, err := os.OpenFile(proofPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	encodeErr := json.NewEncoder(file).Encode(proof)
	if err = errors.Join(encodeErr, file.Sync(), file.Close()); err != nil {
		t.Fatal(err)
	}
	if !proof.Complete || proof.Records != 4 || proof.AdmittedOwners != 4 || proof.Occurrences != 4 || proof.First.EmbeddingAttempts == 0 || proof.First.VectorCalls == 0 || proof.Restart.EmbeddingAttempts != 0 || proof.Restart.VectorCalls != 0 || proof.Restart.Stages != 0 || proof.CatalogBeforeRestart != proof.CatalogAfterRestart {
		t.Fatalf("real frozen worker proof=%+v", proof)
	}
	if len(proof.InitialPasses) < 2 || len(proof.RestartPasses) < 2 || proof.InitialPasses[0].PublishedOwners >= 4 || proof.RestartPasses[0].ProvenSources >= 4 {
		t.Fatalf("real source budget did not split initial and restart sweeps: %+v", proof)
	}
	if proof.RejectedUnexpectedOwner == "" {
		t.Fatal("real catalog did not reject an unexpected committed owner")
	}
	t.Logf("verified frozen worker proof=%+v", proof)
}

func runFrozenCorpusIngestion(t *testing.T, request frozenCorpusRequest, verifyUnexpectedOwner bool) (proof frozenCorpusProof, resultErr error) {
	t.Helper()
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	index, err := openFrozenCorpusIndex(ctx, request)
	if err != nil {
		return proof, err
	}
	if err = validateFrozenRuntime(request); err != nil {
		return proof, err
	}
	if request.CompletionTimeoutSeconds <= 0 || request.CompletionTimeoutSeconds > int((1<<63-1)/int64(time.Second)) || request.ExpectedOwners <= 0 || request.ExpectedOccurrences <= 0 {
		return proof, errors.New("positive completion deadline and exact source counts required")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(request.CompletionTimeoutSeconds)*time.Second)
	defer cancel()
	expected, err := preflightFrozenOwners(ctx, index)
	if err != nil {
		return proof, err
	}
	admin, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: request.Semantic.MilvusAddress})
	if err != nil {
		return proof, err
	}
	existing, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil {
		return proof, errors.Join(err, closeFrozenAdmin(ctx, admin))
	}
	database := request.Semantic.MilvusDatabase
	if request.Resume && !slices.Contains(existing, database) {
		return proof, errors.Join(errors.New("resume database is absent; preserved catalog cannot prove missing vectors"), closeFrozenAdmin(ctx, admin))
	}
	if !request.Resume && slices.Contains(existing, database) {
		return proof, errors.Join(errors.New("explicit fresh database already exists"), closeFrozenAdmin(ctx, admin))
	}
	proof.Database = database
	if request.Resume {
		t.Logf("verified existing frozen database %s and exact source proofs at isolated runtime %s before replay", database, request.RuntimeRoot)
	} else {
		t.Logf("verified absent frozen database %s at isolated runtime %s before create request", database, request.RuntimeRoot)
	}
	defer func() {
		if !request.RetainOnSuccess && !request.Resume {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			databases, inspectErr := admin.ListDatabase(cleanupCtx, milvusclient.NewListDatabaseOption())
			if inspectErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("inspect exact fresh database %s for cleanup: %w", database, inspectErr), closeFrozenAdmin(ctx, admin))
				return
			}
			if slices.Contains(databases, database) {
				resultErr = errors.Join(resultErr, dropFrozenDatabase(cleanupCtx, admin, request.Semantic.MilvusAddress, database))
			}
			resultErr = errors.Join(resultErr, closeFrozenAdmin(ctx, admin))
		} else {
			resultErr = errors.Join(resultErr, closeFrozenAdmin(ctx, admin))
		}
	}()
	if !request.Resume {
		if err = admin.CreateDatabase(ctx, milvusclient.NewCreateDatabaseOption(database)); err != nil {
			return proof, err
		}
	}
	logPath := filepath.Join(request.RuntimeRoot, "frozen-ingestion-operations.jsonl")
	if evidenceRoot := os.Getenv("CLYDE_FROZEN_FIXTURE_EVIDENCE_ROOT"); evidenceRoot != "" {
		if filepath.Dir(evidenceRoot) != "/private/tmp" || !strings.HasPrefix(filepath.Base(evidenceRoot), "lms-frozen-fixture-") {
			return proof, errors.New("fixture evidence root must be a direct /private/tmp/lms-frozen-fixture- directory")
		}
		info, inspectErr := os.Lstat(evidenceRoot)
		if inspectErr != nil {
			return proof, inspectErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return proof, errors.New("fixture evidence root must be an existing physical directory")
		}
		logPath = filepath.Join(evidenceRoot, "frozen-ingestion-operations.jsonl")
	}
	if request.Resume {
		logPath = strings.TrimSuffix(logPath, ".jsonl") + "-resume-" + fmt.Sprint(time.Now().UnixNano()) + ".jsonl"
	}
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return proof, err
	}
	defer func() { resultErr = errors.Join(resultErr, logFile.Close()) }()
	logger := slog.New(slog.NewJSONHandler(logFile, &slog.HandlerOptions{Level: slog.LevelDebug}))
	kinds, err := SemanticContentKinds(request.Semantic)
	if err != nil {
		return proof, err
	}
	createWorker := func() *conversationSemanticSyncWorker {
		worker := newConversationSemanticSyncWorker(index, request.Semantic.CollectionID, logger, kinds)
		worker.embedded = newEmbeddedConversationSync(request.Semantic, conversationSemanticOutboxPath(request.Semantic.PoolID), newEmbeddedSemanticStatus(), index)
		return worker
	}
	worker := createWorker()
	defer func() { resultErr = errors.Join(resultErr, worker.embedded.closeStore(context.WithoutCancel(ctx))) }()
	passFile, err := os.OpenFile(logPath+".passes.jsonl", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return proof, err
	}
	defer func() { resultErr = errors.Join(resultErr, passFile.Close()) }()
	initial, err := runFrozenCompletion(ctx, worker, expected, logPath, passFile, nil, frozenCatalogVersions{})
	if err != nil {
		return proof, err
	}
	firstOwners, occurrences, err := verifyFrozenPublishedOwners(t, ctx, index, worker, expected)
	if err != nil {
		return proof, err
	}
	var rejectedOwner string
	if verifyUnexpectedOwner {
		rejectedOwner, err = verifyFrozenUnexpectedOwner(ctx, worker.embedded.store.library, worker.embedded.store.namespace.ID, expected)
		if err != nil {
			return proof, err
		}
	}
	catalog := openLiveReadOnly(t, request.Semantic.CatalogPath)
	defer catalog.Close()
	observer, err := catalog.Conn(ctx)
	if err != nil {
		return proof, err
	}
	defer observer.Close()
	beforeRestart, err := readFrozenCatalogVersions(ctx, observer)
	if err != nil {
		return proof, err
	}
	if err = worker.embedded.closeStore(ctx); err != nil {
		return proof, err
	}
	worker = createWorker()
	restarted, err := runFrozenCompletion(ctx, worker, expected, logPath, passFile, observer, beforeRestart)
	if err != nil {
		return proof, err
	}
	restartedOwners, restartedOccurrences, err := verifyFrozenPublishedOwners(t, ctx, index, worker, expected)
	if err != nil {
		return proof, err
	}
	if !reflect.DeepEqual(firstOwners, restartedOwners) || occurrences != restartedOccurrences {
		return proof, errors.New("restart changed published owner generations or rows")
	}
	afterRestart, err := readFrozenCatalogVersions(ctx, observer)
	if err != nil {
		return proof, err
	}
	if beforeRestart != afterRestart {
		return proof, fmt.Errorf("unchanged restart committed catalog changes: before=%+v after=%+v", beforeRestart, afterRestart)
	}
	proof = frozenCorpusProof{InitialPasses: initial, RestartPasses: restarted, Complete: true, Database: database, Records: len(index.records), AdmittedOwners: len(firstOwners), Occurrences: occurrences, First: initial[0].Pass, Restart: restarted[0].Pass, CatalogBeforeRestart: beforeRestart, CatalogAfterRestart: afterRestart, Retained: request.RetainOnSuccess, PublicCLIContextMappingVerified: false}
	proof.RejectedUnexpectedOwner = rejectedOwner
	proof.Resumed = request.Resume
	for owner := range expected {
		proof.ExpectedOwnerIDs = append(proof.ExpectedOwnerIDs, owner)
	}
	slices.Sort(proof.ExpectedOwnerIDs)
	return proof, nil
}

func verifyFrozenUnexpectedOwner(ctx context.Context, catalog *library.Library, namespace string, expected map[string]frozenExpectedOwner) (string, error) {
	owners := make([]string, 0, len(expected))
	for owner := range expected {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	if len(owners) != 4 {
		return "", errors.New("unexpected-owner regression requires four committed owners")
	}
	rejected := owners[0]
	subset := make(map[string]frozenExpectedOwner, len(expected)-1)
	for owner, source := range expected {
		if owner != rejected {
			subset[owner] = source
		}
	}
	inventory, err := readFrozenNamespaceInventory(ctx, catalog, namespace, subset, true)
	if err == nil || err.Error() != "actual frozen namespace has unexpected committed owner "+rejected {
		return "", fmt.Errorf("expected exact unexpected owner %s rejection, got %v", rejected, err)
	}
	if inventory.Owners != 4 || inventory.Occurrences != 4 {
		return "", fmt.Errorf("unexpected-owner regression changed actual inventory: %+v", inventory)
	}
	return rejected, nil
}

func readFrozenCatalogVersions(ctx context.Context, connection *sql.Conn) (frozenCatalogVersions, error) {
	var versions frozenCatalogVersions
	err := connection.QueryRowContext(ctx, frozenCatalogVersionsQuery).Scan(&versions.Data, &versions.Schema)
	return versions, err
}

func preflightFrozenOwners(ctx context.Context, index *frozenCorpusIndex) (map[string]frozenExpectedOwner, error) {
	kinds, err := SemanticContentKinds(index.request.Semantic)
	if err != nil {
		return nil, fmt.Errorf("resolve frozen preflight content: %w", err)
	}
	expected := make(map[string]frozenExpectedOwner)
	total := 0
	for _, source := range index.records {
		if err = ctx.Err(); err != nil {
			return nil, fmt.Errorf("project frozen preflight: %w", err)
		}
		admission, admissionErr := ProjectEmbeddedConversationOccurrences(ctx, index.request.Semantic, source.Record, nil, true)
		if admissionErr != nil {
			return nil, fmt.Errorf("admit frozen owner %s: %w", source.Record.ID, admissionErr)
		}
		if !admission.Admitted {
			continue
		}
		messages, loadErr := index.LoadMessagesWithOptions(source.Record, SemanticConversationLoadOptions(kinds))
		if loadErr != nil {
			return nil, fmt.Errorf("load frozen owner %s: %w", source.Record.ID, loadErr)
		}
		projected, projectionErr := ProjectEmbeddedConversationOccurrences(ctx, index.request.Semantic, source.Record, messages, true)
		if projectionErr != nil {
			return nil, fmt.Errorf("project frozen owner %s: %w", source.Record.ID, projectionErr)
		}
		if projected.WithheldOpenFields != 0 || len(projected.Occurrences) == 0 {
			return nil, fmt.Errorf("frozen owner %s is empty or has withheld fields", source.Record.ID)
		}
		seal, sealErr := library.SealRows(projected.Occurrences)
		if sealErr != nil {
			return nil, fmt.Errorf("seal frozen owner %s: %w", source.Record.ID, sealErr)
		}
		expected[source.Record.ID] = frozenExpectedOwner{Source: source, Seal: seal}
		total += len(projected.Occurrences)
	}
	if len(expected) != index.request.ExpectedOwners || total != index.request.ExpectedOccurrences {
		return nil, fmt.Errorf("frozen preflight counts owners=%d occurrences=%d differ from required owners=%d occurrences=%d", len(expected), total, index.request.ExpectedOwners, index.request.ExpectedOccurrences)
	}
	return expected, nil
}

func runFrozenCompletion(ctx context.Context, worker *conversationSemanticSyncWorker, expected map[string]frozenExpectedOwner, logPath string, evidence *os.File, observer *sql.Conn, versions frozenCatalogVersions) ([]frozenSweepProof, error) {
	var passes []frozenSweepProof
	started := time.Now()
	priorOwners, priorRows, priorSources := 0, 0, 0
	priorRun := ""
	completedOwners := make(map[string][32]byte)
	for attempt := 0; attempt <= len(expected); attempt++ {
		entry := frozenSweepProof{Phase: "initial"}
		if observer != nil {
			entry.Phase = "restart"
		}
		passErr := worker.runEmbeddedPass(ctx)
		entry.Pass, passErr = readFrozenPassResult(logPath, passErr)
		if passErr == nil && entry.Pass.RunID == priorRun {
			passErr = errors.New("frozen pass reused an observation RunID")
		}
		if passErr == nil {
			passErr = validateFrozenPass(entry.Pass, observer != nil)
		}
		if passErr == nil {
			passErr = readFrozenCoverage(ctx, worker, expected, completedOwners, &entry)
		}
		if passErr == nil && observer != nil {
			current, versionErr := readFrozenCatalogVersions(ctx, observer)
			entry.CatalogVersions = &current
			passErr = versionErr
			if passErr == nil && current != versions {
				passErr = fmt.Errorf("unchanged restart changed catalog versions: before=%+v after=%+v", versions, current)
			}
		}
		entry.ElapsedMilliseconds = time.Since(started).Milliseconds()
		complete := len(entry.Missing) == 0 && entry.PublishedOwners == len(expected)
		if observer != nil {
			complete = complete && entry.ProvenSources == len(expected)
		}
		if passErr == nil && !complete {
			progress := entry.PublishedOwners > priorOwners || entry.PublishedOccurrences > priorRows
			if observer != nil {
				progress = entry.ProvenSources > priorSources
			}
			if !progress {
				passErr = errors.New("bounded frozen pass made no committed coverage or source-proof progress")
			}
		}
		if passErr == nil && complete {
			passErr = requireFrozenOutboxComplete(ctx, worker)
		}
		if passErr != nil {
			entry.Error = passErr.Error()
		}
		if err := json.NewEncoder(evidence).Encode(entry); err != nil {
			return passes, errors.Join(passErr, fmt.Errorf("write frozen pass evidence: %w", err))
		}
		if err := evidence.Sync(); err != nil {
			return passes, errors.Join(passErr, fmt.Errorf("sync frozen pass evidence: %w", err))
		}
		passes = append(passes, entry)
		if passErr != nil {
			return passes, fmt.Errorf("frozen %s pass %d: %w", entry.Phase, attempt+1, passErr)
		}
		if complete {
			return passes, nil
		}
		priorOwners, priorRows, priorSources = entry.PublishedOwners, entry.PublishedOccurrences, entry.ProvenSources
		priorRun = entry.Pass.RunID
	}
	return passes, errors.New("frozen completion exceeded the bounded pass count")
}

func readFrozenPassResult(path string, passErr error) (frozenCorpusPassProof, error) {
	proof, readErr := readFrozenPassProof(path)
	return proof, errors.Join(passErr, readErr)
}

func validateFrozenPass(pass frozenCorpusPassProof, restart bool) error {
	summary := pass.Summary
	if pass.FailedOperations != 0 || summary.SourceFailed != 0 || summary.Suppressed != 0 || summary.ProjectionFailed != 0 || summary.DeliveryFailed != 0 || summary.MetadataFailed != 0 || summary.ReconcileFailed != 0 || summary.Blocked != 0 || summary.Changed != 0 || summary.Withheld != 0 {
		return fmt.Errorf("frozen pass has observed or reported failures: %+v", pass)
	}
	if restart && (pass.EmbeddingAttempts != 0 || pass.RequestedInputs != 0 || pass.VectorCalls != 0 || pass.Stages != 0) {
		return fmt.Errorf("unchanged restarted pass performed model or write operations: %+v", pass)
	}
	return nil
}

func readFrozenCoverage(ctx context.Context, worker *conversationSemanticSyncWorker, expected map[string]frozenExpectedOwner, completed map[string][32]byte, entry *frozenSweepProof) error {
	inventory, err := readFrozenNamespaceInventory(ctx, worker.embedded.store.library, worker.embedded.store.namespace.ID, expected, false)
	entry.Namespace = inventory
	if err != nil {
		return err
	}
	for owner, expectation := range expected {
		published, err := worker.embedded.store.library.ListOwnerOccurrences(ctx, worker.embedded.store.namespace.ID, owner)
		if err != nil {
			return fmt.Errorf("read frozen coverage of %s: %w", owner, err)
		}
		if uint64(len(published.Rows)) > expectation.Seal.RowCount {
			return fmt.Errorf("frozen owner %s has unexpected extra rows", owner)
		}
		entry.PublishedOccurrences += len(published.Rows)
		if uint64(len(published.Rows)) == expectation.Seal.RowCount && published.State.GenerationOrder > 0 {
			encoded, encodeErr := json.Marshal(published)
			if encodeErr != nil {
				return fmt.Errorf("seal frozen public snapshot %s: %w", owner, encodeErr)
			}
			digest := sha256.Sum256(encoded)
			if prior, found := completed[owner]; found && prior != digest {
				return fmt.Errorf("completed frozen owner %s changed generation or rows", owner)
			}
			completed[owner] = digest
			entry.PublishedOwners++
		} else {
			if _, found := completed[owner]; found {
				return fmt.Errorf("completed frozen owner %s lost published rows", owner)
			}
			entry.Missing = append(entry.Missing, owner)
		}
		if source, found := worker.embedded.processedSources[owner]; found {
			if !reflect.DeepEqual(source, expectation.Source) {
				return fmt.Errorf("processed source identity changed for frozen owner %s", owner)
			}
			entry.ProvenSources++
		} else {
			entry.UnprovenSources = append(entry.UnprovenSources, owner)
		}
	}
	slices.Sort(entry.Missing)
	slices.Sort(entry.UnprovenSources)
	if int64(entry.PublishedOccurrences) != inventory.Occurrences {
		return errors.New("expected-owner row reads differ from actual namespace occurrence count")
	}
	return nil
}

func readFrozenNamespaceInventory(ctx context.Context, catalog *library.Library, namespace string, expected map[string]frozenExpectedOwner, complete bool) (frozenNamespaceInventory, error) {
	var inventory frozenNamespaceInventory
	stats, err := catalog.NamespaceStats(ctx, namespace)
	if err != nil {
		return inventory, fmt.Errorf("count actual frozen namespace: %w", err)
	}
	inventory.Owners, inventory.Occurrences = stats.Owners, stats.Occurrences
	inventory.OwnerIDs, err = catalog.ListOwners(ctx, namespace)
	if err != nil {
		return inventory, fmt.Errorf("enumerate actual frozen namespace owners: %w", err)
	}
	if inventory.Owners != int64(len(inventory.OwnerIDs)) {
		return inventory, errors.New("actual namespace owner enumeration differs from its catalog count")
	}
	for _, owner := range inventory.OwnerIDs {
		if _, found := expected[owner]; !found {
			return inventory, fmt.Errorf("actual frozen namespace has unexpected committed owner %s", owner)
		}
	}
	if !complete {
		return inventory, nil
	}
	wanted := make([]string, 0, len(expected))
	var occurrences uint64
	for owner, expectation := range expected {
		wanted = append(wanted, owner)
		occurrences += expectation.Seal.RowCount
	}
	slices.Sort(wanted)
	if !slices.Equal(inventory.OwnerIDs, wanted) || inventory.Occurrences < 0 || uint64(inventory.Occurrences) != occurrences {
		return inventory, fmt.Errorf("actual frozen namespace is incomplete: owners=%d occurrences=%d", inventory.Owners, inventory.Occurrences)
	}
	return inventory, nil
}

func requireFrozenOutboxComplete(ctx context.Context, worker *conversationSemanticSyncWorker) error {
	outbox := worker.embedded.store.outbox
	batches, err := outbox.pendingBatches(ctx)
	if err != nil {
		return fmt.Errorf("read pending frozen batches: %w", err)
	}
	projections, err := outbox.pendingProjections(ctx)
	if err != nil {
		return fmt.Errorf("read pending frozen projections: %w", err)
	}
	blocked, err := outbox.blockedOwners(ctx, worker.embedded.store.namespace.ID)
	if err != nil {
		return fmt.Errorf("read blocked frozen owners: %w", err)
	}
	if len(batches)+len(projections)+len(blocked) != 0 {
		return errors.New("frozen coverage retains pending or blocked outbox work")
	}
	return nil
}

func TestFrozenCatalogVersionObserverDetectsCommittedWrites(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	dataSource := "file:" + path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL"
	writer, err := sql.Open("sqlite3", dataSource)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	if _, err = writer.ExecContext(ctx, frozenCatalogCalibrationCreate); err != nil {
		t.Fatal(err)
	}
	catalog := openLiveReadOnly(t, path)
	defer catalog.Close()
	observer, err := catalog.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	before, err := readFrozenCatalogVersions(ctx, observer)
	if err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	writer, err = sql.Open("sqlite3", dataSource)
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var selected frozenCatalogVersions
	if err = transaction.QueryRowContext(ctx, frozenCatalogVersionsQuery).Scan(&selected.Data, &selected.Schema); err != nil {
		t.Fatal(err)
	}
	if err = transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	afterRead, err := readFrozenCatalogVersions(ctx, observer)
	if err != nil {
		t.Fatal(err)
	}
	if afterRead != before {
		t.Fatalf("SELECT transaction changed observed versions: before=%+v after=%+v", before, afterRead)
	}
	if _, err = writer.ExecContext(ctx, frozenCatalogCalibrationUpdate); err != nil {
		t.Fatal(err)
	}
	afterWrite, err := readFrozenCatalogVersions(ctx, observer)
	if err != nil {
		t.Fatal(err)
	}
	if afterWrite.Data == before.Data || afterWrite.Schema != before.Schema {
		t.Fatalf("committed UPDATE did not change only observed data version: before=%+v after=%+v", before, afterWrite)
	}
	t.Logf("real SQLite observer calibration before=%+v after_select=%+v after_update=%+v", before, afterRead, afterWrite)
}

func validateFrozenRuntime(request frozenCorpusRequest) error {
	root := request.RuntimeRoot
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) || root == request.SnapshotRoot || root == request.ReadRoot || root == request.OriginalHome || strings.HasPrefix(root, request.SnapshotRoot+string(filepath.Separator)) || strings.HasPrefix(root, request.ReadRoot+string(filepath.Separator)) {
		return errors.New("runtime root must be separate from frozen and original provider roots")
	}
	for _, relative := range []string{".claude", ".codex", ".copilot", ".cursor", "Library/Application Support/Cursor", "Library/Application Support/Zed"} {
		providerRoot := filepath.Join(request.OriginalHome, relative)
		if root == providerRoot || strings.HasPrefix(root, providerRoot+string(filepath.Separator)) {
			return errors.New("runtime root overlaps an original provider store")
		}
	}
	for _, path := range []string{request.Semantic.CatalogPath, request.Semantic.LockPath, request.OutputPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || !strings.HasPrefix(path, root+string(filepath.Separator)) {
			return errors.New("catalog, lock, and result must be under explicit isolated runtime root")
		}
		info, err := os.Lstat(path)
		if request.Resume && path != request.OutputPath {
			if err != nil || !info.Mode().IsRegular() {
				return errors.New("resume catalog and lock must be existing regular files")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("runtime catalog, lock, or result already exists or cannot be inspected")
		}
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		value := os.Getenv(name)
		if !filepath.IsAbs(value) || !strings.HasPrefix(value, root+string(filepath.Separator)) {
			return fmt.Errorf("isolated runtime binding required: %s", name)
		}
	}
	semantic := request.Semantic
	address, err := frozenCorpusMilvusAddress()
	if err != nil {
		return err
	}
	if semantic.EmbeddingBaseURL != liveEmbeddingBaseURL && semantic.EmbeddingBaseURL != "http://[::1]:5400/v1" && semantic.EmbeddingBaseURL != "http://127.0.0.1:5400/v1" {
		return errors.New("frozen embedding URL must be http://localhost:5400/v1, http://[::1]:5400/v1, or http://127.0.0.1:5400/v1")
	}
	if semantic.MilvusAddress != address || !strings.HasPrefix(semantic.MilvusDatabase, "clyde_frozen_") || semantic.MilvusCollection == "" || semantic.CollectionID == "" || semantic.PoolID == "" {
		return errors.New("driver requires explicit fresh clyde_frozen_ database and approved isolated Mac endpoints")
	}
	info, err := os.Lstat(conversationSemanticOutboxPath(semantic.PoolID))
	if request.Resume {
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("resume outbox must be an existing regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("isolated outbox already exists or cannot be inspected")
	}
	return nil
}

func frozenCorpusMilvusAddress() (string, error) {
	address := os.Getenv(frozenCorpusMilvusAddressEnv)
	if address == "" {
		address = liveMilvusAddress
	}
	if address != "localhost:39530" && address != "localhost:39630" {
		return "", errors.New("frozen Milvus address must be localhost:39530 or localhost:39630")
	}
	return address, nil
}

func closeFrozenAdmin(ctx context.Context, admin *milvusclient.Client) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return admin.Close(closeCtx)
}

func dropFrozenDatabase(ctx context.Context, admin *milvusclient.Client, address, database string) (resultErr error) {
	client, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: address, DBName: database})
	if err != nil {
		return fmt.Errorf("connect to frozen database %s at %s for cleanup: %w", database, address, err)
	}
	defer func() { resultErr = errors.Join(resultErr, closeFrozenAdmin(ctx, client)) }()
	collections, err := client.ListCollections(ctx, milvusclient.NewListCollectionOption())
	if err != nil {
		return fmt.Errorf("list frozen database %s collections for cleanup: %w", database, err)
	}
	for _, collection := range collections {
		if err = client.DropCollection(ctx, milvusclient.NewDropCollectionOption(collection)); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("drop frozen collection %s in %s: %w", collection, database, err))
		}
	}
	if resultErr != nil {
		return resultErr
	}
	if err = admin.DropDatabase(ctx, milvusclient.NewDropDatabaseOption(database)); err != nil {
		return fmt.Errorf("drop frozen database %s: %w", database, err)
	}
	remaining, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil {
		return fmt.Errorf("verify frozen database %s deletion: %w", database, err)
	}
	if slices.Contains(remaining, database) {
		return fmt.Errorf("frozen database %s remains after cleanup", database)
	}
	return nil
}

func verifyFrozenPublishedOwners(t *testing.T, ctx context.Context, index *frozenCorpusIndex, worker *conversationSemanticSyncWorker, expectedOwners map[string]frozenExpectedOwner) (map[string]library.OwnerOccurrences, int, error) {
	if _, err := readFrozenNamespaceInventory(ctx, worker.embedded.store.library, worker.embedded.store.namespace.ID, expectedOwners, true); err != nil {
		return nil, 0, err
	}
	database := openLiveReadOnly(t, index.request.Semantic.CatalogPath)
	defer database.Close()
	vectors, err := librarymilvus.New(worker.embedded.store.milvusClient, librarymilvus.Config{Database: index.request.Semantic.MilvusDatabase, Collection: index.request.Semantic.MilvusCollection})
	if err != nil {
		return nil, 0, err
	}
	collection, err := worker.embedded.store.milvusClient.DescribeCollection(ctx, milvusclient.NewDescribeCollectionOption(index.request.Semantic.MilvusCollection))
	if err != nil {
		return nil, 0, err
	}
	if collection.Schema == nil || collection.Schema.Description == "" {
		return nil, 0, errors.New("published collection has no catalog binding description")
	}
	var binding struct {
		CatalogUUID string `json:"catalog_uuid"`
	}
	if err = json.Unmarshal([]byte(collection.Schema.Description), &binding); err != nil {
		return nil, 0, err
	}
	if binding.CatalogUUID == "" {
		return nil, 0, errors.New("published collection binding has no catalog UUID")
	}
	verifier := &frozenVectorVerifier{store: vectors, description: collection.Schema.Description, catalogUUID: binding.CatalogUUID}
	result := make(map[string]library.OwnerOccurrences)
	count := 0
	for _, stamped := range index.records {
		record := stamped.Record
		admission, err := ProjectEmbeddedConversationOccurrences(ctx, index.request.Semantic, record, nil, true)
		if err != nil {
			return nil, 0, err
		}
		if !admission.Admitted {
			continue
		}
		messages, err := index.LoadMessagesWithOptions(record, SemanticConversationLoadOptions(worker.contentKinds))
		if err != nil {
			return nil, 0, err
		}
		projected, err := ProjectEmbeddedConversationOccurrences(ctx, index.request.Semantic, record, messages, true)
		if err != nil {
			return nil, 0, err
		}
		if projected.WithheldOpenFields != 0 {
			return nil, 0, fmt.Errorf("frozen source has withheld open fields: %s", record.ID)
		}
		seal, sealErr := library.SealRows(projected.Occurrences)
		if sealErr != nil {
			return nil, 0, sealErr
		}
		expectation, found := expectedOwners[record.ID]
		if !found || expectation.Seal != seal {
			return nil, 0, fmt.Errorf("source seal changed since preflight for owner %s", record.ID)
		}
		published, err := worker.embedded.store.library.ListOwnerOccurrences(ctx, worker.embedded.store.namespace.ID, record.ID)
		if err != nil {
			return nil, 0, err
		}
		expected := make([]string, 0, len(projected.Occurrences))
		for _, occurrence := range projected.Occurrences {
			expected = append(expected, occurrence.RowKey)
		}
		slices.Sort(expected)
		actual := make([]string, 0, len(published.Rows))
		for _, occurrence := range published.Rows {
			actual = append(actual, occurrence.RowKey)
		}
		if !slices.Equal(expected, actual) || (len(expected) > 0 && published.State.GenerationOrder == 0) {
			return nil, 0, fmt.Errorf("public published occurrence identities differ for original owner %s", record.ID)
		}
		if err = verifyFrozenPublishedContent(ctx, database, verifier, index.request, worker.embedded.store.namespace.ID, record.ID, projected.Occurrences); err != nil {
			return nil, 0, err
		}
		count += len(actual)
		result[record.ID] = published
	}
	return result, count, nil
}

func verifyFrozenPublishedContent(ctx context.Context, database *sql.DB, verifier *frozenVectorVerifier, request frozenCorpusRequest, namespace, owner string, expected []library.Occurrence) error {
	scalars, err := readFrozenPublishedScalars(ctx, database, namespace, owner)
	if err != nil {
		return err
	}
	rows, err := database.QueryContext(ctx, frozenPublishedRowsQuery, namespace, owner)
	if err != nil {
		return err
	}
	defer rows.Close()
	byKey := make(map[string]library.Occurrence, len(expected))
	for _, occurrence := range expected {
		byKey[occurrence.RowKey] = occurrence
	}
	actual := make([]library.Occurrence, 0, len(expected))
	identities := make([]library.VectorIdentity, 0, len(expected))
	for rows.Next() {
		var occurrence library.Occurrence
		var identity library.VectorIdentity
		var sourceDigest, searchDigest, inputDigest, model, normalization, state, catalogUUID string
		var input []byte
		var sourceLength, dimension int
		if err = rows.Scan(&occurrence.RowKey, &occurrence.SortKey, &occurrence.SourceText, &sourceDigest, &searchDigest, &sourceLength, &identity.ID, &identity.IdentityDigest, &inputDigest, &input, &model, &dimension, &normalization, &identity.Checksum, &state, &catalogUUID); err != nil {
			return err
		}
		if catalogUUID != verifier.catalogUUID {
			return errors.New("published collection catalog UUID differs from SQLite store identity")
		}
		prepared, found := byKey[occurrence.RowKey]
		if !found {
			return fmt.Errorf("unexpected published row for owner %s", owner)
		}
		occurrence.EmbeddingInput = string(input)
		occurrence.SearchText = prepared.SearchText
		occurrence.Scalars = scalars[occurrence.RowKey]
		if sourceDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(prepared.SourceText))) || sourceLength != len(prepared.SourceText) || searchDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(prepared.SearchText))) || inputDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(prepared.EmbeddingInput))) {
			return fmt.Errorf("published content digest differs for owner %s row %s", owner, occurrence.RowKey)
		}
		if model != request.Model.Name+"@"+request.Model.Revision || dimension != request.Model.Dimension || normalization != request.Model.Normalization || state != "verified" || identity.ID == "" || identity.IdentityDigest == "" || identity.Checksum == "" {
			return fmt.Errorf("published vector descriptor differs for owner %s row %s", owner, occurrence.RowKey)
		}
		if err = compareFrozenOccurrence(owner, prepared, occurrence); err != nil {
			return err
		}
		actual = append(actual, occurrence)
		identities = append(identities, identity)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	expectedSeal, err := library.SealRows(expected)
	if err != nil {
		return err
	}
	actualSeal, err := library.SealRows(actual)
	if err != nil {
		return err
	}
	if actualSeal != expectedSeal {
		return fmt.Errorf("published source, prepared input, or scalar coordinates differ for owner %s", owner)
	}
	if !verifier.bound && len(identities) > 0 {
		if err = verifier.store.BindCatalog(ctx, verifier.description); err != nil {
			return err
		}
		verifier.bound = true
	}
	return verifyFrozenVectorBatches(ctx, verifier.store, identities)
}

func verifyFrozenVectorBatches(ctx context.Context, store *librarymilvus.Store, identities []library.VectorIdentity) error {
	for start := 0; start < len(identities); start += frozenVectorVerificationBatchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+frozenVectorVerificationBatchSize, len(identities))
		if err := store.VerifyStrong(ctx, identities[start:end]); err != nil {
			return fmt.Errorf("verify frozen canonical vectors [%d:%d] of %d: %w", start, end, len(identities), err)
		}
		slog.InfoContext(ctx, "frozen canonical vector batch verified", "start", start, "end", end, "identities", len(identities))
	}
	return nil
}

func readFrozenPublishedScalars(ctx context.Context, database *sql.DB, namespace, owner string) (map[string]map[string]library.ScalarValue, error) {
	rows, err := database.QueryContext(ctx, frozenPublishedScalarsQuery, namespace, owner, namespace, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]map[string]library.ScalarValue)
	for rows.Next() {
		var rowKey, name string
		var value library.ScalarValue
		var text sql.NullString
		var integer, boolean sql.NullInt64
		if err = rows.Scan(&rowKey, &name, &value.Type, &text, &integer, &boolean, &value.Null); err != nil {
			return nil, err
		}
		value.String = text.String
		value.Int64 = integer.Int64
		value.Bool = boolean.Int64 != 0
		if result[rowKey] == nil {
			result[rowKey] = make(map[string]library.ScalarValue)
		}
		result[rowKey][name] = value
	}
	return result, rows.Err()
}

func compareFrozenOccurrence(owner string, expected, actual library.Occurrence) error {
	for _, field := range []struct {
		name     string
		expected string
		actual   string
	}{
		{name: "sort_key", expected: expected.SortKey, actual: actual.SortKey},
		{name: "source_text", expected: expected.SourceText, actual: actual.SourceText},
		{name: "embedding_input", expected: expected.EmbeddingInput, actual: actual.EmbeddingInput},
	} {
		if field.expected != field.actual {
			return fmt.Errorf("published %s differs for owner %s row %s", field.name, owner, expected.RowKey)
		}
	}
	keys := make([]string, 0, len(expected.Scalars)+len(actual.Scalars))
	for key := range expected.Scalars {
		keys = append(keys, key)
	}
	for key := range actual.Scalars {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		expectedValue, expectedPresent := expected.Scalars[key]
		actualValue, actualPresent := actual.Scalars[key]
		if expectedPresent != actualPresent || expectedValue != actualValue {
			return fmt.Errorf("published scalar %s differs for owner %s row %s: expected_present=%t actual_present=%t expected_type=%d actual_type=%d expected_null=%t actual_null=%t", key, owner, expected.RowKey, expectedPresent, actualPresent, expectedValue.Type, actualValue.Type, expectedValue.Null, actualValue.Null)
		}
	}
	return nil
}

func readFrozenPassProof(path string) (frozenCorpusPassProof, error) {
	file, err := os.Open(path)
	if err != nil {
		return frozenCorpusPassProof{}, err
	}
	defer file.Close()
	var proof frozenCorpusPassProof
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 65536), 1024*1024)
	for scanner.Scan() {
		var event struct {
			Message   string                `json:"msg"`
			RunID     string                `json:"run_id"`
			Operation observation.Operation `json:"operation"`
			Outcome   observation.Outcome   `json:"outcome"`
			Requested int                   `json:"embedding_requested"`
		}
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return proof, err
		}
		if event.Message == "daemon.conversation_semantic_sync.pass_started" {
			proof = frozenCorpusPassProof{RunID: event.RunID}
			continue
		}
		if event.RunID != proof.RunID {
			continue
		}
		if event.Message == "daemon.conversation_semantic_sync.pass_completed" {
			proof.Completed = true
			if err = json.Unmarshal(scanner.Bytes(), &proof.Summary); err != nil {
				return proof, err
			}
		}
		if event.Message != "daemon.conversation_semantic_embedded.operation_completed" {
			continue
		}
		if event.Outcome != observation.Success {
			proof.FailedOperations++
		}
		switch event.Operation {
		case observation.EmbeddingAttempt:
			proof.EmbeddingAttempts++
			proof.RequestedInputs += event.Requested
		case observation.UpsertCall:
			proof.VectorCalls++
		case observation.CatalogTransaction:
			proof.CatalogTransactions++
		case observation.Stage:
			proof.Stages++
		}
	}
	if err = scanner.Err(); err != nil {
		return proof, err
	}
	if proof.RunID == "" || !proof.Completed {
		return proof, errors.New("observed pass lacks start or completion marker")
	}
	return proof, nil
}

func bindFrozenRuntime(t *testing.T, root string) {
	t.Helper()
	for name, directory := range map[string]string{"XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state", "XDG_RUNTIME_DIR": "run"} {
		path := filepath.Join(root, directory)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, path)
	}
}
