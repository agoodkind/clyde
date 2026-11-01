package parser

import (
	"bytes"
	"encoding/json"
	"slices"
)

const (
	// These Claude Code record types store request snapshots and the messages they
	// reference.
	provenanceTypeRequest     = "api-request"
	provenanceTypeRequestBlob = "api-request-blob"
	// The index keeps up to this many recent request snapshots that preceded a
	// request with keep 0.
	provenanceResetCandidates = 3
)

// lineSpan stores the byte offset and length of one transcript line without
// its line ending.
type lineSpan struct {
	offset int64
	length int
}

// provenanceRecord stores a user or assistant record's location, whether it is
// a user record, its assistant message.id, and whether every content block is
// a tool_result.
type provenanceRecord struct {
	span            lineSpan
	user            bool
	messageID       string
	toolResultsOnly bool
}

// requestSnapshot stores one replayed api-request's id and ordered message
// references after applying keep and tail.
type requestSnapshot struct {
	id   string
	refs []string
}

// summaryProvenance stores a compact summary record's location and the request
// snapshots that preceded a reset before that summary, newest first.
type summaryProvenance struct {
	span       lineSpan
	candidates []requestSnapshot
}

// provenanceIndex stores byte locations of blobs, typed records, and compact
// summaries in one transcript, plus the api-request replay state. The resolver
// reads lines again at these locations instead of retaining their content in
// memory.
type provenanceIndex struct {
	blobs     map[string]lineSpan
	records   map[string]provenanceRecord
	order     []string
	summaries map[string]summaryProvenance
	refs      []string
	replaying bool
	latest    requestSnapshot
	hasLatest bool
	resets    []requestSnapshot
}

// provenanceLine stores the fields the index decodes from each transcript
// line.
type provenanceLine struct {
	Type             string             `json:"type"`
	UUID             string             `json:"uuid"`
	Hash             string             `json:"hash"`
	ID               string             `json:"id"`
	Keep             *int               `json:"keep"`
	Tail             []string           `json:"tail"`
	IsCompactSummary bool               `json:"isCompactSummary"`
	Message          *provenanceMessage `json:"message"`
}

// provenanceMessage stores the message.id, role, and content block types that
// the index decodes.
type provenanceMessage struct {
	ID      string                 `json:"id"`
	Role    string                 `json:"role"`
	Content provenanceContentTypes `json:"content"`
}

// provenanceContentTypes records whether content is a string and the type of
// each block when content is an array.
type provenanceContentTypes struct {
	text       bool
	blockTypes []string
}

// UnmarshalJSON records string content or the block types of array content.
// Other shapes leave both empty without failing the line decode.
func (content *provenanceContentTypes) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		content.text = true
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		content.blockTypes = make([]string, 0, len(blocks))
		for _, block := range blocks {
			content.blockTypes = append(content.blockTypes, block.Type)
		}
	}
	return nil
}

func (content provenanceContentTypes) toolResultsOnly() bool {
	if content.text || len(content.blockTypes) == 0 {
		return false
	}
	for _, blockType := range content.blockTypes {
		if contentBlockType(blockType) != contentBlockToolResult {
			return false
		}
	}
	return true
}

func newProvenanceIndex() *provenanceIndex {
	return &provenanceIndex{
		blobs:     make(map[string]lineSpan),
		records:   make(map[string]provenanceRecord),
		order:     make([]string, 0),
		summaries: make(map[string]summaryProvenance),
		refs:      nil,
		replaying: false,
		latest:    requestSnapshot{id: "", refs: nil},
		hasLatest: false,
		resets:    make([]requestSnapshot, 0, provenanceResetCandidates),
	}
}

// observe indexes one transcript line starting at offset. It records the first
// blob for each hash, replays api-request records, and records user and
// assistant records. Invalid JSON does not change the index.
func (index *provenanceIndex) observe(line []byte, offset int64) {
	var decoded provenanceLine
	if err := json.Unmarshal(line, &decoded); err != nil {
		return
	}
	span := lineSpan{offset: offset, length: len(line)}
	switch decoded.Type {
	case provenanceTypeRequestBlob:
		if decoded.Hash != "" {
			if _, seen := index.blobs[decoded.Hash]; !seen {
				index.blobs[decoded.Hash] = span
			}
		}
	case provenanceTypeRequest:
		index.observeRequest(decoded)
	case string(EntryTypeUser), string(EntryTypeAssistant):
		index.observeRecord(decoded, span)
	}
}

// observeRequest replays an api-request by retaining the first keep references
// from the previous snapshot and appending tail. A request with keep 0 starts
// a new replay and saves the previous snapshot as a reset candidate. If keep
// is missing or exceeds the previous snapshot's reference count,
// observeRequest invalidates replay until the next request with keep 0.
func (index *provenanceIndex) observeRequest(decoded provenanceLine) {
	if decoded.Keep == nil {
		index.invalidateReplay()
		return
	}
	keep := *decoded.Keep
	if keep == 0 {
		if index.hasLatest {
			index.resets = append(index.resets, index.latest)
			if len(index.resets) > provenanceResetCandidates {
				index.resets = index.resets[len(index.resets)-provenanceResetCandidates:]
			}
		}
		index.replaying = true
	}
	if !index.replaying || keep < 0 || keep > len(index.refs) {
		index.invalidateReplay()
		return
	}
	refs := make([]string, 0, keep+len(decoded.Tail))
	refs = append(refs, index.refs[:keep]...)
	refs = append(refs, decoded.Tail...)
	index.refs = refs
	index.latest = requestSnapshot{id: decoded.ID, refs: refs}
	index.hasLatest = true
}

// invalidateReplay stops replay and clears the latest snapshot and live reset
// candidates because the transcript cannot prove them. It does not change
// candidates that the index already copied into earlier summaries.
func (index *provenanceIndex) invalidateReplay() {
	index.replaying = false
	index.hasLatest = false
	index.resets = index.resets[:0]
}

func (index *provenanceIndex) observeRecord(decoded provenanceLine, span lineSpan) {
	if decoded.UUID == "" || decoded.Message == nil {
		return
	}
	if _, seen := index.records[decoded.UUID]; seen {
		return
	}
	index.records[decoded.UUID] = provenanceRecord{
		span:            span,
		user:            decoded.Type == string(EntryTypeUser),
		messageID:       decoded.Message.ID,
		toolResultsOnly: decoded.Type == string(EntryTypeUser) && decoded.Message.Content.toolResultsOnly(),
	}
	index.order = append(index.order, decoded.UUID)
	if decoded.Type == string(EntryTypeUser) && decoded.IsCompactSummary && decoded.Message.Content.text {
		candidates := slices.Clone(index.resets)
		slices.Reverse(candidates)
		index.summaries[decoded.UUID] = summaryProvenance{span: span, candidates: candidates}
		// A compaction request precedes its summary record. The index discards request
		// snapshots from before this summary here because later summaries cannot use
		// them as their source.
		index.resets = index.resets[:0]
	}
}
