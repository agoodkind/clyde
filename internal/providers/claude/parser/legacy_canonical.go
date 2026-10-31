package parser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// Request hashing replaces a JSON string value in this message field with one
// text block.
const canonicalContentKey = "content"

// canonicalMember stores one object member's key and the canonical encoding of
// its value.
type canonicalMember struct {
	key   string
	value []byte
}

// canonicalMessageHash returns the lowercase hex SHA-256 of one request
// message's canonical encoding. Claude Code uses it as the api-request
// reference and api-request-blob hash. It returns false for invalid JSON.
func canonicalMessageHash(message []byte) (string, bool) {
	encoded, ok := canonicalMessage(message)
	if !ok {
		return "", false
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), true
}

// canonicalMessage encodes a message object as compact JSON with sorted keys
// and converts string content to one text block. It includes other fields,
// such as output_config, in the encoding.
func canonicalMessage(message []byte) ([]byte, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(message, &fields); err != nil {
		return nil, false
	}
	members := make([]canonicalMember, 0, len(fields))
	for key, raw := range fields {
		value, ok := canonicalMessageField(key, raw)
		if !ok {
			return nil, false
		}
		members = append(members, canonicalMember{key: key, value: value})
	}
	var out bytes.Buffer
	writeCanonicalObject(&out, members)
	return out.Bytes(), true
}

func canonicalMessageField(key string, raw json.RawMessage) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if key != canonicalContentKey || len(trimmed) == 0 || trimmed[0] != '"' {
		return canonicalJSON(raw)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, false
	}
	var out bytes.Buffer
	out.WriteString(`[{"text":`)
	writeCanonicalString(&out, text)
	out.WriteString(`,"type":"text"}]`)
	return out.Bytes(), true
}

// canonicalJSON encodes one JSON value compactly, sorts object keys at every
// depth, and preserves array order and input number literals. It returns false
// for invalid JSON or trailing data.
func canonicalJSON(raw []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out bytes.Buffer
	if !writeCanonicalValue(decoder, &out) {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return out.Bytes(), true
}

func writeCanonicalValue(decoder *json.Decoder, out *bytes.Buffer) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	switch value := token.(type) {
	case json.Delim:
		if value == '[' {
			return writeCanonicalArray(decoder, out)
		}
		if value == '{' {
			return writeCanonicalObjectValue(decoder, out)
		}
		return false
	case string:
		writeCanonicalString(out, value)
	case json.Number:
		out.WriteString(value.String())
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case nil:
		out.WriteString("null")
	default:
		return false
	}
	return true
}

func writeCanonicalArray(decoder *json.Decoder, out *bytes.Buffer) bool {
	out.WriteByte('[')
	first := true
	for decoder.More() {
		if !first {
			out.WriteByte(',')
		}
		first = false
		if !writeCanonicalValue(decoder, out) {
			return false
		}
	}
	if _, err := decoder.Token(); err != nil {
		return false
	}
	out.WriteByte(']')
	return true
}

func writeCanonicalObjectValue(decoder *json.Decoder, out *bytes.Buffer) bool {
	members := make([]canonicalMember, 0)
	positions := make(map[string]int)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok {
			return false
		}
		var value bytes.Buffer
		if !writeCanonicalValue(decoder, &value) {
			return false
		}
		// A repeated key keeps its first position and takes its last value, as
		// JSON.parse does.
		if position, seen := positions[key]; seen {
			members[position].value = value.Bytes()
			continue
		}
		positions[key] = len(members)
		members = append(members, canonicalMember{key: key, value: value.Bytes()})
	}
	if _, err := decoder.Token(); err != nil {
		return false
	}
	writeCanonicalObject(out, members)
	return true
}

// writeCanonicalObject sorts members by key in UTF-16 code unit order, as
// JavaScript Array sort does, and writes the object.
func writeCanonicalObject(out *bytes.Buffer, members []canonicalMember) {
	slices.SortFunc(members, func(left canonicalMember, right canonicalMember) int {
		return compareUTF16(left.key, right.key)
	})
	out.WriteByte('{')
	for index, member := range members {
		if index > 0 {
			out.WriteByte(',')
		}
		writeCanonicalString(out, member.key)
		out.WriteByte(':')
		out.Write(member.value)
	}
	out.WriteByte('}')
}

func compareUTF16(left string, right string) int {
	return slices.Compare(utf16.Encode([]rune(left)), utf16.Encode([]rune(right)))
}

// writeCanonicalString writes a JSON string with JSON.stringify escapes for
// quotes, backslashes, and control characters. It uses short control escapes
// where available and lowercase \u00XX escapes for other control characters.
// It writes other characters as UTF-8.
func writeCanonicalString(out *bytes.Buffer, text string) {
	out.WriteByte('"')
	for _, character := range text {
		switch character {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if character < ' ' {
				fmt.Fprintf(out, `\u%04x`, character)
				continue
			}
			out.WriteRune(character)
		}
	}
	out.WriteByte('"')
}

// isRuneBoundary reports whether a byte offset starts a rune in text or equals
// the text length.
func isRuneBoundary(text string, offset int) bool {
	if offset < 0 || offset > len(text) {
		return false
	}
	return offset == len(text) || utf8.RuneStart(text[offset])
}
