package searchacceptance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
)

// ValidateTraversal checks every returned page against the frozen source IDs.
// It does not establish score correctness or performance acceptance.
func ValidateTraversal(query Query, traversal Traversal, timeoutMS int64) error {
	if traversal.Error != "" {
		return fmt.Errorf("query %s failed: %s", query.ID, traversal.Error)
	}
	if query.ID == "" || query.ID != traversal.QueryID || query.PageSize <= 0 || timeoutMS <= 0 {
		return errors.New("query identity, positive page size, and timeout are required")
	}
	if query.ExpectedTotal != len(query.ExpectedOccurrenceIDs) || traversal.Total != query.ExpectedTotal {
		return fmt.Errorf("query %s total differs from the frozen source count", query.ID)
	}
	expected := make(map[string]bool, query.ExpectedTotal)
	for _, identity := range query.ExpectedOccurrenceIDs {
		if identity == "" || expected[identity] {
			return fmt.Errorf("query %s has an empty or repeated expected identity", query.ID)
		}
		expected[identity] = true
	}
	if len(traversal.Pages) == 0 {
		return fmt.Errorf("query %s has no terminal page", query.ID)
	}
	seen := make(map[string]bool, query.ExpectedTotal)
	for index, page := range traversal.Pages {
		last := index == len(traversal.Pages)-1
		if err := validatePage(query, page, index, last, len(traversal.Pages), timeoutMS); err != nil {
			return err
		}
		for _, identity := range page.OccurrenceIDs {
			if !expected[identity] || seen[identity] {
				return fmt.Errorf("query %s returns an unexpected or repeated identity %q", query.ID, identity)
			}
			seen[identity] = true
		}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("query %s returns %d of %d expected occurrences", query.ID, len(seen), len(expected))
	}
	return nil
}

func validatePage(query Query, page Page, index int, last bool, pageCount int, timeoutMS int64) error {
	if !finiteNonnegative(page.ElapsedMS) || page.ElapsedMS > float64(timeoutMS) {
		return fmt.Errorf("query %s page %d exceeds its timeout or has an invalid duration", query.ID, index)
	}
	if page.HasMore == last {
		return fmt.Errorf("query %s page %d has an inconsistent continuation state", query.ID, index)
	}
	if len(page.OccurrenceIDs) > query.PageSize || (!last && len(page.OccurrenceIDs) != query.PageSize) {
		return fmt.Errorf("query %s page %d has an oversized or premature short page", query.ID, index)
	}
	if len(page.OccurrenceIDs) == 0 && (query.ExpectedTotal != 0 || !last || pageCount != 1) {
		return fmt.Errorf("query %s has an unexpected empty page", query.ID)
	}
	return nil
}

// ValidateReport requires complete cold and warm traversals for every query.
// Repeated traversals must retain the order recorded for their cache state.
func ValidateReport(report Report, battery Battery, batteryBytes []byte) error {
	if err := validateReportIdentity(report, battery, batteryBytes); err != nil {
		return err
	}
	if err := validateMeasurements(report); err != nil {
		return err
	}
	return validateReportTraversals(report, battery)
}

