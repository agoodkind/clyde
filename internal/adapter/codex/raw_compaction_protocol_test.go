package codex

import (
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
