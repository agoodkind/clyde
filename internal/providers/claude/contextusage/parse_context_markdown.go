package contextusage

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"goodkind.io/clyde/internal/contextusage"
)

// unmarshalContextMarkdown decodes the markdown payload claude
// emits for /context. The payload is a sequence of sections; only
// the header fields (Model, Tokens) and the "Estimated usage by
// category" table feed the Snapshot. The MCP tools, Memory files,
// and Skills tables below are ignored: their totals are already
// rolled up into the header table rows.
//
// The expected shape is:
//
//	## Context Usage
//
//	**Model:** claude-haiku-4-5
//	**Tokens:** 95k / 200k (48%)
//
//	### Estimated usage by category
//
//	| Category | Tokens | Percentage |
//	|----------|--------|------------|
//	| System prompt | 6.6k | 3.3% |
//	...
//
// A missing header field or an empty category table returns an
// error; the planner cannot project a floor without a total and
// at least one category row.
func unmarshalContextMarkdown(payload string) (contextusage.Snapshot, error) {
	state := newMarkdownParseState()
	for raw := range strings.SplitSeq(payload, "\n") {
		if err := state.consume(raw); err != nil {
			return emptySnapshot(), err
		}
	}
	return state.finalize()
}

// markdownParseState carries the running parser context across
// lines. The state machine has three modes: header lines outside
// any table, inside the "Estimated usage by category" table, and
// past it. The split lets the top-level loop stay short.
type markdownParseState struct {
	snapshot            contextusage.Snapshot
	headerSeen          bool
	tableMode           bool
	tableHeaderConsumed bool
}

func newMarkdownParseState() *markdownParseState {
	return &markdownParseState{
		snapshot:            emptySnapshot(),
		headerSeen:          false,
		tableMode:           false,
		tableHeaderConsumed: false,
	}
}

func (s *markdownParseState) consume(raw string) error {
	trimmed := strings.TrimSpace(strings.TrimRight(raw, " \t\r"))
	if trimmed == "" {
		return nil
	}
	if after, ok := strings.CutPrefix(trimmed, "**Model:**"); ok {
		s.snapshot.Model = strings.TrimSpace(after)
		return nil
	}
	if after, ok := strings.CutPrefix(trimmed, "**Tokens:**"); ok {
		total, maxTokens, pct, err := unmarshalTokensHeader(after)
		if err != nil {
			return err
		}
		s.snapshot.TotalTokens = total
		s.snapshot.MaxTokens = maxTokens
		s.snapshot.Percentage = pct
		s.headerSeen = true
		return nil
	}
	if after, ok := strings.CutPrefix(trimmed, "### "); ok {
		s.tableMode = strings.TrimSpace(after) == "Estimated usage by category"
		s.tableHeaderConsumed = false
		return nil
	}
	if strings.HasPrefix(trimmed, "## ") {
		s.tableMode = false
		s.tableHeaderConsumed = false
		return nil
	}
	return s.consumeTableRow(trimmed)
}

func (s *markdownParseState) consumeTableRow(trimmed string) error {
	if !s.tableMode {
		return nil
	}
	if !strings.HasPrefix(trimmed, "|") {
		s.tableMode = false
		s.tableHeaderConsumed = false
		return nil
	}
	if isTableSeparator(trimmed) {
		s.tableHeaderConsumed = true
		return nil
	}
	if !s.tableHeaderConsumed {
		s.tableHeaderConsumed = true
		return nil
	}
	category, err := unmarshalCategoryRow(trimmed)
	if err != nil {
		return err
	}
	s.snapshot.Categories = append(s.snapshot.Categories, category)
	return nil
}

func (s *markdownParseState) finalize() (contextusage.Snapshot, error) {
	if !s.headerSeen {
		return emptySnapshot(), fmt.Errorf("unmarshal /context markdown: missing **Tokens:** header")
	}
	if len(s.snapshot.Categories) == 0 {
		return emptySnapshot(), fmt.Errorf("unmarshal /context markdown: empty category table")
	}
	return s.snapshot, nil
}

func emptySnapshot() contextusage.Snapshot {
	return contextusage.Snapshot{
		Model:       "",
		TotalTokens: 0,
		MaxTokens:   0,
		Percentage:  0,
		Categories:  nil,
	}
}

