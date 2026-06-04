// Package response renders user-visible CLI and MCP responses with Clyde's
// existing gklog correlation metadata.
package response

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"goodkind.io/gklog/correlation"
)

// Metadata is the user-visible subset of the gklog correlation context.
type Metadata struct {
	RequestID    string `json:"request_id,omitempty"`
	TraceID      string `json:"trace_id,omitempty"`
	SpanID       string `json:"span_id,omitempty"`
	ParentSpanID string `json:"parent_span_id,omitempty"`
}

// FromContext projects the current gklog correlation context.
func FromContext(ctx context.Context) Metadata {
	corr := correlation.FromContext(ctx)
	return Metadata{
		RequestID:    strings.TrimSpace(corr.RequestID),
		TraceID:      string(corr.TraceID),
		SpanID:       string(corr.SpanID),
		ParentSpanID: string(corr.ParentSpanID),
	}
}

// Empty reports whether the metadata contains no visible fields.
func (metadata Metadata) Empty() bool {
	return metadata.RequestID == "" &&
		metadata.TraceID == "" &&
		metadata.SpanID == "" &&
		metadata.ParentSpanID == ""
}

// headerMarker prefixes the metadata header line so callers can detect a body
// that already carries one and avoid stamping a second header. It is the shared
// gklog marker, so every producer renders the header the same way.
const headerMarker = correlation.HeaderMarker

// HeaderLine returns the first line for text responses, rendered through the
// shared gklog helper so the format stays canonical across producers.
func (metadata Metadata) HeaderLine() string {
	return correlation.MarkerLine(
		"trace_id", metadata.TraceID,
		"span_id", metadata.SpanID,
		"parent_span_id", metadata.ParentSpanID,
		"request_id", metadata.RequestID,
	)
}

// Text renders a text response using this metadata value. When body already
// begins with a metadata header (for example a daemon RPC response that the
// daemon already stamped with its own trace id), it is returned unchanged so a
// front end does not stamp a second header and the original id is preserved end
// to end.
func (metadata Metadata) Text(body string) string {
	if strings.HasPrefix(body, headerMarker) {
		return body
	}
	header := metadata.HeaderLine()
	if header == "" {
		return body
	}
	if body == "" {
		return header + "\n"
	}
	return header + "\n" + body
}

// Text renders a text response with the metadata line first when metadata exists.
func Text(ctx context.Context, body string) string {
	return FromContext(ctx).Text(body)
}

// WriteText writes a text response to writer.
func WriteText(ctx context.Context, writer io.Writer, body string) error {
	if _, err := io.WriteString(writer, Text(ctx, body)); err != nil {
		slog.WarnContext(ctx, "response.write_text_failed", "concern", "cli.output", "component", "response", "err", err)
		return fmt.Errorf("response: write text: %w", err)
	}
	return nil
}

// JSONStyle controls how a JSON response envelope is formatted.
type JSONStyle uint8

const (
	// JSONCompact writes the response envelope on one line.
	JSONCompact JSONStyle = iota
	// JSONIndented writes the response envelope with two-space indentation.
	JSONIndented
)

// JSONDocument is Clyde's top-level JSON response contract.
type JSONDocument struct {
	Meta   Metadata        `json:"_meta"`
	Result json.RawMessage `json:"result"`
}

// JSON renders a JSON response envelope with metadata first and the command
// payload under result.
func JSON(ctx context.Context, payload []byte, style JSONStyle) ([]byte, error) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("response: empty json payload")
	}
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("response: invalid json payload")
	}
	meta := FromContext(ctx)
	objectPayload := make(map[string]json.RawMessage)
	if err := json.Unmarshal(trimmed, &objectPayload); err == nil {
		metaBody, metaErr := json.Marshal(meta)
		if metaErr != nil {
			slog.WarnContext(ctx, "response.encode_json_meta_failed", "concern", "cli.output", "component", "response", "err", metaErr)
			return nil, fmt.Errorf("response: encode json metadata: %w", metaErr)
		}
		objectPayload["_meta"] = metaBody
		return marshalJSONObjectBody(ctx, objectPayload, style)
	}
	document := JSONDocument{
		Meta:   meta,
		Result: append(json.RawMessage(nil), trimmed...),
	}
	return marshalJSONDocumentBody(ctx, document, style)
}

// WriteJSON writes a JSON response envelope to writer.
func WriteJSON(ctx context.Context, writer io.Writer, payload []byte, style JSONStyle) error {
	body, err := JSON(ctx, payload, style)
	if err != nil {
		return err
	}
	if _, err := writer.Write(body); err != nil {
		slog.WarnContext(ctx, "response.write_json_failed", "concern", "cli.output", "component", "response", "err", err)
		return fmt.Errorf("response: write json: %w", err)
	}
	return nil
}

func marshalJSONObjectBody(ctx context.Context, payload map[string]json.RawMessage, style JSONStyle) ([]byte, error) {
	var body []byte
	var err error
	switch style {
	case JSONIndented:
		body, err = json.MarshalIndent(payload, "", "  ")
	case JSONCompact:
		fallthrough
	default:
		body, err = json.Marshal(payload)
	}
	if err != nil {
		slog.WarnContext(ctx, "response.encode_json_failed", "concern", "cli.output", "component", "response", "err", err)
		return nil, fmt.Errorf("response: encode json envelope: %w", err)
	}
	return append(body, '\n'), nil
}

func marshalJSONDocumentBody(ctx context.Context, payload JSONDocument, style JSONStyle) ([]byte, error) {
	var body []byte
	var err error
	switch style {
	case JSONIndented:
		body, err = json.MarshalIndent(payload, "", "  ")
	case JSONCompact:
		fallthrough
	default:
		body, err = json.Marshal(payload)
	}
	if err != nil {
		slog.WarnContext(ctx, "response.encode_json_failed", "concern", "cli.output", "component", "response", "err", err)
		return nil, fmt.Errorf("response: encode json envelope: %w", err)
	}
	return append(body, '\n'), nil
}
