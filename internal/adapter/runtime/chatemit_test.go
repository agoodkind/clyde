package runtime

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"goodkind.io/clyde/internal/adapter/anthropic"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
)

func TestNoticeForResponseHeadersSuccess(t *testing.T) {
	t.Parallel()
	resp := ChatResponse{
		Choices: []ChatChoice{{
			Index: 0,
			Message: ChatMessage{
				Content: json.RawMessage(`"hello"`),
			},
		}},
	}
	notice := &anthropic.Notice{
		Kind:     anthropic.NoticeKindOverageWarning,
		Text:     "notice text",
		ResetsAt: time.Unix(1, 0),
	}
	unclaimCalled := false
	encoded, ok := noticeSentinelChunkForTest()
	if !ok {
		t.Fatalf("could not build sentinel")
	}
	updated, injected := NoticeForResponseHeaders(resp, notice, func(kind string, resetsAt time.Time) {
		unclaimCalled = true
	}, json.Marshal)
	if injected {
		var got string
		if err := json.Unmarshal(updated.Choices[0].Message.Content, &got); err != nil {
			t.Fatalf("unmarshal message content: %v", err)
		}
		if got != encoded {
			t.Fatalf("expected %q, got %q", encoded, got)
		}
	}
	if !injected {
		t.Fatalf("expected notice to be injected")
	}
	if unclaimCalled {
		t.Fatalf("did not expect unclaim on success")
	}
}

func TestNoticeForResponseHeadersInvalidMessageContentUnclaims(t *testing.T) {
	t.Parallel()
	resp := ChatResponse{
		Choices: []ChatChoice{{
			Index: 0,
			Message: ChatMessage{
				Content: json.RawMessage(`{bad`),
			},
		}},
	}
	unclaimCalled := false
	updated, injected := NoticeForResponseHeaders(resp, &anthropic.Notice{Kind: "x", Text: "note", ResetsAt: time.Unix(1, 0)}, func(kind string, resetsAt time.Time) {
		unclaimCalled = true
	}, json.Marshal)
	if injected {
		t.Fatalf("did not expect notice injection")
	}
	if updated.Choices[0].Message.Content == nil {
		t.Fatalf("expected original content preserved")
	}
	if !unclaimCalled {
		t.Fatalf("expected unclaim on invalid content")
	}
}

func TestNoticeForResponseHeadersNilNotice(t *testing.T) {
	t.Parallel()
	resp := ChatResponse{
		Choices: []ChatChoice{{
			Index: 0,
			Message: ChatMessage{
				Content: json.RawMessage(`"hello"`),
			},
		}},
	}
	updated, injected := NoticeForResponseHeaders(resp, nil, func(kind string, resetsAt time.Time) {
		t.Fatalf("should not unclaim when notice nil")
	}, json.Marshal)
	if injected {
		t.Fatalf("expected no injection")
	}
	if string(updated.Choices[0].Message.Content) != `"hello"` {
		t.Fatalf("content changed unexpectedly: %q", updated.Choices[0].Message.Content)
	}
}

func TestNoticeForStreamHeadersInjectsAndSkipsOnError(t *testing.T) {
	t.Parallel()
	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "rejected")
	h.Set("anthropic-ratelimit-unified-overage-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-overage-reset", "1735689600")
	h.Set("anthropic-ratelimit-unified-representative-claim", "messages")
	calls := 0
	_, err := NoticeForStreamHeaders(
		"req-1",
		"model",
		h,
		true,
		[]float64{75, 95},
		func(chunk adapteropenai.StreamChunk) error {
			calls++
			return nil
		},
		func(kind string, resetsAt time.Time) bool { return true },
		func(kind string, resetsAt time.Time) {},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 emitted notice chunk, got %d", calls)
	}

	calls = 0
	unclaimed := false
	_, err = NoticeForStreamHeaders(
		"req-1",
		"model",
		h,
		true,
		[]float64{75, 95},
		func(chunk adapteropenai.StreamChunk) error {
			calls++
			return errors.New("emit failed")
		},
		func(kind string, resetsAt time.Time) bool { return true },
		func(kind string, resetsAt time.Time) { unclaimed = true },
	)
	if err == nil {
		t.Fatalf("expected emit error to be returned")
	}
	if calls != 1 {
		t.Fatalf("expected emit to be attempted once, got %d", calls)
	}
	if !unclaimed {
		t.Fatalf("expected unclaim when emit fails")
	}
}

func TestEscalateOrWrite(t *testing.T) {
	t.Parallel()
	escalateErr := errors.New("upstream")
	wasWritten := false
	got := EscalateOrWrite(
		escalateErr,
		true,
		func(status int, code, msg string) error {
			wasWritten = true
			return nil
		},
		500,
		"code",
		"msg",
	)
	if !errors.Is(got, escalateErr) {
		t.Fatalf("expected escalate return, got %v", got)
	}
	if wasWritten {
		t.Fatalf("did not expect write path on escalate")
	}

	var writeStatus int
	var writeCode, writeMsg string
	wasWritten = false
	err := EscalateOrWrite(
		errors.New("upstream"),
		false,
		func(status int, code, msg string) error {
			wasWritten = true
			writeStatus = status
			writeCode = code
			writeMsg = msg
			return nil
		},
		400,
		"bad_request",
		"bad message",
	)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !wasWritten {
		t.Fatalf("expected write path")
	}
	if writeStatus != 400 || writeCode != "bad_request" || writeMsg != "bad message" {
		t.Fatalf("unexpected write args: %d %q %q", writeStatus, writeCode, writeMsg)
	}
}

func noticeSentinelChunkForTest() (string, bool) {
	text := noticeSentinelText("notice text")
	if text == "" {
		return "", false
	}
	return text + "hello", true
}
