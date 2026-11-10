package cursorstore

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	toolCallShellField       = protowire.Number(1)
	toolCallShellArgsField   = protowire.Number(1)
	shellArgsToolCallIDField = protowire.Number(4)
	toolCallIDField          = protowire.Number(57)
	toolCallStartedAtMsField = protowire.Number(59)
	toolCallCompletedAtField = protowire.Number(60)
)

// ToolCallRecord contains tool call identifiers, timestamps, and timestamp presence flags from Cursor protobuf data.
type ToolCallRecord struct {
	ID string
	// ShellArgsID identifies the tool call in nested shell arguments.
	ShellArgsID string
	// StartedAtMs records the start time in milliseconds since the Unix epoch.
	StartedAtMs int64
	HasStart    bool
	// CompletedAtMs records the completion time in milliseconds since the Unix epoch.
	CompletedAtMs int64
	HasCompleted  bool
}

// DecodeToolCallBinary decodes tool call identifiers and timestamps from a base64 protobuf record. It accepts an empty input without an error.
func DecodeToolCallBinary(encoded string) (ToolCallRecord, error) {
	record := ToolCallRecord{ID: "", ShellArgsID: "", StartedAtMs: 0, HasStart: false, CompletedAtMs: 0, HasCompleted: false}
	if encoded == "" {
		return record, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		slog.Warn("providers.cursor.store.tool_call_binary_base64_failed", "concern", concern, "err", err)
		return record, fmt.Errorf("decode tool call binary base64: %w", err)
	}
	fields, err := unmarshalProtobufFields(raw)
	if err != nil {
		return record, err
	}
	for _, field := range fields {
		switch {
		case field.number == toolCallIDField && field.wire == protowire.BytesType:
			record.ID = string(field.bytes)
		case field.number == toolCallShellField && field.wire == protowire.BytesType:
			shellID, shellErr := shellArgsToolCallID(field.bytes)
			if shellErr != nil {
				return record, shellErr
			}
			record.ShellArgsID = shellID
		case field.number == toolCallStartedAtMsField && field.wire == protowire.VarintType:
			record.StartedAtMs, record.HasStart = toolCallMillis(field.varint)
		case field.number == toolCallCompletedAtField && field.wire == protowire.VarintType:
			record.CompletedAtMs, record.HasCompleted = toolCallMillis(field.varint)
		}
	}
	return record, nil
}

func toolCallMillis(value uint64) (int64, bool) {
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

func shellArgsToolCallID(shellCall []byte) (string, error) {
	shellFields, err := unmarshalProtobufFields(shellCall)
	if err != nil {
		return "", err
	}
	for _, shellField := range shellFields {
		if shellField.number != toolCallShellArgsField || shellField.wire != protowire.BytesType {
			continue
		}
		argsFields, argsErr := unmarshalProtobufFields(shellField.bytes)
		if argsErr != nil {
			return "", argsErr
		}
		for _, argsField := range argsFields {
			if argsField.number == shellArgsToolCallIDField && argsField.wire == protowire.BytesType {
				return string(argsField.bytes), nil
			}
		}
	}
	return "", nil
}
