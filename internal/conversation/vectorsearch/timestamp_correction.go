package vectorsearch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/milvus-io/milvus/client/v3/column"
	"github.com/milvus-io/milvus/client/v3/entity"
	"github.com/milvus-io/milvus/client/v3/milvusclient"
	"goodkind.io/lm-semantic-search/collection"
	milvusstore "goodkind.io/lm-semantic-search/collection/milvus"
)

const (
	// CorrectionPlanned indicates that the planner selected a timestamp update for the row.
	CorrectionPlanned = "planned"
	// CorrectionCurrent indicates that the planner accepts the stored row without another timestamp write.
	CorrectionCurrent = "current"
	// CorrectionGuardMismatch indicates that the stored row fails the expected state checks.
	CorrectionGuardMismatch = "guard_mismatch"
	// CorrectionRowAbsent indicates that the stored collection does not contain the listed row.
	CorrectionRowAbsent = "row_absent"
	// CorrectionNotEvaluated indicates that the planner could not select a valid timestamp update.
	CorrectionNotEvaluated = "not_evaluated"
	// CorrectionSourceLoadFailed indicates that the command failed to load the
	// provider source.
	CorrectionSourceLoadFailed = "source_load_failed"
)

// TimestampCorrectionGuard specifies the expected timestamp, metadata hash, and vector key for a row.
type TimestampCorrectionGuard struct {
	TimestampUnix  int64
	MetadataSHA256 string
	VectorKey      string
}

// TimestampCorrectionEvidence records a row's source matching outcome, correction decision, timestamps, and verified write status.
type TimestampCorrectionEvidence struct {
	RowID          string `json:"row_id"`
	ConversationID string `json:"conversation_id"`
	Family         string `json:"family"`
	SourceOutcome  string `json:"source_outcome"`
	Correction     string `json:"correction"`
	FromUnix       int64  `json:"from_timestamp_unix"`
	ToUnix         int64  `json:"to_timestamp_unix"`
	Written        bool   `json:"written"`
}

// TimestampCorrectionReport records correction counts and evidence for listed rows.
type TimestampCorrectionReport struct {
	Conversations  int                           `json:"conversations"`
	RowsListed     int                           `json:"rows_listed"`
	RowsPlanned    int                           `json:"rows_planned"`
	RowsCurrent    int                           `json:"rows_current"`
	RowsWritten    int                           `json:"rows_written"`
	GuardMismatch  int                           `json:"guard_mismatch"`
	RowsAbsent     int                           `json:"rows_absent"`
	NotEvaluated   int                           `json:"not_evaluated"`
	ToKnown        int                           `json:"to_known"`
	ToUnknown      int                           `json:"to_unknown"`
	SourceOutcomes map[string]int                `json:"source_outcomes"`
	Evidence       []TimestampCorrectionEvidence `json:"evidence,omitempty"`
}

// NewCorrectionReport returns a report with zero counts and an initialized source outcome map.
func NewCorrectionReport() TimestampCorrectionReport {
	return TimestampCorrectionReport{
		Conversations: 0, RowsListed: 0, RowsPlanned: 0, RowsCurrent: 0, RowsWritten: 0, GuardMismatch: 0,
		RowsAbsent: 0, NotEvaluated: 0, ToKnown: 0, ToUnknown: 0, SourceOutcomes: map[string]int{}, Evidence: nil,
	}
}

// Add combines correction counts and source outcome counts without appending evidence.
func (report *TimestampCorrectionReport) Add(other TimestampCorrectionReport) {
	report.Conversations += other.Conversations
	report.RowsListed += other.RowsListed
	report.RowsPlanned += other.RowsPlanned
	report.RowsCurrent += other.RowsCurrent
	report.RowsWritten += other.RowsWritten
	report.GuardMismatch += other.GuardMismatch
	report.RowsAbsent += other.RowsAbsent
	report.NotEvaluated += other.NotEvaluated
	report.ToKnown += other.ToKnown
	report.ToUnknown += other.ToUnknown
	if report.SourceOutcomes == nil {
		report.SourceOutcomes = map[string]int{}
	}
	for outcome, count := range other.SourceOutcomes {
		report.SourceOutcomes[outcome] += count
	}
}

