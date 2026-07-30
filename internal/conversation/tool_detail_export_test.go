package conversation

import (
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/transcript"
)

func newToolAwareIndex(seen *[]LoadOptions) (*Index, Record) {
	registry := NewRegistry()
	registry.Register(&streamFuncParser{stream: func(path string, opts LoadOptions) iter.Seq2[transcript.Message, error] {
		*seen = append(*seen, opts)
		return func(yield func(transcript.Message, error) bool) {
			output := ""
			if opts.IncludeToolOutputs {
				output = "sse bytes: 4096"
			}
			message := transcript.Message{
				UUID:              "assistant-1",
				ParentUUID:        "",
				LogicalParentUUID: "",
				Role:              "assistant",
				Visibility:        transcript.MessageVisibilityVisible,
				Compaction:        nil,
				Timestamp:         time.Unix(2, 0),
				Text:              "",
				Thinking:          "",
				HasTools:          true,
				Tools: []transcript.ToolCall{{
					ID:   "tool-1",
					Name: "Bash",
					Input: transcript.ToolInputJSON{
						Raw: json.RawMessage(`{"command":"echo \"===","description":"Diagnose streaming probe timeout: SSE bytes, crash, memory"}`),
					},
					Display:     "",
					DisplayLang: "",
					Output:      output,
					IsError:     false,
				}},
			}
			yield(message, nil)
		}
	}})
	idx := newTestIndex(registry)
	idx.records = []Record{{
		ID:            "claude:tool-detail",
		Provider:      ProviderClaude,
		NativeID:      "tool-detail",
		Title:         "tool-detail",
		WorkspaceRoot: "/repo",
		ArtifactPath:  "/tmp/tool-detail.jsonl",
		ArtifactKind:  "transcript",
		Model:         "model",
		CreatedAt:     time.Unix(1, 0),
		UpdatedAt:     time.Unix(2, 0),
		SizeBytes:     10,
		Archived:      false,
	}}
	idx.loaded = true
	return idx, idx.records[0]
}

func TestExportToolDetailLevels(t *testing.T) {
	cases := []struct {
		name            string
		content         ContentKindSet
		wantLoadOutputs bool
		wantContains    []string
		wantAbsent      []string
	}{
		{
			name:            "summaries",
			content:         NewContentKindSet(ContentKindToolSummaries),
			wantLoadOutputs: false,
			wantContains: []string{
				"[tool: Bash] Diagnose streaming probe timeout: SSE bytes, crash, memory",
			},
			wantAbsent: []string{
				`{"command":"echo \"==="`,
				"[tool output]",
				"sse bytes: 4096",
			},
		},
		{
			name:            "calls",
			content:         NewContentKindSet(ContentKindToolCalls),
			wantLoadOutputs: false,
			wantContains: []string{
				`[tool: Bash]`,
			},
			wantAbsent: []string{
				`{"command":"echo \"===","description":"Diagnose streaming probe timeout: SSE bytes, crash, memory"}`,
				"[tool output]",
				"sse bytes: 4096",
			},
		},
		{
			name:            "outputs",
			content:         NewContentKindSet(ContentKindToolOutputs),
			wantLoadOutputs: true,
			wantContains: []string{
				`[tool: Bash]`,
				"[tool output]",
				"sse bytes: 4096",
			},
			wantAbsent: []string{
				`{"command":"echo \"===","description":"Diagnose streaming probe timeout: SSE bytes, crash, memory"}`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := []LoadOptions{}
			idx, record := newToolAwareIndex(&seen)
			body, err := idx.Export(record, ExportOptions{
				Format:       ExportFormatPlainText,
				HistoryStart: 0,
				LastN:        0,
				Whitespace:   WhitespacePreserve,
				Content:      tc.content,
				Compaction: CompactionExportOptions{
					IncludeSelector: "all",
					FullHistory:     false,
				},
			})
			if err != nil {
				t.Fatalf("Export returned error: %v", err)
			}
			if len(seen) != 1 {
				t.Fatalf("loader calls = %d, want 1", len(seen))
			}
			if seen[0].IncludeToolOutputs != tc.wantLoadOutputs {
				t.Fatalf("IncludeToolOutputs = %t, want %t", seen[0].IncludeToolOutputs, tc.wantLoadOutputs)
			}
			text := string(body)
			for _, want := range tc.wantContains {
				if !strings.Contains(text, want) {
					t.Fatalf("export text missing %q: %q", want, text)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(text, absent) {
					t.Fatalf("export text unexpectedly contained %q: %q", absent, text)
				}
			}
		})
	}
}
