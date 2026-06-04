package logevent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
)

// PayloadView is the inline payload representation logged with each request
// story. It carries only a summary: the full request and response bodies live
// in the SQLite capture store (capture.db), so the JSONL leg never restates body
// content and keeps just enough to identify and join the exchange.
type PayloadView struct {
	Summary PayloadSummary `json:"payload_summary,omitzero"`
}

// PayloadSummary records the shape, size, and hash of a body without storing
// any of its content. The sha256 is not a capture.db column, so it serves as a
// content-addressed join key from a JSONL leg to its stored body.
type PayloadSummary struct {
	ContentType string `json:"content_type,omitempty"`
	BodyType    string `json:"body_type,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Bytes       int    `json:"bytes,omitempty"`
	FieldCount  int    `json:"field_count,omitempty"`
	ArrayItems  int    `json:"array_items,omitempty"`
}

func appendPayloadAttrs(attrs []slog.Attr, payload PayloadView) []slog.Attr {
	return append(attrs, slog.Any("payload_summary", payload.Summary))
}

// FilterPayload returns the summary-only payload view for a request or response
// body. It records the content type, decoded shape, byte size, sha256, and a
// top-level field or item count, and never includes any field value.
func FilterPayload(raw []byte, contentType string) PayloadView {
	trimmed := strings.TrimSpace(string(raw))
	summary := PayloadSummary{
		ContentType: strings.TrimSpace(contentType),
		BodyType:    classifyJSONBytes([]byte(trimmed)),
		SHA256:      sha256Hex(raw),
		Bytes:       len(raw),
		FieldCount:  0,
		ArrayItems:  0,
	}
	if trimmed == "" {
		summary.BodyType = "empty"
		return PayloadView{Summary: summary}
	}
	if strings.HasPrefix(trimmed, "{") {
		fields := make(map[string]json.RawMessage)
		if err := json.Unmarshal([]byte(trimmed), &fields); err == nil {
			summary.FieldCount = len(fields)
		} else {
			summary.BodyType = "bytes"
		}
		return PayloadView{Summary: summary}
	}
	if strings.HasPrefix(trimmed, "[") {
		values := make([]json.RawMessage, 0)
		if err := json.Unmarshal([]byte(trimmed), &values); err == nil {
			summary.ArrayItems = len(values)
		} else {
			summary.BodyType = "bytes"
		}
		return PayloadView{Summary: summary}
	}
	return PayloadView{Summary: summary}
}

func classifyJSONBytes(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		return "empty"
	case strings.HasPrefix(trimmed, "{"):
		return "json_object"
	case strings.HasPrefix(trimmed, "["):
		return "json_array"
	case strings.HasPrefix(trimmed, `"`), trimmed == "true", trimmed == "false", trimmed == "null":
		return "json_scalar"
	default:
		return "bytes"
	}
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
