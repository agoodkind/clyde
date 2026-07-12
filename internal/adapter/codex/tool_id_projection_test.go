package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestToolIDProjectionAvoidsOccupiedLongIDCandidate(t *testing.T) {
	sourceID := "call_" + strings.Repeat("d", 79)
	digest := sha256.Sum256([]byte(sourceID))
	candidate := "call_" + hex.EncodeToString(digest[:])[:codexToolCallIDMaxLength-len("call_")]
	projection := newToolIDProjection()
	projection.sourceToWire["existing"] = candidate
	projection.wireToSource[candidate] = "existing"

	projectedID := projection.project(sourceID)
	if projectedID == candidate {
		t.Fatalf("projected ID collided with occupied candidate %q", candidate)
	}
	if len(projectedID) > codexToolCallIDMaxLength {
		t.Fatalf("projected ID length=%d want at most %d", len(projectedID), codexToolCallIDMaxLength)
	}
	if projectedAgain := projection.project(sourceID); projectedAgain != projectedID {
		t.Fatalf("projected ID=%q want stable %q", projectedAgain, projectedID)
	}
}
