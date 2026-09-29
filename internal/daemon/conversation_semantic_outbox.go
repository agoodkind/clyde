package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"goodkind.io/lm-semantic-search/library"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/config"
)

// conversationSemanticOutboxSchemaVersion is the outbox schema this build
// creates and reads. openConversationSemanticOutbox rejects an outbox saved
// with another version.
const conversationSemanticOutboxSchemaVersion = 1

// conversationSemanticOutboxBusyTimeoutMilliseconds bounds how long an outbox
// connection waits for another connection's write transaction.
const conversationSemanticOutboxBusyTimeoutMilliseconds = 30000

// Outbox batch states.
const (
	embeddedOutboxStatePending   = "pending"
	embeddedOutboxStateDelivered = "delivered"
)

// conversationSemanticOutboxSchemaStatements creates every outbox table. Each
// statement is idempotent.
var conversationSemanticOutboxSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS batches (
		batch_id TEXT PRIMARY KEY,
		namespace TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		generation_order INTEGER NOT NULL,
		source_path TEXT NOT NULL,
		source_stamp TEXT NOT NULL,
		projection_profile TEXT NOT NULL,
		row_count INTEGER NOT NULL,
		manifest_hash TEXT NOT NULL,
		provider TEXT NOT NULL,
		workspace_root TEXT NOT NULL,
		archived INTEGER NOT NULL,
		subagent INTEGER NOT NULL,
		state TEXT NOT NULL,
		receipt_fingerprint TEXT NOT NULL,
		created_unix INTEGER NOT NULL,
		delivered_unix INTEGER NOT NULL,
		blocked_class TEXT NOT NULL DEFAULT '',
		blocked_library_order INTEGER NOT NULL DEFAULT 0,
		blocked_unix INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS batches_state ON batches (state, created_unix)`,
	`CREATE TABLE IF NOT EXISTS batch_rows (
		batch_id TEXT NOT NULL REFERENCES batches (batch_id),
		ordinal INTEGER NOT NULL,
		row_key TEXT NOT NULL,
		field_key TEXT NOT NULL,
		field_digest TEXT NOT NULL,
		provider_message_id TEXT NOT NULL,
		occurrence BLOB NOT NULL,
		PRIMARY KEY (batch_id, row_key)
	)`,
	`CREATE TABLE IF NOT EXISTS committed_fields (
		namespace TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		field_key TEXT NOT NULL,
		digest TEXT NOT NULL,
		provider_message_id TEXT NOT NULL,
		committed_generation INTEGER NOT NULL,
		PRIMARY KEY (namespace, owner_id, field_key)
	)`,
	`CREATE TABLE IF NOT EXISTS owner_metadata (
		namespace TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		provider TEXT NOT NULL,
		workspace_root TEXT NOT NULL,
		archived INTEGER NOT NULL,
		subagent INTEGER NOT NULL,
		projection_order INTEGER NOT NULL,
		stale INTEGER NOT NULL,
		PRIMARY KEY (namespace, owner_id)
	)`,
	`CREATE TABLE IF NOT EXISTS projections (
		namespace TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		projection_order INTEGER NOT NULL,
		token TEXT NOT NULL,
		provider TEXT NOT NULL,
		workspace_root TEXT NOT NULL,
		archived INTEGER NOT NULL,
		subagent INTEGER NOT NULL,
		state TEXT NOT NULL,
		receipt_fingerprint TEXT NOT NULL,
		created_unix INTEGER NOT NULL,
		blocked_class TEXT NOT NULL DEFAULT '',
		blocked_library_order INTEGER NOT NULL DEFAULT 0,
		blocked_unix INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (namespace, owner_id, projection_order)
	)`,
	`CREATE INDEX IF NOT EXISTS projections_state ON projections (state, created_unix)`,
	`CREATE TABLE IF NOT EXISTS projection_rows (
		namespace TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		projection_order INTEGER NOT NULL,
		row_key TEXT NOT NULL,
		PRIMARY KEY (namespace, owner_id, projection_order, row_key)
	)`,
}

// conversationSemanticOutbox is the Clyde-owned SQLite record of embedded
// ingestion batches. Every batch and its rows are written before the library
// receives them. A batch stays pending until a CommitGeneration receipt for its
// generation order is acknowledged. The acknowledgment also records the
// committed fields that later passes compare against. The outbox is separate
// from the library catalog and from the MITM capture database.
type conversationSemanticOutbox struct {
	db   *sql.DB
	path string
	// lock is the outbox lock file. The open outbox owns its exclusive kernel
	// lock, and Close releases it.
	lock *os.File
}

// embeddedOutboxBatch is one owner generation in the outbox. BatchID is the
// generation idempotency token.
type embeddedOutboxBatch struct {
	BatchID           string
	Namespace         string
	OwnerID           string
	GenerationOrder   uint64
	SourcePath        string
	SourceStamp       string
	ProjectionProfile string
	RowCount          uint64
	ManifestHash      string
	// Metadata is the mutable owner metadata that every row of the batch
	// stores.
	Metadata embeddedOwnerMetadata
}

// embeddedOutboxRow is one occurrence of a batch with the field it was
// prepared from.
type embeddedOutboxRow struct {
	FieldKey          string
	FieldDigest       string
	ProviderMessageID string
	Occurrence        library.Occurrence
}

// embeddedOutboxOccurrence is the stored form of one [library.Occurrence]. A
// replay decodes it into the same occurrence that the first delivery staged.
type embeddedOutboxOccurrence struct {
	RowKey         string                          `json:"row_key"`
	SortKey        string                          `json:"sort_key"`
	SourceText     string                          `json:"source_text"`
	SearchText     string                          `json:"search_text"`
	EmbeddingInput string                          `json:"embedding_input"`
	Scalars        map[string]embeddedOutboxScalar `json:"scalars"`
}

// embeddedOutboxScalar is the stored form of one [library.ScalarValue].
type embeddedOutboxScalar struct {
	Type   library.ScalarType `json:"type"`
	Null   bool               `json:"null"`
	String string             `json:"string"`
	Bool   bool               `json:"bool"`
	Int64  int64              `json:"int64"`
}

// conversationSemanticOutboxPath returns the outbox file of one vector pool
// under the Clyde state directory.
func conversationSemanticOutboxPath(poolID string) string {
	return filepath.Join(config.DefaultStateDir(), "conversation-semantic", "outbox-"+sanitizeOutboxPoolID(poolID)+".sqlite")
}

// sanitizeOutboxPoolID keeps ASCII letters, digits, dots, underscores, and
// hyphens, and replaces every other character with an underscore.
func sanitizeOutboxPoolID(poolID string) string {
	return strings.Map(func(character rune) rune {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '.', character == '_', character == '-':
			return character
		default:
			return '_'
		}
	}, poolID)
}

// openConversationSemanticOutbox takes the outbox lock, opens or creates the
// outbox at path in WAL mode, and checks its schema version. An outbox that
// another open outbox locks returns an error that wraps errOutboxOwned.
func openConversationSemanticOutbox(ctx context.Context, path string) (*conversationSemanticOutbox, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"path", path,
			"err", err,
		)
		return nil, fmt.Errorf("create conversation semantic outbox directory %s: %w", filepath.Dir(path), err)
	}
	lock, err := lockConversationSemanticOutbox(ctx, path)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	query.Set("_busy_timeout", strconv.Itoa(conversationSemanticOutboxBusyTimeoutMilliseconds))
	query.Set("_journal_mode", "WAL")
	query.Set("_synchronous", "FULL")
	query.Set("_foreign_keys", "on")
	query.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite3", "file:"+path+"?"+query.Encode())
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.open_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"path", path,
			"err", err,
		)
		return nil, errors.Join(fmt.Errorf("open conversation semantic outbox %s: %w", path, err), lock.Close())
	}
	outbox := &conversationSemanticOutbox{db: db, path: path, lock: lock}
	if err := outbox.initializeSchema(ctx); err != nil {
		return nil, errors.Join(err, outbox.Close())
	}
	return outbox, nil
}

func (outbox *conversationSemanticOutbox) initializeSchema(ctx context.Context) error {
	err := outbox.write(ctx, func(tx *sql.Tx) error {
		for _, statement := range conversationSemanticOutboxSchemaStatements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("create outbox schema: %w", err)
			}
		}
		var saved int
		err := tx.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&saved)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, conversationSemanticOutboxSchemaVersion); err != nil {
				return fmt.Errorf("save outbox schema version: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read outbox schema version: %w", err)
		}
		if saved != conversationSemanticOutboxSchemaVersion {
			return fmt.Errorf("outbox %s schema version is %d, this build reads %d", outbox.path, saved, conversationSemanticOutboxSchemaVersion)
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.schema_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"path", outbox.path,
			"err", err,
		)
		return fmt.Errorf("initialize conversation semantic outbox %s: %w", outbox.path, err)
	}
	return nil
}

// Close closes the outbox database and then the lock file, which releases the
// outbox lock.
func (outbox *conversationSemanticOutbox) Close() error {
	err := outbox.db.Close()
	if outbox.lock != nil {
		err = errors.Join(err, outbox.lock.Close())
	}
	if err != nil {
		slog.Warn("daemon.conversation_semantic_outbox.close_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"path", outbox.path,
			"err", err,
		)
		return fmt.Errorf("close conversation semantic outbox %s: %w", outbox.path, err)
	}
	return nil
}

// recordBatch saves one pending batch and all of its rows in one transaction.
func (outbox *conversationSemanticOutbox) recordBatch(ctx context.Context, batch embeddedOutboxBatch, rows []embeddedOutboxRow) error {
	err := outbox.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO batches (batch_id, namespace, owner_id, generation_order, source_path, source_stamp, projection_profile,
			row_count, manifest_hash, provider, workspace_root, archived, subagent, state, receipt_fingerprint, created_unix, delivered_unix)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 0)`,
			batch.BatchID, batch.Namespace, batch.OwnerID, batch.GenerationOrder, batch.SourcePath, batch.SourceStamp,
			batch.ProjectionProfile, batch.RowCount, batch.ManifestHash, batch.Metadata.Provider, batch.Metadata.WorkspaceRoot,
			batch.Metadata.Archived, batch.Metadata.Subagent, embeddedOutboxStatePending, clock.Now().Unix(),
		); err != nil {
			return fmt.Errorf("save batch: %w", err)
		}
		for ordinal, row := range rows {
			payload, err := json.Marshal(newEmbeddedOutboxOccurrence(row.Occurrence))
			if err != nil {
				return fmt.Errorf("encode row %s: %w", row.Occurrence.RowKey, err)
			}
			if _, err := tx.ExecContext(
				ctx,
				`INSERT INTO batch_rows (batch_id, ordinal, row_key, field_key, field_digest, provider_message_id, occurrence)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				batch.BatchID, ordinal, row.Occurrence.RowKey, row.FieldKey, row.FieldDigest, row.ProviderMessageID, payload,
			); err != nil {
				return fmt.Errorf("save row %s: %w", row.Occurrence.RowKey, err)
			}
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.record_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", batch.OwnerID,
			"batch_id", batch.BatchID,
			"err", err,
		)
		return fmt.Errorf("record outbox batch %s for %s: %w", batch.BatchID, batch.OwnerID, err)
	}
	return nil
}

// pendingBatches returns every pending batch in the order it was recorded.
func (outbox *conversationSemanticOutbox) pendingBatches(ctx context.Context) (batches []embeddedOutboxBatch, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_pending_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"path", outbox.path,
				"err", err,
			)
		}
	}()
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT batch_id, namespace, owner_id, generation_order, source_path, source_stamp, projection_profile, row_count, manifest_hash,
		provider, workspace_root, archived, subagent
		FROM batches WHERE state = ? ORDER BY created_unix, owner_id, generation_order`,
		embeddedOutboxStatePending,
	)
	if err != nil {
		return nil, fmt.Errorf("query pending outbox batches: %w", err)
	}
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	for rows.Next() {
		var batch embeddedOutboxBatch
		if err := rows.Scan(
			&batch.BatchID, &batch.Namespace, &batch.OwnerID, &batch.GenerationOrder, &batch.SourcePath, &batch.SourceStamp,
			&batch.ProjectionProfile, &batch.RowCount, &batch.ManifestHash, &batch.Metadata.Provider, &batch.Metadata.WorkspaceRoot,
			&batch.Metadata.Archived, &batch.Metadata.Subagent,
		); err != nil {
			return nil, fmt.Errorf("scan pending outbox batch: %w", err)
		}
		batches = append(batches, batch)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending outbox batches: %w", err)
	}
	return batches, nil
}

