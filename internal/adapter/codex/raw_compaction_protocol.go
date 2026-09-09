package codex

import (
	"encoding/json"
	"net/http"
	"strings"
)

// RawResponsesCompactionProtocol identifies an exact native Responses compaction implementation.
type RawResponsesCompactionProtocol string

const (
	// RawResponsesCompactionNone represents missing, malformed, or unknown metadata.
	RawResponsesCompactionNone RawResponsesCompactionProtocol = ""
	// RawResponsesCompactionV1 represents the native Responses text-summary compaction protocol.
	RawResponsesCompactionV1 RawResponsesCompactionProtocol = "responses"
	// RawResponsesCompactionV2 represents the native encrypted compaction protocol.
	RawResponsesCompactionV2 RawResponsesCompactionProtocol = "responses_compaction_v2"
)

type rawResponsesCompactionMetadata struct {
	RequestKind string `json:"request_kind"`
	Compaction  struct {
		Implementation string `json:"implementation"`
		Phase          string `json:"phase"`
		Strategy       string `json:"strategy"`
	} `json:"compaction"`
}

// DetectRawResponsesCompactionProtocol classifies the native turn metadata header.
func DetectRawResponsesCompactionProtocol(header http.Header) RawResponsesCompactionProtocol {
	metadataValue := strings.TrimSpace(header.Get(CodexTurnMetadataHeader))
	if metadataValue == "" {
		return RawResponsesCompactionNone
	}
	var metadata rawResponsesCompactionMetadata
	if json.Unmarshal([]byte(metadataValue), &metadata) != nil || metadata.RequestKind != "compaction" {
		return RawResponsesCompactionNone
	}
	switch RawResponsesCompactionProtocol(metadata.Compaction.Implementation) {
	case RawResponsesCompactionNone:
		return RawResponsesCompactionNone
	case RawResponsesCompactionV1:
		return RawResponsesCompactionV1
	case RawResponsesCompactionV2:
		return RawResponsesCompactionV2
	default:
		return RawResponsesCompactionNone
	}
}