type correctionTarget struct {
	conversationID string
	family         string
	outcome        string
	desired        int64
	evaluated      bool
}

// correctionDesired uses the chosen timestamp for a match and the unknown timestamp for other evaluated outcomes. It leaves empty content, incomplete content, and missing sources unevaluated.
func correctionDesired(evidence SourceRepairEvidence) (int64, bool) {
	switch evidence.Outcome {
	case SourceOutcomeMatched:
		return evidence.Chosen, true
	case SourceOutcomeEmpty, SourceOutcomeIncomplete, SourceOutcomeMissingSource:
		return 0, false
	default:
		return missingTimestampUnix, true
	}
}

// CorrectTimestampsFromSource plans timestamp corrections from source evidence
// for rows in Corrections. It writes and verifies updates when dryRun is false
// and every listed row exists and passes its expected state checks.
func (c *Client) CorrectTimestampsFromSource(ctx context.Context, collectionID string, sources []TimestampSourceRepairSource, dryRun bool) (TimestampCorrectionReport, error) {
	report := NewCorrectionReport()
	if c == nil {
		return report, errors.New("conversation search client is nil")
	}
	milvus, isMilvus := c.store.(*milvusBackend)
	if !isMilvus || milvus.shared == nil {
		return report, ErrTimestampRepairUnsupported
	}
	collectionName := CollectionName(strings.TrimSpace(collectionID))
	exists, err := c.loadCollectionIfPresent(ctx, collectionName)
	if err != nil {
		return report, err
	}
	if !exists {
		slog.WarnContext(ctx, "conversation.vectorsearch.timestamp_correction_collection_absent",
			"concern", "conversation.semantic", "component", "conversation", "collection", collectionName)
		return report, fmt.Errorf("collection does not exist %s: %w", collectionName, ErrCollectionAbsent)
	}
	targets := make(map[string]correctionTarget)
	guards := make(map[string]TimestampCorrectionGuard)
	for _, source := range sources {
		if source.Corrections == nil {
			return report, errors.New("correction source requires a non-nil Corrections map")
		}
		report.Conversations++
		report.RowsListed += len(source.Corrections)
		maps.Copy(guards, source.Corrections)
		for id, guard := range source.Corrections {
			targets[id] = correctionTarget{conversationID: source.ConversationID, family: "", outcome: "", desired: guard.TimestampUnix, evaluated: false}
		}
		evidence, readErr := c.sourceEvidence(ctx, collectionName, source)
		if readErr != nil {
			return report, readErr
		}
		for _, entry := range evidence {
			desired, evaluated := correctionDesired(entry)
			for _, id := range entry.Rows {
				report.SourceOutcomes[entry.Outcome]++
				target := correctionTarget{conversationID: source.ConversationID, family: entry.Family, outcome: entry.Outcome, desired: desired, evaluated: evaluated}
				if !evaluated {
					target.desired = guards[id].TimestampUnix
				}
				targets[id] = target
			}
		}
	}
	updates, err := c.planCorrections(ctx, milvus, collectionName, guards, targets, &report)
	if err != nil || dryRun || len(updates) == 0 || report.GuardMismatch > 0 || report.RowsAbsent > 0 {
		return report, err
	}
	written := TimestampRepairReport{Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0, RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0}
	writeErr := writeSourceTimestamps(ctx, c, milvus, collectionName, updates, &written)
	report.RowsWritten = written.RowsWritten
	if writeErr != nil {
		return report, writeErr
	}
	if err := milvus.verifyCorrections(ctx, collectionName, updates, guards); err != nil {
		return report, err
	}
	writtenIDs := make(map[string]bool, len(updates))
	for _, update := range updates {
		writtenIDs[update.id] = true
	}
	for index := range report.Evidence {
		report.Evidence[index].Written = writtenIDs[report.Evidence[index].RowID]
	}
	slog.InfoContext(ctx, "conversation.vectorsearch.timestamp_correction_completed",
		"concern", "conversation.semantic", "component", "conversation", "collection", collectionName,
		"conversations", report.Conversations, "rows_listed", report.RowsListed, "rows_planned", report.RowsPlanned,
		"rows_written", report.RowsWritten, "guard_mismatch", report.GuardMismatch, "rows_absent", report.RowsAbsent)
	return report, nil
}

