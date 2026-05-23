package mitmcontrib

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// BidiAppendDiagnostic is the typed payload appended to a Cursor
// capture record when the intercepted exchange looks like a Cursor
// BidiAppend gRPC-Web call. The diagnostic is best-effort; the
// decoder walks the protobuf wire bytes by tag without a generated
// schema, so the fields surface only when the wire layout matches
// the empirically-observed Cursor BidiAppend frame.
type BidiAppendDiagnostic struct {
	RequestID         string `json:"request_id,omitempty"`
	AppendSeqno       uint64 `json:"append_seqno,omitempty"`
	DecodedBytes      int    `json:"decoded_bytes"`
	DecodedSHA256     string `json:"decoded_sha256"`
	PayloadBytes      int    `json:"payload_bytes,omitempty"`
	PayloadSHA256     string `json:"payload_sha256,omitempty"`
	SentinelFound     bool   `json:"sentinel_found,omitempty"`
	SentinelRequested bool   `json:"sentinel_requested,omitempty"`
}

// RequestCapture is the typed input the diagnostic decoder consumes.
// Callers populate Path, Headers, and Body with the raw values from
// the intercepted HTTP request and call DiagnoseRequest. The decoder
// already accounts for gRPC-web base64/compression and does not need
// the caller to decode the body in advance.
type RequestCapture struct {
	Path    string
	Headers http.Header
	Body    []byte
}

// DiagnoseRequest returns a typed Cursor BidiAppend diagnostic and
// `true` when the intercepted request looks like a BidiAppend call.
// The decoder walks the request body by protobuf wire tags; the
// match-detection logic uses path and header heuristics so it
// remains robust to gRPC-web frame variations.
func DiagnoseRequest(req RequestCapture, sentinel []byte) (BidiAppendDiagnostic, bool) {
	if !looksLikeBidiAppend(req.Path, req.Headers) {
		return emptyBidiAppendDiagnostic(), false
	}
	decoded := req.Body
	return decodeBidiAppendDiagnostic(decoded, sentinel), true
}

func emptyBidiAppendDiagnostic() BidiAppendDiagnostic {
	return BidiAppendDiagnostic{
		RequestID:         "",
		AppendSeqno:       0,
		DecodedBytes:      0,
		DecodedSHA256:     "",
		PayloadBytes:      0,
		PayloadSHA256:     "",
		SentinelFound:     false,
		SentinelRequested: false,
	}
}

func looksLikeBidiAppend(path string, headers http.Header) bool {
	if strings.Contains(strings.ToLower(path), "bidiappend") {
		return true
	}
	if strings.Contains(strings.ToLower(path), "bidi_append") {
		return true
	}
	method := strings.ToLower(headers.Get("X-Grpc-Web"))
	return strings.Contains(method, "bidiappend")
}

func decodeBidiAppendDiagnostic(decoded []byte, sentinel []byte) BidiAppendDiagnostic {
	diag := BidiAppendDiagnostic{
		RequestID:         "",
		AppendSeqno:       0,
		DecodedBytes:      len(decoded),
		DecodedSHA256:     sha256Hex(decoded),
		PayloadBytes:      0,
		PayloadSHA256:     "",
		SentinelFound:     false,
		SentinelRequested: false,
	}
	if len(sentinel) > 0 {
		diag.SentinelRequested = true
		diag.SentinelFound = bytes.Contains(decoded, sentinel)
	}
	values := scanProtoValues(decoded, 0)
	var longestBytes []byte
	for _, value := range values {
		if diag.RequestID == "" && likelyRequestID(value.Bytes) {
			diag.RequestID = string(value.Bytes)
		}
		if value.WireType == 0 && diag.AppendSeqno == 0 && value.Number > 0 {
			diag.AppendSeqno = value.Varint
		}
		if len(value.Bytes) > len(longestBytes) {
			longestBytes = value.Bytes
		}
	}
	if len(longestBytes) > 0 {
		diag.PayloadBytes = len(longestBytes)
		diag.PayloadSHA256 = sha256Hex(longestBytes)
	}
	return diag
}

type scannedProtoValue struct {
	Number   uint64
	WireType uint64
	Varint   uint64
	Bytes    []byte
}

func scanProtoValues(raw []byte, depth int) []scannedProtoValue {
	if depth > 2 {
		return nil
	}
	var out []scannedProtoValue
	for i := 0; i < len(raw); {
		fieldNumber, wireType, next, ok := readProtoKey(raw, i)
		if !ok {
			break
		}
		i = next
		switch wireType {
		case 0:
			value, next, ok := readProtoVarint(raw, i)
			if !ok {
				return out
			}
			i = next
			out = append(out, scannedProtoValue{
				Number:   fieldNumber,
				WireType: wireType,
				Varint:   value,
				Bytes:    nil,
			})
		case 1:
			if i+8 > len(raw) {
				return out
			}
			i += 8
		case 2:
			value, next, ok := readProtoBytes(raw, i)
			if !ok {
				return out
			}
			i = next
			out = append(out, scannedProtoValue{
				Number:   fieldNumber,
				WireType: wireType,
				Varint:   0,
				Bytes:    value,
			})
			if utf8.Valid(value) || json.Valid(value) {
				continue
			}
			out = append(out, scanProtoValues(value, depth+1)...)
		case 5:
			if i+4 > len(raw) {
				return out
			}
			i += 4
		default:
			return out
		}
	}
	return out
}

func readProtoKey(raw []byte, offset int) (fieldNumber uint64, wireType uint64, next int, ok bool) {
	key, n := binary.Uvarint(raw[offset:])
	if n <= 0 {
		return 0, 0, offset, false
	}
	fieldNumber = key >> 3
	if fieldNumber <= 0 {
		return 0, 0, offset, false
	}
	return fieldNumber, key & 0x7, offset + n, true
}

func readProtoVarint(raw []byte, offset int) (uint64, int, bool) {
	value, n := binary.Uvarint(raw[offset:])
	if n <= 0 {
		return 0, offset, false
	}
	return value, offset + n, true
}

func readProtoBytes(raw []byte, offset int) ([]byte, int, bool) {
	length, n := binary.Uvarint(raw[offset:])
	if n <= 0 {
		return nil, offset, false
	}
	offset += n
	maxInt := uint64(1)<<(strconv.IntSize-1) - 1
	if length > maxInt {
		return nil, offset, false
	}
	valueLength := int(length)
	if valueLength > len(raw)-offset {
		return nil, offset, false
	}
	return raw[offset : offset+valueLength], offset + valueLength, true
}

func likelyRequestID(raw []byte) bool {
	if len(raw) < 4 || len(raw) > 128 || !utf8.Valid(raw) {
		return false
	}
	text := string(raw)
	if strings.Contains(text, "\x00") {
		return false
	}
	lower := strings.ToLower(text)
	if strings.Contains(lower, "request") || strings.Contains(lower, "req") {
		return true
	}
	return strings.Count(text, "-") >= 4
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
