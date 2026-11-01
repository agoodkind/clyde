package transcript

import (
	"bytes"
	"encoding/json"
	"slices"
)

// UserTurn records a user message position and its text.
type UserTurn struct {
	Message int
	Text    string
}

// ToolIdentity contains a tool name and complete arguments for comparison
// without a call ID.
type ToolIdentity struct {
	Name  string
	Input json.RawMessage
}

// UnprovenToolPosition indicates that comparison did not prove a unique source
// position for a saved call.
const UnprovenToolPosition = -1

// ProvenToolPositions returns a saved call's source position only when its
// earliest and latest possible matches agree on that position while preserving
// call order and comparing names and complete arguments. A skipped source call
// is a call the transcript omitted.
func ProvenToolPositions(saved []ToolIdentity, source []ToolIdentity) []int {
	savedKeys := make([]string, 0, len(saved))
	for _, identity := range saved {
		savedKeys = append(savedKeys, toolIdentityKey(identity, savedInvalidToolKey))
	}
	sourceKeys := make([]string, 0, len(source))
	for _, identity := range source {
		sourceKeys = append(sourceKeys, toolIdentityKey(identity, sourceInvalidToolKey))
	}
	positions := slices.Repeat([]int{UnprovenToolPosition}, len(saved))
	earliest := make([]int, 0, len(saved))
	next := 0
	for _, key := range savedKeys {
		position := next
		for position < len(sourceKeys) && sourceKeys[position] != key {
			position++
		}
		if position == len(sourceKeys) {
			break
		}
		earliest = append(earliest, position)
		next = position + 1
	}
	if len(earliest) < len(saved) {
		earliest = alignedToolPrefix(earliest)
	}
	next = len(sourceKeys) - 1
	for index := len(earliest) - 1; index >= 0; index-- {
		position := next
		for sourceKeys[position] != savedKeys[index] {
			position--
		}
		if position == earliest[index] {
			positions[index] = position
		}
		next = position - 1
	}
	return positions
}

// alignedToolPrefix returns positions before the first skipped source call when
// a later saved call cannot match the remaining source calls.
func alignedToolPrefix(earliest []int) []int {
	for index, position := range earliest {
		if position != index {
			return earliest[:index]
		}
	}
	return earliest
}

const (
	toolIdentitySeparator = "\x00"
	savedInvalidToolKey   = toolIdentitySeparator + "saved"
	sourceInvalidToolKey  = toolIdentitySeparator + "source"
)

// toolIdentityKey returns a comparison key containing the tool name and
// canonical JSON arguments, or the supplied invalid key when argument parsing
// fails.
func toolIdentityKey(identity ToolIdentity, invalid string) string {
	if len(bytes.TrimSpace(identity.Input)) == 0 {
		return identity.Name + toolIdentitySeparator
	}
	canonical, valid := CanonicalToolInput(identity.Input)
	if !valid {
		return invalid
	}
	return identity.Name + toolIdentitySeparator + canonical
}

// SameToolInput compares two tool inputs as canonical JSON.
func SameToolInput(left json.RawMessage, right json.RawMessage) bool {
	leftCanonical, leftOK := CanonicalToolInput(left)
	rightCanonical, rightOK := CanonicalToolInput(right)
	return leftOK && rightOK && leftCanonical == rightCanonical
}

// CanonicalToolInput decodes a JSON input and returns its canonical encoding.
func CanonicalToolInput(raw json.RawMessage) (string, bool) {
	var builder bytes.Buffer
	if !writeCanonicalJSON(&builder, bytes.TrimSpace(raw)) {
		return "", false
	}
	return builder.String(), true
}

func writeCanonicalObject(builder *bytes.Buffer, raw []byte) bool {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	builder.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			builder.WriteByte(',')
		}
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return false
		}
		builder.Write(encodedKey)
		builder.WriteByte(':')
		if !writeCanonicalJSON(builder, bytes.TrimSpace(object[key])) {
			return false
		}
	}
	builder.WriteByte('}')
	return true
}

// writeCanonicalJSON writes JSON with sorted object keys and compact values.
// It preserves array order and returns false when the input is invalid.
func writeCanonicalJSON(builder *bytes.Buffer, raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '{':
		return writeCanonicalObject(builder, raw)
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return false
		}
		builder.WriteByte('[')
		for index, item := range items {
			if index > 0 {
				builder.WriteByte(',')
			}
			if !writeCanonicalJSON(builder, bytes.TrimSpace(item)) {
				return false
			}
		}
		builder.WriteByte(']')
	case '"':
		// Decode and re-encode strings to normalize equivalent JSON escapes.
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return false
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return false
		}
		builder.Write(encoded)
	default:
		if err := json.Compact(builder, raw); err != nil {
			return false
		}
	}
	return true
}
