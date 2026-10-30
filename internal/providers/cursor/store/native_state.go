package cursorstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

const (
	nativeStatePrefix         = "~"
	nativeBlobKeyPrefix       = "agentKv:blob:"
	nativeRootMessagesField   = protowire.Number(1)
	nativeArchivesField       = protowire.Number(13)
	nativeArchiveMessageField = protowire.Number(1)
	nativeBlobReferenceBytes  = 32
	nativeToolCallPartType    = "tool-call"
	nativeRootArchive         = -1
	// NativeProjectionVersion identifies the native tool call projection format.
	NativeProjectionVersion = 1
	// NativeSourceArchive identifies a message referenced by an archive.
	NativeSourceArchive = "archive"
	// NativeSourceRoot identifies a message referenced by the root state.
	NativeSourceRoot = "root"
)

// NativeToolCall contains a native call ID, name, and arguments.
type NativeToolCall struct {
	ID   string
	Name string
	Args json.RawMessage
}

// NativeMessage records a referenced native message and its tool calls.
type NativeMessage struct {
	Source string
	// Archive is the archive ordinal, or -1 for a root message.
	Archive int
	// Index is the message position within its archive or the root state.
	Index   int
	BlobRef string
	Role    string
	// Duplicate reports whether an earlier reference used the same blob.
	Duplicate bool
	// Missing reports whether the referenced message blob is absent.
	Missing bool
	Calls   []NativeToolCall
}

// NativeToolCalls records the projection version and referenced messages.
type NativeToolCalls struct {
	Version  int
	Messages []NativeMessage
	// Complete reports whether every referenced blob exists.
	Complete bool
	// MissingBlobs counts referenced blobs that do not exist.
	MissingBlobs int
}

// NativeSequenceCall records a tool call and its position in the native state.
type NativeSequenceCall struct {
	Call NativeToolCall
	// Message is the position in NativeToolCalls.Messages.
	Message int
	// Ordinal is the tool call position within the native message.
	Ordinal int
	// MessageStart reports whether this is the first tool call in the native message.
	MessageStart bool
}

// Sequence returns tool calls in source order, excluding duplicate and missing messages.
func (state NativeToolCalls) Sequence() []NativeSequenceCall {
	sequence := make([]NativeSequenceCall, 0)
	for messageIndex, message := range state.Messages {
		if message.Duplicate || message.Missing {
			continue
		}
		for ordinal, call := range message.Calls {
			sequence = append(sequence, NativeSequenceCall{Call: call, Message: messageIndex, Ordinal: ordinal, MessageStart: ordinal == 0})
		}
	}
	return sequence
}

type protobufField struct {
	number protowire.Number
	wire   protowire.Type
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

type nativeReference struct {
	source  string
	archive int
	index   int
	digest  []byte
}

// ReadComposerNativeToolCalls reads calls from summary archives and root messages.
// It reports whether the native state exists and whether its referenced blobs exist.
func ReadComposerNativeToolCalls(ctx context.Context, db *sql.DB, composerID string) (NativeToolCalls, bool, error) {
	result := NativeToolCalls{Version: NativeProjectionVersion, Messages: nil, Complete: false, MissingBlobs: 0}
	raw, found, err := readNativeState(ctx, db, composerID)
	if err != nil {
		return result, false, err
	}
	if !found {
		return result, false, nil
	}
	top, err := unmarshalProtobufFields(raw)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.native_state_decode_failed", "concern", concern, "composer_id", composerID, "err", err)
		return result, false, fmt.Errorf("decode native conversation state for composer %s: %w", composerID, err)
	}
	references, err := nativeMessageReferences(ctx, db, composerID, top, &result)
	if err != nil {
		return result, false, err
	}
	seen := make(map[string]bool, len(references))
	result.Messages = make([]NativeMessage, 0, len(references))
	for _, reference := range references {
		message, readErr := readNativeMessage(ctx, db, composerID, reference, seen)
		if readErr != nil {
			return result, false, readErr
		}
		if message.Missing {
			result.MissingBlobs++
		}
		result.Messages = append(result.Messages, message)
	}
	result.Complete = result.MissingBlobs == 0
	return result, true, nil
}

// readNativeMessage reads a referenced blob and extracts its role and tool calls.
func readNativeMessage(ctx context.Context, db *sql.DB, composerID string, reference nativeReference, seen map[string]bool) (NativeMessage, error) {
	key := hex.EncodeToString(reference.digest)
	message := NativeMessage{Source: reference.source, Archive: reference.archive, Index: reference.index, BlobRef: key, Role: "", Duplicate: seen[key], Missing: false, Calls: nil}
	if len(reference.digest) != nativeBlobReferenceBytes {
		slog.WarnContext(ctx, "providers.cursor.store.native_reference_invalid", "concern", concern, "composer_id", composerID, "length", len(reference.digest))
		return message, fmt.Errorf("invalid native message reference length for composer %s: %d", composerID, len(reference.digest))
	}
	seen[key] = true
	data, blobFound, err := readNativeBlob(ctx, db, reference.digest)
	if err != nil {
		return message, err
	}
	if !blobFound {
		message.Missing = true
		return message, nil
	}
	role, calls, err := unmarshalNativeToolCalls(data)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.native_message_decode_failed", "concern", concern, "composer_id", composerID, "blob", key, "err", err)
		return message, fmt.Errorf("decode native message for composer and blob %s %s: %w", composerID, key, err)
	}
	message.Role = role
	message.Calls = calls
	return message, nil
}

