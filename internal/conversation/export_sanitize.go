package conversation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"goodkind.io/clyde/internal/transcript"
)

const encryptedContentJSONKey = "encrypted_content"

type sanitizableExportValue interface {
	CompactionSegmentSelection | transcript.CompactionMetadata
}

func stripEncryptedContentJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err == nil && fields != nil {
		delete(fields, encryptedContentJSONKey)
		for name, value := range fields {
			sanitized, sanitizeErr := stripEncryptedContentJSON(value)
			if sanitizeErr != nil {
				return nil, sanitizeErr
			}
			fields[name] = sanitized
		}
		sanitized, err := json.Marshal(fields)
		if err != nil {
			slog.Warn("conversation.export.sanitize_object_failed", "concern", "conversation.export", "component", "conversation", "err", err)
			return nil, fmt.Errorf("marshal sanitized export object: %w", err)
		}
		return sanitized, nil
	}

	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err == nil && values != nil {
		for i, value := range values {
			sanitized, sanitizeErr := stripEncryptedContentJSON(value)
			if sanitizeErr != nil {
				return nil, sanitizeErr
			}
			values[i] = sanitized
		}
		sanitized, err := json.Marshal(values)
		if err != nil {
			slog.Warn("conversation.export.sanitize_array_failed", "concern", "conversation.export", "component", "conversation", "err", err)
			return nil, fmt.Errorf("marshal sanitized export array: %w", err)
		}
		return sanitized, nil
	}

	return slices.Clone(raw), nil
}

func sanitizeExportValue[T sanitizableExportValue](value T) (T, error) {
	var sanitized T
	raw, err := json.Marshal(value)
	if err != nil {
		slog.Warn("conversation.export.sanitize_value_marshal_failed", "concern", "conversation.export", "component", "conversation", "err", err)
		return sanitized, fmt.Errorf("marshal export value: %w", err)
	}
	raw, err = stripEncryptedContentJSON(raw)
	if err != nil {
		return sanitized, err
	}
	if err := json.Unmarshal(raw, &sanitized); err != nil {
		slog.Warn("conversation.export.sanitize_value_unmarshal_failed", "concern", "conversation.export", "component", "conversation", "err", err)
		return sanitized, fmt.Errorf("unmarshal sanitized export value: %w", err)
	}
	return sanitized, nil
}

func sanitizeCompactionSelectionForExport(
	selection CompactionSegmentSelection,
) (CompactionSegmentSelection, error) {
	return sanitizeExportValue(selection)
}

func sanitizeCompactionMessagesForExport(
	messages []transcript.Message,
) ([]transcript.Message, error) {
	sanitized := slices.Clone(messages)
	for i := range sanitized {
		if sanitized[i].Compaction == nil {
			continue
		}
		metadata, err := sanitizeExportValue(*sanitized[i].Compaction)
		if err != nil {
			slog.Warn("conversation.export.sanitize_message_failed", "concern", "conversation.export", "component", "conversation", "message_index", i, "err", err)
			return nil, fmt.Errorf("sanitize message %d compaction: %w", i, err)
		}
		sanitized[i].Compaction = &metadata
	}
	return sanitized, nil
}
