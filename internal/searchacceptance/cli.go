package searchacceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/conversation"
	"goodkind.io/clyde/internal/sandbox"
)

// PublicSearchHit retains the public fields compared across CLI and MCP.
type PublicSearchHit struct {
	SourceIdentity *SourceIdentity                 `json:"source_identity"`
	Conversation   conversation.Record             `json:"conversation"`
	MessageIndex   int                             `json:"message_index"`
	Role           string                          `json:"role"`
	Timestamp      time.Time                       `json:"timestamp"`
	Snippet        string                          `json:"snippet"`
	LoadRules      string                          `json:"load_rules"`
	Score          float64                         `json:"score"`
	ContextWindow  string                          `json:"context_window"`
	ContextState   conversation.SearchContextState `json:"context_state"`
	WireFields     PublicWirePresence              `json:"-"`
}

// PublicSearchFacetCount decodes the shared CLI/MCP facet value and count.
type PublicSearchFacetCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// PublicSearchFacets decodes the shared CLI/MCP facet groups.
type PublicSearchFacets struct {
	Workspaces []PublicSearchFacetCount `json:"workspaces,omitempty"`
	Providers  []PublicSearchFacetCount `json:"providers,omitempty"`
	Models     []PublicSearchFacetCount `json:"models,omitempty"`
}

// PublicSearchFreshness decodes the shared CLI/MCP semantic sync counters.
type PublicSearchFreshness struct {
	Manifest     int   `json:"manifest"`
	Needed       int   `json:"needed"`
	Embedded     int   `json:"embedded"`
	Pending      int   `json:"pending"`
	LastSyncUnix int64 `json:"last_sync_unix"`
}

// PublicSearchFilterStage decodes one shared CLI/MCP filter counter.
type PublicSearchFilterStage struct {
	Name      string `json:"name"`
	Remaining int    `json:"remaining"`
}

// PublicWireState distinguishes an omitted JSON field from null and a value.
type PublicWireState uint8

const (
	// PublicWireAbsent is returned for a JSON pointer absent from WireFields.
	PublicWireAbsent PublicWireState = iota
	// PublicWireNull identifies an explicit JSON null.
	PublicWireNull
	// PublicWireValue identifies a value, including false, zero and empty text.
	PublicWireValue
)

// PublicWirePresence records JSON pointer states without retaining payloads.
// WireFields is decoder metadata and is excluded from the public wire shape.
type PublicWirePresence map[string]PublicWireState

// PublicSearchPage preserves typed hit values before report hashing.
type PublicSearchPage struct {
	Matches              []PublicSearchHit         `json:"matches"`
	ReturnedCount        int                       `json:"returned_count"`
	Limit                int                       `json:"limit"`
	Offset               int                       `json:"offset"`
	NextOffset           int                       `json:"next_offset"`
	ConversationsScanned int                       `json:"conversations_scanned"`
	HasMore              bool                      `json:"has_more"`
	NextCursor           string                    `json:"next_cursor"`
	Source               string                    `json:"source"`
	Facets               PublicSearchFacets        `json:"facets"`
	SemanticFreshness    PublicSearchFreshness     `json:"semantic_freshness"`
	FilterAccounting     []PublicSearchFilterStage `json:"filter_accounting,omitempty"`
	WireFields           PublicWirePresence        `json:"-"`
}

// UnmarshalJSON retains presence separately from existing typed hit values.
func (hit *PublicSearchHit) UnmarshalJSON(data []byte) error {
	type decodedHit PublicSearchHit
	var decoded decodedHit
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode public search hit: %w", err)
	}
	presence, err := publicWirePresence(data)
	if err != nil {
		return fmt.Errorf("decode public search hit field presence: %w", err)
	}
	decoded.WireFields = presence
	*hit = PublicSearchHit(decoded)
	return nil
}

// UnmarshalJSON retains nested JSON presence separately from page values.
func (page *PublicSearchPage) UnmarshalJSON(data []byte) error {
	type decodedPage PublicSearchPage
	var decoded decodedPage
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode public search result: %w", err)
	}
	presence, err := publicWirePresence(data)
	if err != nil {
		return fmt.Errorf("decode public search result field presence: %w", err)
	}
	decoded.WireFields = presence
	*page = PublicSearchPage(decoded)
	return nil
}

func publicWirePresence(data []byte) (PublicWirePresence, error) {
	presence := make(PublicWirePresence)
	if err := appendPublicWirePresence(data, "", presence); err != nil {
		return nil, err
	}
	return presence, nil
}

