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

type recordingMultiScanParser struct {
	candidate ScanCandidate
	result    MultiConversationScanResult
	found     bool
	scans     []MultiConversationScan
}

func (*recordingMultiScanParser) Provider() providerid.Provider {
	return providerid.ProviderClaude
}

func (p *recordingMultiScanParser) Discover(context.Context, map[string]Record) ([]ScanCandidate, error) {
	return []ScanCandidate{p.candidate}, nil
}

func (*recordingMultiScanParser) ScanRecord(string, FileStamp) (Record, bool) {
	return Record{}, false
}

func (*recordingMultiScanParser) Stream(string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
}

func (p *recordingMultiScanParser) ScanRecords(input MultiConversationScan) (MultiConversationScanResult, bool) {
	p.scans = append(p.scans, input)
	return p.result, p.found
}

func (*recordingMultiScanParser) StreamSelected(string, string, LoadOptions) iter.Seq2[transcript.Message, error] {
	return func(func(transcript.Message, error) bool) {}
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

func TestScanReusesEveryRecordFromUnchangedMultiConversationArtifact(t *testing.T) {
	const path = "/artifacts/events.jsonl"
	stamp := FileStamp{Size: 20}
	root := Record{ID: "copilot:root", ArtifactPath: path}
	agent := Record{ID: "copilot:agent", Selector: "agent-1", ArtifactPath: path}
	parser := &recordingMultiScanParser{
		candidate: ScanCandidate{Path: path, Stamp: stamp},
		found:     true,
	}
	registry := NewRegistry()
	registry.Register(parser)

	result, err := scan(context.Background(), registry, scanCache{
		records: map[string]Record{
			recordKey(path, ""):        root,
			recordKey(path, "agent-1"): agent,
		},
		stamps: map[string]FileStamp{
			recordKey(path, ""):        stamp,
			recordKey(path, "agent-1"): stamp,
		},
		multiStates: map[string]MultiConversationScanState{
			path: {Stamp: stamp, CompleteOffset: 20},
		},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(parser.scans) != 0 {
		t.Fatalf("ScanRecords calls = %d, want none for an unchanged artifact", len(parser.scans))
	}
	if len(result.records) != 2 || result.records[0].ID != root.ID || result.records[1].ID != agent.ID {
		t.Fatalf("records = %+v, want cached root and agent", result.records)
	}
	if result.stamps[recordKey(path, "agent-1")].Size != stamp.Size {
		t.Fatalf("agent stamp = %+v, want %+v", result.stamps[recordKey(path, "agent-1")], stamp)
	}
	if result.multiStates[path].CompleteOffset != 20 {
		t.Fatalf("complete offset = %d, want 20", result.multiStates[path].CompleteOffset)
	}
}

func TestScanReadsOnlyAppendedMultiConversationBytesAndKeepsSelectors(t *testing.T) {
	const path = "/artifacts/events.jsonl"
	priorStamp := FileStamp{Size: 20}
	currentStamp := FileStamp{Size: 35}
	root := Record{ID: "copilot:root", ArtifactPath: path}
	agent := Record{ID: "copilot:agent", Selector: "agent-1", ArtifactPath: path}
	newAgent := Record{ID: "copilot:new-agent", Selector: "agent-2", ArtifactPath: path}
	parser := &recordingMultiScanParser{
		candidate: ScanCandidate{Path: path, Stamp: currentStamp},
		result: MultiConversationScanResult{
			Records:        []Record{root, agent, newAgent},
			CompleteOffset: 34,
		},
		found: true,
	}
	registry := NewRegistry()
	registry.Register(parser)

	result, err := scan(context.Background(), registry, scanCache{
		records: map[string]Record{
			recordKey(path, ""):        root,
			recordKey(path, "agent-1"): agent,
		},
		stamps: map[string]FileStamp{
			recordKey(path, ""):        priorStamp,
			recordKey(path, "agent-1"): priorStamp,
		},
		multiStates: map[string]MultiConversationScanState{
			path: {Stamp: priorStamp, CompleteOffset: 19},
		},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(parser.scans) != 1 {
		t.Fatalf("ScanRecords calls = %d, want 1", len(parser.scans))
	}
	input := parser.scans[0]
	if input.StartOffset != 19 {
		t.Fatalf("start offset = %d, want 19", input.StartOffset)
	}
	if len(input.PriorRecords) != 2 || input.PriorRecords[1].Selector != "agent-1" {
		t.Fatalf("prior records = %+v, want root and selected agent", input.PriorRecords)
	}
	if len(result.records) != 3 || result.records[2].Selector != "agent-2" {
		t.Fatalf("records = %+v, want appended agent discovery", result.records)
	}
	if result.multiStates[path].CompleteOffset != 34 {
		t.Fatalf("complete offset = %d, want 34", result.multiStates[path].CompleteOffset)
	}
	if result.stamps[recordKey(path, "agent-2")].Size != currentStamp.Size {
		t.Fatalf("new agent stamp = %+v, want %+v", result.stamps[recordKey(path, "agent-2")], currentStamp)
	}
}

func TestScanRestartsMultiConversationArtifactAfterTruncation(t *testing.T) {
	const path = "/artifacts/events.jsonl"
	priorStamp := FileStamp{Size: 20}
	currentStamp := FileStamp{Size: 8}
	root := Record{ID: "copilot:root", ArtifactPath: path}
	parser := &recordingMultiScanParser{
		candidate: ScanCandidate{Path: path, Stamp: currentStamp},
		result: MultiConversationScanResult{
			Records:        []Record{root},
			CompleteOffset: 8,
		},
		found: true,
	}
	registry := NewRegistry()
	registry.Register(parser)

	_, err := scan(context.Background(), registry, scanCache{
		records: map[string]Record{recordKey(path, ""): root},
		stamps:  map[string]FileStamp{recordKey(path, ""): priorStamp},
		multiStates: map[string]MultiConversationScanState{
			path: {Stamp: priorStamp, CompleteOffset: 20},
		},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(parser.scans) != 1 {
		t.Fatalf("ScanRecords calls = %d, want 1", len(parser.scans))
	}
	if parser.scans[0].StartOffset != 0 {
		t.Fatalf("start offset = %d, want 0 after truncation", parser.scans[0].StartOffset)
	}
	if len(parser.scans[0].PriorRecords) != 0 {
		t.Fatalf("prior records = %+v, want none after truncation", parser.scans[0].PriorRecords)
	}
}

func TestScanRestartsMultiConversationArtifactWithoutScanState(t *testing.T) {
	const path = "/artifacts/events.jsonl"
	stamp := FileStamp{Size: 20}
	root := Record{ID: "copilot:root", ArtifactPath: path}
	parser := &recordingMultiScanParser{
		candidate: ScanCandidate{Path: path, Stamp: stamp},
		result: MultiConversationScanResult{
			Records:        []Record{root},
			CompleteOffset: 20,
		},
		found: true,
	}
	registry := NewRegistry()
	registry.Register(parser)

	_, err := scan(context.Background(), registry, scanCache{
		records: map[string]Record{recordKey(path, ""): root},
		stamps:  map[string]FileStamp{recordKey(path, ""): stamp},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(parser.scans) != 1 {
		t.Fatalf("ScanRecords calls = %d, want 1", len(parser.scans))
	}
	if parser.scans[0].StartOffset != 0 || len(parser.scans[0].PriorRecords) != 0 {
		t.Fatalf("scan input = %+v, want a full restart", parser.scans[0])
	}
}

func TestScanKeepsEveryPriorMultiConversationRecordWhenReadFails(t *testing.T) {
	const path = "/artifacts/events.jsonl"
	priorStamp := FileStamp{Size: 20}
	currentStamp := FileStamp{Size: 30}
	root := Record{ID: "copilot:root", ArtifactPath: path}
	agent := Record{ID: "copilot:agent", Selector: "agent-1", ArtifactPath: path}
	parser := &recordingMultiScanParser{
		candidate: ScanCandidate{Path: path, Stamp: currentStamp},
		found:     false,
	}
	registry := NewRegistry()
	registry.Register(parser)

	result, err := scan(context.Background(), registry, scanCache{
		records: map[string]Record{
			recordKey(path, ""):        root,
			recordKey(path, "agent-1"): agent,
		},
		stamps: map[string]FileStamp{
			recordKey(path, ""):        priorStamp,
			recordKey(path, "agent-1"): priorStamp,
		},
		multiStates: map[string]MultiConversationScanState{
			path: {Stamp: priorStamp, CompleteOffset: 20},
		},
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if len(result.records) != 2 || result.records[0].ID != root.ID || result.records[1].ID != agent.ID {
		t.Fatalf("records = %+v, want cached root and agent", result.records)
	}
	if _, ok := result.multiStates[path]; ok {
		t.Fatalf("scan state recorded after a failed read: %+v", result.multiStates[path])
	}
}
