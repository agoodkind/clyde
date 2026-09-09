package codex

import (
	"bytes"
	"net/http"
	"testing"
)

func TestDetectRawResponsesCompactionProtocol(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		want   RawResponsesCompactionProtocol
	}{
		{
			name:   "ordinary turn",
			header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"turn","compaction":{"implementation":"responses"}}`}},
			want:   RawResponsesCompactionNone,
		},
		{
			name:   "empty implementation",
			header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"compaction","compaction":{}}`}},
			want:   RawResponsesCompactionNone,
		},
		{
			name:   "exact v1",
			header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"compaction","compaction":{"implementation":"responses"}}`}},
			want:   RawResponsesCompactionV1,
		},
		{
			name:   "exact v2",
			header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`}},
			want:   RawResponsesCompactionV2,
		},
		{
			name:   "v2 prefix",
			header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2_preview"}}`}},
			want:   RawResponsesCompactionNone,
		},
		{
			name:   "future implementation",
			header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v3"}}`}},
			want:   RawResponsesCompactionNone,
		},
		{
			name:   "missing metadata",
			header: http.Header{},
			want:   RawResponsesCompactionNone,
		},
		{
			name:   "malformed metadata",
			header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":`}},
			want:   RawResponsesCompactionNone,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := DetectRawResponsesCompactionProtocol(testCase.header)
			if got != testCase.want {
				t.Fatalf("DetectRawResponsesCompactionProtocol() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestRawResponsesCompactionPassesThroughNonV1Protocol(t *testing.T) {
	requestBody := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"recent user"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recent assistant"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}]}`)
	tests := []struct {
		name     string
		metadata string
	}{
		{
			name:     "v2",
			metadata: `{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`,
		},
		{
			name:     "unknown",
			metadata: `{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v3"}}`,
		},
	}
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := RawResponsesRequest{
				Body:   requestBody,
				Header: http.Header{CodexTurnMetadataHeader: {testCase.metadata}},
			}
			transformed, transformer := PrepareRawResponsesCompaction(raw, settings)
			if transformer != nil {
				t.Fatal("unexpected compaction transformer")
			}
			if !bytes.Equal(transformed.Body, requestBody) {
				t.Fatalf("request body changed:\n got: %s\nwant: %s", transformed.Body, requestBody)
			}
		})
	}
}

func TestRawResponsesCompactionV2PassesThroughMalformedLayout(t *testing.T) {
	body := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"patch"},{"type":"message","role":"user","content":[{"type":"input_text","text":"current"}]},{"type":"compaction_trigger"}]}`)
	settings := RawResponsesCompactionSettings{
		Enabled: true, ContextWindowTokens: 10_000, MaxTokens: 10_000,
		ContextWindowFraction: 1, BytesPerToken: 1, RecentFraction: 0.5,
	}
	raw := RawResponsesRequest{
		Body:   body,
		Header: http.Header{CodexTurnMetadataHeader: {`{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","phase":"mid_turn","strategy":"memento"}}`}},
	}
	transformed, transformer := PrepareRawResponsesCompaction(raw, settings)
	if transformer != nil || !bytes.Equal(transformed.Body, body) {
		t.Fatalf("v2 did not pass through: %s", transformed.Body)
	}
}
