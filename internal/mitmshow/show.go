// Package mitmshow correlates one request id across Clyde adapter, daemon, and
// MITM per-concern wire logs and the SQLite capture store, and renders the
// result as human-readable text or a typed JSON document. The daemon owns the
// capture store and the logs, so this logic runs daemon-side and the CLI prints
// the rendered string.
package mitmshow

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	_ "github.com/mattn/go-sqlite3" // database/sql driver "sqlite3"

	"goodkind.io/clyde/internal/config"
	"goodkind.io/clyde/internal/slogger"
)

// IDKind names one of the three correlation id shapes the show command
// recognizes. The "any" value is used when the input does not match a known
// shape so the lookup is still attempted across every source.
type IDKind string

const (
	// IDKindClyde labels a Clyde adapter request id of the form chatcmpl-<hex>.
	IDKindClyde IDKind = "clyde"
	// IDKindCursor labels a Cursor request id, a UUID.
	IDKindCursor IDKind = "cursor"
	// IDKindUpstream labels an Anthropic upstream request id of the form req_<token>.
	IDKindUpstream IDKind = "upstream"
	// IDKindAny labels an opaque id whose shape did not match any known kind.
	IDKindAny IDKind = "any"
)

// Section is one named source's matched lines for one lookup pass.
type Section struct {
	Source  string   `json:"source"`
	Path    string   `json:"path"`
	Matches []string `json:"matches"`
}

// CaptureSection lists SQLite capture-store rows whose correlation ids match
// the looked-up id for one lookup pass.
type CaptureSection struct {
	Source string       `json:"source"`
	Path   string       `json:"path"`
	Rows   []CaptureRow `json:"rows"`
}

// CaptureRow is the typed projection of one `requests` row the show command
// surfaces. Bodies live in the side table and are not loaded here.
type CaptureRow struct {
	Timestamp         int64  `json:"ts"`
	Client            string `json:"client"`
	Provider          string `json:"provider"`
	Concern           string `json:"concern"`
	Host              string `json:"host"`
	Method            string `json:"method"`
	Path              string `json:"path"`
	Status            int    `json:"status"`
	RequestID         string `json:"request_id"`
	UpstreamRequestID string `json:"upstream_request_id"`
	SessionID         string `json:"session_id"`
	TraceID           string `json:"trace_id"`
}

// Correlation captures the resolved correlation context after a lookup. Each
// field is the empty string when the field was never observed in any matched
// log line.
type Correlation struct {
	ClydeRequestID    string `json:"clyde_request_id"`
	CursorRequestID   string `json:"cursor_request_id"`
	UpstreamRequestID string `json:"upstream_request_id"`
	TraceID           string `json:"trace_id"`
}

// LookupPass is the result of one lookup against a single id.
type LookupPass struct {
	ID       string         `json:"id"`
	Sections []Section      `json:"sections"`
	Capture  CaptureSection `json:"capture"`
	Found    Correlation    `json:"found"`
}

// ShowOutput is the typed JSON document emitted by clyde mitm show.
type ShowOutput struct {
	Query       string       `json:"query"`
	Kind        IDKind       `json:"kind"`
	Correlation Correlation  `json:"correlation"`
	Passes      []LookupPass `json:"passes"`
}

// uuidPattern matches the canonical 8-4-4-4-12 UUID shape, case insensitive.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ClassifyID detects an id's shape so the caller can label the kind in
// output. The detector is intentionally tolerant: an unrecognized shape
// falls through to IDKindAny rather than aborting, since the underlying
// lookup is a literal substring scan and works for any opaque id.
func ClassifyID(id string) IDKind {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "chatcmpl-") {
		return IDKindClyde
	}
	if strings.HasPrefix(id, "req_") {
		return IDKindUpstream
	}
	if uuidPattern.MatchString(id) {
		return IDKindCursor
	}
	return IDKindAny
}