func validateMeasurements(report Report) error {
	resources := report.Resources
	if resources.ClydePeakRSSBytes <= 0 || resources.ModelPeakRSSBytes <= 0 ||
		resources.MilvusPeakRSSBytes <= 0 || resources.TotalPeakRSSBytes <= 0 ||
		resources.TemporaryBytes < 0 || resources.CompactedBytes <= 0 {
		return errors.New("report requires measured process RSS and compacted storage")
	}
	for _, peak := range []int64{resources.ClydePeakRSSBytes, resources.ModelPeakRSSBytes, resources.MilvusPeakRSSBytes} {
		if peak > resources.TotalPeakRSSBytes {
			return errors.New("process peak RSS exceeds the recorded total workload peak")
		}
	}
	ingestion := report.Ingestion
	for _, duration := range []float64{
		ingestion.SourceReadMS, ingestion.SelectionMS, ingestion.EmbeddingMS,
		ingestion.PersistenceMS, ingestion.SearchableCompletionMS, ingestion.ModelStartupMS,
	} {
		if !finiteNonnegative(duration) {
			return errors.New("ingestion stage duration is invalid")
		}
	}
	if ingestion.SearchableCompletionMS <= 0 || ingestion.SelectedCounts == nil || ingestion.ExcludedCounts == nil {
		return errors.New("report requires ingestion completion timing and selected and excluded counts")
	}
	for _, counts := range []map[string]int64{ingestion.SelectedCounts, ingestion.ExcludedCounts} {
		for identity, count := range counts {
			if identity == "" || count < 0 {
				return errors.New("content counts require nonempty identities and nonnegative values")
			}
		}
	}
	if ingestion.DistinctEmbeddings < 0 || ingestion.VectorWrites < 0 || ingestion.VectorReuse < 0 {
		return errors.New("embedding and vector counters must be nonnegative")
	}
	return nil
}

func validateReportIdentity(report Report, battery Battery, batteryBytes []byte) error {
	if report.SchemaVersion != SchemaVersion || report.BuildRevision == "" || report.HardwareIdentity == "" {
		return errors.New("report schema, build revision, and hardware identity are required")
	}
	if !validDigest(report.CorpusDigest) || report.BatteryDigest != Digest(batteryBytes) {
		return errors.New("report corpus or battery digest is invalid")
	}
	if battery.Concurrency <= 0 || report.Concurrency != battery.Concurrency || len(battery.Entries) == 0 {
		return errors.New("report requires a nonempty battery and matching positive concurrency")
	}
	model := report.ModelDescriptor
	if model.Name == "" || model.Revision == "" || model.Dimension <= 0 || model.Normalization == "" {
		return errors.New("report requires a complete model descriptor")
	}
	return nil
}

func validateReportTraversals(report Report, battery Battery) error {
	queries := make(map[string]Query, len(battery.Entries))
	for _, query := range battery.Entries {
		if _, exists := queries[query.ID]; exists || query.ID == "" || query.Query == "" {
			return errors.New("battery query IDs must be unique and query text must be nonempty")
		}
		queries[query.ID] = query
	}
	type traversalKey struct {
		query string
		cold  bool
	}
	orders := make(map[traversalKey][]string)
	for _, traversal := range report.Traversals {
		query, exists := queries[traversal.QueryID]
		if !exists {
			return fmt.Errorf("report includes unknown query %q", traversal.QueryID)
		}
		if err := ValidateTraversal(query, traversal, battery.TimeoutMS); err != nil {
			return err
		}
		var order []string
		for _, page := range traversal.Pages {
			order = append(order, page.OccurrenceIDs...)
		}
		key := traversalKey{query: query.ID, cold: traversal.Cold}
		if previous, exists := orders[key]; exists && !slices.Equal(previous, order) {
			return fmt.Errorf("query %s changes order within its recorded cache state", query.ID)
		}
		orders[key] = order
	}
	for id := range queries {
		for _, cold := range []bool{false, true} {
			if _, exists := orders[traversalKey{query: id, cold: cold}]; !exists {
				return fmt.Errorf("query %s is missing a cold or warm traversal", id)
			}
		}
	}
	if !report.Ingestion.Completed || !report.Ingestion.UnchangedPassCompleted {
		return errors.New("initial and unchanged ingestion measurements must complete")
	}
	if report.Ingestion.UnchangedPassEmbeddingRequests != 0 || report.Ingestion.UnchangedPassVectorWrites != 0 {
		return errors.New("unchanged ingestion performed an embedding request or vector write")
	}
	return nil
}

// Digest computes the SHA-256 identity of an immutable input artifact.
func Digest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func finiteNonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}
