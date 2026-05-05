package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	adaptercodex "goodkind.io/clyde/internal/adapter/codex"
)

func TestParseReasoningEffortAllowsLiveCodexLevels(t *testing.T) {
	for _, raw := range []string{"", "low", "medium", "high", "xhigh"} {
		if _, err := ParseReasoningEffort(raw); err != nil {
			t.Fatalf("ParseReasoningEffort(%q) returned error: %v", raw, err)
		}
	}
	got, err := ParseReasoningEffort("xhigh")
	if err != nil {
		t.Fatalf("ParseReasoningEffort xhigh: %v", err)
	}
	if got != adaptercodex.ReasoningEffortXHigh {
		t.Fatalf("xhigh parsed as %q", got)
	}
}

func TestParseReasoningEffortRejectsNonLiveCodexLevels(t *testing.T) {
	for _, raw := range []string{"none", "minimal", "max", "HIGH"} {
		if _, err := ParseReasoningEffort(raw); err == nil {
			t.Fatalf("ParseReasoningEffort(%q) returned nil error", raw)
		}
	}
}

func TestAppServerRuntimeStartSendStream(t *testing.T) {
	runtime := newFakeAppServerRuntime(t, "stream")
	defer func() { _ = runtime.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := runtime.Start(ctx, LiveStartRequest{
		WorkDir:     t.TempDir(),
		Model:       "gpt-5.1-codex",
		SessionName: "codex-live-test",
	})
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if session.ThreadID != "thread-1" {
		t.Fatalf("ThreadID=%q, want thread-1", session.ThreadID)
	}

	turn, err := runtime.Send(ctx, LiveSendRequest{
		ThreadID: session.ThreadID,
		Text:     "hello",
	})
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if turn.TurnID != "turn-1" {
		t.Fatalf("TurnID=%q, want turn-1", turn.TurnID)
	}

	events, err := runtime.Stream(ctx, LiveStreamRequest{
		ThreadID: session.ThreadID,
		TurnID:   turn.TurnID,
	})
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	var got []LiveEvent
	for event := range events {
		if event.Err != nil {
			t.Fatalf("stream event error: %v", event.Err)
		}
		got = append(got, event)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %#v", len(got), got)
	}
	if got[0].Kind != LiveEventDelta || got[0].Delta != "hello from codex" {
		t.Fatalf("first event=%#v, want delta", got[0])
	}
	if got[1].Kind != LiveEventCompleted || got[1].Status != LiveTurnStatusCompleted {
		t.Fatalf("second event=%#v, want completed", got[1])
	}
}

func TestAppServerRuntimeAttachAndStop(t *testing.T) {
	runtime := newFakeAppServerRuntime(t, "stop")
	defer func() { _ = runtime.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := runtime.Attach(ctx, LiveAttachRequest{ThreadID: "thread-existing"})
	if err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	if session.ThreadID != "thread-existing" {
		t.Fatalf("ThreadID=%q, want thread-existing", session.ThreadID)
	}
	if err := runtime.Stop(ctx, LiveStopRequest{ThreadID: session.ThreadID, TurnID: "turn-stop"}); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
}

func newFakeAppServerRuntime(t *testing.T, mode string) *AppServerRuntime {
	t.Helper()
	return NewAppServerRuntime(AppServerRuntimeOptions{
		Command: []string{
			os.Args[0],
			"-test.run=TestCodexAppServerFakeProcess",
			"--",
			mode,
		},
		Env: []string{"CLYDE_CODEX_FAKE_APPSERVER=1"},
	})
}

func TestCodexAppServerFakeProcess(t *testing.T) {
	if os.Getenv("CLYDE_CODEX_FAKE_APPSERVER") != "1" {
		return
	}
	mode := "stream"
	if len(os.Args) > 0 {
		mode = os.Args[len(os.Args)-1]
	}
	if err := runFakeCodexAppServer(mode); err != nil {
		fmt.Fprintf(os.Stderr, "fake app-server failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runFakeCodexAppServer(mode string) error {
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req fakeJSONRPCMessage
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return err
		}
		switch req.Method {
		case "initialize":
			if err := enc.Encode(fakeJSONRPCResponse{
				ID: req.ID,
				Result: fakeInitializeResult{
					UserAgent: "fake-codex",
				},
			}); err != nil {
				return err
			}
		case "initialized":
		case "thread/start":
			if err := enc.Encode(fakeJSONRPCResponse{
				ID: req.ID,
				Result: fakeThreadResult{
					Thread: fakeThread{ID: "thread-1"},
					CWD:    "/tmp",
					Model:  "gpt-5.1-codex",
				},
			}); err != nil {
				return err
			}
		case "thread/name/set":
			if err := enc.Encode(fakeJSONRPCResponse{
				ID:     req.ID,
				Result: fakeStatusResult{Status: "ok"},
			}); err != nil {
				return err
			}
		case "thread/resume":
			if err := enc.Encode(fakeJSONRPCResponse{
				ID: req.ID,
				Result: fakeThreadResult{
					Thread: fakeThread{ID: "thread-existing"},
					CWD:    "/tmp",
					Model:  "gpt-5.1-codex",
				},
			}); err != nil {
				return err
			}
		case "turn/start":
			if err := enc.Encode(fakeJSONRPCResponse{
				ID: req.ID,
				Result: fakeTurnResult{
					Turn: fakeTurn{ID: "turn-1", Status: "inProgress"},
				},
			}); err != nil {
				return err
			}
			if mode == "stream" {
				if err := enc.Encode(fakeJSONRPCNotification{
					Method: "item/agentMessage/delta",
					Params: fakeDeltaNotification{
						ThreadID: "thread-1",
						TurnID:   "turn-1",
						ItemID:   "item-1",
						Delta:    "hello from codex",
					},
				}); err != nil {
					return err
				}
				if err := enc.Encode(fakeJSONRPCNotification{
					Method: "turn/completed",
					Params: fakeTurnCompletedNotification{
						ThreadID: "thread-1",
						Turn:     fakeTurn{ID: "turn-1", Status: "completed"},
					},
				}); err != nil {
					return err
				}
			}
		case "turn/interrupt":
			if err := enc.Encode(fakeJSONRPCResponse{
				ID:     req.ID,
				Result: fakeStatusResult{Status: "interrupted"},
			}); err != nil {
				return err
			}
		default:
			if err := enc.Encode(fakeJSONRPCResponse{
				ID: req.ID,
				Error: &fakeJSONRPCError{
					Code:    -32601,
					Message: "unknown method",
				},
			}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}

type fakeJSONRPCMessage struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

type fakeJSONRPCResponse struct {
	ID     string            `json:"id"`
	Result fakeResultObject  `json:"result,omitempty"`
	Error  *fakeJSONRPCError `json:"error,omitempty"`
}

type fakeJSONRPCNotification struct {
	Method string           `json:"method"`
	Params fakeParamsObject `json:"params"`
}

type fakeResultObject interface {
	fakeResultObject()
}

type fakeParamsObject interface {
	fakeParamsObject()
}

type fakeJSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type fakeInitializeResult struct {
	UserAgent string `json:"userAgent"`
}

func (fakeInitializeResult) fakeResultObject() {}

type fakeThreadResult struct {
	Thread fakeThread `json:"thread"`
	CWD    string     `json:"cwd"`
	Model  string     `json:"model"`
}

func (fakeThreadResult) fakeResultObject() {}

type fakeTurnResult struct {
	Turn fakeTurn `json:"turn"`
}

func (fakeTurnResult) fakeResultObject() {}

type fakeStatusResult struct {
	Status string `json:"status"`
}

func (fakeStatusResult) fakeResultObject() {}

type fakeThread struct {
	ID string `json:"id"`
}

type fakeTurn struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type fakeDeltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Delta    string `json:"delta"`
}

func (fakeDeltaNotification) fakeParamsObject() {}

type fakeTurnCompletedNotification struct {
	ThreadID string   `json:"threadId"`
	Turn     fakeTurn `json:"turn"`
}

func (fakeTurnCompletedNotification) fakeParamsObject() {}
