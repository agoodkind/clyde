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
	"goodkind.io/lm-semantic-search/library"
	librarymilvus "goodkind.io/lm-semantic-search/library/milvus"
	"goodkind.io/lm-semantic-search/library/observation"
)

const frozenCorpusDriverRequestEnv = "CLYDE_FROZEN_INGESTION_REQUEST"

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
	RunID               string `json:"run_id"`
	Completed           bool   `json:"completed"`
	EmbeddingAttempts   int    `json:"embedding_sdk_attempts"`
	RequestedInputs     int    `json:"embedding_requested_inputs"`
	VectorCalls         int    `json:"milvus_sdk_upsert_calls"`
	CatalogTransactions int    `json:"catalog_transactions"`
	Stages              int    `json:"stages"`
	FailedOperations    int    `json:"failed_operations"`
}

type frozenVectorVerifier struct {
	store       *librarymilvus.Store
	description string
	catalogUUID string
	bound       bool
}

type frozenCorpusProof struct {
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
	proof, err := runFrozenCorpusIngestion(t, request)
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
	request := createFrozenCorpusFixture(t)
	root := t.TempDir()
	request.RuntimeRoot = root
	request.OutputPath = filepath.Join(root, "proof.json")
	bindFrozenRuntime(t, root)
	request.Semantic.CatalogPath = filepath.Join(root, "catalog.sqlite")
	request.Semantic.LockPath = filepath.Join(root, "catalog.lock")
	request.Semantic.PoolID = "frozen-fixture"
	request.Semantic.CollectionID = "frozen-fixture"
	request.Semantic.MilvusCollection = "frozen_vectors"
	request.Semantic.MilvusAddress = liveMilvusAddress
	request.Semantic.MilvusDatabase = "clyde_frozen_" + fmt.Sprint(time.Now().UnixNano())
	if registeredDatabase := os.Getenv("CLYDE_FROZEN_FIXTURE_DATABASE"); registeredDatabase != "" {
		request.Semantic.MilvusDatabase = registeredDatabase
	}
	request.Semantic.EmbeddingBaseURL = liveEmbeddingBaseURL
	request.Semantic.EmbeddingRequestTimeout = config.Duration(2 * time.Minute)
	proof, err := runFrozenCorpusIngestion(t, request)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.Complete || proof.Records != 4 || proof.AdmittedOwners != 4 || proof.Occurrences != 4 || proof.First.EmbeddingAttempts == 0 || proof.First.VectorCalls == 0 || proof.Restart.EmbeddingAttempts != 0 || proof.Restart.VectorCalls != 0 || proof.Restart.Stages != 0 || proof.CatalogBeforeRestart != proof.CatalogAfterRestart {
		t.Fatalf("real frozen worker proof=%+v", proof)
	}
	t.Logf("verified frozen worker proof=%+v", proof)
}

