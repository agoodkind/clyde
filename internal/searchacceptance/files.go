package searchacceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// ReadReport validates a saved report against the exact frozen battery bytes.
func ReadReport(reportPath, batteryPath string) (report Report, battery Battery, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.report_rejected", "component", "searchacceptance", "concern", "report", "err", err)
		}
	}()
	batteryBytes, err := os.ReadFile(batteryPath)
	if err != nil {
		return Report{}, Battery{}, fmt.Errorf("read frozen battery: %w", err)
	}
	battery, err = UnmarshalJSON[Battery](batteryBytes)
	if err != nil {
		return Report{}, Battery{}, fmt.Errorf("decode frozen battery: %w", err)
	}
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		return Report{}, Battery{}, fmt.Errorf("read search report: %w", err)
	}
	report, err = UnmarshalJSON[Report](reportBytes)
	if err != nil {
		return Report{}, Battery{}, fmt.Errorf("decode search report: %w", err)
	}
	if err := ValidateReport(report, battery, batteryBytes); err != nil {
		return Report{}, Battery{}, err
	}
	return report, battery, nil
}

// CompareCompatibility rejects reports from different measured workloads.
// Build revisions may differ between a healthy baseline and a candidate.
func CompareCompatibility(baseline, candidate Report) error {
	if baseline.SchemaVersion != candidate.SchemaVersion ||
		baseline.CorpusDigest != candidate.CorpusDigest ||
		baseline.HardwareIdentity != candidate.HardwareIdentity ||
		baseline.ModelDescriptor != candidate.ModelDescriptor ||
		baseline.BatteryDigest != candidate.BatteryDigest ||
		baseline.Concurrency != candidate.Concurrency {
		return errors.New("baseline and candidate workload identities differ")
	}
	return nil
}

// UnmarshalJSON decodes one strict report or frozen battery document.
func UnmarshalJSON[T Report | Battery](content []byte) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		slog.Warn("search.acceptance.json_rejected", "component", "searchacceptance", "concern", "report", "err", err)
		return value, fmt.Errorf("decode JSON document: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, errors.New("expected one complete JSON document")
	}
	return value, nil
}
