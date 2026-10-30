package cursorstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const (
	nativeStatePrefix         = "~"
	nativeBlobKeyPrefix       = "agentKv:blob:"
	nativeRootMessagesField   = 1
	nativeArchivesField       = 13
	nativeArchiveMessageField = 1
	nativeBlobReferenceBytes  = 32
	nativeToolCallPartType    = "tool-call"
	protobufWireVarint        = 0
	protobufWireFixed64       = 1
	protobufWireBytes         = 2
	protobufWireFixed32       = 5
	protobufFieldShift        = 3
	protobufWireMask          = 7
	protobufFixed64Bytes      = 8
	protobufFixed32Bytes      = 4
	protobufVarintContinue    = 0x80
	protobufVarintPayload     = 0x7F
	protobufVarintShiftBits   = 7
	protobufVarintMaxShift    = 63
)

// NativeToolCall contains a native call ID, name, and arguments.
type NativeToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// NativeToolCalls contains calls and the completeness of their native state.
type NativeToolCalls struct {
	Calls []NativeToolCall
	// Complete reports whether every referenced blob exists.
	Complete bool
	// MissingBlobs counts referenced blobs that do not exist.
	MissingBlobs int
}

type protobufField struct {
	number int
	wire   int
	bytes  []byte
}

type nativeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type nativeContentPart struct {
	Type       string          `json:"type"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
}

var errNativeStateAbsent = errors.New("native conversation state is absent")

// ReadComposerNativeToolCalls reads calls from summary archives and root messages.
// It reports whether the native state exists and whether its referenced blobs exist.
func ReadComposerNativeToolCalls(ctx context.Context, db *sql.DB, composerID string) (NativeToolCalls, bool, error) {
	result := NativeToolCalls{Calls: nil, Complete: false, MissingBlobs: 0}
	raw, found, err := readNativeState(ctx, db, composerID)
	if errors.Is(err, errNativeStateAbsent) || !found {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	top, err := parseProtobufFields(raw)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.native_state_decode_failed", "concern", concern, "composer_id", composerID, "err", err)
		return result, false, fmt.Errorf("decode native conversation state for composer %s: %w", composerID, err)
	}
	references, err := nativeMessageReferences(ctx, db, composerID, top, &result)
	if err != nil {
		return result, false, err
	}
	seen := make(map[string]bool, len(references))
	for _, reference := range references {
		if len(reference) != nativeBlobReferenceBytes {
			return result, false, fmt.Errorf("invalid native message reference length for composer %s: %d", composerID, len(reference))
		}
		key := hex.EncodeToString(reference)
		if seen[key] {
			continue
		}
		seen[key] = true
		data, blobFound, blobErr := readNativeBlob(ctx, db, reference)
		if blobErr != nil {
			return result, false, blobErr
		}
		if !blobFound {
			result.MissingBlobs++
			continue
		}
		calls, decodeErr := unmarshalNativeToolCalls(data)
		if decodeErr != nil {
			slog.WarnContext(ctx, "providers.cursor.store.native_message_decode_failed", "concern", concern, "composer_id", composerID, "blob", key, "err", decodeErr)
			return result, false, fmt.Errorf("decode native message for composer and blob %s %s: %w", composerID, key, decodeErr)
		}
		result.Calls = append(result.Calls, calls...)
	}
	result.Complete = result.MissingBlobs == 0
	return result, true, nil
}

// nativeMessageReferences collects message references from archives and root state.
// It counts missing archive blobs in result.
func nativeMessageReferences(ctx context.Context, db *sql.DB, composerID string, top []protobufField, result *NativeToolCalls) ([][]byte, error) {
	references := make([][]byte, 0)
	for _, field := range top {
		if field.number != nativeArchivesField || field.wire != protobufWireBytes {
			continue
		}
		archive, archiveFound, archiveErr := readNativeBlob(ctx, db, field.bytes)
		if archiveErr != nil {
			return nil, archiveErr
		}
		if !archiveFound {
			result.MissingBlobs++
			continue
		}
		archiveFields, parseErr := parseProtobufFields(archive)
		if parseErr != nil {
			slog.WarnContext(ctx, "providers.cursor.store.native_archive_decode_failed", "concern", concern, "composer_id", composerID, "err", parseErr)
			return nil, fmt.Errorf("decode native archive for composer %s: %w", composerID, parseErr)
		}
		for _, archiveField := range archiveFields {
			if archiveField.number == nativeArchiveMessageField && archiveField.wire == protobufWireBytes {
				references = append(references, archiveField.bytes)
			}
		}
	}
	for _, field := range top {
		if field.number == nativeRootMessagesField && field.wire == protobufWireBytes {
			references = append(references, field.bytes)
		}
	}
	return references, nil
}

func readNativeState(ctx context.Context, db *sql.DB, composerID string) ([]byte, bool, error) {
	value, found, err := ReadKVValue(ctx, db, KVTableCursorDiskKV, composerDataKeyPrefix+composerID)
	if err != nil || !found {
		return nil, found, err
	}
	var header struct {
		ConversationState string `json:"conversationState"`
	}
	if err := json.Unmarshal(value, &header); err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.native_header_decode_failed", "concern", concern, "composer_id", composerID, "err", err)
		return nil, false, fmt.Errorf("decode native header for composer %s: %w", composerID, err)
	}
	encoded, cut := strings.CutPrefix(header.ConversationState, nativeStatePrefix)
	if !cut || encoded == "" {
		return nil, false, errNativeStateAbsent
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.native_state_base64_failed", "concern", concern, "composer_id", composerID, "err", err)
		return nil, false, fmt.Errorf("decode native state base64 for composer %s: %w", composerID, err)
	}
	return raw, true, nil
}

func readNativeBlob(ctx context.Context, db *sql.DB, reference []byte) ([]byte, bool, error) {
	return ReadKVValue(ctx, db, KVTableCursorDiskKV, nativeBlobKeyPrefix+hex.EncodeToString(reference))
}

// unmarshalNativeToolCalls extracts tool calls from a native message's content.
func unmarshalNativeToolCalls(data []byte) ([]NativeToolCall, error) {
	var message nativeMessage
	if err := json.Unmarshal(data, &message); err != nil {
		slog.Warn("providers.cursor.store.native_message_json_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("decode native message JSON: %w", err)
	}
	if len(message.Content) == 0 || message.Content[0] != '[' {
		return nil, nil
	}
	var parts []nativeContentPart
	if err := json.Unmarshal(message.Content, &parts); err != nil {
		slog.Warn("providers.cursor.store.native_message_parts_failed", "concern", concern, "err", err)
		return nil, fmt.Errorf("decode native message content parts: %w", err)
	}
	calls := make([]NativeToolCall, 0)
	for _, part := range parts {
		if part.Type != nativeToolCallPartType {
			continue
		}
		calls = append(calls, NativeToolCall{ID: part.ToolCallID, Name: part.ToolName, Args: part.Args})
	}
	return calls, nil
}

// parseProtobufFields reads protobuf field numbers, wire types, and byte values.
func parseProtobufFields(data []byte) ([]protobufField, error) {
	fields := make([]protobufField, 0)
	offset := 0
	for offset < len(data) {
		key, next, err := protobufVarint(data, offset)
		if err != nil {
			return nil, err
		}
		offset = next
		field := protobufField{number: int(key >> protobufFieldShift), wire: int(key & protobufWireMask), bytes: nil}
		switch field.wire {
		case protobufWireVarint:
			_, next, err = protobufVarint(data, offset)
			if err != nil {
				return nil, err
			}
			offset = next
		case protobufWireFixed64:
			offset += protobufFixed64Bytes
		case protobufWireFixed32:
			offset += protobufFixed32Bytes
		case protobufWireBytes:
			length, next, lengthErr := protobufVarint(data, offset)
			if lengthErr != nil {
				return nil, lengthErr
			}
			remaining := data[next:]
			if length > uint64(len(remaining)) {
				return nil, fmt.Errorf("truncated protobuf field %d", field.number)
			}
			field.bytes = remaining[:length]
			offset = next + len(field.bytes)
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type and field number %d %d", field.wire, field.number)
		}
		if offset > len(data) {
			return nil, fmt.Errorf("truncated protobuf field %d", field.number)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func protobufVarint(data []byte, offset int) (uint64, int, error) {
	var value uint64
	shift := 0
	for offset < len(data) {
		current := data[offset]
		offset++
		value |= uint64(current&protobufVarintPayload) << shift
		if current&protobufVarintContinue == 0 {
			return value, offset, nil
		}
		shift += protobufVarintShiftBits
		if shift > protobufVarintMaxShift {
			return 0, offset, errors.New("protobuf varint is too long")
		}
	}
	return 0, offset, errors.New("truncated protobuf varint")
}
