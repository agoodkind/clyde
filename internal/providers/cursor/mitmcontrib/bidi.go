package mitmcontrib

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"goodkind.io/clyde/internal/mitm/capture"
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

type protobufField struct {
	FieldNumber uint64          `json:"field_number"`
	WireType    string          `json:"wire_type"`
	Varint      *uint64         `json:"varint,omitempty"`
	DataBase64  string          `json:"data_base64,omitempty"`
	Text        string          `json:"text,omitempty"`
	Children    []protobufField `json:"children,omitempty"`
}

type bidiCaptureRepresentation struct {
	OuterFields []protobufField `json:"outer_fields"`
	HexEnvelope *hexEnvelope    `json:"hex_envelope,omitempty"`
}

type hexEnvelope struct {
	SourceField uint64          `json:"source_field"`
	DataBase64  string          `json:"data_base64"`
	Fields      []protobufField `json:"fields"`
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

// DecodeCaptureRequest creates a lossless, local representation for a Cursor
// BidiAppend body. It preserves each protobuf field's wire value and decodes a
// recognized hex protobuf envelope without changing the raw capture body.
func DecodeCaptureRequest(req RequestCapture) (capture.DecodedBody, bool) {
	if !looksLikeBidiAppend(req.Path, req.Headers) {
		return capture.DecodedBody{
			Format:             "",
			Status:             "",
			Error:              "",
			RepresentationJSON: nil,
			ToolEvents:         nil,
		}, false
	}
	outerFields, err := parseProtobufFields(req.Body, 0)
	if err != nil {
		return failedDecodedBody(err), true
	}
	representation := bidiCaptureRepresentation{OuterFields: outerFields, HexEnvelope: nil}
	decodedBody := capture.DecodedBody{
		Format:             "cursor.bidi_append.protobuf_hex",
		Status:             capture.DecodeStatusFailed,
		Error:              "cursor BidiAppend body does not contain a recognized hex protobuf envelope",
		RepresentationJSON: nil,
		ToolEvents:         nil,
	}
	envelope, events, envelopeErr := UnmarshalHexEnvelope(outerFields)
	if envelopeErr == nil {
		representation.HexEnvelope = &envelope
		decodedBody.Status = capture.DecodeStatusSuccess
		decodedBody.Error = ""
		appendSeqno := outerFieldVarint(outerFields, 2)
		for i := range events {
			events[i].Ordering = appendSeqno
		}
		decodedBody.ToolEvents = events
	} else {
		decodedBody.Error = envelopeErr.Error()
	}
	representationJSON, marshalErr := json.Marshal(representation)
	if marshalErr != nil {
		return failedDecodedBody(fmt.Errorf("marshal BidiAppend representation: %w", marshalErr)), true
	}
	decodedBody.RepresentationJSON = representationJSON
	return decodedBody, true
}

func failedDecodedBody(err error) capture.DecodedBody {
	representationJSON, marshalErr := json.Marshal(bidiCaptureRepresentation{
		OuterFields: nil,
		HexEnvelope: nil,
	})
	if marshalErr != nil {
		representationJSON = []byte(`{"outer_fields":[]}`)
	}
	return capture.DecodedBody{
		Format:             "cursor.bidi_append.protobuf_hex",
		Status:             capture.DecodeStatusFailed,
		Error:              err.Error(),
		RepresentationJSON: representationJSON,
		ToolEvents:         nil,
	}
}

func outerFieldVarint(fields []protobufField, fieldNumber uint64) uint64 {
	for _, field := range fields {
		if field.FieldNumber == fieldNumber && field.Varint != nil {
			return *field.Varint
		}
	}
	return 0
}

func parseProtobufFields(raw []byte, depth int) ([]protobufField, error) {
	if depth > 8 {
		return nil, fmt.Errorf("protobuf nesting exceeds capture limit")
	}
	fields := make([]protobufField, 0)
	for offset := 0; offset < len(raw); {
		fieldNumber, wireType, next, ok := readProtoKey(raw, offset)
		if !ok {
			return nil, fmt.Errorf("invalid protobuf field key at byte %d", offset)
		}
		offset = next
		field := protobufField{FieldNumber: fieldNumber, WireType: "", Varint: nil, DataBase64: "", Text: "", Children: nil}
		switch wireType {
		case 0:
			value, nextOffset, valid := readProtoVarint(raw, offset)
			if !valid {
				return nil, fmt.Errorf("invalid protobuf varint for field %d", fieldNumber)
			}
			field.WireType = "varint"
			field.Varint = &value
			offset = nextOffset
		case 1:
			if offset+8 > len(raw) {
				return nil, fmt.Errorf("truncated protobuf fixed64 field %d", fieldNumber)
			}
			field.WireType = "fixed64"
			field.DataBase64 = base64.StdEncoding.EncodeToString(raw[offset : offset+8])
			offset += 8
		case 2:
			value, nextOffset, valid := readProtoBytes(raw, offset)
			if !valid {
				return nil, fmt.Errorf("invalid protobuf bytes field %d", fieldNumber)
			}
			field.WireType = "bytes"
			field.DataBase64 = base64.StdEncoding.EncodeToString(value)
			if utf8.Valid(value) {
				field.Text = string(value)
			}
			children, childErr := parseProtobufFields(value, depth+1)
			if childErr == nil && len(children) > 0 {
				field.Children = children
			}
			offset = nextOffset
		case 5:
			if offset+4 > len(raw) {
				return nil, fmt.Errorf("truncated protobuf fixed32 field %d", fieldNumber)
			}
			field.WireType = "fixed32"
			field.DataBase64 = base64.StdEncoding.EncodeToString(raw[offset : offset+4])
			offset += 4
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d for field %d", wireType, fieldNumber)
		}
		fields = append(fields, field)
	}
	return fields, nil
}

// UnmarshalHexEnvelope decodes the first recognized hexadecimal protobuf
// envelope from the BidiAppend field tree.
func UnmarshalHexEnvelope(fields []protobufField) (hexEnvelope, []capture.ToolEvent, error) {
	var lastError error
	for _, field := range fields {
		if field.WireType != "bytes" || field.Text == "" || !isHexEnvelope(field.Text) {
			continue
		}
		data, err := hex.DecodeString(field.Text)
		if err != nil {
			lastError = fmt.Errorf("decode hex envelope: %w", err)
			continue
		}
		envelopeFields, err := parseProtobufFields(data, 0)
		if err != nil {
			lastError = fmt.Errorf("decode hex envelope protobuf: %w", err)
			continue
		}
		event, understood := decodeToolEvent(envelopeFields)
		events := make([]capture.ToolEvent, 0)
		if understood {
			events = append(events, event)
		}
		return hexEnvelope{
			SourceField: field.FieldNumber,
			DataBase64:  base64.StdEncoding.EncodeToString(data),
			Fields:      envelopeFields,
		}, events, nil
	}
	if lastError != nil {
		slog.Warn("mitm.cursor.bidi_append.hex_envelope_decode_failed",
			"component", "mitm",
			"concern", "providers.mitm.wire",
			"provider", ProviderName,
			"err", lastError,
		)
		return hexEnvelope{}, nil, fmt.Errorf("cursor BidiAppend body does not contain a recognized hex protobuf envelope: %w", lastError)
	}
	return hexEnvelope{}, nil, fmt.Errorf("cursor BidiAppend body does not contain a recognized hex protobuf envelope")
}

func isHexEnvelope(value string) bool {
	if len(value) == 0 || len(value)%2 != 0 {
		return false
	}
	for _, char := range value {
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}

func decodeToolEvent(fields []protobufField) (capture.ToolEvent, bool) {
	var callID string
	var toolName string
	var inputRepresentation string
	var inputEncoding capture.ToolInputEncoding
	for _, field := range fields {
		if field.WireType != "bytes" {
			continue
		}
		switch field.FieldNumber {
		case 1:
			callID = field.Text
		case 2:
			toolName = field.Text
		case 3:
			if json.Valid(decodeBase64(field.DataBase64)) {
				inputRepresentation = field.Text
				inputEncoding = capture.ToolInputEncodingJSON
			} else {
				inputRepresentation = field.DataBase64
				inputEncoding = capture.ToolInputEncodingBase64
			}
		}
	}
	if callID == "" || toolName == "" || inputRepresentation == "" {
		return capture.ToolEvent{
			Ordering:            0,
			CallID:              "",
			ToolName:            "",
			InputRepresentation: "",
			InputEncoding:       "",
		}, false
	}
	return capture.ToolEvent{Ordering: 0, CallID: callID, ToolName: toolName, InputRepresentation: inputRepresentation, InputEncoding: inputEncoding}, true
}

func decodeBase64(value string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil
	}
	return decoded
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
	outerFields, parseErr := parseProtobufFields(decoded, 0)
	if parseErr == nil {
		for _, field := range outerFields {
			if field.FieldNumber != 2 || field.WireType != "varint" || field.Varint == nil {
				continue
			}
			diag.AppendSeqno = *field.Varint
			break
		}
	}
	var longestBytes []byte
	for _, value := range values {
		if diag.RequestID == "" && likelyRequestID(value.Bytes) {
			diag.RequestID = string(value.Bytes)
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