// The clispec search output permits omitted context, cursor, facet and record
// fields. RawMessage is limited to inspecting JSON presence at this wire edge;
// the decoded result uses the concrete public types above.
func appendPublicWirePresence(data []byte, pointer string, presence PublicWirePresence) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		presence[pointer] = PublicWireNull
		return nil
	}
	presence[pointer] = PublicWireValue
	if len(data) == 0 {
		return errors.New("public JSON field has no value")
	}
	switch data[0] {
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			slog.Warn("search.acceptance.object_presence_decode_failed", "component", "searchacceptance", "concern", "query", "err", err)
			return fmt.Errorf("decode object field presence at %q: %w", pointer, err)
		}
		for field, value := range fields {
			field = strings.ReplaceAll(strings.ReplaceAll(field, "~", "~0"), "/", "~1")
			if err := appendPublicWirePresence(value, pointer+"/"+field, presence); err != nil {
				return err
			}
		}
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			slog.Warn("search.acceptance.array_presence_decode_failed", "component", "searchacceptance", "concern", "query", "err", err)
			return fmt.Errorf("decode array field presence at %q: %w", pointer, err)
		}
		for index, value := range values {
			if err := appendPublicWirePresence(value, pointer+"/"+strconv.Itoa(index), presence); err != nil {
				return err
			}
		}
	}
	return nil
}

type publicPage = PublicSearchPage

// ReadCLISearchPage executes one actual public command with bounded lifetime.
func ReadCLISearchPage(ctx context.Context, binary string, roots sandbox.Roots, home string, query Query, cursor string, timeoutMS int64) (PublicSearchPage, float64, error) {
	if err := validateCLIPaths(binary, roots, home); err != nil {
		return PublicSearchPage{}, 0, err
	}
	if query.PageSize <= 0 || timeoutMS <= 0 {
		return PublicSearchPage{}, 0, errors.New("positive public page size and deadline required")
	}
	args := queryArguments(query)
	if cursor != "" {
		args = append(args, "--cursor", cursor)
	}
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	for _, variable := range sandbox.Env(roots) {
		environment = append(environment, variable.Name+"="+variable.Value)
	}
	return readCLIPage(ctx, binary, environment, args, timeoutMS)
}

