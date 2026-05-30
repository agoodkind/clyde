package mitm

import (
	"bufio"
	"context"
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

	"github.com/spf13/cobra"

	"goodkind.io/clyde/internal/cli"
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

// RawSection lists raw-byte capture file paths discovered under the MITM
// capture directory for one lookup pass.
type RawSection struct {
	Source string   `json:"source"`
	Path   string   `json:"path"`
	Files  []string `json:"files"`
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
	ID       string      `json:"id"`
	Sections []Section   `json:"sections"`
	Raw      RawSection  `json:"raw"`
	Found    Correlation `json:"found"`
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

func newShowCmd(f *cli.Factory) *cobra.Command {
	return newShowCmdWithLoader(f, config.LoadGlobalOrDefault)
}

func newShowCmdWithLoader(f *cli.Factory, loadConfig func() (*config.Config, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Print log lines and MITM raw-capture paths that correlate to one request id",
		Long: "Show searches Clyde adapter, daemon, and MITM per-concern wire logs for any line " +
			"that mentions the given id. The id may be a Clyde request id (chatcmpl-<hex>), " +
			"a Cursor request id (UUID), or an Anthropic upstream request id (req_<token>); " +
			"the kind is detected by shape.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				slog.WarnContext(cmd.Context(), "cli.mitm.show.load_config_failed", "concern", "cli.mitm", "err", err)
				return fmt.Errorf("load config: %w", err)
			}
			return runShow(cmd.Context(), f.IOStreams.Out, cfg, args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit a typed JSON document instead of human-readable text")
	return cmd
}

// runShow executes the lookup for inputID and writes either the human readable
// text report or the typed JSON document to out. It performs at most one
// expansion: when the first pass matches an adapter line that exposes an
// upstream_request_id we did not start with, the lookup runs a second time
// against that upstream id and both rounds are reported.
func runShow(ctx context.Context, out io.Writer, cfg *config.Config, inputID string, asJSON bool) error {
	inputID = strings.TrimSpace(inputID)
	if inputID == "" {
		return errors.New("id must not be empty")
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

	firstPass := runOnePass(sources, inputID)
	mergeCorrelation(&correlation, firstPass.Found)

	passes := []LookupPass{firstPass}

	if kind != IDKindUpstream && correlation.UpstreamRequestID != "" && correlation.UpstreamRequestID != inputID {
		secondPass := runOnePass(sources, correlation.UpstreamRequestID)
		mergeCorrelation(&correlation, secondPass.Found)
		passes = append(passes, secondPass)
	}

	output := ShowOutput{
		Query:       inputID,
		Kind:        kind,
		Correlation: correlation,
		Passes:      passes,
	}

	if asJSON {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(output); err != nil {
			slog.WarnContext(ctx, "cli.mitm.show.encode_json_failed", "concern", "cli.mitm", "err", err)
			return fmt.Errorf("encode json: %w", err)
		}
		return nil
	}
	writeText(out, output)
	return nil
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
	mitmRawDir          string
}

func resolveSources(cfg *config.Config) sourceSet {
	concernRoot := slogger.DefaultConcernRoot(cfg.Logging, slogger.ProcessRoleDaemon)
	daemonLog := slogger.DefaultProcessPath(cfg.Logging, slogger.ProcessRoleDaemon)
	captureDir := expandHomeLocal(strings.TrimSpace(cfg.MITM.CaptureDir))
	return sourceSet{
		adapterAnthropicReq: filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernAdapterProviderAnthReq)),
		adapterHTTPErrors:   filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernAdapterHTTPErrors)),
		adapterChatDir:      filepath.Join(concernRoot, "adapter", "chat"),
		daemonLog:           daemonLog,
		mitmWire:            filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernProviderMITMWire)),
		mitmErrors:          filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernProviderMITMErrors)),
		mitmLifecycle:       filepath.Join(concernRoot, slogger.ConcernRelPath(slogger.ConcernProviderMITMLifecycle)),
		mitmRawDir:          filepath.Join(captureDir, "raw"),
	}
}

// runOnePass searches every configured source for literal substring matches
// of id and updates the correlation context from each matched JSONL line.
func runOnePass(sources sourceSet, id string) LookupPass {
	pass := LookupPass{
		ID:       id,
		Sections: []Section{},
		Raw:      RawSection{Source: "mitm raw capture files", Path: sources.mitmRawDir, Files: []string{}},
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
	pass.Raw = searchRawDir(sources.mitmRawDir, id)
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

// searchRawDir lists raw-byte capture files whose name encodes the id and any
// sibling .metadata.jsonl file that references the id. The directory layout
// is ${CaptureDir}/raw/<host>/. The walk is rooted with [os.Root] so
// any symlink that escapes the capture dir cannot redirect a metadata read.
func searchRawDir(dir, id string) RawSection {
	raw := RawSection{Source: "mitm raw capture files", Path: dir, Files: []string{}}
	if dir == "" {
		return raw
	}
	root, openErr := os.OpenRoot(dir)
	if openErr != nil {
		if !errors.Is(openErr, fs.ErrNotExist) {
			slog.Warn("cli.mitm.show.raw_open_failed", "concern", "cli.mitm", "dir", dir, "err", openErr)
		}
		return raw
	}
	defer func() { _ = root.Close() }()
	rootFS := root.FS()
	walkErr := fs.WalkDir(rootFS, ".", func(relPath string, entry fs.DirEntry, perEntryErr error) error {
		if perEntryErr != nil {
			return filepath.SkipDir
		}
		if entry.IsDir() {
			return nil
		}
		fullPath := filepath.Join(dir, relPath)
		if strings.Contains(entry.Name(), id) {
			raw.Files = append(raw.Files, fullPath)
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".metadata.jsonl") {
			content, readErr := fs.ReadFile(rootFS, relPath)
			if readErr == nil && strings.Contains(string(content), id) {
				raw.Files = append(raw.Files, fullPath)
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		slog.Warn("cli.mitm.show.raw_walk_failed", "concern", "cli.mitm", "dir", dir, "err", walkErr)
	}
	return raw
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

// writeText renders the human-readable report. The shell script's output is
// the reference for layout: one section per source, header line, then matched
// lines or "no matches".
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
		writeRawSection(out, pass.Raw)
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

func writeRawSection(out io.Writer, raw RawSection) {
	_, _ = fmt.Fprintf(out, "\n=== %s ===\n", raw.Source)
	_, _ = fmt.Fprintf(out, "source: %s\n", raw.Path)
	if len(raw.Files) == 0 {
		_, _ = fmt.Fprintln(out, "no matches")
		return
	}
	for _, file := range raw.Files {
		_, _ = fmt.Fprintln(out, file)
	}
}

func emptyAsUnset(value string) string {
	if value == "" {
		return "unset"
	}
	return value
}

// expandHomeLocal mirrors config.expandHome so the CLI can normalize a tilde
// prefix in MITMConfig.CaptureDir without depending on an internal config
// helper. The config loader already expands user-supplied CA paths; the
// CaptureDir field is normalized here so a "~/captures" override still
// resolves to an absolute path on disk.
func expandHomeLocal(path string) string {
	if path == "" {
		return path
	}
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