// batchRows returns the stored rows of one batch in the order they were
// recorded.
func (outbox *conversationSemanticOutbox) batchRows(ctx context.Context, batchID string) (batchRows []embeddedOutboxRow, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_rows_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"batch_id", batchID,
				"err", err,
			)
		}
	}()
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT field_key, field_digest, provider_message_id, occurrence FROM batch_rows WHERE batch_id = ? ORDER BY ordinal`,
		batchID,
	)
	if err != nil {
		return nil, fmt.Errorf("query outbox batch %s rows: %w", batchID, err)
	}
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	for rows.Next() {
		var row embeddedOutboxRow
		var payload []byte
		if err := rows.Scan(&row.FieldKey, &row.FieldDigest, &row.ProviderMessageID, &payload); err != nil {
			return nil, fmt.Errorf("scan outbox batch %s row: %w", batchID, err)
		}
		var stored embeddedOutboxOccurrence
		if err := json.Unmarshal(payload, &stored); err != nil {
			return nil, fmt.Errorf("decode outbox batch %s row: %w", batchID, err)
		}
		row.Occurrence = stored.occurrence()
		batchRows = append(batchRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read outbox batch %s rows: %w", batchID, err)
	}
	return batchRows, nil
}

// acknowledgeBatch marks one pending batch delivered with its receipt
// fingerprint, records the batch fields as committed at the batch generation
// order, deletes the stored rows of the batch, and records the batch owner
// metadata, in one transaction.
func (outbox *conversationSemanticOutbox) acknowledgeBatch(ctx context.Context, batch embeddedOutboxBatch, receiptFingerprint string) error {
	err := outbox.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(
			ctx,
			`UPDATE batches SET state = ?, receipt_fingerprint = ?, delivered_unix = ? WHERE batch_id = ? AND state = ?`,
			embeddedOutboxStateDelivered, receiptFingerprint, clock.Now().Unix(), batch.BatchID, embeddedOutboxStatePending,
		)
		if err != nil {
			return fmt.Errorf("mark batch delivered: %w", err)
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count delivered batches: %w", err)
		}
		if updated != 1 {
			return errors.New("the outbox has no pending batch with that ID")
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO committed_fields (namespace, owner_id, field_key, digest, provider_message_id, committed_generation)
			SELECT DISTINCT ?, ?, field_key, field_digest, provider_message_id, ? FROM batch_rows WHERE batch_id = ?
			ON CONFLICT (namespace, owner_id, field_key) DO UPDATE SET digest = excluded.digest,
			provider_message_id = excluded.provider_message_id, committed_generation = excluded.committed_generation`,
			batch.Namespace, batch.OwnerID, batch.GenerationOrder, batch.BatchID,
		); err != nil {
			return fmt.Errorf("record committed fields: %w", err)
		}
		// A delivered batch keeps no stored text. Replay reads batch_rows only
		// of pending batches, and reconciliation and reprojection read the
		// published row keys from the library.
		if _, err := tx.ExecContext(ctx, `DELETE FROM batch_rows WHERE batch_id = ?`, batch.BatchID); err != nil {
			return fmt.Errorf("delete delivered batch rows: %w", err)
		}
		// SQLite evaluates every SET expression against the row before the
		// update. The owner turns stale when the batch rows store other
		// metadata than the rows that committed earlier.
		metadata := batch.Metadata
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO owner_metadata (namespace, owner_id, provider, workspace_root, archived, subagent, projection_order, stale)
			VALUES (?, ?, ?, ?, ?, ?, 0, 0)
			ON CONFLICT (namespace, owner_id) DO UPDATE SET
			stale = CASE WHEN provider = excluded.provider AND workspace_root = excluded.workspace_root
				AND archived = excluded.archived AND subagent = excluded.subagent THEN stale ELSE 1 END,
			provider = excluded.provider, workspace_root = excluded.workspace_root,
			archived = excluded.archived, subagent = excluded.subagent`,
			batch.Namespace, batch.OwnerID, metadata.Provider, metadata.WorkspaceRoot, metadata.Archived, metadata.Subagent,
		); err != nil {
			return fmt.Errorf("record owner metadata: %w", err)
		}
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.acknowledge_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"conversation_id", batch.OwnerID,
			"batch_id", batch.BatchID,
			"err", err,
		)
		return fmt.Errorf("acknowledge outbox batch %s for %s: %w", batch.BatchID, batch.OwnerID, err)
	}
	return nil
}

// committedFields maps each committed field key of one owner to its digest.
func (outbox *conversationSemanticOutbox) committedFields(ctx context.Context, namespace string, ownerID string) (committed map[string]string, err error) {
	defer func() {
		if err != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.read_committed_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"conversation_id", ownerID,
				"err", err,
			)
		}
	}()
	rows, err := outbox.db.QueryContext(
		ctx,
		`SELECT field_key, digest FROM committed_fields WHERE namespace = ? AND owner_id = ?`,
		namespace, ownerID,
	)
	if err != nil {
		return nil, fmt.Errorf("query committed fields of %s: %w", ownerID, err)
	}
	defer func() {
		err = errors.Join(err, closeOutboxRows(rows))
	}()
	committed = make(map[string]string)
	for rows.Next() {
		var fieldKey, digest string
		if err := rows.Scan(&fieldKey, &digest); err != nil {
			return nil, fmt.Errorf("scan committed field of %s: %w", ownerID, err)
		}
		committed[fieldKey] = digest
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read committed fields of %s: %w", ownerID, err)
	}
	return committed, nil
}

// write runs work in one immediate write transaction and commits it when work
// returns nil. The caller logs a failure of work with its own identifiers.
func (outbox *conversationSemanticOutbox) write(ctx context.Context, work func(*sql.Tx) error) error {
	tx, err := outbox.db.BeginTx(ctx, nil)
	if err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.begin_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"path", outbox.path,
			"err", err,
		)
		return fmt.Errorf("begin outbox transaction: %w", err)
	}
	if err := work(tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.rollback_failed",
				"concern", "conversation.semantic",
				"component", "daemon",
				"path", outbox.path,
				"err", rollbackErr,
			)
			return errors.Join(err, fmt.Errorf("roll back outbox transaction: %w", rollbackErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		slog.WarnContext(ctx, "daemon.conversation_semantic_outbox.commit_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"path", outbox.path,
			"err", err,
		)
		return fmt.Errorf("commit outbox transaction: %w", err)
	}
	return nil
}

func closeOutboxRows(rows *sql.Rows) error {
	if err := rows.Close(); err != nil {
		slog.Warn("daemon.conversation_semantic_outbox.close_rows_failed",
			"concern", "conversation.semantic",
			"component", "daemon",
			"err", err,
		)
		return fmt.Errorf("close outbox rows: %w", err)
	}
	return nil
}

func newEmbeddedOutboxOccurrence(occurrence library.Occurrence) embeddedOutboxOccurrence {
	scalars := make(map[string]embeddedOutboxScalar, len(occurrence.Scalars))
	for name, value := range occurrence.Scalars {
		scalars[name] = embeddedOutboxScalar{Type: value.Type, Null: value.Null, String: value.String, Bool: value.Bool, Int64: value.Int64}
	}
	return embeddedOutboxOccurrence{
		RowKey:         occurrence.RowKey,
		SortKey:        occurrence.SortKey,
		SourceText:     occurrence.SourceText,
		SearchText:     occurrence.SearchText,
		EmbeddingInput: occurrence.EmbeddingInput,
		Scalars:        scalars,
	}
}

func (stored embeddedOutboxOccurrence) occurrence() library.Occurrence {
	scalars := make(map[string]library.ScalarValue, len(stored.Scalars))
	for name, value := range stored.Scalars {
		scalars[name] = library.ScalarValue{Type: value.Type, Null: value.Null, String: value.String, Bool: value.Bool, Int64: value.Int64}
	}
	return library.Occurrence{
		RowKey:         stored.RowKey,
		SortKey:        stored.SortKey,
		SourceText:     stored.SourceText,
		SearchText:     stored.SearchText,
		EmbeddingInput: stored.EmbeddingInput,
		Scalars:        scalars,
	}
}
