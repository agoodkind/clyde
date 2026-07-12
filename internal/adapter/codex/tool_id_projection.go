package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

const codexToolCallIDMaxLength = 64

// toolIDProjection keeps one Codex-safe wire ID for each external tool ID
// while a request history is converted for Codex egress.
type toolIDProjection struct {
	sourceToWire map[string]string
	wireToSource map[string]string
}

func newToolIDProjection() *toolIDProjection {
	return &toolIDProjection{
		sourceToWire: make(map[string]string),
		wireToSource: make(map[string]string),
	}
}

func (projection *toolIDProjection) reserveCompliant(sourceIDs []string) {
	for _, sourceID := range sourceIDs {
		if sourceID == "" || len(sourceID) > codexToolCallIDMaxLength {
			continue
		}
		if _, ok := projection.sourceToWire[sourceID]; ok {
			continue
		}
		projection.sourceToWire[sourceID] = sourceID
		projection.wireToSource[sourceID] = sourceID
	}
}

func (projection *toolIDProjection) project(sourceID string) string {
	if sourceID == "" {
		return ""
	}
	if wireID, ok := projection.sourceToWire[sourceID]; ok {
		return wireID
	}

	candidate := sourceID
	if len(candidate) > codexToolCallIDMaxLength {
		digest := sha256.Sum256([]byte(sourceID))
		candidate = "call_" + hex.EncodeToString(digest[:])[:codexToolCallIDMaxLength-len("call_")]
	}
	candidate = projection.reserve(candidate, sourceID)
	projection.sourceToWire[sourceID] = candidate
	projection.wireToSource[candidate] = sourceID
	return candidate
}

func (projection *toolIDProjection) reserve(candidate, sourceID string) string {
	if recordedSource, ok := projection.wireToSource[candidate]; !ok || recordedSource == sourceID {
		return candidate
	}

	for suffix := 1; ; suffix++ {
		suffixText := "_" + strconv.Itoa(suffix)
		prefixLength := codexToolCallIDMaxLength - len(suffixText)
		alternate := candidate[:prefixLength] + suffixText
		if _, occupied := projection.wireToSource[alternate]; !occupied {
			return alternate
		}
	}
}
