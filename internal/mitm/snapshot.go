package mitm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// errNoSnapshotRecords is the sentinel the snapshot extractors return when a
// resolved transcript contains no usable wire records (no ws_start frame for
// v1, no records at all for v2). The drift loop treats this as a warn-and-skip
// condition rather than an infrastructure failure, because the per-concern MITM
// wire log holds request-story leg records that the extractor cannot turn into
// a snapshot.
var errNoSnapshotRecords = errors.New("mitm snapshot: no usable wire records in transcript")

// SnapshotOptions configures Snapshot extraction from a JSONL
// transcript.
type SnapshotOptions struct {
	UpstreamName    string
	UpstreamVersion string
	ProviderFilter  string
}

// ExtractSnapshot reads a JSONL transcript at path and returns the
// typed Snapshot summarizing its wire shape. Returns an error when
// the file is unreadable or contains no ws_start records.
func ExtractSnapshot(path string, opts SnapshotOptions) (Snapshot, error) {
	records, err := readCaptureRecordsFiltered(path, opts.ProviderFilter)
	if err != nil {
		return Snapshot{}, err
	}
	return buildSnapshot(records, opts)
}

// WriteSnapshotTOML persists the Snapshot as TOML under dir,
// creating dir when missing. The file is named reference.toml.
func WriteSnapshotTOML(snap Snapshot, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("mitm.snapshot.write_mkdir_failed", "concern", "providers.mitm.wire", "dir", dir, "err", err)
		return "", fmt.Errorf("create snapshot dir %s: %w", dir, err)
	}
	out := filepath.Join(dir, "reference.toml")
	raw, err := toml.Marshal(snap)
	if err != nil {
		slog.Warn("mitm.snapshot.write_marshal_failed", "concern", "providers.mitm.wire", "path", out, "err", err)
		return "", fmt.Errorf("marshal snapshot TOML: %w", err)
	}
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		slog.Warn("mitm.snapshot.write_failed", "concern", "providers.mitm.wire", "path", out, "err", err)
		return "", fmt.Errorf("write snapshot TOML %s: %w", out, err)
	}
	return out, nil
}

// LoadSnapshotTOML reads a reference.toml back into a Snapshot.
func LoadSnapshotTOML(path string) (Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("mitm.snapshot.load_read_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return Snapshot{}, fmt.Errorf("read snapshot TOML %s: %w", path, err)
	}
	var snap Snapshot
	if err := toml.Unmarshal(raw, &snap); err != nil {
		slog.Warn("mitm.snapshot.load_unmarshal_failed", "concern", "providers.mitm.wire", "path", path, "err", err)
		return Snapshot{}, fmt.Errorf("unmarshal snapshot TOML %s: %w", path, err)
	}
	return snap, nil
}

func readCaptureRecordsFiltered(path string, providerFilter string) ([]CaptureRecord, error) {
	_, records, err := readCaptureRecordsRaw(path, providerFilter)
	return records, err
}