// nativeMessageReferences collects message references from archives and root state.
// It counts missing archive blobs in result.
func nativeMessageReferences(ctx context.Context, db *sql.DB, composerID string, top []protobufField, result *NativeToolCalls) ([]nativeReference, error) {
	references := make([]nativeReference, 0)
	archiveOrdinal := 0
	for _, field := range top {
		if field.number != nativeArchivesField || field.wire != protowire.BytesType {
			continue
		}
		archive := archiveOrdinal
		archiveOrdinal++
		data, archiveFound, archiveErr := readNativeBlob(ctx, db, field.bytes)
		if archiveErr != nil {
			return nil, archiveErr
		}
		if !archiveFound {
			result.MissingBlobs++
			continue
		}
		archiveFields, parseErr := unmarshalProtobufFields(data)
		if parseErr != nil {
			slog.WarnContext(ctx, "providers.cursor.store.native_archive_decode_failed", "concern", concern, "composer_id", composerID, "err", parseErr)
			return nil, fmt.Errorf("decode native archive for composer %s: %w", composerID, parseErr)
		}
		index := 0
		for _, archiveField := range archiveFields {
			if archiveField.number == nativeArchiveMessageField && archiveField.wire == protowire.BytesType {
				references = append(references, nativeReference{source: NativeSourceArchive, archive: archive, index: index, digest: archiveField.bytes})
				index++
			}
		}
	}
	index := 0
	for _, field := range top {
		if field.number == nativeRootMessagesField && field.wire == protowire.BytesType {
			references = append(references, nativeReference{source: NativeSourceRoot, archive: nativeRootArchive, index: index, digest: field.bytes})
			index++
		}
	}
	return references, nil
}

// readNativeState reads and decodes the composer's conversation state.
func readNativeState(ctx context.Context, db *sql.DB, composerID string) ([]byte, bool, error) {
	value, found, err := ReadKVValue(ctx, db, KVTableCursorDiskKV, composerDataKeyPrefix+composerID)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
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
		return nil, false, nil
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
func unmarshalNativeToolCalls(data []byte) (string, []NativeToolCall, error) {
	var message nativeMessage
	if err := json.Unmarshal(data, &message); err != nil {
		slog.Warn("providers.cursor.store.native_message_json_failed", "concern", concern, "err", err)
		return "", nil, fmt.Errorf("decode native message JSON: %w", err)
	}
	if len(message.Content) == 0 || message.Content[0] != '[' {
		return message.Role, nil, nil
	}
	var parts []nativeContentPart
	if err := json.Unmarshal(message.Content, &parts); err != nil {
		slog.Warn("providers.cursor.store.native_message_parts_failed", "concern", concern, "err", err)
		return message.Role, nil, fmt.Errorf("decode native message content parts: %w", err)
	}
	calls := make([]NativeToolCall, 0)
	for _, part := range parts {
		if part.Type != nativeToolCallPartType {
			continue
		}
		calls = append(calls, NativeToolCall{ID: part.ToolCallID, Name: part.ToolName, Args: part.Args})
	}
	return message.Role, calls, nil
}

// unmarshalProtobufFields reads protobuf field numbers, wire types, and byte values.
func unmarshalProtobufFields(data []byte) ([]protobufField, error) {
	fields := make([]protobufField, 0)
	for len(data) > 0 {
		number, wire, tagLength := protowire.ConsumeTag(data)
		if tagLength < 0 {
			return nil, protobufParseError(tagLength)
		}
		data = data[tagLength:]
		field := protobufField{number: number, wire: wire, bytes: nil}
		if wire == protowire.BytesType {
			value, valueLength := protowire.ConsumeBytes(data)
			if valueLength < 0 {
				return nil, protobufParseError(valueLength)
			}
			field.bytes = value
			data = data[valueLength:]
		} else {
			valueLength := protowire.ConsumeFieldValue(number, wire, data)
			if valueLength < 0 {
				return nil, protobufParseError(valueLength)
			}
			data = data[valueLength:]
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// protobufParseError wraps a protowire parse error for the given error code.
func protobufParseError(code int) error {
	err := protowire.ParseError(code)
	slog.Warn("providers.cursor.store.protobuf_parse_failed", "concern", concern, "err", err)
	return fmt.Errorf("parse protobuf field: %w", err)
}
