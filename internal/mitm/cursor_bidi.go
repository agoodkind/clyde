package mitm

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

type cursorBidiAppendDiagnostic struct {
	RequestID         string `json:"request_id,omitempty"`
	AppendSeqno       uint64 `json:"append_seqno,omitempty"`
	DecodedBytes      int    `json:"decoded_bytes"`
	DecodedSHA256     string `json:"decoded_sha256"`
	PayloadBytes      int    `json:"payload_bytes,omitempty"`
	PayloadSHA256     string `json:"payload_sha256,omitempty"`
	SentinelFound     bool   `json:"sentinel_found,omitempty"`
	SentinelRequested bool   `json:"sentinel_requested,omitempty"`
}

func cursorBidiAppendDiagnosticForRequest(req *httpRequestCapture, sentinel []byte) (*cursorBidiAppendDiagnostic, bool) {
	if !looksLikeCursorBidiAppend(req.Path, req.Headers) {
		return nil, false
	}
	decoded, decodedOK := decodeForCapture(req.Body, req.Headers.Get("content-encoding"))
	if !decodedOK {
		decoded = req.Body
	}
	diag := decodeCursorBidiAppendDiagnostic(decoded, sentinel)
	return &diag, true
}

type httpRequestCapture struct {
	Path    string
	Headers http.Header
	Body    []byte
}

func looksLikeCursorBidiAppend(path string, headers http.Header) bool {
	if strings.Contains(strings.ToLower(path), "bidiappend") {
		return true
	}
	if strings.Contains(strings.ToLower(path), "bidi_append") {
		return true
	}
	method := strings.ToLower(headers.Get("x-grpc-web"))
	return strings.Contains(method, "bidiappend")
}

func decodeCursorBidiAppendDiagnostic(decoded []byte, sentinel []byte) cursorBidiAppendDiagnostic {
	diag := cursorBidiAppendDiagnostic{
		DecodedBytes:  len(decoded),
		DecodedSHA256: sha256Hex(decoded),
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
	Number   int
	WireType int
	Varint   uint64
	Bytes    []byte
}

func scanProtoValues(raw []byte, depth int) []scannedProtoValue {
	if depth > 2 {
		return nil
	}
	var out []scannedProtoValue
	for i := 0; i < len(raw); {
		key, n := binary.Uvarint(raw[i:])
		if n <= 0 {
			break
		}
		i += n
		fieldNumber := int(key >> 3)
		wireType := int(key & 0x7)
		if fieldNumber <= 0 {
			break
		}
		switch wireType {
		case 0:
			value, vn := binary.Uvarint(raw[i:])
			if vn <= 0 {
				return out
			}
			i += vn
			out = append(out, scannedProtoValue{Number: fieldNumber, WireType: wireType, Varint: value})
		case 1:
			if i+8 > len(raw) {
				return out
			}
			i += 8
		case 2:
			length, ln := binary.Uvarint(raw[i:])
			if ln <= 0 {
				return out
			}
			i += ln
			if length > uint64(len(raw)-i) {
				return out
			}
			value := raw[i : i+int(length)]
			i += int(length)
			out = append(out, scannedProtoValue{Number: fieldNumber, WireType: wireType, Bytes: value})
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

func appendProtoString(dst []byte, fieldNumber int, value string) []byte {
	dst = appendProtoKey(dst, fieldNumber, 2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendProtoBytes(dst []byte, fieldNumber int, value []byte) []byte {
	dst = appendProtoKey(dst, fieldNumber, 2)
	dst = binary.AppendUvarint(dst, uint64(len(value)))
	return append(dst, value...)
}

func appendProtoVarint(dst []byte, fieldNumber int, value uint64) []byte {
	dst = appendProtoKey(dst, fieldNumber, 0)
	return binary.AppendUvarint(dst, value)
}

func appendProtoKey(dst []byte, fieldNumber int, wireType int) []byte {
	return binary.AppendUvarint(dst, uint64(fieldNumber<<3|wireType))
}

func mustCursorDiagnosticJSON(diag cursorBidiAppendDiagnostic) []byte {
	raw, err := json.Marshal(diag)
	if err != nil {
		panic(fmt.Sprintf("marshal cursor diagnostic: %v", err))
	}
	return raw
}