// unmarshalTokensHeader extracts total, max, and percentage from a
// string of the form " N / M (P%)". The header line uses short
// forms for N and M, so unmarshalShortTokens handles `6.6k`,
// `200k`, `< 20`, and bare integers. Inner parse failures are
// folded into the returned error string with %s rather than %w
// because the upstream caller surfaces this as a parse diagnostic,
// not a typed error chain.
func unmarshalTokensHeader(body string) (int, int, int, error) {
	body = strings.TrimSpace(body)
	totalText, rest, ok := strings.Cut(body, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("unmarshal /context tokens header: missing slash in %q", body)
	}
	totalText = strings.TrimSpace(totalText)
	rest = strings.TrimSpace(rest)

	maxText, paren, ok := strings.Cut(rest, "(")
	if !ok {
		return 0, 0, 0, fmt.Errorf("unmarshal /context tokens header: missing percent group in %q", body)
	}
	pctText, _, ok := strings.Cut(paren, ")")
	if !ok {
		return 0, 0, 0, fmt.Errorf("unmarshal /context tokens header: missing close paren in %q", body)
	}
	maxText = strings.TrimSpace(maxText)
	pctText = strings.TrimSpace(strings.TrimSuffix(pctText, "%"))

	total, err := unmarshalShortTokens(totalText)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("unmarshal /context tokens header total: %s", err.Error())
	}
	maxTokens, err := unmarshalShortTokens(maxText)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("unmarshal /context tokens header max: %s", err.Error())
	}
	pct, err := strconv.Atoi(pctText)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("unmarshal /context tokens header percentage %q: %s", pctText, err.Error())
	}
	return total, maxTokens, pct, nil
}

// unmarshalCategoryRow turns one pipe-split row of the category
// table into a Category. The row carries three cells: name,
// tokens, and percentage. Percentage feeds nothing downstream so
// the parser validates only that it is present.
func unmarshalCategoryRow(row string) (contextusage.Category, error) {
	cells := splitPipeRow(row)
	if len(cells) < 2 {
		return contextusage.Category{Name: "", Tokens: 0, IsDeferred: false},
			fmt.Errorf("unmarshal /context category row: not enough cells in %q", row)
	}
	name := strings.TrimSpace(cells[0])
	if name == "" {
		return contextusage.Category{Name: "", Tokens: 0, IsDeferred: false},
			fmt.Errorf("unmarshal /context category row: empty name in %q", row)
	}
	tokens, err := unmarshalShortTokens(strings.TrimSpace(cells[1]))
	if err != nil {
		return contextusage.Category{Name: "", Tokens: 0, IsDeferred: false},
			fmt.Errorf("unmarshal /context category row %q: %s", name, err.Error())
	}
	return contextusage.Category{
		Name:       name,
		Tokens:     tokens,
		IsDeferred: strings.HasSuffix(name, "(deferred)"),
	}, nil
}

// splitPipeRow returns the inner cells of a markdown table row,
// dropping the leading and trailing empty cells that surround the
// outer pipes.
func splitPipeRow(row string) []string {
	parts := strings.Split(row, "|")
	out := make([]string, 0, len(parts))
	for i, part := range parts {
		if (i == 0 || i == len(parts)-1) && strings.TrimSpace(part) == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// isTableSeparator reports whether the given table row is the
// `|----|----|` rule that separates the header from the body. The
// separator carries only dashes, colons, pipes, and whitespace.
func isTableSeparator(row string) bool {
	for _, r := range row {
		switch r {
		case '|', '-', ':', ' ', '\t':
			continue
		default:
			return false
		}
	}
	return true
}

// unmarshalShortTokens turns one cell of the tokens column into an
// integer. The cell can carry several forms claude's renderer
// produces:
//
//   - plain integer:   "123"          -> 123
//   - kilo suffix:     "6.6k"         -> 6600
//   - mega suffix:     "1.2m"         -> 1200000
//   - small bucket:    "< 20"         -> 20
//
// A leading "~" is also accepted; claude's skills table emits
// "~40" for approximate counts. The parser rounds to the nearest
// integer because the upstream rendering is already lossy.
func unmarshalShortTokens(text string) (int, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, fmt.Errorf("unmarshal short tokens: empty cell")
	}
	if after, ok := strings.CutPrefix(text, "<"); ok {
		text = strings.TrimSpace(after)
	}
	if after, ok := strings.CutPrefix(text, "~"); ok {
		text = strings.TrimSpace(after)
	}

	multiplier := 1.0
	if last := text[len(text)-1]; last == 'k' || last == 'K' {
		multiplier = 1_000
		text = text[:len(text)-1]
	} else if last == 'm' || last == 'M' {
		multiplier = 1_000_000
		text = text[:len(text)-1]
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return 0, fmt.Errorf("unmarshal short tokens %q: %s", text, err.Error())
	}
	return int(math.Round(value * multiplier)), nil
}
