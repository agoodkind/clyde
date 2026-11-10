package transcript

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// mapPanicking stands for a mapping function whose reader trips a defect part
// way through one record, with the unnamed results every real mapping site
// has. The deployed daemon hit this shape: a regex splice inverted its slice
// bounds on a Cursor turn whose query was empty.
func mapPanicking() (Message, bool) {
	defer ContainMappingPanic()
	partial := Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "assistant",
		Visibility:        "",
		Compaction:        nil,
		Timestamp:         time.Unix(1, 0),
		Text:              "partial",
		Thinking:          "",
		HasTools:          false,
		Tools:             []ToolCall{},
	}
	// The reader builds part of the message, then trips the defect. The guard
	// must report the zero message rather than this half-built one.
	_ = partial
	panic("slice bounds out of range [345:344]")
}

// mapPanickingThreeResults stands for claude parseLine, the one mapping site
// that also returns the tool results a line carried.
func mapPanickingThreeResults() (Message, []ToolCall, bool) {
	defer ContainMappingPanic()
	panic("slice bounds out of range [57:30]")
}

// mapUsable stands for a record the reader maps normally.
func mapUsable() (Message, bool) {
	defer ContainMappingPanic()
	message := Message{
		UUID:              "",
		ParentUUID:        "",
		LogicalParentUUID: "",
		Role:              "user",
		Visibility:        "",
		Compaction:        nil,
		Timestamp:         time.Time{},
		Text:              "kept",
		Thinking:          "",
		HasTools:          false,
		Tools:             nil,
	}
	return message, true
}

// TestContainMappingPanicReportsTheSkipCallersAlreadyHandle states the contract
// the provider mapping sites rely on. A panic leaves the function reporting a
// zero message and a false include, which is what those sites already return
// when they decline a record, so no caller needs to change.
func TestContainMappingPanicReportsTheSkipCallersAlreadyHandle(t *testing.T) {
	t.Parallel()
	message, include := mapPanicking()
	if include {
		t.Fatal("include = true after a panic, want false so the caller skips the record")
	}
	if message.Role != "" || message.Text != "" || !message.Timestamp.IsZero() || message.Tools != nil {
		t.Fatalf("message = %+v, want the zero message a declined record reports", message)
	}
}

// TestContainMappingPanicZeroesEveryResult covers the three-result shape, so
// the guard is proven on claude parseLine and not only on the five two-result
// mapping sites.
func TestContainMappingPanicZeroesEveryResult(t *testing.T) {
	t.Parallel()
	message, tools, ok := mapPanickingThreeResults()
	if ok {
		t.Fatal("ok = true after a panic")
	}
	if tools != nil {
		t.Fatalf("tools = %v, want nil", tools)
	}
	if message.Role != "" {
		t.Fatalf("message = %+v, want zero", message)
	}
}

// TestContainMappingPanicDoesNotUnwind proves the panic stops at the mapping
// function. Without this it escapes the provider's push iterator, which cannot
// be resumed, and every later message in the conversation is lost.
func TestContainMappingPanicDoesNotUnwind(t *testing.T) {
	t.Parallel()
	reached := false
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("panic escaped the mapping function: %v", recovered)
			}
		}()
		_, _ = mapPanicking()
		reached = true
	}()
	if !reached {
		t.Fatal("the statement after the mapping call never ran")
	}
}

// TestContainMappingPanicLeavesTheOrdinaryPathAlone pins that the guard changes
// nothing when no panic occurs.
func TestContainMappingPanicLeavesTheOrdinaryPathAlone(t *testing.T) {
	t.Parallel()
	message, include := mapUsable()
	if !include {
		t.Fatal("include = false for a usable record")
	}
	if message.Role != "user" || message.Text != "kept" {
		t.Fatalf("message = %+v, want the mapped role and text", message)
	}
}

// TestContainMappingPanicKeepsMappingAfterOneBadRecord is the behavior the
// guard exists for: one bad record costs one record, and the records after it
// still map. A conversation that still delivers documents is marked satisfied
// by the engine and leaves its needed list, so the daemon stops re-reading it
// on every pass.
func TestContainMappingPanicKeepsMappingAfterOneBadRecord(t *testing.T) {
	t.Parallel()
	records := []func() (Message, bool){mapUsable, mapPanicking, mapUsable}
	kept := make([]Message, 0, len(records))
	for _, mapRecord := range records {
		message, include := mapRecord()
		if !include {
			continue
		}
		kept = append(kept, message)
	}
	if len(kept) != 2 {
		t.Fatalf("kept %d records, want 2: the bad record must cost only itself", len(kept))
	}
}

// TestContainMappingPanicRecordsWhereToLook checks the log carries the panic
// value and the panicking function, which are what point at the defect. A
// contained panic that said nothing would hide a bug rather than isolate it.
func TestContainMappingPanicRecordsWhereToLook(t *testing.T) {
	buffer := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buffer, &slog.HandlerOptions{
		AddSource:   false,
		Level:       slog.LevelDebug,
		ReplaceAttr: nil,
	})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, _ = mapPanicking()

	logs := buffer.String()
	for _, want := range []string{"transcript.mapping_panic", "transcript", "345:344", "mapPanicking"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("logs missing %q: %s", want, logs)
		}
	}
}
