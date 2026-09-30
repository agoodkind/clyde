package searchacceptance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FrozenBattery preserves the original workload before oracle expectations exist.
type FrozenBattery struct {
	SchemaVersion           int                      `json:"schema_version"`
	QuerySetID              string                   `json:"query_set_id"`
	CorpusSnapshot          FrozenCorpusReference    `json:"corpus_snapshot"`
	RecoveredRequests       FrozenFileReference      `json:"recovered_requests"`
	Concurrency             int                      `json:"concurrency"`
	LatencyConcurrency      int                      `json:"latency_concurrency"`
	PageTimeoutSeconds      int64                    `json:"page_timeout_seconds"`
	TraversalTimeoutSeconds int64                    `json:"traversal_timeout_seconds"`
	Identity                string                   `json:"identity"`
	FilterFields            FrozenFilterDescriptions `json:"filter_fields"`
	Entries                 []FrozenQuery            `json:"entries"`
	Scenarios               []FrozenScenario         `json:"scenarios"`
}

// FrozenCorpusReference binds an immutable corpus manifest.
type FrozenCorpusReference struct {
	Path           string `json:"path"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

// FrozenFileReference binds an original input file.
type FrozenFileReference struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// FrozenFilterDescriptions preserves the declared selector documentation.
type FrozenFilterDescriptions struct {
	Provider             string `json:"provider"`
	Workspace            string `json:"workspace"`
	ConversationIDs      string `json:"conversation_ids"`
	After                string `json:"after"`
	Before               string `json:"before"`
	Roles                string `json:"roles"`
	IncludeArchived      string `json:"include_archived"`
	IncludeSubagents     string `json:"include_subagents"`
	MinScore             string `json:"min_score"`
	PerConversationLimit string `json:"per_conversation_limit"`
}

// FrozenQuery retains controls without inferring expected result identities.
type FrozenQuery struct {
	ID       string   `json:"id"`
	Tags     []string `json:"tags"`
	Query    string   `json:"query"`
	PageSize int      `json:"page_size"`
	Filter   Filter   `json:"filter"`
}

// FrozenScenario records an original public behavior requirement.
type FrozenScenario struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Description     string   `json:"description"`
	ConversationIDs []string `json:"conversation_ids"`
	QueryIDs        []string `json:"query_ids"`
}

// BatteryInspection binds validated workload values to the original bytes.
type BatteryInspection struct {
	OriginalDigest string        `json:"original_sha256"`
	Battery        FrozenBattery `json:"battery"`
}

// BatteryConstraints supplies the caller's required workload identities.
type BatteryConstraints struct {
	SchemaVersion int
	QuerySetID    string
	QueryIDs      []string
	Scenarios     []ScenarioConstraint
}

// ScenarioConstraint specifies the required identity, kind, and references.
type ScenarioConstraint struct {
	ID                string
	Kind              string
	RequireReferences bool
}

// InspectFrozenBattery validates only workload structure and byte identity.
// It does not read the corpus or establish expected results or acceptance.
func InspectFrozenBattery(path, expectedDigest string, constraints BatteryConstraints) (inspection BatteryInspection, err error) {
	defer func() {
		if err != nil {
			slog.Warn("search.acceptance.battery_rejected", "component", "searchacceptance", "concern", "battery", "err", err)
		}
	}()
	if !filepath.IsAbs(path) || !validDigest(expectedDigest) {
		return inspection, errors.New("absolute battery path and SHA-256 digest are required")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return inspection, fmt.Errorf("read original battery: %w", err)
	}
	if Digest(content) != expectedDigest {
		return inspection, errors.New("original battery SHA-256 differs")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inspection.Battery); err != nil {
		return inspection, fmt.Errorf("decode original battery: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return inspection, errors.New("original battery requires one JSON document")
	}
	if err := validateFrozenBattery(inspection.Battery, constraints); err != nil {
		return BatteryInspection{}, err
	}
	inspection.OriginalDigest = expectedDigest
	return inspection, nil
}

func validateFrozenBattery(battery FrozenBattery, constraints BatteryConstraints) error {
	if constraints.SchemaVersion <= 0 || constraints.QuerySetID == "" || len(constraints.QueryIDs) == 0 || len(constraints.Scenarios) == 0 {
		return errors.New("battery constraints require schema and workload identities")
	}
	if battery.SchemaVersion != constraints.SchemaVersion || battery.QuerySetID != constraints.QuerySetID || strings.TrimSpace(battery.Identity) == "" {
		return errors.New("unsupported original battery schema or identity")
	}
	if !filepath.IsAbs(battery.CorpusSnapshot.Path) || !validDigest(battery.CorpusSnapshot.ManifestSHA256) || !filepath.IsAbs(battery.RecoveredRequests.Path) || !validDigest(battery.RecoveredRequests.SHA256) {
		return errors.New("original battery requires absolute provenance paths and digests")
	}
	if battery.Concurrency <= 0 || battery.LatencyConcurrency <= 0 || battery.PageTimeoutSeconds <= 0 || battery.TraversalTimeoutSeconds < battery.PageTimeoutSeconds {
		return errors.New("original battery concurrency and timeouts are invalid")
	}
	if len(battery.Entries) != len(constraints.QueryIDs) || len(battery.Scenarios) != len(constraints.Scenarios) {
		return errors.New("original battery query or scenario count differs from constraints")
	}
	queries := make(map[string]bool, len(constraints.QueryIDs))
	for _, query := range battery.Entries {
		if queries[query.ID] || strings.TrimSpace(query.Query) == "" || query.PageSize <= 0 || len(query.Tags) == 0 {
			return fmt.Errorf("original query %s has repeated identity or invalid controls", query.ID)
		}
		if err := validateFrozenFilter(query.Filter); err != nil {
			return fmt.Errorf("validate original query %s: %w", query.ID, err)
		}
		queries[query.ID] = true
	}
	expected := make(map[string]bool, len(constraints.QueryIDs))
	for _, id := range constraints.QueryIDs {
		if id == "" || expected[id] {
			return errors.New("battery constraints contain invalid query identities")
		}
		expected[id] = true
		if !queries[id] {
			return errors.New("original battery query identity is missing")
		}
	}
	return validateFrozenScenarios(battery.Scenarios, queries, constraints.Scenarios)
}

func validateFrozenFilter(filter Filter) error {
	if filter.Provider != nil {
		switch *filter.Provider {
		case "claude", "codex", "cursor", "zed", "copilot":
		default:
			return errors.New("original filter provider is unsupported")
		}
	}
	if filter.MinScore != nil && !finiteNonnegative(*filter.MinScore) || filter.PerConversationLimit != nil && *filter.PerConversationLimit < 0 {
		return errors.New("original filter score or group limit is invalid")
	}
	var dates [2]time.Time
	for index, value := range []*string{filter.After, filter.Before} {
		if value == nil {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, *value)
		if err != nil {
			parsed, err = time.Parse(time.DateOnly, *value)
		}
		if err != nil {
			return fmt.Errorf("parse original filter date: %w", err)
		}
		dates[index] = parsed
	}
	if !dates[0].IsZero() && !dates[1].IsZero() && !dates[0].Before(dates[1]) {
		return errors.New("original filter time range is empty or reversed")
	}
	return nil
}

func validateFrozenScenarios(scenarios []FrozenScenario, queries map[string]bool, constraints []ScenarioConstraint) error {
	expected := make(map[string]ScenarioConstraint, len(constraints))
	for _, constraint := range constraints {
		if _, exists := expected[constraint.ID]; exists || constraint.ID == "" || constraint.Kind == "" {
			return errors.New("battery constraints contain invalid scenario identities")
		}
		expected[constraint.ID] = constraint
	}
	seen := make(map[string]bool, len(constraints))
	for _, scenario := range scenarios {
		constraint, exists := expected[scenario.ID]
		if !exists || seen[scenario.ID] || constraint.Kind != scenario.Kind || strings.TrimSpace(scenario.Description) == "" {
			return errors.New("original scenario identity or kind is invalid")
		}
		seen[scenario.ID] = true
		for _, id := range scenario.QueryIDs {
			if !queries[id] {
				return errors.New("original scenario references an unknown query")
			}
		}
		if constraint.RequireReferences && (len(scenario.QueryIDs) == 0 || len(scenario.ConversationIDs) == 0) {
			return errors.New("original scenario requires conversation and query identities")
		}
	}
	return nil
}