func runFrozenCorpusIngestion(t *testing.T, request frozenCorpusRequest) (proof frozenCorpusProof, resultErr error) {
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
	admin, err := milvusclient.New(ctx, &milvusclient.ClientConfig{Address: request.Semantic.MilvusAddress})
	if err != nil {
		return proof, err
	}
	existing, err := admin.ListDatabase(ctx, milvusclient.NewListDatabaseOption())
	if err != nil {
		return proof, errors.Join(err, closeFrozenAdmin(ctx, admin))
	}
	database := request.Semantic.MilvusDatabase
	if slices.Contains(existing, database) {
		return proof, errors.Join(errors.New("explicit fresh database already exists"), closeFrozenAdmin(ctx, admin))
	}
	proof.Database = database
	t.Logf("verified absent frozen database %s at isolated runtime %s before create request", database, request.RuntimeRoot)
	defer func() {
		if resultErr != nil || !request.RetainOnSuccess {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			databases, inspectErr := admin.ListDatabase(cleanupCtx, milvusclient.NewListDatabaseOption())
			if inspectErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("inspect exact fresh database %s for cleanup: %w", database, inspectErr), closeFrozenAdmin(ctx, admin))
				return
			}
			if slices.Contains(databases, database) {
				dropLiveMilvusDatabase(t, admin, database)
			} else {
				resultErr = errors.Join(resultErr, closeFrozenAdmin(ctx, admin))
			}
		} else {
			resultErr = errors.Join(resultErr, closeFrozenAdmin(ctx, admin))
		}
	}()
	if err = admin.CreateDatabase(ctx, milvusclient.NewCreateDatabaseOption(database)); err != nil {
		return proof, err
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
		worker := newConversationSemanticSyncWorker(index, nil, request.Semantic.CollectionID, logger, kinds)
		worker.embedded = newEmbeddedConversationSync(request.Semantic, conversationSemanticOutboxPath(request.Semantic.PoolID), newEmbeddedSemanticStatus(), index)
		return worker
	}
	worker := createWorker()
	defer func() { resultErr = errors.Join(resultErr, worker.embedded.closeStore(context.WithoutCancel(ctx))) }()
	if err = worker.runEmbeddedPass(ctx); err != nil {
		return proof, err
	}
	firstOwners, occurrences, err := verifyFrozenPublishedOwners(t, ctx, index, worker)
	if err != nil {
		return proof, err
	}
	first, err := readFrozenPassProof(logPath)
	if err != nil {
		return proof, err
	}
	if first.FailedOperations != 0 {
		return proof, errors.New("first pass contains failed observed operation")
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
	if err = worker.runEmbeddedPass(ctx); err != nil {
		return proof, err
	}
	restartedOwners, restartedOccurrences, err := verifyFrozenPublishedOwners(t, ctx, index, worker)
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
	restart, err := readFrozenPassProof(logPath)
	if err != nil {
		return proof, err
	}
	if restart.RunID == first.RunID || restart.EmbeddingAttempts != 0 || restart.RequestedInputs != 0 || restart.VectorCalls != 0 || restart.Stages != 0 || restart.FailedOperations != 0 {
		return proof, fmt.Errorf("unchanged restarted pass performed operations: %+v", restart)
	}
	proof = frozenCorpusProof{Complete: true, Database: database, Records: len(index.records), AdmittedOwners: len(firstOwners), Occurrences: occurrences, First: first, Restart: restart, CatalogBeforeRestart: beforeRestart, CatalogAfterRestart: afterRestart, Retained: request.RetainOnSuccess, PublicCLIContextMappingVerified: false}
	return proof, nil
}

func readFrozenCatalogVersions(ctx context.Context, connection *sql.Conn) (frozenCatalogVersions, error) {
	var versions frozenCatalogVersions
	err := connection.QueryRowContext(ctx, frozenCatalogVersionsQuery).Scan(&versions.Data, &versions.Schema)
	return versions, err
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
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
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
	if semantic.MilvusAddress != liveMilvusAddress || semantic.EmbeddingBaseURL != liveEmbeddingBaseURL || !strings.HasPrefix(semantic.MilvusDatabase, "clyde_frozen_") || semantic.MilvusCollection == "" || semantic.CollectionID == "" || semantic.PoolID == "" {
		return errors.New("driver requires explicit fresh clyde_frozen_ database and approved isolated Mac endpoints")
	}
	if _, err := os.Lstat(conversationSemanticOutboxPath(semantic.PoolID)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("isolated outbox already exists or cannot be inspected")
	}
	return nil
}

func closeFrozenAdmin(ctx context.Context, admin *milvusclient.Client) error {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return admin.Close(closeCtx)
}

func verifyFrozenPublishedOwners(t *testing.T, ctx context.Context, index *frozenCorpusIndex, worker *conversationSemanticSyncWorker) (map[string]library.OwnerOccurrences, int, error) {
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
	return verifier.store.VerifyStrong(ctx, identities)
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
