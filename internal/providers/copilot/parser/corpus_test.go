package parser

import (
	"context"
	"os"
	"testing"

	"goodkind.io/clyde/internal/conversation"
)

func TestLocalCorpusReplay(t *testing.T) {
	if os.Getenv("CLYDE_CORPUS_REPLAY") != "1" {
		t.Skip("set CLYDE_CORPUS_REPLAY=1 to replay local Copilot artifacts")
	}
	parser := New()
	candidates, err := parser.Discover(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		t.Fatal("no Copilot event logs discovered")
	}
	conversationCount := 0
	for _, candidate := range candidates {
		result, ok := parser.ScanRecords(conversation.MultiConversationScan{
			Candidate:    candidate,
			PriorRecords: nil,
			StartOffset:  0,
		})
		if !ok {
			t.Fatalf("scan %s returned no records", candidate.Path)
		}
		for _, record := range result.Records {
			if _, err := conversation.CollectMessages(parser.StreamSelected(
				record.ArtifactPath,
				record.Selector,
				conversation.LoadOptions{},
			)); err != nil {
				t.Fatalf("load %s: %v", record.ID, err)
			}
			conversationCount++
		}
	}
	if conversationCount == 0 {
		t.Fatal("Copilot replay loaded no conversations")
	}
}
