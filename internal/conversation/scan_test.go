package conversation

import (
	"context"
	"iter"
	"testing"

	"goodkind.io/clyde/internal/providerid"
	"goodkind.io/clyde/internal/transcript"
)

// failingScanParser discovers one candidate whose stamp differs from the prior
// scan's and then fails to read it, which is what a transient store error looks
// like from the scan's side.
type failingScanParser struct {
	path  string
	stamp FileStamp
}

func (failingScanParser) Provider() providerid.Provider { return providerid.ProviderClaude }

func (p failingScanParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return []ScanCandidate{{Path: p.path, Stamp: p.stamp}}, nil
}

func (failingScanParser) ScanRecord(string, FileStamp) (Record, bool) {
	return Record{}, false
}

func (failingScanParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

// TestScanKeepsAPriorRecordWhenTheReadFails pins that a conversation already in
// the cache survives a failed read. Recording the candidate's stamp after a
// failed read would claim the artifact was handled, so the next pass would treat
// it as unchanged and never read it again, dropping the conversation until its
// size or modification time happened to change.
func TestScanKeepsAPriorRecordWhenTheReadFails(t *testing.T) {
	const path = "/artifacts/known.jsonl"
	priorRecord := Record{
		ID:           "claude:known",
		Provider:     providerid.ProviderClaude,
		NativeID:     "known",
		ArtifactPath: path,
		ArtifactKind: "transcript",
	}
	priorStamp := FileStamp{Size: 10}
	currentStamp := FileStamp{Size: 20}

	registry := NewRegistry()
	registry.Register(failingScanParser{path: path, stamp: currentStamp})

	result, err := scan(context.Background(), registry, scanCache{
		records: map[string]Record{path: priorRecord},
		stamps:  map[string]FileStamp{path: priorStamp},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(result.records) != 1 || result.records[0].ID != priorRecord.ID {
		t.Fatalf("records = %+v, want the prior record carried forward", result.records)
	}
	if stamp, ok := result.stamps[path]; ok {
		t.Fatalf("stamp = %+v recorded for a failed read, want none so the next pass re-reads", stamp)
	}
}

// TestScanRecordsTheStampForAnArtifactThatNeverYieldedARecord pins that the
// failed-read handling does not force a re-read of an artifact that legitimately
// carries no conversation, which would make every pass re-read every such file.
func TestScanRecordsTheStampForAnArtifactThatNeverYieldedARecord(t *testing.T) {
	const path = "/artifacts/empty.jsonl"
	stamp := FileStamp{Size: 7}

	registry := NewRegistry()
	registry.Register(failingScanParser{path: path, stamp: stamp})

	result, err := scan(context.Background(), registry, scanCache{
		records: map[string]Record{},
		stamps:  map[string]FileStamp{},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(result.records) != 0 {
		t.Fatalf("records = %+v, want none", result.records)
	}
	if _, ok := result.stamps[path]; !ok {
		t.Fatalf("stamp missing for an artifact that never yielded a record, want it recorded")
	}
}