func (c *Client) planCorrections(ctx context.Context, milvus *milvusBackend, collectionName string, guards map[string]TimestampCorrectionGuard, targets map[string]correctionTarget, report *TimestampCorrectionReport) ([]timestampUpdate, error) {
	ids := make([]string, 0, len(guards))
	for id := range guards {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	stored, err := milvus.readRowGuards(ctx, collectionName, ids)
	if err != nil {
		return nil, err
	}
	updates := make([]timestampUpdate, 0)
	for _, id := range ids {
		guard := guards[id]
		target := targets[id]
		evaluated := target.evaluated
		evidence := TimestampCorrectionEvidence{RowID: id, ConversationID: target.conversationID, Family: target.family, SourceOutcome: target.outcome, Correction: CorrectionNotEvaluated, FromUnix: guard.TimestampUnix, ToUnix: target.desired, Written: false}
		current, found := stored[id]
		switch {
		case !found:
			evidence.Correction = CorrectionRowAbsent
			report.RowsAbsent++
		case current.vectorKey != guard.VectorKey:
			evidence.Correction = CorrectionGuardMismatch
			report.GuardMismatch++
		case evaluated && current.timestampUnix == target.desired && current.timestampUnix != guard.TimestampUnix && correctedFromGuard(id, current, guard):
			evidence.Correction = CorrectionCurrent
			report.RowsCurrent++
		case current.timestampUnix != guard.TimestampUnix || current.metadataSHA256 != guard.MetadataSHA256:
			evidence.Correction = CorrectionGuardMismatch
			report.GuardMismatch++
		case !evaluated:
			report.NotEvaluated++
		default:
			rewritten, metadataTimestamp, replaceErr := replaceMetadataTimestamp(id, current.metadata, target.desired)
			switch {
			case replaceErr != nil:
				report.NotEvaluated++
			case metadataTimestamp == target.desired && current.timestampUnix == target.desired:
				evidence.Correction = CorrectionCurrent
				report.RowsCurrent++
			default:
				evidence.Correction = CorrectionPlanned
				report.RowsPlanned++
				if target.desired == missingTimestampUnix {
					report.ToUnknown++
				} else {
					report.ToKnown++
				}
				updates = append(updates, timestampUpdate{id: id, relativePath: current.relativePath, metadata: rewritten, timestampUnix: target.desired})
			}
		}
		report.Evidence = append(report.Evidence, evidence)
	}
	return updates, nil
}

// correctedFromGuard reports whether restoring the expected timestamp in the current metadata reproduces the expected metadata hash.
func correctedFromGuard(id string, current storedGuard, guard TimestampCorrectionGuard) bool {
	restored, _, err := replaceMetadataTimestamp(id, current.metadata, guard.TimestampUnix)
	if err != nil {
		return false
	}
	forms := []string{restored}
	if guard.TimestampUnix == missingTimestampUnix {
		nullField := fmt.Sprintf("%q:%s", metadataTimestampKey, metadataNull)
		markerField := fmt.Sprintf("%q:%d", metadataTimestampKey, missingTimestampUnix)
		forms = append(forms, strings.Replace(restored, nullField, markerField, 1))
	}
	for _, form := range forms {
		sum := sha256.Sum256([]byte(form))
		if hex.EncodeToString(sum[:]) == guard.MetadataSHA256 {
			return true
		}
	}
	return false
}

func (c *Client) sourceEvidence(ctx context.Context, collectionName string, source TimestampSourceRepairSource) ([]SourceRepairEvidence, error) {
	conversationID := strings.TrimSpace(source.ConversationID)
	rows, err := c.store.QueryRows(ctx, collection.RowsRequest{
		Collection:  collectionName,
		Declaration: sourceRepairRowsDeclaration(),
		ItemIDs:     []string{conversationID},
		PathPrefixes: []string{
			conversationRelativePathPrefix(conversationID),
			conversationToolRelativePathPrefix(conversationID),
			conversationThinkingRelativePathPrefix(conversationID),
		},
		IncludeVector: false,
	})
	if err != nil {
		return nil, failRead("read conversation rows from collection "+collectionName, err)
	}
	report := TimestampSourceRepairReport{
		TimestampRepairReport: TimestampRepairReport{Conversations: 0, RowsRead: 0, RowsCurrent: 0, RowsChanged: 0, RowsWritten: 0, RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0},
		RowsNotZeroTime:       0, RowsEmptyContent: 0, RowsIncomplete: 0, Evidence: nil, Outcomes: map[string]int{},
	}
	matchSourceFamilies(conversationID, rows, newSourceTimeLookup(source), make(map[string]matchedRow), &report)
	return report.Evidence, nil
}

type storedGuard struct {
	relativePath   string
	metadata       string
	metadataSHA256 string
	timestampUnix  int64
	vectorKey      string
}

// readRowGuards reads stored row values with strong consistency and computes metadata hashes for correction checks.
func (b *milvusBackend) readRowGuards(ctx context.Context, collectionName string, ids []string) (map[string]storedGuard, error) {
	stored := make(map[string]storedGuard, len(ids))
	timestampColumn := collection.ScalarColumn{Name: timestampUnixColumn, Type: collection.ScalarTypeInt64, Nullable: true, MaxLength: 0}
	for start := 0; start < len(ids); start += conversationFilterIDBatchSize {
		end := min(start+conversationFilterIDBatchSize, len(ids))
		resultSet, err := b.client.Query(ctx, milvusclient.NewQueryOption(collectionName).
			WithIDs(column.NewColumnVarChar(milvusstore.IDField, slices.Clone(ids[start:end]))).
			WithOutputFields(milvusstore.IDField, milvusstore.RelativePathField, milvusstore.MetadataField, timestampUnixColumn, milvusstore.VectorKeyField).
			WithConsistencyLevel(entity.ClStrong))
		if err != nil {
			return nil, failRead("read stored row guards from "+collectionName, err)
		}
		if resultSet.ResultCount == 0 {
			continue
		}
		idColumn := resultSet.GetColumn(milvusstore.IDField)
		pathColumn := resultSet.GetColumn(milvusstore.RelativePathField)
		metadataColumn := resultSet.GetColumn(milvusstore.MetadataField)
		cellColumn := resultSet.GetColumn(timestampUnixColumn)
		keyColumn := resultSet.GetColumn(milvusstore.VectorKeyField)
		if idColumn == nil || pathColumn == nil || metadataColumn == nil || cellColumn == nil || keyColumn == nil {
			return nil, collection.ErrSearchResultIncomplete
		}
		for rowIndex := range resultSet.ResultCount {
			id, idErr := idColumn.GetAsString(rowIndex)
			path, pathErr := pathColumn.GetAsString(rowIndex)
			metadata, metadataErr := metadataColumn.GetAsString(rowIndex)
			key, keyErr := keyColumn.GetAsString(rowIndex)
			value, valueErr := milvusstore.ScalarValueAt(cellColumn, timestampColumn, rowIndex)
			if err := errors.Join(idErr, pathErr, metadataErr, keyErr, valueErr); err != nil {
				return nil, failRead(fmt.Sprintf("read guard values of row %d from %s", rowIndex, collectionName), err)
			}
			stamp := value.Int64
			if value.Null {
				stamp = missingTimestampUnix
			}
			sum := sha256.Sum256([]byte(metadata))
			stored[id] = storedGuard{relativePath: path, metadata: metadata, metadataSHA256: hex.EncodeToString(sum[:]), timestampUnix: stamp, vectorKey: key}
		}
	}
	return stored, nil
}

func (b *milvusBackend) verifyCorrections(ctx context.Context, collectionName string, updates []timestampUpdate, guards map[string]TimestampCorrectionGuard) error {
	ids := make([]string, 0, len(updates))
	for _, update := range updates {
		ids = append(ids, update.id)
	}
	stored, err := b.readRowGuards(ctx, collectionName, ids)
	if err != nil {
		return err
	}
	for _, update := range updates {
		current, found := stored[update.id]
		if !found || current.timestampUnix != update.timestampUnix || current.metadata != update.metadata || current.vectorKey != guards[update.id].VectorKey {
			verifyErr := fmt.Errorf("timestamp correction verification failed for row %s", update.id)
			slog.ErrorContext(ctx, "conversation.vectorsearch.timestamp_correction_verify_failed", "concern", "conversation.semantic", "component", "conversation", "row_id", update.id, "err", verifyErr)
			return verifyErr
		}
	}
	return nil
}
