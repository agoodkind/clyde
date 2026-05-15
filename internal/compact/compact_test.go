package compact

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

// writeTranscript dumps lines (already JSON, no newline) into a temp
// JSONL file and returns its path. Each test gets its own tempdir so
// LoadSlice's file size and mtime are independent.
func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func testApplyOptions(summary string) SynthOptions {
	opts := newSynthOptions()
	opts.Summary = summary
	return opts
}

// userText builds a JSONL line for a user entry whose message.content
// is a plain string. Sufficient for the boundary-discovery and
// chat-stripper cases.
func userText(uuid, parent, text string, ts time.Time) string {
	payload := map[string]any{
		"uuid":       uuid,
		"parentUuid": parent,
		"type":       "user",
		"timestamp":  ts.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// assistantBlocks builds an assistant entry with a typed content
// array. Useful for tool_use, thinking, and image cases.
func assistantBlocks(uuid, parent string, ts time.Time, blocks []map[string]any) string {
	payload := map[string]any{
		"uuid":       uuid,
		"parentUuid": parent,
		"type":       "assistant",
		"timestamp":  ts.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "assistant",
			"content": blocks,
		},
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// userBlocks builds a user entry with a typed content array. Used
// for tool_result entries.
func userBlocks(uuid, parent string, ts time.Time, blocks []map[string]any) string {
	payload := map[string]any{
		"uuid":       uuid,
		"parentUuid": parent,
		"type":       "user",
		"timestamp":  ts.UTC().Format(time.RFC3339Nano),
		"message": map[string]any{
			"role":    "user",
			"content": blocks,
		},
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// boundaryLine builds a system compact_boundary entry the way Claude
// Code (and the new apply path) emits them.
func boundaryLine(uuid, parent string, ts time.Time) string {
	payload := map[string]any{
		"uuid":       uuid,
		"parentUuid": parent,
		"type":       "system",
		"subtype":    "compact_boundary",
		"timestamp":  ts.UTC().Format(time.RFC3339Nano),
		"isMeta":     true,
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

// TestLoadSlice_BoundaryDiscovery verifies the slice splits at the
// most recent compact_boundary, exposes both the full entry list and
// the post-boundary tail, and produces -1 when no boundary exists.
func TestLoadSlice_BoundaryDiscovery(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name             string
		lines            []string
		wantBoundaryLine int
		wantTotal        int
		wantPost         int
		wantBoundaryUUID string
	}{
		{
			name: "no boundary",
			lines: []string{
				userText("u1", "", "hi", t0),
				userText("u2", "u1", "world", t0.Add(time.Second)),
			},
			wantBoundaryLine: -1,
			wantTotal:        2,
			wantPost:         2,
		},
		{
			name: "single boundary at index 1",
			lines: []string{
				userText("u1", "", "before", t0),
				boundaryLine("b1", "u1", t0.Add(time.Second)),
				userText("u2", "b1", "after", t0.Add(2*time.Second)),
				userText("u3", "u2", "more", t0.Add(3*time.Second)),
			},
			wantBoundaryLine: 1,
			wantTotal:        4,
			wantPost:         2,
			wantBoundaryUUID: "b1",
		},
		{
			name: "two boundaries: most recent wins",
			lines: []string{
				userText("u1", "", "very old", t0),
				boundaryLine("b1", "u1", t0.Add(time.Second)),
				userText("u2", "b1", "old tail", t0.Add(2*time.Second)),
				boundaryLine("b2", "u2", t0.Add(3*time.Second)),
				userText("u3", "b2", "new", t0.Add(4*time.Second)),
			},
			wantBoundaryLine: 3,
			wantTotal:        5,
			wantPost:         1,
			wantBoundaryUUID: "b2",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTranscript(t, tc.lines)
			slice, err := LoadSlice(path)
			if err != nil {
				t.Fatalf("LoadSlice: %v", err)
			}
			if slice.BoundaryLine != tc.wantBoundaryLine {
				t.Errorf("BoundaryLine = %d, want %d", slice.BoundaryLine, tc.wantBoundaryLine)
			}
			if len(slice.AllEntries) != tc.wantTotal {
				t.Errorf("len(AllEntries) = %d, want %d", len(slice.AllEntries), tc.wantTotal)
			}
			if len(slice.PostBoundary) != tc.wantPost {
				t.Errorf("len(PostBoundary) = %d, want %d", len(slice.PostBoundary), tc.wantPost)
			}
			if tc.wantBoundaryUUID != "" && slice.BoundaryUUID != tc.wantBoundaryUUID {
				t.Errorf("BoundaryUUID = %q, want %q", slice.BoundaryUUID, tc.wantBoundaryUUID)
			}
		})
	}
}

type testSummarizeAdapter struct {
	calls int
	text  string
}

func (a *testSummarizeAdapter) SummarizeDropped(_ context.Context, _ SummarizeRequest) (string, error) {
	a.calls++
	return a.text, nil
}

func TestDoSummarizeAutoRunsOnlyAfterChatDrop(t *testing.T) {
	slice := &Slice{PostBoundary: []Entry{{Type: "user", RehydratedFrom: -1}}}
	adapter := &testSummarizeAdapter{text: "chat recap"}
	decision, err := DoSummarize(context.Background(), SummarizeRequest{
		Slice: slice,
		Options: SynthOptions{
			DroppedChatEntries: map[int]bool{0: true},
		},
		Mode:    SummarizeModeAuto,
		Adapter: adapter,
	})
	if err != nil {
		t.Fatalf("DoSummarize returned error: %v", err)
	}
	if !decision.ShouldSummarize || decision.Summary != "chat recap" {
		t.Fatalf("decision = %#v, want auto chat summary", decision)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestDoSummarizeAutoSkipsToolOnlyDrops(t *testing.T) {
	slice := &Slice{PostBoundary: []Entry{{Type: "assistant", Content: []ContentBlock{{Type: "tool_use", ToolUseID: "tool-1"}}, RehydratedFrom: -1}}}
	adapter := &testSummarizeAdapter{text: "tool recap"}
	decision, err := DoSummarize(context.Background(), SummarizeRequest{
		Slice: slice,
		Options: SynthOptions{
			ToolDetailOverride: map[string]ToolDetail{"tool-1": ToolDetailDrop},
		},
		Mode:    SummarizeModeAuto,
		Adapter: adapter,
	})
	if err != nil {
		t.Fatalf("DoSummarize returned error: %v", err)
	}
	if decision.ShouldSummarize {
		t.Fatalf("decision = %#v, want auto skip without chat truncation", decision)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
}

func TestDoSummarizeAutoRunsAfterPriorSummaryChunkDrop(t *testing.T) {
	slice := &Slice{PostBoundary: []Entry{{Type: "user", RehydratedFrom: -1}}}
	adapter := &testSummarizeAdapter{text: "prior recap"}
	decision, err := DoSummarize(context.Background(), SummarizeRequest{
		Slice: slice,
		Options: SynthOptions{
			DroppedSummaryChunks: map[int]map[string]bool{
				0: {"summary_of_dropped_content": true},
			},
		},
		Mode:    SummarizeModeAuto,
		Adapter: adapter,
	})
	if err != nil {
		t.Fatalf("DoSummarize returned error: %v", err)
	}
	if !decision.ShouldSummarize || decision.Summary != "prior recap" {
		t.Fatalf("decision = %#v, want auto prior-summary recap", decision)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestDoSummarizeOnRunsForToolOnlyDrops(t *testing.T) {
	slice := &Slice{PostBoundary: []Entry{{Type: "assistant", Content: []ContentBlock{{Type: "tool_use", ToolUseID: "tool-1"}}, RehydratedFrom: -1}}}
	adapter := &testSummarizeAdapter{text: "tool recap"}
	decision, err := DoSummarize(context.Background(), SummarizeRequest{
		Slice: slice,
		Options: SynthOptions{
			ToolDetailOverride: map[string]ToolDetail{"tool-1": ToolDetailDrop},
		},
		Mode:    SummarizeModeOn,
		Adapter: adapter,
	})
	if err != nil {
		t.Fatalf("DoSummarize returned error: %v", err)
	}
	if !decision.ShouldSummarize || decision.Summary != "tool recap" {
		t.Fatalf("decision = %#v, want forced tool summary", decision)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
}

func TestDoSummarizeOffSkipsChatDrops(t *testing.T) {
	slice := &Slice{PostBoundary: []Entry{{Type: "user", RehydratedFrom: -1}}}
	adapter := &testSummarizeAdapter{text: "chat recap"}
	decision, err := DoSummarize(context.Background(), SummarizeRequest{
		Slice: slice,
		Options: SynthOptions{
			DroppedChatEntries: map[int]bool{0: true},
		},
		Mode:    SummarizeModeOff,
		Adapter: adapter,
	})
	if err != nil {
		t.Fatalf("DoSummarize returned error: %v", err)
	}
	if decision.ShouldSummarize {
		t.Fatalf("decision = %#v, want disabled skip", decision)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
}

func TestApplyAllowsRecentlyModifiedTranscript(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	path := writeTranscript(t, []string{
		userText("u1", "", "fresh", t0),
	})
	slice, err := LoadSlice(path)
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	res, err := Apply(ApplyInput{
		Slice:         slice,
		SessionID:     "session-fresh",
		Cwd:           tmp,
		Version:       "test",
		Target:        50_000,
		BoundaryTail:  []OutputBlock{{Text: "surviving context"}},
		PreCompactTok: 1000,
		Options:       testApplyOptions("fresh transcript summary"),
	})
	if err != nil {
		t.Fatalf("Apply on fresh transcript: %v", err)
	}
	if res.PreApplyOffset <= 0 || res.PostApplyOffset <= res.PreApplyOffset {
		t.Fatalf("unexpected apply offsets: pre=%d post=%d", res.PreApplyOffset, res.PostApplyOffset)
	}
}

// TestLoadSlice_PairIndex confirms tool_use/tool_result blocks in
// the post-boundary slice link up by id.
func TestLoadSlice_PairIndex(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		boundaryLine("b1", "", t0),
		assistantBlocks("a1", "b1", t0.Add(time.Second), []map[string]any{
			{"type": "tool_use", "id": "tool_a", "name": "Bash", "input": map[string]any{"cmd": "ls"}},
		}),
		userBlocks("u1", "a1", t0.Add(2*time.Second), []map[string]any{
			{"type": "tool_result", "tool_use_id": "tool_a", "content": "file1\nfile2\n"},
		}),
		assistantBlocks("a2", "u1", t0.Add(3*time.Second), []map[string]any{
			{"type": "tool_use", "id": "tool_b", "name": "Read", "input": map[string]any{"path": "x"}},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}
	pairA, ok := slice.PairIndex["tool_a"]
	if !ok {
		t.Fatalf("PairIndex missing tool_a")
	}
	if pairA.UseEntryIdx != 0 || pairA.ResultEntryIdx != 1 {
		t.Errorf("tool_a pair = %+v, want use=0 result=1", pairA)
	}
	pairB, ok := slice.PairIndex["tool_b"]
	if !ok {
		t.Fatalf("PairIndex missing tool_b")
	}
	if pairB.UseEntryIdx != 2 || pairB.ResultEntryIdx != -1 {
		t.Errorf("tool_b pair = %+v, want use=2 result=-1 (no result yet)", pairB)
	}
}

// TestApplyStrippersFully_TableDriven exercises each stripper alone
// and the --all combination by applying the direct stripper helper and
// then synthesizing the output blocks. The assertions look at the
// produced []OutputBlock to confirm the stripper's intent showed up in
// the synthesized array.
func TestApplyStrippersFully_TableDriven(t *testing.T) {
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	imageData := "ZmFrZWltYWdl" // base64 "fakeimage"
	lines := []string{
		boundaryLine("b1", "", t0),
		userText("u1", "b1", "first user turn", t0.Add(time.Second)),
		assistantBlocks("a1", "u1", t0.Add(2*time.Second), []map[string]any{
			{"type": "thinking", "thinking": "let me think hard"},
			{"type": "text", "text": "first assistant reply"},
			{"type": "tool_use", "id": "tool_a", "name": "Bash", "input": map[string]any{"cmd": "ls"}},
		}),
		userBlocks("u2", "a1", t0.Add(3*time.Second), []map[string]any{
			{"type": "tool_result", "tool_use_id": "tool_a", "content": "file1\nfile2\n"},
		}),
		userBlocks("u3", "u2", t0.Add(4*time.Second), []map[string]any{
			{"type": "text", "text": "look at this image"},
			{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": imageData}},
		}),
		assistantBlocks("a2", "u3", t0.Add(5*time.Second), []map[string]any{
			{"type": "text", "text": "final assistant reply"},
		}),
	}
	slice, err := LoadSlice(writeTranscript(t, lines))
	if err != nil {
		t.Fatalf("LoadSlice: %v", err)
	}

	// joinedText concatenates every text block in the synthesized
	// array. Image blocks contribute nothing to this string, so
	// asserting on its presence cleanly distinguishes "image kept"
	// from "image as placeholder".
	joinedText := func(out []OutputBlock) string {
		var sb strings.Builder
		for _, b := range out {
			if b.Image == nil {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}

	// hasImageBlock reports whether at least one block was emitted
	// as an actual image (not a placeholder).
	hasImageBlock := func(out []OutputBlock) bool {
		for _, b := range out {
			if b.Image != nil {
				return true
			}
		}
		return false
	}

	cases := []struct {
		name            string
		strippers       Strippers
		wantThinking    bool // "[thinking]" substring present?
		wantImageBlock  bool // an image block survived?
		wantPlaceholder bool // "[image:" substring present?
		wantToolBody    bool // the result body "file1" appears?
		wantToolLine    bool // the "Bash(...)" tool activity line appears?
		wantFirstUser   bool // "first user turn" appears?
	}{
		{
			name:            "no strippers, full fidelity",
			strippers:       Strippers{},
			wantThinking:    true,
			wantImageBlock:  true,
			wantPlaceholder: false,
			wantToolBody:    true,
			wantToolLine:    true,
			wantFirstUser:   true,
		},
		{
			name:           "thinking only",
			strippers:      Strippers{Thinking: true},
			wantThinking:   false,
			wantImageBlock: true,
			wantToolBody:   true,
			wantToolLine:   true,
			wantFirstUser:  true,
		},
		{
			name:            "images only",
			strippers:       Strippers{Images: true},
			wantThinking:    true,
			wantImageBlock:  false,
			wantPlaceholder: true,
			wantToolBody:    true,
			wantToolLine:    true,
			wantFirstUser:   true,
		},
		{
			name:           "tools only",
			strippers:      Strippers{Tools: true},
			wantThinking:   true,
			wantImageBlock: true,
			wantToolBody:   false,
			wantToolLine:   false,
			wantFirstUser:  true,
		},
		{
			name:      "chat only drops oldest",
			strippers: Strippers{Chat: true},
			// Thinking lived on a1 (old assistant turn), which is
			// dropped by the chat stripper, so the thinking marker
			// disappears as a side effect.
			wantThinking:   false,
			wantImageBlock: true,
			wantToolBody:   true,
			wantToolLine:   true,
			// chatDropOrder preserves the most recent assistant +
			// its preceding user. "first user turn" is the OLDEST
			// chat user turn, so it should be dropped.
			wantFirstUser: false,
		},
		{
			name:            "all",
			strippers:       Strippers{Thinking: true, Images: true, Tools: true, Chat: true},
			wantThinking:    false,
			wantImageBlock:  false,
			wantPlaceholder: true,
			wantToolBody:    false,
			wantToolLine:    false,
			wantFirstUser:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := newSynthOptions()
			applyTestStrippersFully(slice, tc.strippers, &opts)
			res := &PlanResult{
				Options:      opts,
				BoundaryTail: Synthesize(slice, opts),
			}
			text := joinedText(res.BoundaryTail)
			gotImage := hasImageBlock(res.BoundaryTail)
			gotPlaceholder := strings.Contains(text, "[image:")
			gotThinking := strings.Contains(text, "[thinking]")
			gotToolBody := strings.Contains(text, "file1")
			gotToolLine := strings.Contains(text, "Bash(")
			gotFirstUser := strings.Contains(text, "first user turn")

			if gotThinking != tc.wantThinking {
				t.Errorf("thinking present = %v, want %v", gotThinking, tc.wantThinking)
			}
			if gotImage != tc.wantImageBlock {
				t.Errorf("image block present = %v, want %v", gotImage, tc.wantImageBlock)
			}
			if gotPlaceholder != tc.wantPlaceholder {
				t.Errorf("image placeholder present = %v, want %v", gotPlaceholder, tc.wantPlaceholder)
			}
			if gotToolBody != tc.wantToolBody {
				t.Errorf("tool body present = %v, want %v", gotToolBody, tc.wantToolBody)
			}
			if gotToolLine != tc.wantToolLine {
				t.Errorf("tool line present = %v, want %v", gotToolLine, tc.wantToolLine)
			}
			if gotFirstUser != tc.wantFirstUser {
				t.Errorf("first user turn present = %v, want %v", gotFirstUser, tc.wantFirstUser)
			}
		})
	}
}

func applyTestStrippersFully(slice *Slice, s Strippers, opts *SynthOptions) {
	if s.Thinking {
		opts.DropThinking = true
	}
	if s.Images {
		opts.ImagesAsPlaceholder = true
	}
	if s.Tools {
		for id := range slice.PairIndex {
			opts.ToolDetailOverride[id] = ToolDetailDrop
		}
	}
	if s.Chat {
		for _, step := range chatDropOrder(slice) {
			applyChatDropStep(opts.DroppedChatEntries, opts.DroppedSummaryChunks, step)
		}
	}
}

func TestRunPlanRejectsOmittedTarget(t *testing.T) {
	slice := &Slice{PostBoundary: []Entry{{Type: "user", TextOnly: "hello", RehydratedFrom: -1}}}

	_, err := RunPlan(context.Background(), PlanInput{
		Slice:     slice,
		Strippers: Strippers{Thinking: true},
	})
	if err == nil {
		t.Fatalf("RunPlan returned nil error for omitted target")
	}
	if !strings.Contains(err.Error(), "target must be greater than zero") {
		t.Fatalf("RunPlan error = %v, want target validation", err)
	}
}

// TestBuildBoundaryEntry_FieldOrder freezes the on-disk JSON shape of
// a compact_boundary line. Field order matters: Claude Code's
// large-file boundary scanner reads the first ~256 bytes looking for
// "compact_boundary". A shuffled emitter would silently break that
// detection.
func TestBuildBoundaryEntry_FieldOrder(t *testing.T) {
	ts := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	got, err := buildBoundaryEntry(boundaryEntryArgs{
		UUID:          "boundary-uuid-1",
		ParentUUID:    "parent-uuid-1",
		SessionID:     "sess-1",
		Cwd:           "/tmp/x",
		Version:       "clyde-test",
		Timestamp:     ts,
		PreCompactTok: 12345,
	})
	if err != nil {
		t.Fatalf("buildBoundaryEntry: %v", err)
	}
	want := `{"parentUuid":"parent-uuid-1","isSidechain":false,"type":"system","subtype":"compact_boundary","content":"Conversation compacted by clyde.","isMeta":true,"timestamp":"2026-04-18T12:00:00Z","uuid":"boundary-uuid-1","compactMetadata":{"trigger":"manual","preCompactTokenCount":12345},"cwd":"/tmp/x","sessionId":"sess-1","version":"clyde-test"}`
	if string(got) != want {
		t.Errorf("boundary JSON mismatch\n got:  %s\n want: %s", got, want)
	}
}

// TestBuildBoundaryEntry_NullParent confirms optionalString emits
// JSON null when ParentUUID is empty (the very first chain entry
// shape).
func TestBuildBoundaryEntry_NullParent(t *testing.T) {
	ts := time.Date(2026, 4, 18, 12, 0, 0, 0, time.UTC)
	got, err := buildBoundaryEntry(boundaryEntryArgs{
		UUID:      "b1",
		SessionID: "s1",
		Cwd:       "/x",
		Version:   "v",
		Timestamp: ts,
	})
	if err != nil {
		t.Fatalf("buildBoundaryEntry: %v", err)
	}
	if !strings.HasPrefix(string(got), `{"parentUuid":null,`) {
		t.Errorf("expected leading parentUuid:null, got prefix %q", string(got)[:40])
	}
}

// TestBuildSyntheticUserEntry_FieldOrderAndContent freezes the
// synthetic user entry shape including the embedded content array.
func TestBuildSyntheticUserEntry_FieldOrderAndContent(t *testing.T) {
	ts := time.Date(2026, 4, 18, 12, 0, 0, 1_000_000, time.UTC)
	got, err := buildSyntheticUserEntry(syntheticEntryArgs{
		UUID:       "syn-1",
		ParentUUID: "boundary-1",
		SessionID:  "sess-1",
		Cwd:        "/tmp",
		Version:    "v",
		Timestamp:  ts,
		Content: []OutputBlock{
			{Text: "hello"},
			{Image: &OutputImage{MediaType: "image/png", DataB64: "QUJD"}},
		},
	})
	if err != nil {
		t.Fatalf("buildSyntheticUserEntry: %v", err)
	}
	want := `{"parentUuid":"boundary-1","isSidechain":false,"type":"user","isCompactSummary":true,"timestamp":"2026-04-18T12:00:00.001Z","uuid":"syn-1","message":{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]},"cwd":"/tmp","sessionId":"sess-1","version":"v"}`
	if string(got) != want {
		t.Errorf("synthetic user JSON mismatch\n got:  %s\n want: %s", got, want)
	}
}

func TestResolveModelForCountingNormalizesSettingsModel(t *testing.T) {
	store := session.NewFileStore(t.TempDir())
	sess := session.NewSession("chat-compact", "uuid-compact")
	if err := store.Create(sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SaveSettings("chat-compact", &session.Settings{
		Model: "clyde-gpt-5.4-1m-medium",
	}); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	modelForCount, modelForRender, source := ResolveModelForCounting(store, sess, "")
	if modelForCount != "clyde-gpt-5.4-1m-medium" {
		t.Fatalf("modelForCount=%q want %q", modelForCount, "clyde-gpt-5.4-1m-medium")
	}
	if modelForRender != "clyde-gpt-5.4-1m-medium" {
		t.Fatalf("modelForRender=%q want %q", modelForRender, "clyde-gpt-5.4-1m-medium")
	}
	if source != "settings" {
		t.Fatalf("source=%q want %q", source, "settings")
	}
}
