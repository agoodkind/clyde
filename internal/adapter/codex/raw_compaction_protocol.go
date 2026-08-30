package codex

import (
	"encoding/json"
	"net/http"
	"strings"
)

type RawResponsesCompactionProtocol string

const (
	RawResponsesCompactionNone RawResponsesCompactionProtocol = ""
	RawResponsesCompactionV1   RawResponsesCompactionProtocol = "responses"
	RawResponsesCompactionV2   RawResponsesCompactionProtocol = "responses_compaction_v2"
)

type rawResponsesCompactionMetadata struct {
	RequestKind string `json:"request_kind"`
	Compaction  struct {
		Implementation string `json:"implementation"`
		Phase          string `json:"phase"`
		Strategy       string `json:"strategy"`
	} `json:"compaction"`
}

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
	case RawResponsesCompactionV1:
		return RawResponsesCompactionV1
	case RawResponsesCompactionV2:
		return RawResponsesCompactionV2
	default:
		return RawResponsesCompactionNone
	}
}
