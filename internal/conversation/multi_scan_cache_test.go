package conversation

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMultiConversationScanStateRoundTripsCache(t *testing.T) {
	const artifactPath = "/artifacts/events.jsonl"
	cachePath := filepath.Join(t.TempDir(), cacheFilename)
	stamp := FileStamp{Size: 31, Mtime: time.Unix(42, 0).UTC()}
	records := []Record{
		{ID: "copilot:root", Selector: "", ArtifactPath: artifactPath},
		{ID: "copilot:agent", Selector: "agent-1", ArtifactPath: artifactPath},
	}
	stamps := map[string]FileStamp{
		recordKey(artifactPath, ""):        stamp,
		recordKey(artifactPath, "agent-1"): stamp,
	}
	states := map[string]MultiConversationScanState{
		artifactPath: {Stamp: stamp, CompleteOffset: 29},
	}

	if err := writeCache(cachePath, records, stamps, states); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	gotRecords, gotStamps, gotStates, err := readCache(cachePath)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if len(gotRecords) != 2 {
		t.Fatalf("records = %+v, want %+v", gotRecords, records)
	}
	var agentSelector string
	for _, record := range gotRecords {
		if record.ID == "copilot:agent" {
			agentSelector = record.Selector
		}
	}
	if agentSelector != "agent-1" {
		t.Fatalf("agent selector = %q, want agent-1", agentSelector)
	}
	if !gotStamps[recordKey(artifactPath, "agent-1")].Equal(stamp) {
		t.Fatalf("agent stamp = %+v, want %+v", gotStamps[recordKey(artifactPath, "agent-1")], stamp)
	}
	if gotStates[artifactPath].CompleteOffset != 29 ||
		!gotStates[artifactPath].Stamp.Equal(stamp) {
		t.Fatalf("scan state = %+v, want offset 29 and stamp %+v", gotStates[artifactPath], stamp)
	}
}
