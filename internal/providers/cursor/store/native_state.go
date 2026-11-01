package cursorstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
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
	nativeUserRole            = "user"
	nativeAssistantRole       = "assistant"
	nativeTextPartType        = "text"
	nativeUserQueryTag        = "<user_query>"
	nativeRootArchive         = -1
	// NativeProjectionVersion identifies the native tool call projection format.
	NativeProjectionVersion = 1
	// NativeSourceArchive identifies a message referenced by an archive.
	NativeSourceArchive = "archive"
	// NativeSourceRoot identifies a message referenced by the root state.
	NativeSourceRoot = "root"
)

var (
	// nativeUserQueryPattern captures content between user_query tags across lines.
	nativeUserQueryPattern = regexp.MustCompile(`(?s)<user_query>(.*?)</user_query>`)
	// nativeThinkPattern matches think blocks across lines.
	nativeThinkPattern = regexp.MustCompile(`(?s)<think>.*?</think>`)
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
	// RealUser reports whether this user message contains real user text.
	RealUser bool
	// Text contains trimmed user or assistant text for matching stored bubbles.
	Text string
	// RequestID is the Cursor request ID from providerOptions.cursor.requestId.
	RequestID string
	// ContentHash is the hexadecimal SHA-256 of the raw content JSON.
	ContentHash string
	Calls       []NativeToolCall
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
	// AfterUser reports whether a real user turn appeared since the previous call.
	AfterUser bool
	// GroupedMessage records the message position when the projection joins
	// consecutive native assistant messages.
	GroupedMessage int
	// GroupedOrdinal records the call position within the combined assistant
	// message.
	GroupedOrdinal int
	// ActualMessage records the message position when the projection counts
	// native messages separately.
	ActualMessage int
	// ActualOrdinal records the call position within its native message.
	ActualOrdinal int
}

// NativeUserTurn records a real user message position and its text.
type NativeUserTurn struct {
	Message int
	Text    string
}

// NativeProjection contains native calls and user turns under both grouped and
// actual message numbering.
type NativeProjection struct {
	Calls []NativeSequenceCall
	// GroupedUsers records user turns under the message numbering that combines
	// consecutive native assistant messages.
	GroupedUsers []NativeUserTurn
	// ActualUsers records user turns under the message numbering that counts
	// native messages separately.
	ActualUsers []NativeUserTurn
}

// Project returns calls and user turns under grouped and actual message
// numbering. The grouped numbering joins consecutive native assistant messages,
// and the actual numbering counts them separately.
func (state NativeToolCalls) Project() NativeProjection {
	projection := NativeProjection{Calls: make([]NativeSequenceCall, 0), GroupedUsers: make([]NativeUserTurn, 0), ActualUsers: make([]NativeUserTurn, 0)}
	afterUser := false
	actualMessage := -1
	groupedMessage := -1
	groupedOrdinal := 0
	insideAssistantGroup := false
	for messageIndex, message := range state.Messages {
		if message.Missing {
			continue
		}
		// Count a repeated user message before skipping its duplicate tool calls.
		if message.RealUser {
			afterUser = true
			actualMessage++
			groupedMessage++
			projection.ActualUsers = append(projection.ActualUsers, NativeUserTurn{Message: actualMessage, Text: message.Text})
			projection.GroupedUsers = append(projection.GroupedUsers, NativeUserTurn{Message: groupedMessage, Text: message.Text})
			groupedOrdinal = 0
			insideAssistantGroup = false
		}
		if message.Duplicate {
			continue
		}
		if message.Role == nativeAssistantRole {
			actualMessage++
			if !insideAssistantGroup {
				groupedMessage++
				groupedOrdinal = 0
				insideAssistantGroup = true
			}
		}
		for ordinal, call := range message.Calls {
			projection.Calls = append(projection.Calls, NativeSequenceCall{
				Call: call, Message: messageIndex, Ordinal: ordinal, MessageStart: ordinal == 0, AfterUser: afterUser,
				GroupedMessage: groupedMessage, GroupedOrdinal: groupedOrdinal, ActualMessage: actualMessage, ActualOrdinal: ordinal,
			})
			afterUser = false
			groupedOrdinal++
		}
	}
	return projection
}

type protobufField struct {
	number protowire.Number
	wire   protowire.Type
	bytes  []byte
	varint uint64
}

type nativeMessage struct {
	Role            string                `json:"role"`
	Content         json.RawMessage       `json:"content"`
	ProviderOptions nativeProviderOptions `json:"providerOptions"`
}

type nativeProviderOptions struct {
	Cursor nativeCursorOptions `json:"cursor"`
}

type nativeCursorOptions struct {
	RequestID string `json:"requestId"`
}

type nativeMessageFields struct {
	role        string
	realUser    bool
	text        string
	requestID   string
	contentHash string
	calls       []NativeToolCall
}