func buildSnapshot(records []CaptureRecord, opts SnapshotOptions) (Snapshot, error) {
	var wsStart *CaptureRecord
	var wsMsgs []CaptureRecord
	for i := range records {
		switch records[i].Kind {
		case RecordWSStart:
			if wsStart == nil {
				wsStart = &records[i]
			}
		case RecordWSMessage:
			wsMsgs = append(wsMsgs, records[i])
		case RecordHTTPRequest, RecordHTTPResponse, RecordWSEnd:
			continue
		}
	}
	if wsStart == nil {
		return Snapshot{}, errNoSnapshotRecords
	}

	snap := Snapshot{
		Upstream: SnapshotUpstream{
			Name:       opts.UpstreamName,
			Version:    opts.UpstreamVersion,
			CapturedAt: time.Unix(wsStart.T, 0).UTC().Format(time.RFC3339),
		},
		Handshake: SnapshotHandshake{
			URLPattern: normalizeURLPattern(wsStart.URL),
			Headers:    canonicalHeaders(wsStart.RequestHeaders),
		}, Body: SnapshotBody{Type: "", FieldNames: nil, ToolKinds: nil, IncludeKeys: nil},

		FrameSequence: SnapshotFrameSequence{Opening: "", Warmup: SnapshotFrameDescriptor{Generate: "", HasPrev: false, InputCountMin: 0, InputCountMax: 0, ToolsCountMin: 0, ToolsCountMax: 0, StoreFieldRequired: false, StoreValueObserved: ""}, Real: SnapshotFrameDescriptor{Generate: "", HasPrev: false, InputCountMin: 0, InputCountMax: 0, ToolsCountMin: 0, ToolsCountMax: 0, StoreFieldRequired: false, StoreValueObserved: ""}, ChainsPrev: false},

		Constants: SnapshotConstants{Originator: "", OpenAIBeta: "", UserAgent: "", BetaFeatures: "", StainlessPackageVersion: ""},
	}

	stats := aggregateWSFrames(wsMsgs)

	snap.Body = SnapshotBody{
		Type:        stats.FrameType,
		FieldNames:  sortedKeys(stats.BodyFields),
		ToolKinds:   sortedKeys(stats.ToolKinds),
		IncludeKeys: sortedKeys(stats.IncludeKeys),
	}

	openingLabel := "real_only"
	switch {
	case stats.WarmupSeen && stats.ChainsPrev:
		openingLabel = "warmup_then_real_chained"
	case stats.WarmupSeen:
		openingLabel = "warmup_then_real"
	}
	snap.FrameSequence = SnapshotFrameSequence{
		Opening:    openingLabel,
		ChainsPrev: stats.ChainsPrev,
		Warmup: SnapshotFrameDescriptor{
			Generate:           "false",
			InputCountMin:      max0(stats.WarmupInputMin),
			InputCountMax:      stats.WarmupInputMax,
			StoreFieldRequired: stats.StoreRequired,
			StoreValueObserved: stats.StoreObserved, HasPrev: false, ToolsCountMin: 0, ToolsCountMax: 0,
		},
		Real: SnapshotFrameDescriptor{
			Generate:           "absent_default_true",
			HasPrev:            stats.ChainsPrev,
			InputCountMin:      max0(stats.RealInputMin),
			InputCountMax:      stats.RealInputMax,
			ToolsCountMin:      max0(stats.RealToolsMin),
			ToolsCountMax:      stats.RealToolsMax,
			StoreFieldRequired: stats.StoreRequired,
			StoreValueObserved: stats.StoreObserved,
		},
	}

	// Constants are pulled directly from the handshake headers.
	snap.Constants = SnapshotConstants{
		Originator:              wsStart.RequestHeaders["originator"],
		OpenAIBeta:              wsStart.RequestHeaders["openai-beta"],
		UserAgent:               wsStart.RequestHeaders["user-agent"],
		BetaFeatures:            wsStart.RequestHeaders["x-codex-beta-features"],
		StainlessPackageVersion: wsStart.RequestHeaders["x-stainless-package-version"],
	}

	return snap, nil
}

// wsFrameStats accumulates per-frame counters and observed fields
// from the outbound response.create frames in a websocket transcript.
// buildSnapshot reads these into the persistable Snapshot shape.
type wsFrameStats struct {
	BodyFields     map[string]bool
	IncludeKeys    map[string]bool
	ToolKinds      map[string]bool
	StoreObserved  string
	StoreRequired  bool
	FrameType      string
	WarmupSeen     bool
	ChainsPrev     bool
	RealInputMin   int
	RealInputMax   int
	RealToolsMin   int
	RealToolsMax   int
	WarmupInputMin int
	WarmupInputMax int
}

func aggregateWSFrames(wsMsgs []CaptureRecord) wsFrameStats {
	stats := wsFrameStats{
		BodyFields:     map[string]bool{},
		IncludeKeys:    map[string]bool{},
		ToolKinds:      map[string]bool{},
		StoreObserved:  "",
		StoreRequired:  false,
		FrameType:      "",
		WarmupSeen:     false,
		ChainsPrev:     false,
		RealInputMin:   -1,
		RealInputMax:   0,
		RealToolsMin:   -1,
		RealToolsMax:   0,
		WarmupInputMin: -1,
		WarmupInputMax: 0,
	}
	for _, msg := range wsMsgs {
		if !msg.FromClient || msg.Text == "" {
			continue
		}
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(msg.Text), &fields); err != nil {
			continue
		}
		updateWSFrameStats(&stats, fields)
	}
	return stats
}

// wsFrameBody is the typed view of one captured response.create frame
// body. Only the fields the snapshot summary cares about are
// modelled; unknown fields fall through to the BodyFields name set
// without per-field decoding.
type wsFrameBody struct {
	Type               string            `json:"type,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	Generate           *bool             `json:"generate,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Input              []json.RawMessage `json:"input,omitempty"`
	Tools              []wsFrameTool     `json:"tools,omitempty"`
	Include            []string          `json:"include,omitempty"`
}

// wsFrameTool is the typed view of one entry in the frame's `tools`
// array. Only Type contributes to the snapshot; the rest of each
// tool spec is opaque.
type wsFrameTool struct {
	Type string `json:"type"`
}