// ReadCLITraversal measures every public cursor page in an isolated daemon.
// The caller establishes the recorded cache state before the first request.
func ReadCLITraversal(ctx context.Context, binary string, roots sandbox.Roots, home string,
	query Query, cold bool, timeoutMS int64,
) (traversal Traversal, err error) {
	traversal = Traversal{QueryID: query.ID, Cold: cold, Total: 0, Pages: nil, Error: ""}
	defer func() {
		if err != nil {
			traversal.Error = err.Error()
			slog.WarnContext(ctx, "search.acceptance.traversal_failed", "component", "searchacceptance",
				"concern", "query", "query_id", query.ID, "err", err)
		}
	}()
	if err := validateCLIPaths(binary, roots, home); err != nil {
		return traversal, err
	}
	if query.PageSize <= 0 || timeoutMS <= 0 || query.ExpectedTotal < 0 {
		return traversal, errors.New("query page size and timeout must be positive and expected total must be nonnegative")
	}
	args := queryArguments(query)
	environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	for _, variable := range sandbox.Env(roots) {
		environment = append(environment, variable.Name+"="+variable.Value)
	}
	cursor := ""
	seenCursors := make(map[string]bool)
	effectiveQuery := query
	maximumPages := 1
	for pageIndex := 0; pageIndex < maximumPages; pageIndex++ {
		pageArgs := append([]string(nil), args...)
		if cursor != "" {
			pageArgs = append(pageArgs, "--cursor", cursor)
		}
		page, duration, err := readCLIPage(ctx, binary, environment, pageArgs, timeoutMS)
		if err != nil {
			return traversal, err
		}
		if pageIndex == 0 {
			if page.Limit <= 0 || page.Limit > query.PageSize {
				return traversal, errors.New("public search returned an invalid effective page limit")
			}
			effectiveQuery.PageSize = page.Limit
			maximumPages = query.ExpectedTotal/page.Limit + 2
		} else if page.Limit != effectiveQuery.PageSize {
			return traversal, errors.New("public search changed its effective page limit during traversal")
		}
		recorded, err := recordPublicPage(page, duration)
		if err != nil {
			return traversal, err
		}
		traversal.Pages = append(traversal.Pages, recorded)
		traversal.Total += len(recorded.OccurrenceIDs)
		if !page.HasMore {
			return traversal, ValidateTraversal(effectiveQuery, traversal, timeoutMS)
		}
		if page.NextCursor == "" || seenCursors[page.NextCursor] {
			return traversal, errors.New("public search returned an empty or repeated continuation cursor")
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return traversal, errors.New("public search did not terminate within the frozen expected result count")
}

func validateCLIPaths(binary string, roots sandbox.Roots, home string) error {
	if !filepath.IsAbs(binary) || !sandbox.UnderTempRoot(home) {
		return errors.New("measurement requires an absolute binary and temporary provider HOME")
	}
	if err := sandbox.PreflightRoots(roots); err != nil {
		slog.Warn("search.acceptance.sandbox_rejected", "component", "searchacceptance", "concern", "query", "err", err)
		return fmt.Errorf("validate measurement sandbox roots: %w", err)
	}
	if !strings.HasPrefix(filepath.Base(roots.Base), sandbox.RootPattern) {
		return errors.New("measurement requires a dedicated Clyde sandbox root")
	}
	for _, path := range []string{roots.State, roots.Config, roots.Cache, roots.Runtime} {
		if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(roots.Base)+string(os.PathSeparator)) {
			return errors.New("measurement roots must be children of the dedicated sandbox")
		}
	}
	return nil
}

func queryArguments(query Query) []string {
	filter := query.Filter
	args := []string{"conversation", "search"}
	if filter.ConversationIDs != nil {
		args = append(args, "--conversation-ids="+strings.Join(filter.ConversationIDs, ","))
	}
	args = append(args, "--query", query.Query, "--limit", strconv.Itoa(query.PageSize), "--output-format", "json")
	for _, selector := range []struct {
		flag  string
		value *string
	}{{"--provider", filter.Provider}, {"--workspace", filter.Workspace}, {"--after", filter.After}, {"--before", filter.Before}} {
		if selector.value != nil {
			args = append(args, selector.flag, *selector.value)
		}
	}
	if len(filter.Roles) > 0 {
		args = append(args, "--roles", strings.Join(filter.Roles, ","))
	}
	if filter.IncludeArchived {
		args = append(args, "--include-archived")
	}
	if filter.IncludeSubagents {
		args = append(args, "--include-subagents")
	}
	if filter.PerConversationLimit != nil {
		args = append(args, "--per-conversation-limit", strconv.Itoa(*filter.PerConversationLimit))
	}
	if filter.MinScore != nil {
		args = append(args, "--min-score", strconv.FormatFloat(*filter.MinScore, 'g', -1, 64))
	}
	return args
}

func readCLIPage(ctx context.Context, binary string, environment, args []string, timeoutMS int64) (publicPage, float64, error) {
	pageContext, cancel := context.WithTimeout(ctx, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(pageContext, binary, args...)
	command.Env = environment
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	slog.DebugContext(ctx, "search.acceptance.page_started", "component", "searchacceptance", "concern", "query")
	started := clock.Now()
	if err := command.Run(); err != nil {
		slog.WarnContext(ctx, "search.acceptance.command_failed", "component", "searchacceptance", "concern", "query", "err", err)
		return publicPage{}, 0, fmt.Errorf("public search command failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	duration := float64(clock.Since(started)) / float64(time.Millisecond)
	var page publicPage
	if err := json.Unmarshal(stdout.Bytes(), &page); err != nil {
		slog.WarnContext(ctx, "search.acceptance.page_decode_failed", "component", "searchacceptance", "concern", "query", "err", err)
		return page, duration, fmt.Errorf("decode public search page: %w", err)
	}
	return page, duration, nil
}

func recordPublicPage(page publicPage, duration float64) (Page, error) {
	limit := page.Limit
	result := Page{OccurrenceIDs: nil, ElapsedMS: duration, HasMore: page.HasMore, Limit: &limit}
	if page.ReturnedCount != len(page.Matches) || !page.HasMore && page.NextCursor != "" {
		return result, errors.New("public search page has inconsistent count or terminal cursor")
	}
	for _, hit := range page.Matches {
		if hit.SourceIdentity == nil {
			return result, errors.New("public search hit lacks source-manifest identity")
		}
		identity, err := IdentityKey(*hit.SourceIdentity)
		if err != nil {
			return result, err
		}
		result.OccurrenceIDs = append(result.OccurrenceIDs, identity)
	}
	return result, nil
}