// Lookup runs the capture correlation lookup and returns the typed output.
func Lookup(ctx context.Context, cfg *config.Config, inputID string) (ShowOutput, error) {
	inputID = strings.TrimSpace(inputID)
	if inputID == "" {
		return ShowOutput{}, errors.New("id must not be empty")
	}
	sources := resolveSources(cfg)
	kind := ClassifyID(inputID)

	correlation := Correlation{
		ClydeRequestID:    "",
		CursorRequestID:   "",
		UpstreamRequestID: "",
		TraceID:           "",
	}
	switch kind {
	case IDKindClyde:
		correlation.ClydeRequestID = inputID
	case IDKindCursor:
		correlation.CursorRequestID = inputID
	case IDKindUpstream:
		correlation.UpstreamRequestID = inputID
	case IDKindAny:
	}

	firstPass := runOnePass(ctx, sources, inputID)
	mergeCorrelation(&correlation, firstPass.Found)

	passes := []LookupPass{firstPass}

	if kind != IDKindUpstream && correlation.UpstreamRequestID != "" && correlation.UpstreamRequestID != inputID {
		secondPass := runOnePass(ctx, sources, correlation.UpstreamRequestID)
		mergeCorrelation(&correlation, secondPass.Found)
		passes = append(passes, secondPass)
	}

	return ShowOutput{
		Query:       inputID,
		Kind:        kind,
		Correlation: correlation,
		Passes:      passes,
	}, nil
}

// sourceSet groups every input path the lookup walks for one config snapshot.
// Keeping this as a named struct keeps runOnePass independent of config and
// trivial to fixture in tests.
type sourceSet struct {
	adapterAnthropicReq string
	adapterHTTPErrors   string
	adapterChatDir      string
	daemonLog           string
	mitmWire            string
	mitmErrors          string
	mitmLifecycle       string
	captureDBPath       string
}

func resolveSources(cfg *config.Config) sourceSet {
	concernRoot := slogger.DefaultConcernRoot(cfg.Logging, slogger.ProcessRoleDaemon)
	daemonLog := slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon)
	return sourceSet{
		adapterAnthropicReq: filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernAdapterProviderAnthReq)),
		adapterHTTPErrors:   filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernAdapterHTTPErrors)),
		adapterChatDir:      filepath.Join(concernRoot, "adapter", "chat"),
		daemonLog:           daemonLog,
		mitmWire:            filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernProviderMITMWire)),
		mitmErrors:          filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernProviderMITMErrors)),
		mitmLifecycle:       filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernProviderMITMLifecycle)),
		captureDBPath:       strings.TrimSpace(cfg.MITM.CaptureStore.DBPath),
	}
}

// runOnePass searches every configured source for literal substring matches
// of id and updates the correlation context from each matched JSONL line.
func runOnePass(ctx context.Context, sources sourceSet, id string) LookupPass {
	pass := LookupPass{
		ID:       id,
		Sections: []Section{},
		Capture:  CaptureSection{Source: "mitm capture store", Path: sources.captureDBPath, Rows: []CaptureRow{}},
		Found: Correlation{
			ClydeRequestID:    "",
			CursorRequestID:   "",
			UpstreamRequestID: "",
			TraceID:           "",
		},
	}

	pass.Sections = append(pass.Sections, searchFile(
		"adapter anthropic provider request log", sources.adapterAnthropicReq, id, sourceKindAdapter, &pass.Found,
	))
	pass.Sections = append(pass.Sections, searchFile(
		"adapter http errors log", sources.adapterHTTPErrors, id, sourceKindAdapter, &pass.Found,
	))
	pass.Sections = append(pass.Sections, searchChatDir(sources.adapterChatDir, id, &pass.Found))
	pass.Sections = append(pass.Sections, searchFile(
		"clyde daemon log", sources.daemonLog, id, sourceKindAdapter, &pass.Found,
	))
	pass.Sections = append(pass.Sections, searchFile(
		"mitm wire concern log", sources.mitmWire, id, sourceKindCapture, &pass.Found,
	))
	pass.Sections = append(pass.Sections, searchFile(
		"mitm errors concern log", sources.mitmErrors, id, sourceKindCapture, &pass.Found,
	))
	pass.Sections = append(pass.Sections, searchFile(
		"mitm lifecycle concern log", sources.mitmLifecycle, id, sourceKindCapture, &pass.Found,
	))
	pass.Capture = searchCaptureStore(ctx, sources.captureDBPath, id, &pass.Found)
	return pass
}

// sourceKind tells the correlation merger how to interpret the overloaded
// request_id field, which carries the Clyde request id in adapter and daemon
// logs and the Cursor request id in the MITM per-concern wire logs.
type sourceKind int

const (
	sourceKindAdapter sourceKind = iota
	sourceKindCapture
)

