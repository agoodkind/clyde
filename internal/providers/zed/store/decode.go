package zedstore

import (
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const maxDecodedThreadBytes = 64 << 20

// UnknownThreadDataTypeError reports an unsupported Zed thread payload
// encoding.
type UnknownThreadDataTypeError struct {
	DataType DataType
}

// Error renders the unsupported Zed thread payload encoding.
func (err UnknownThreadDataTypeError) Error() string {
	return fmt.Sprintf("unsupported zed thread data type %q", err.DataType)
}

// ZstdDecoderCreateError reports one failure while constructing a Zed zstd
// decoder.
type ZstdDecoderCreateError struct {
	Err error
}

// Error renders the zstd decoder creation failure.
func (err ZstdDecoderCreateError) Error() string {
	return fmt.Sprintf("create zed zstd decoder: %v", err.Err)
}

// Unwrap exposes the underlying zstd decoder creation failure.
func (err ZstdDecoderCreateError) Unwrap() error {
	return err.Err
}

// ZstdThreadDecodeError reports one failure while decoding a compressed Zed
// thread payload.
type ZstdThreadDecodeError struct {
	Err error
}

// Error renders the zstd thread decode failure.
func (err ZstdThreadDecodeError) Error() string {
	return fmt.Sprintf("decode zed zstd thread payload: %v", err.Err)
}

// Unwrap exposes the underlying zstd payload decode failure.
func (err ZstdThreadDecodeError) Unwrap() error {
	return err.Err
}

// ZstdThreadTooLargeError reports one decoded Zed thread payload that exceeded
// the configured size limit.
type ZstdThreadTooLargeError struct {
	LimitBytes  int
	ActualBytes int
}

// Error renders the decoded-size limit failure.
func (err ZstdThreadTooLargeError) Error() string {
	if err.ActualBytes <= 0 {
		return fmt.Sprintf("decoded zed zstd thread payload exceeds limit %d bytes", err.LimitBytes)
	}
	return fmt.Sprintf("decoded zed zstd thread payload %d bytes exceeds limit %d bytes", err.ActualBytes, err.LimitBytes)
}

// DecodeThreadJSON decodes one stored Zed thread payload into raw JSON bytes.
func DecodeThreadJSON(dataType DataType, data []byte) ([]byte, error) {
	switch dataType {
	case DataTypeJSON:
		return append([]byte(nil), data...), nil
	case DataTypeZstd:
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxDecodedThreadBytes))
		if err != nil {
			return nil, ZstdDecoderCreateError{Err: err}
		}
		defer decoder.Close()
		decoded, err := decoder.DecodeAll(data, nil)
		if err != nil {
			if strings.Contains(err.Error(), "decompressed size exceeds configured limit") {
				return nil, ZstdThreadTooLargeError{LimitBytes: maxDecodedThreadBytes, ActualBytes: 0}
			}
			return nil, ZstdThreadDecodeError{Err: err}
		}
		if len(decoded) > maxDecodedThreadBytes {
			return nil, ZstdThreadTooLargeError{LimitBytes: maxDecodedThreadBytes, ActualBytes: len(decoded)}
		}
		return decoded, nil
	default:
		return nil, UnknownThreadDataTypeError{DataType: dataType}
	}
}
