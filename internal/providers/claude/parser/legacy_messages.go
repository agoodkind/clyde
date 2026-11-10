package parser

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"

	"goodkind.io/clyde/internal/adapter/anthropic"
)

// resolvedSource records whether a request message comes from a blob that
// hashes as stored, a blob that hashes after converting string content to one
// text block, or a group of typed records.
type resolvedSource int

const (
	resolvedSourceBlob resolvedSource = iota
	resolvedSourceNormalizedBlob
	resolvedSourceGroup
)

// resolvedMessage stores one request message as JSON and its source.
type resolvedMessage struct {
	raw    []byte
	source resolvedSource
}

// recordMessageLine stores the message field of a transcript line.
type recordMessageLine struct {
	Message json.RawMessage `json:"message"`
}

// recordContent stores the content field of a message.
type recordContent struct {
	Content json.RawMessage `json:"content"`
}

// provenanceReader reads indexed transcript lines by byte location and builds
// hashes of typed record groups on first use.
type provenanceReader struct {
	file        io.ReaderAt
	index       *provenanceIndex
	groups      map[string][]string
	groupsBuilt bool
}

func newProvenanceReader(file io.ReaderAt, index *provenanceIndex) *provenanceReader {
	return &provenanceReader{file: file, index: index, groups: nil, groupsBuilt: false}
}

func (reader *provenanceReader) readLine(span lineSpan) ([]byte, error) {
	line := make([]byte, span.length)
	if _, err := reader.file.ReadAt(line, span.offset); err != nil {
		slog.Warn("providers.claude.parser.legacy_transcript_read_failed",
			"concern", concern, "component", "claude", "offset", span.offset, "err", err)
		return nil, newResolutionError("transcript line at offset "+strconv.FormatInt(span.offset, 10), "read failed", err)
	}
	return line, nil
}

// lineMessage reads one indexed line and returns its message field.
func (reader *provenanceReader) lineMessage(span lineSpan) ([]byte, error) {
	line, err := reader.readLine(span)
	if err != nil {
		return nil, err
	}
	subject := "transcript line at offset " + strconv.FormatInt(span.offset, 10)
	var decoded recordMessageLine
	if err := json.Unmarshal(line, &decoded); err != nil {
		return nil, newResolutionError(subject, "decode failed", err)
	}
	if len(decoded.Message) == 0 {
		return nil, newResolutionError(subject, "the line has no message")
	}
	return decoded.Message, nil
}

// resolveReference returns the request message for one reference hash. A blob
// with that hash must hash to the reference; without a blob, a group of typed
// records with the same hash supplies the message. It returns an error with
// the reference hash in every other case.
func (reader *provenanceReader) resolveReference(ref string) (resolvedMessage, error) {
	noMessage := resolvedMessage{raw: nil, source: resolvedSourceGroup}
	subject := "request reference " + ref
	if span, ok := reader.index.blobs[ref]; ok {
		message, err := reader.lineMessage(span)
		if err != nil {
			return noMessage, err
		}
		hash, ok := canonicalMessageHash(message)
		if !ok {
			return noMessage, newResolutionError(subject, "the blob message is not valid JSON")
		}
		if hash != ref {
			return noMessage, newResolutionError(subject, "the blob message hashes to "+hash)
		}
		return resolvedMessage{raw: message, source: blobSource(message)}, nil
	}
	reader.buildGroups()
	uuids, ok := reader.groups[ref]
	if !ok {
		return noMessage, newResolutionError(subject, "no blob or typed record group has this hash")
	}
	message, err := reader.groupMessage(uuids)
	if err != nil {
		return noMessage, err
	}
	return resolvedMessage{raw: message, source: resolvedSourceGroup}, nil
}

func blobSource(message []byte) resolvedSource {
	var content recordContent
	if err := json.Unmarshal(message, &content); err != nil {
		return resolvedSourceBlob
	}
	trimmed := bytes.TrimSpace(content.Content)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		return resolvedSourceNormalizedBlob
	}
	return resolvedSourceBlob
}