// searchFile returns the matched lines for one jsonl file. A non-existent
// path is reported as an empty Matches slice; the caller decides how to
// render that case.
func searchFile(name, path, id string, kind sourceKind, found *Correlation) Section {
	section := Section{Source: name, Path: path, Matches: []string{}}
	file, err := os.Open(path)
	if err != nil {
		return section
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, id) {
			continue
		}
		section.Matches = append(section.Matches, line)
		updateCorrelation(line, kind, found)
	}
	return section
}

// searchChatDir scans every jsonl file directly under the adapter chat
// directory. It collapses matches from every file into one Section.
func searchChatDir(dir, id string, found *Correlation) Section {
	section := Section{Source: "adapter chat logs", Path: dir, Matches: []string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return section
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		filePath := filepath.Join(dir, name)
		fileSection := searchFile(name, filePath, id, sourceKindAdapter, found)
		section.Matches = append(section.Matches, fileSection.Matches...)
	}
	return section
}

// searchCaptureStore returns every SQLite capture-store row whose request id,
// upstream request id, session id, or trace id matches id, newest first. The
// database is opened read-only and immutable so the lookup never contends with
// the daemon's live writer. A missing database is reported as an empty section.
func searchCaptureStore(ctx context.Context, dbPath, id string, found *Correlation) CaptureSection {
	section := CaptureSection{Source: "mitm capture store", Path: dbPath, Rows: []CaptureRow{}}
	if dbPath == "" {
		return section
	}
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if !errors.Is(statErr, fs.ErrNotExist) {
			slog.WarnContext(ctx, "mitmshow.capture_stat_failed", "concern", "providers.mitm.lifecycle", "component", "daemon", "path", dbPath, "err", statErr)
		}
		return section
	}
	db, openErr := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&immutable=1&_busy_timeout=5000")
	if openErr != nil {
		slog.WarnContext(ctx, "mitmshow.capture_open_failed", "concern", "providers.mitm.lifecycle", "component", "daemon", "path", dbPath, "err", openErr)
		return section
	}
	defer func() { _ = db.Close() }()
	rows, queryErr := db.QueryContext(
		ctx,
		`SELECT ts, client, provider, concern, host, method, path, status,
			request_id, upstream_request_id, session_id, trace_id
		 FROM requests
		 WHERE request_id=? OR upstream_request_id=? OR session_id=? OR trace_id=?
		 ORDER BY ts DESC`,
		id, id, id, id,
	)
	if queryErr != nil {
		slog.WarnContext(ctx, "mitmshow.capture_query_failed", "concern", "providers.mitm.lifecycle", "component", "daemon", "path", dbPath, "err", queryErr)
		return section
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var row CaptureRow
		if scanErr := rows.Scan(
			&row.Timestamp, &row.Client, &row.Provider, &row.Concern, &row.Host, &row.Method, &row.Path, &row.Status,
			&row.RequestID, &row.UpstreamRequestID, &row.SessionID, &row.TraceID,
		); scanErr != nil {
			slog.WarnContext(ctx, "mitmshow.capture_scan_failed", "concern", "providers.mitm.lifecycle", "component", "daemon", "path", dbPath, "err", scanErr)
			continue
		}
		section.Rows = append(section.Rows, row)
		updateCorrelationFromCaptureRow(row, found)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		slog.WarnContext(ctx, "mitmshow.capture_rows_failed", "concern", "providers.mitm.lifecycle", "component", "daemon", "path", dbPath, "err", rowsErr)
	}
	return section
}

// updateCorrelationFromCaptureRow fills any still-empty correlation slot from a
// capture-store row. The store records the Cursor request id in request_id, so
// it maps to the Cursor slot, matching the per-concern wire-log convention.
func updateCorrelationFromCaptureRow(row CaptureRow, found *Correlation) {
	if found.TraceID == "" && row.TraceID != "" {
		found.TraceID = row.TraceID
	}
	if found.CursorRequestID == "" && row.RequestID != "" {
		found.CursorRequestID = row.RequestID
	}
	if found.UpstreamRequestID == "" && row.UpstreamRequestID != "" {
		found.UpstreamRequestID = row.UpstreamRequestID
	}
}