type nativeContentPart struct {
	Type       string          `json:"type"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Text       string          `json:"text"`
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
	message := NativeMessage{Source: reference.source, Archive: reference.archive, Index: reference.index, BlobRef: key, Role: "", Duplicate: seen[key], Missing: false, RealUser: false, Text: "", RequestID: "", ContentHash: "", Calls: nil}
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
	decoded, err := unmarshalNativeMessage(data)
	if err != nil {
		slog.WarnContext(ctx, "providers.cursor.store.native_message_decode_failed", "concern", concern, "composer_id", composerID, "blob", key, "err", err)
		return message, fmt.Errorf("decode native message for composer and blob %s %s: %w", composerID, key, err)
	}
	message.Role = decoded.role
	message.RealUser = decoded.realUser
	message.Text = decoded.text
	message.RequestID = decoded.requestID
	message.ContentHash = decoded.contentHash
	message.Calls = decoded.calls
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

// unmarshalNativeMessage decodes the role, request ID, text, and tool calls.
// It hashes the raw content JSON and detects real user text.
func unmarshalNativeMessage(data []byte) (nativeMessageFields, error) {
	fields := nativeMessageFields{role: "", realUser: false, text: "", requestID: "", contentHash: "", calls: nil}
	var message nativeMessage
	if err := json.Unmarshal(data, &message); err != nil {
		slog.Warn("providers.cursor.store.native_message_json_failed", "concern", concern, "err", err)
		return fields, fmt.Errorf("decode native message JSON: %w", err)
	}
	fields.role = message.Role
	fields.requestID = message.ProviderOptions.Cursor.RequestID
	if len(message.Content) == 0 {
		return fields, nil
	}
	sum := sha256.Sum256(message.Content)
	fields.contentHash = hex.EncodeToString(sum[:])
	texts := make([]string, 0)
	if message.Content[0] != '[' {
		var plain string
		if err := json.Unmarshal(message.Content, &plain); err != nil {
			slog.Warn("providers.cursor.store.native_message_text_failed", "concern", concern, "err", err)
			return fields, fmt.Errorf("decode native message text: %w", err)
		}
		texts = append(texts, plain)
	} else {
		var parts []nativeContentPart
		if err := json.Unmarshal(message.Content, &parts); err != nil {
			slog.Warn("providers.cursor.store.native_message_parts_failed", "concern", concern, "err", err)
			return fields, fmt.Errorf("decode native message content parts: %w", err)
		}
		fields.calls = make([]NativeToolCall, 0)
		for _, part := range parts {
			switch part.Type {
			case nativeToolCallPartType:
				fields.calls = append(fields.calls, NativeToolCall{ID: part.ToolCallID, Name: part.ToolName, Args: part.Args})
			case nativeTextPartType:
				texts = append(texts, part.Text)
			}
		}
	}
	for _, part := range texts {
		if message.Role == nativeUserRole && RealUserText(part) {
			fields.realUser = true
		}
	}
	fields.text = nativeMatchText(message.Role, texts)
	return fields, nil
}

// nativeMatchText returns trimmed text for matching user or assistant bubbles.
// For users, it selects the first user_query match. Without a match, it joins
// nonempty text parts that do not start with < after trimming.
// For assistants, it joins text parts and removes think blocks.
func nativeMatchText(role string, texts []string) string {
	switch role {
	case nativeUserRole:
		plain := make([]string, 0, len(texts))
		for _, part := range texts {
			if match := nativeUserQueryPattern.FindStringSubmatch(part); match != nil {
				return strings.TrimSpace(match[1])
			}
			if trimmed := strings.TrimSpace(part); trimmed != "" && !strings.HasPrefix(trimmed, "<") {
				plain = append(plain, part)
			}
		}
		return strings.TrimSpace(strings.Join(plain, "\n"))
	case nativeAssistantRole:
		return strings.TrimSpace(nativeThinkPattern.ReplaceAllString(strings.Join(texts, ""), ""))
	default:
		return ""
	}
}

// RealUserText reports whether text contains a user_query tag or nonempty text
// that does not start with "<" after whitespace is trimmed.
func RealUserText(text string) bool {
	if strings.Contains(text, nativeUserQueryTag) {
		return true
	}
	trimmed := strings.TrimSpace(text)
	return trimmed != "" && !strings.HasPrefix(trimmed, "<")
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
		field := protobufField{number: number, wire: wire, bytes: nil, varint: 0}
		switch wire {
		case protowire.BytesType:
			value, valueLength := protowire.ConsumeBytes(data)
			if valueLength < 0 {
				return nil, protobufParseError(valueLength)
			}
			field.bytes = value
			data = data[valueLength:]
		case protowire.VarintType:
			value, valueLength := protowire.ConsumeVarint(data)
			if valueLength < 0 {
				return nil, protobufParseError(valueLength)
			}
			field.varint = value
			data = data[valueLength:]
		case protowire.Fixed32Type, protowire.Fixed64Type, protowire.StartGroupType, protowire.EndGroupType:
			fallthrough
		default:
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

func protobufParseError(code int) error {
	err := protowire.ParseError(code)
	slog.Warn("providers.cursor.store.protobuf_parse_failed", "concern", concern, "err", err)
	return fmt.Errorf("parse protobuf field: %w", err)
}