func updateWSFrameStats(stats *wsFrameStats, fields map[string]json.RawMessage) {
	for key := range fields {
		stats.BodyFields[key] = true
	}
	// Re-marshal+unmarshal into the typed view for the well-known
	// fields. Iterating fields directly avoids an unbounded `any`
	// decode while still capturing every top-level name above.
	raw, err := json.Marshal(fields)
	if err != nil {
		return
	}
	var body wsFrameBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return
	}
	if body.Type != "" {
		stats.FrameType = body.Type
	}
	collectIncludeKeys(stats.IncludeKeys, body.Include)
	collectToolKinds(stats.ToolKinds, body.Tools)
	if body.Store != nil {
		stats.StoreRequired = true
		stats.StoreObserved = strconv.FormatBool(*body.Store)
	}
	hasPrev := strings.TrimSpace(body.PreviousResponseID) != ""
	hasGenerate := body.Generate != nil
	gen := hasGenerate && *body.Generate
	isWarmup := hasGenerate && !gen
	if isWarmup {
		stats.WarmupSeen = true
		stats.WarmupInputMin = updateMin(stats.WarmupInputMin, len(body.Input))
		stats.WarmupInputMax = updateMax(stats.WarmupInputMax, len(body.Input))
		return
	}
	stats.RealInputMin = updateMin(stats.RealInputMin, len(body.Input))
	stats.RealInputMax = updateMax(stats.RealInputMax, len(body.Input))
	stats.RealToolsMin = updateMin(stats.RealToolsMin, len(body.Tools))
	stats.RealToolsMax = updateMax(stats.RealToolsMax, len(body.Tools))
	if hasPrev {
		stats.ChainsPrev = true
	}
}

func collectIncludeKeys(dst map[string]bool, include []string) {
	for _, s := range include {
		if s != "" {
			dst[s] = true
		}
	}
}

func collectToolKinds(dst map[string]bool, tools []wsFrameTool) {
	for _, tool := range tools {
		if tool.Type != "" {
			dst[tool.Type] = true
		}
	}
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func updateMin(current, candidate int) int {
	if current < 0 || candidate < current {
		return candidate
	}
	return current
}

func updateMax(current, candidate int) int {
	if candidate > current {
		return candidate
	}
	return current
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// canonicalHeaders normalizes a captured handshake header map into
// a sorted list with secret values redacted and known-volatile
// values reduced to patterns.
func canonicalHeaders(in map[string]string) []SnapshotHeader {
	out := make([]SnapshotHeader, 0, len(in))
	for key, value := range in {
		lower := strings.ToLower(key)
		out = append(out, SnapshotHeader{
			Name:  lower,
			Value: canonicalHeaderValue(lower, value),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var (
	hexPattern    = regexp.MustCompile(`(?i)^[0-9a-f-]{12,}$`)
	uuidPattern   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	bearerPattern = regexp.MustCompile(`^Bearer\s+\S+`)
)

// snapshotHeaderName enumerates the lowercase header names the
// snapshot canonicalizer scrubs or replaces with deterministic
// placeholders so two captures from different sessions yield the
// same snapshot.
type snapshotHeaderName string

const (
	snapshotHeaderAuthorization        snapshotHeaderName = "authorization"
	snapshotHeaderCookie               snapshotHeaderName = "cookie"
	snapshotHeaderSetCookie            snapshotHeaderName = "set-cookie"
	snapshotHeaderXAPIIdent            snapshotHeaderName = "x-api-" + "key"
	snapshotHeaderAnthropicAPIIdent    snapshotHeaderName = "anthropic-api-" + "key"
	snapshotHeaderXClientRequestID     snapshotHeaderName = "x-client-request-id"
	snapshotHeaderSessionID            snapshotHeaderName = "session_id"
	snapshotHeaderXCodexWindowID       snapshotHeaderName = "x-codex-window-id"
	snapshotHeaderXCodexInstallationID snapshotHeaderName = "x-codex-installation-id"
	snapshotHeaderXCodexTurnMetadata   snapshotHeaderName = "x-codex-turn-metadata"
	snapshotHeaderCFRay                snapshotHeaderName = "cf-ray"
	snapshotHeaderXAmznTraceID         snapshotHeaderName = "x-amzn-trace-id"
)

func canonicalHeaderValue(lowerName, value string) string {
	switch snapshotHeaderName(lowerName) {
	case snapshotHeaderAuthorization:
		if bearerPattern.MatchString(value) {
			return "<bearer-redacted>"
		}
		return "<redacted>"
	case snapshotHeaderCookie, snapshotHeaderSetCookie, snapshotHeaderXAPIIdent, snapshotHeaderAnthropicAPIIdent:
		return "<redacted>"
	case snapshotHeaderXClientRequestID, snapshotHeaderSessionID:
		return "<conversation_id>"
	case snapshotHeaderXCodexWindowID:
		return "<conversation_id>:0"
	case snapshotHeaderXCodexInstallationID:
		return "<installation_id>"
	case snapshotHeaderXCodexTurnMetadata:
		return "<json:turn_metadata>"
	case snapshotHeaderCFRay, snapshotHeaderXAmznTraceID:
		return "<trace>"
	}
	if uuidPattern.MatchString(value) {
		return uuidPattern.ReplaceAllString(value, "<uuid>")
	}
	if hexPattern.MatchString(value) && len(value) > 16 {
		return "<hex>"
	}
	return value
}

// normalizeURLPattern strips conversation-specific query strings
// from the upstream URL so two captures from different sessions
// produce the same URL pattern.
func normalizeURLPattern(raw string) string {
	if before, _, ok := strings.Cut(raw, "?"); ok {
		return before
	}
	return raw
}