// lineFields is the typed projection of the few correlation fields the show
// command pulls out of one matched JSONL line. Anything else on the record is
// ignored.
type lineFields struct {
	RequestID         string `json:"request_id,omitempty"`
	CursorRequestID   string `json:"cursor_request_id,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	TraceID           string `json:"trace_id,omitempty"`
}

// updateCorrelation parses one JSONL line and fills in any correlation slots
// that are still empty. Source kind disambiguates the overloaded request_id
// field: adapter and daemon logs record the Clyde id; the MITM per-concern
// wire logs record the Cursor id.
func updateCorrelation(line string, kind sourceKind, found *Correlation) {
	var fields lineFields
	if err := json.Unmarshal([]byte(line), &fields); err != nil {
		return
	}
	if fields.TraceID != "" && found.TraceID == "" {
		found.TraceID = fields.TraceID
	}
	if fields.CursorRequestID != "" && found.CursorRequestID == "" {
		found.CursorRequestID = fields.CursorRequestID
	}
	if fields.UpstreamRequestID != "" && found.UpstreamRequestID == "" {
		found.UpstreamRequestID = fields.UpstreamRequestID
	}
	if fields.RequestID != "" {
		switch kind {
		case sourceKindAdapter:
			if found.ClydeRequestID == "" {
				found.ClydeRequestID = fields.RequestID
			}
		case sourceKindCapture:
			if found.CursorRequestID == "" {
				found.CursorRequestID = fields.RequestID
			}
		}
	}
}

// mergeCorrelation keeps the first non-empty value for each slot across
// passes; a second pass enriches the summary but never overwrites an id we
// already learned from the first.
func mergeCorrelation(dst *Correlation, src Correlation) {
	if dst.ClydeRequestID == "" {
		dst.ClydeRequestID = src.ClydeRequestID
	}
	if dst.CursorRequestID == "" {
		dst.CursorRequestID = src.CursorRequestID
	}
	if dst.UpstreamRequestID == "" {
		dst.UpstreamRequestID = src.UpstreamRequestID
	}
	if dst.TraceID == "" {
		dst.TraceID = src.TraceID
	}
}

// RenderText renders the human-readable report.
func RenderText(output ShowOutput) string {
	var builder strings.Builder
	writeText(&builder, output)
	return builder.String()
}

// writeText renders the human-readable report. One section per source, header
// line, then matched lines or "no matches".
func writeText(out io.Writer, output ShowOutput) {
	_, _ = fmt.Fprintf(out, "input id  : %s\n", output.Query)
	_, _ = fmt.Fprintf(out, "id kind   : %s\n", output.Kind)
	for index, pass := range output.Passes {
		if index > 0 {
			_, _ = fmt.Fprintf(out, "\n### expansion pass for upstream_request_id %s ###\n", pass.ID)
		}
		for _, section := range pass.Sections {
			writeSection(out, section)
		}
		writeCaptureSection(out, pass.Capture)
	}
	_, _ = fmt.Fprintf(out, "\n=== correlation ===\n")
	_, _ = fmt.Fprintf(out, "  clyde_request_id   : %s\n", emptyAsUnset(output.Correlation.ClydeRequestID))
	_, _ = fmt.Fprintf(out, "  cursor_request_id  : %s\n", emptyAsUnset(output.Correlation.CursorRequestID))
	_, _ = fmt.Fprintf(out, "  upstream_request_id: %s\n", emptyAsUnset(output.Correlation.UpstreamRequestID))
	_, _ = fmt.Fprintf(out, "  trace_id           : %s\n", emptyAsUnset(output.Correlation.TraceID))
}

func writeSection(out io.Writer, section Section) {
	_, _ = fmt.Fprintf(out, "\n=== %s ===\n", section.Source)
	_, _ = fmt.Fprintf(out, "source: %s\n", section.Path)
	if len(section.Matches) == 0 {
		_, _ = fmt.Fprintln(out, "no matches")
		return
	}
	for _, line := range section.Matches {
		_, _ = fmt.Fprintln(out, line)
	}
}

func writeCaptureSection(out io.Writer, capture CaptureSection) {
	_, _ = fmt.Fprintf(out, "\n=== %s ===\n", capture.Source)
	_, _ = fmt.Fprintf(out, "source: %s\n", capture.Path)
	if len(capture.Rows) == 0 {
		_, _ = fmt.Fprintln(out, "no matches")
		return
	}
	for _, row := range capture.Rows {
		_, _ = fmt.Fprintf(
			out, "%s %s %s %s %s %d req=%s upstream=%s session=%s trace=%s\n",
			row.Provider, row.Client, row.Concern, row.Method, row.Path, row.Status,
			emptyAsUnset(row.RequestID), emptyAsUnset(row.UpstreamRequestID),
			emptyAsUnset(row.SessionID), emptyAsUnset(row.TraceID),
		)
	}
}

func emptyAsUnset(value string) string {
	if value == "" {
		return "unset"
	}
	return value
}
