package capture

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

const (
	evidenceIngressClient  = "adapter.ingress"
	evidenceUpstreamClient = "adapter.codex"
	evidenceResponsesPath  = "/v1/responses"
)

//go:embed sql/evidence_exchanges.sql
var evidenceExchangesSQL string

// ExchangeEvidence stores the complete client request, client response, and
// upstream response bodies from one adapter exchange.
type ExchangeEvidence struct {
	RequestID        string
	ClientRequest    []byte
	ClientResponse   []byte
	UpstreamResponse []byte
}

// ExchangeEvidenceQuery selects successful adapter Responses exchanges in a
// time range when their client responses contain MessageID.
type ExchangeEvidenceQuery struct {
	DBPath    string
	MessageID string
	Start     time.Time
	End       time.Time
	Limit     int
}

// ReadExchangeEvidence opens the live database for reading only and calls
// visit for each exchange with complete bodies. It returns nil when the
// database is missing and stops iteration when visit returns false.
func ReadExchangeEvidence(ctx context.Context, query ExchangeEvidenceQuery, visit func(ExchangeEvidence) bool) error {
	if _, err := os.Stat(query.DBPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		slog.Warn("mitm.capture.evidence_stat_failed", "path", query.DBPath, "err", err)
		return fmt.Errorf("capture: the evidence reader could not read database file information with stat: %w", err)
	}
	db, err := sql.Open("sqlite3", "file:"+query.DBPath+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		slog.Warn("mitm.capture.evidence_open_failed", "path", query.DBPath, "err", err)
		return fmt.Errorf("capture: the evidence reader could not open the evidence database: %w", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(
		ctx,
		evidenceExchangesSQL,
		evidenceUpstreamClient,
		evidenceIngressClient,
		evidenceResponsesPath,
		query.Start.UnixNano(),
		query.End.UnixNano(),
		query.MessageID,
		query.Limit,
	)
	if err != nil {
		slog.Warn("mitm.capture.evidence_query_failed", "path", query.DBPath, "err", err)
		return fmt.Errorf("capture: the evidence reader could not query the evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var evidence ExchangeEvidence
		if err := rows.Scan(&evidence.RequestID, &evidence.ClientRequest, &evidence.ClientResponse, &evidence.UpstreamResponse); err != nil {
			slog.Warn("mitm.capture.evidence_scan_failed", "path", query.DBPath, "err", err)
			return fmt.Errorf("capture: the evidence reader could not scan an evidence row: %w", err)
		}
		if !visit(evidence) {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("mitm.capture.evidence_read_failed", "path", query.DBPath, "err", err)
		return fmt.Errorf("capture: the evidence reader could not read the evidence: %w", err)
	}
	return nil
}
