package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/conversation/vectorsearch"
)

// correctionManifestLineBytes limits one manifest line.
const correctionManifestLineBytes = 1 << 20

type correctionState struct {
	rows         map[string]map[string]vectorsearch.TimestampCorrectionGuard
	totals       vectorsearch.TimestampCorrectionReport
	manifestHash string
}

type correctionManifestRow struct {
	RowID                         string `json:"row_id"`
	ConversationID                string `json:"conversation_id"`
	ExpectedTimestampUnix         *int64 `json:"expected_timestamp_unix"`
	ExpectedMetadataTimestampUnix *int64 `json:"expected_metadata_timestamp_unix"`
	ExpectedMetadataHash          string `json:"expected_metadata_sha256"`
	ExpectedVectorKey             string `json:"expected_vector_key"`
}

// loadCorrectionManifest reads expected row values, rejects incomplete entries and duplicate row IDs within a conversation, and groups rows by conversation ID.
func loadCorrectionManifest(path string) (*correctionState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Error("cli.daemon.repair_timestamps.correction_manifest_open_failed", "concern", "cli.daemon", "component", "cli", "path", path, "err", err)
		return nil, fmt.Errorf("open correction manifest %s: %w", path, err)
	}
	sum := sha256.Sum256(data)
	state := &correctionState{rows: map[string]map[string]vectorsearch.TimestampCorrectionGuard{}, totals: vectorsearch.NewCorrectionReport(), manifestHash: hex.EncodeToString(sum[:])}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, correctionManifestLineBytes), correctionManifestLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var row correctionManifestRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("cannot decode correction manifest row at %s:%d: %w", path, line, err)
		}
		if row.RowID == "" || row.ConversationID == "" || row.ExpectedTimestampUnix == nil || row.ExpectedMetadataTimestampUnix == nil || row.ExpectedMetadataHash == "" || row.ExpectedVectorKey == "" {
			return nil, fmt.Errorf("correction manifest row requires row_id, conversation_id, expected_timestamp_unix, expected_metadata_timestamp_unix, expected_metadata_sha256, and expected_vector_key at %s:%d", path, line)
		}
		conversationRows := state.rows[row.ConversationID]
		if conversationRows == nil {
			conversationRows = map[string]vectorsearch.TimestampCorrectionGuard{}
			state.rows[row.ConversationID] = conversationRows
		}
		if _, duplicate := conversationRows[row.RowID]; duplicate {
			return nil, fmt.Errorf("correction manifest repeats a row ID within a conversation at %s:%d: %s", path, line, row.RowID)
		}
		conversationRows[row.RowID] = vectorsearch.TimestampCorrectionGuard{TimestampUnix: *row.ExpectedTimestampUnix, MetadataTimestampUnix: *row.ExpectedMetadataTimestampUnix, MetadataSHA256: row.ExpectedMetadataHash, VectorKey: row.ExpectedVectorKey}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read correction manifest %s: %w", path, err)
	}
	if len(state.rows) == 0 {
		return nil, fmt.Errorf("correction manifest requires at least one row in %s", path)
	}
	return state, nil
}