// buildGroups hashes candidate typed record groups once, grouping all
// assistant records with one message.id in transcript order and each run of
// consecutive user records containing only tool_result blocks. It also hashes
// each record in a run of more than one. The first group with a hash keeps
// that hash.
func (reader *provenanceReader) buildGroups() {
	if reader.groupsBuilt {
		return
	}
	reader.groupsBuilt = true
	reader.groups = make(map[string][]string)
	assistantGroups := make(map[string][]string)
	assistantOrder := make([]string, 0)
	runs := make([][]string, 0)
	run := make([]string, 0)
	for _, uuid := range reader.index.order {
		record := reader.index.records[uuid]
		if record.user && record.toolResultsOnly {
			run = append(run, uuid)
			continue
		}
		if len(run) > 0 {
			runs = append(runs, run)
			run = make([]string, 0)
		}
		if !record.user && record.messageID != "" {
			if _, seen := assistantGroups[record.messageID]; !seen {
				assistantOrder = append(assistantOrder, record.messageID)
			}
			assistantGroups[record.messageID] = append(assistantGroups[record.messageID], uuid)
		}
	}
	if len(run) > 0 {
		runs = append(runs, run)
	}
	for _, messageID := range assistantOrder {
		reader.addGroup(assistantGroups[messageID])
	}
	for _, members := range runs {
		reader.addGroup(members)
		if len(members) == 1 {
			continue
		}
		for _, member := range members {
			reader.addGroup([]string{member})
		}
	}
}

// addGroup hashes the records' combined message and adds the group only when
// another group does not already use that hash. It omits the group if it
// cannot read or decode a record.
func (reader *provenanceReader) addGroup(uuids []string) {
	message, err := reader.groupMessage(uuids)
	if err != nil {
		return
	}
	hash, ok := canonicalMessageHash(message)
	if !ok {
		return
	}
	if _, seen := reader.groups[hash]; !seen {
		reader.groups[hash] = uuids
	}
}

// groupMessage builds one request message using the first record's role and
// all records' content blocks in order.
func (reader *provenanceReader) groupMessage(uuids []string) ([]byte, error) {
	if len(uuids) == 0 {
		return nil, newResolutionError("typed record group", "the group is empty")
	}
	role := string(EntryTypeAssistant)
	if reader.index.records[uuids[0]].user {
		role = string(EntryTypeUser)
	}
	var out bytes.Buffer
	out.WriteString(`{"role":`)
	out.WriteString(strconv.Quote(role))
	out.WriteString(`,"content":[`)
	written := 0
	for _, uuid := range uuids {
		parts, err := reader.recordParts(uuid)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			if written > 0 {
				out.WriteByte(',')
			}
			out.Write(part)
			written++
		}
	}
	out.WriteString(`]}`)
	return out.Bytes(), nil
}

// recordParts returns one record's content blocks, converting string content
// to one text block.
func (reader *provenanceReader) recordParts(uuid string) ([]json.RawMessage, error) {
	subject := "typed record " + uuid
	record, ok := reader.index.records[uuid]
	if !ok {
		return nil, newResolutionError(subject, "the transcript has no such record")
	}
	message, err := reader.lineMessage(record.span)
	if err != nil {
		return nil, err
	}
	var content recordContent
	if err := json.Unmarshal(message, &content); err != nil {
		return nil, newResolutionError(subject, "decode message content failed", err)
	}
	trimmed := bytes.TrimSpace(content.Content)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text bytes.Buffer
		text.WriteString(`{"type":"text","text":`)
		text.Write(trimmed)
		text.WriteByte('}')
		return []json.RawMessage{text.Bytes()}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(content.Content, &parts); err != nil {
		return nil, newResolutionError(subject, "decode content blocks failed", err)
	}
	return parts, nil
}

// decodeSnapshotRequest decodes resolved messages with the Anthropic
// compaction request decoder to obtain the segment kinds and text the injector
// used. It preserves raw message bytes, and tool input text equals the stored
// JSON bytes.
func decodeSnapshotRequest(messages []resolvedMessage) (anthropic.CompactionRequest, error) {
	var body bytes.Buffer
	body.WriteString(`{"messages":[`)
	for index, message := range messages {
		if index > 0 {
			body.WriteByte(',')
		}
		body.Write(message.raw)
	}
	body.WriteString(`]}`)
	request, err := anthropic.DecodeCompactionRequest(body.Bytes())
	if err != nil {
		return request, newResolutionError("replayed request", "decode failed", err)
	}
	if len(request.Messages) != len(messages) {
		return request, newResolutionError("replayed request", "decoded "+strconv.Itoa(len(request.Messages))+" messages, want "+strconv.Itoa(len(messages)))
	}
	return request, nil
}
