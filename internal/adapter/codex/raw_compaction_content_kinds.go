package codex

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"

	codexstore "goodkind.io/clyde/internal/providers/codex/store"
)

const (
	rawCompactionMetadataField      = "internal_chat_message_metadata_passthrough"
	rawCompactionContentKindsField  = "content_item_kinds"
	rawCompactionUnknownContentKind = "unknown"
)

// rawCompactionAssistantHasTranscript reports whether a part with the Clyde
// content kind has text exactly equal to transcriptText. Ordinary text that
// quotes the transcript does not count.
func rawCompactionAssistantHasTranscript(item []byte, content []byte, transcriptText string) bool {
	var parts []rawCompactionSSEContentPart
	if json.Unmarshal(content, &parts) != nil {
		return false
	}
	for _, index := range codexstore.InjectedTranscriptIndexes(item) {
		if index < len(parts) && parts[index].Text == transcriptText {
			return true
		}
	}
	return false
}

// markRawCompactionTranscriptPart writes the Clyde content kind at partIndex
// in the item's content_item_kinds metadata and preserves other metadata
// fields. It returns false when the item has duplicate keys or metadata that
// it cannot extend.
func markRawCompactionTranscriptPart(item []byte, partIndex int) ([]byte, bool) {
	if !rawCompactionJSONHasUniqueObjectKeys(item) {
		return item, false
	}
	metadataStart, metadataEnd, hasMetadata := jsonObjectFieldValueRange(item, rawCompactionMetadataField)
	if !hasMetadata {
		metadata, ok := rawCompactionMetadataWithKinds([]byte("{}"), partIndex)
		if !ok {
			return item, false
		}
		return appendRawCompactionObjectField(item, rawCompactionMetadataField, metadata)
	}
	existing := item[metadataStart:metadataEnd]
	if bytes.Equal(existing, []byte("null")) {
		existing = []byte("{}")
	}
	metadata, ok := rawCompactionMetadataWithKinds(existing, partIndex)
	if !ok {
		return item, false
	}
	return replaceByteRange(item, metadataStart, metadataEnd, metadata), true
}

// rawCompactionMetadataWithKinds returns the metadata object with the extended
// content_item_kinds array. It returns false when metadata is not an object or
// the existing kinds are not an array.
func rawCompactionMetadataWithKinds(metadata []byte, partIndex int) ([]byte, bool) {
	if len(metadata) == 0 || metadata[0] != '{' {
		return nil, false
	}
	kindsStart, kindsEnd, hasKinds := jsonObjectFieldValueRange(metadata, rawCompactionContentKindsField)
	if !hasKinds {
		kinds, ok := rawCompactionContentKinds(nil, partIndex)
		if !ok {
			return nil, false
		}
		return appendRawCompactionObjectField(metadata, rawCompactionContentKindsField, kinds)
	}
	var existing []json.RawMessage
	if json.Unmarshal(metadata[kindsStart:kindsEnd], &existing) != nil {
		return nil, false
	}
	kinds, ok := rawCompactionContentKinds(existing, partIndex)
	if !ok {
		return nil, false
	}
	return replaceByteRange(metadata, kindsStart, kindsEnd, kinds), true
}

// rawCompactionContentKinds appends the Clyde content kind at partIndex and
// fills absent earlier entries with unknown. It returns false when existing
// entries already cover partIndex because the kinds array does not align with
// the content array.
func rawCompactionContentKinds(existing []json.RawMessage, partIndex int) ([]byte, bool) {
	if len(existing) > partIndex {
		return nil, false
	}
	kinds := append([]json.RawMessage(nil), existing...)
	for len(kinds) < partIndex {
		kinds = append(kinds, json.RawMessage(strconv.Quote(rawCompactionUnknownContentKind)))
	}
	kinds = append(kinds, json.RawMessage(strconv.Quote(codexstore.InjectedTranscriptContentKind)))
	encoded, err := marshalRawArray(kinds)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// appendRawCompactionObjectField inserts one field before the closing brace of
// a JSON object. It returns false when the input does not end in a closing
// brace.
func appendRawCompactionObjectField(object []byte, field string, value []byte) ([]byte, bool) {
	closing := len(object) - 1
	for closing >= 0 && (object[closing] == ' ' || object[closing] == '\t' || object[closing] == '\r' || object[closing] == '\n') {
		closing--
	}
	if closing < 0 || object[closing] != '}' {
		return object, false
	}
	separator := rawCompactionMemberSeparator(object[1:closing])
	name := strconv.AppendQuote(nil, field)
	return slices.Concat(object[:closing], separator, name, []byte{':'}, value, object[closing:]), true
}

func rawCompactionMemberSeparator(members []byte) []byte {
	if len(bytes.TrimSpace(members)) == 0 {
		return nil
	}
	return []byte{','}
}