func (state *correctionState) conversationIDs(provider string) []string {
	ids := make([]string, 0, len(state.rows))
	for id := range state.rows {
		if conversationInProvider(id, provider) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (state *correctionState) summary() string {
	totals := state.totals
	counts := []struct {
		name  string
		count int
	}{
		{name: "rows_listed", count: totals.RowsListed},
		{name: "planned", count: totals.RowsPlanned},
		{name: "to_known", count: totals.ToKnown},
		{name: "to_unknown", count: totals.ToUnknown},
		{name: "current", count: totals.RowsCurrent},
		{name: "written", count: totals.RowsWritten},
		{name: "guard_mismatch", count: totals.GuardMismatch},
		{name: "row_absent", count: totals.RowsAbsent},
		{name: "not_evaluated", count: totals.NotEvaluated},
	}
	parts := make([]string, 0, len(counts))
	for _, entry := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", entry.name, entry.count))
	}
	return "Correction row counts are " + strings.Join(parts, ", ") + ".\n" +
		"Source outcome counts are " + formatOutcomes(totals.SourceOutcomes) + ".\n"
}

// repairConversationIDs selects manifest conversations that match the selected
// provider during correction. Otherwise, it queries conversations with zero
// timestamp rows for that provider.
func (run repairTimestampsRun) repairConversationIDs(ctx context.Context) ([]string, error) {
	if run.correction != nil {
		return run.correction.conversationIDs(run.options.Provider), nil
	}
	ids, err := run.client.ZeroTimeConversationIDs(ctx, run.collectionID, run.options.Provider)
	if err != nil {
		slog.ErrorContext(ctx, "cli.daemon.repair_timestamps.stored_ids_failed", "concern", "cli.daemon", "component", "cli", "err", err)
		return nil, fmt.Errorf("cannot list conversations with zero timestamp rows: %w", err)
	}
	return ids, nil
}

// correctSourceStoreRecord returns false after a source load failure, a row
// state mismatch, or an absent manifest row. The caller adds the conversation
// to the retry list.
func (run repairTimestampsRun) correctSourceStoreRecord(ctx context.Context, record conversation.Record) (vectorsearch.TimestampRepairReport, bool, error) {
	var empty vectorsearch.TimestampRepairReport
	rows := run.correction.rows[record.ID]
	if len(rows) == 0 {
		return empty, true, nil
	}
	source, loadErr := run.loadStoreSource(ctx, record)
	if loadErr != nil {
		slog.WarnContext(ctx, "cli.daemon.repair_timestamps.correction_load_failed", "concern", "cli.daemon", "component", "cli", "conversation_id", record.ID, "err", loadErr)
		return empty, false, run.recordCorrectionLoadFailure(record.ID, rows)
	}
	source.Corrections = rows
	report, correctErr := run.client.CorrectTimestampsFromSource(ctx, run.collectionID, []vectorsearch.TimestampSourceRepairSource{source}, run.options.DryRun)
	run.correction.totals.Add(report)
	evidenceErr := run.writeCorrectionEvidence(report.Evidence)
	repair := vectorsearch.TimestampRepairReport{
		Conversations: report.Conversations, RowsRead: report.RowsListed, RowsCurrent: report.RowsCurrent, RowsChanged: report.RowsPlanned,
		RowsWritten: report.RowsWritten, RowsUnmatched: 0, RowsMissingTimestamp: 0, RowsAmbiguous: 0, RowsInvalidMetadata: 0,
	}
	if correctErr != nil {
		return repair, true, errors.Join(fmt.Errorf("correct timestamps from source store: %w", correctErr), evidenceErr)
	}
	return repair, report.GuardMismatch == 0 && report.RowsAbsent == 0, evidenceErr
}

func (run repairTimestampsRun) recordCorrectionLoadFailure(conversationID string, rows map[string]vectorsearch.TimestampCorrectionGuard) error {
	report := vectorsearch.NewCorrectionReport()
	report.Conversations = 1
	report.RowsListed = len(rows)
	report.NotEvaluated = len(rows)
	report.SourceOutcomes[vectorsearch.CorrectionSourceLoadFailed] = len(rows)
	run.correction.totals.Add(report)
	evidence := make([]vectorsearch.TimestampCorrectionEvidence, 0, len(rows))
	for _, id := range slices.Sorted(maps.Keys(rows)) {
		guard := rows[id]
		evidence = append(evidence, vectorsearch.TimestampCorrectionEvidence{
			RowID: id, ConversationID: conversationID, Family: "", SourceOutcome: vectorsearch.CorrectionSourceLoadFailed,
			Correction: vectorsearch.CorrectionNotEvaluated, FromUnix: guard.TimestampUnix, ToUnix: guard.TimestampUnix, Written: false,
			ToolCallID: "", ToolProjection: "",
		})
	}
	return run.writeCorrectionEvidence(evidence)
}

func (run repairTimestampsRun) correctionManifestHash() string {
	if run.correction == nil {
		return ""
	}
	return run.correction.manifestHash
}

func (run repairTimestampsRun) writeCorrectionEvidence(evidence []vectorsearch.TimestampCorrectionEvidence) error {
	if run.evidenceOut == nil {
		return nil
	}
	encoder := json.NewEncoder(run.evidenceOut)
	for _, entry := range evidence {
		if err := encoder.Encode(entry); err != nil {
			slog.Error("cli.daemon.repair_timestamps.correction_evidence_write_failed", "concern", "cli.daemon", "component", "cli", "err", err)
			return fmt.Errorf("write correction evidence: %w", err)
		}
	}
	return nil
}
