package zedstore

import (
	"fmt"
	"log/slog"

	"github.com/klauspost/compress/zstd"
)

// UnknownThreadDataTypeError reports one unsupported Zed thread payload
// encoding.
type UnknownThreadDataTypeError struct {
	DataType DataType
}

// Error renders the unsupported Zed thread payload encoding.
func (err UnknownThreadDataTypeError) Error() string {
	return fmt.Sprintf("unsupported zed thread data type %q", err.DataType)
}

// DecodeThreadJSON decodes one persisted Zed thread payload into raw JSON
// bytes, handling both plain JSON and Zstandard-compressed storage.
func DecodeThreadJSON(dataType DataType, data []byte) ([]byte, error) {
	switch dataType {
	case DataTypeJSON:
		return append([]byte(nil), data...), nil
	case DataTypeZstd:
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			slog.Warn("providers.zed.store.zstd_decoder_create_failed", "concern", "providers.zed.store", "err", err)
			return nil, fmt.Errorf("create zed zstd decoder: %w", err)
		}
		defer decoder.Close()
		decoded, err := decoder.DecodeAll(data, nil)
		if err != nil {
			slog.Warn("providers.zed.store.zstd_thread_decode_failed", "concern", "providers.zed.store", "err", err)
			return nil, fmt.Errorf("decode zed zstd thread payload: %w", err)
		}
		return decoded, nil
	default:
		return nil, UnknownThreadDataTypeError{DataType: dataType}
	}
}
