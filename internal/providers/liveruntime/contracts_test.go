package liveruntime

import (
	"context"
	"errors"
	"testing"
	"time"

	"goodkind.io/clyde/internal/session"
)

func TestRegistryReturnsRuntimeForNormalizedProvider(t *testing.T) {
	resetRegistryForTest(t)
	ctx := context.Background()
	provider := session.ProviderID("fake-live")
	now := time.Unix(1710000000, 123)
	runtime := &fakeLiveRuntime{
		startResponse: &LiveSession{
			Provider:       provider,
			SessionName:    "fake-session",
			SessionID:      "fake-session-id",
			Status:         "running",
			Basedir:        "/tmp/fake",
			URL:            "http://localhost/fake",
			StartedAt:      now,
			SupportsSend:   true,
			SupportsStream: true,
			SupportsStop:   true,
		},
		attachResponse: &LiveSession{
			Provider:    provider,
			SessionName: "fake-session",
			SessionID:   "fake-session-id",
			Status:      "attached",
		},
		sendResponse: &LiveTurn{
			SessionID: "fake-session-id",
			TurnID:    "fake-turn",
			Status:    "accepted",
		},
	}
	streamResponse := make(chan LiveEvent, 1)
	streamResponse <- LiveEvent{
		SessionID: "fake-session-id",
		Kind:      "delta",
		Role:      "assistant",
		Text:      "fake text",
		Timestamp: now,
	}
	close(streamResponse)
	runtime.streamResponse = streamResponse

	var gotDeps Deps
	Register(provider, func(deps Deps) LiveRuntime {
		gotDeps = deps
		return runtime
	})

	got, ok := Get(provider, Deps{Now: func() time.Time { return now }})
	if !ok {
		t.Fatalf("expected registered runtime")
	}
	if got != runtime {
		t.Fatalf("runtime = %p, want %p", got, runtime)
	}
	if gotDeps.Now == nil {
		t.Fatalf("expected factory to receive deps")
	}

	startRequest := StartRequest{
		Provider:    provider,
		SessionName: "fake-session",
		SessionID:   "fake-session-id",
		Basedir:     "/tmp/fake",
		Model:       "fake-model",
		Effort:      EffortLevel("high"),
		Incognito:   true,
		PeerAddr:    "localhost:1234",
	}
	startSession, err := got.Start(ctx, startRequest)
	if err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if startSession != runtime.startResponse {
		t.Fatalf("Start response = %p, want %p", startSession, runtime.startResponse)
	}
	if runtime.startRequest != startRequest {
		t.Fatalf("Start request = %#v, want %#v", runtime.startRequest, startRequest)
	}

	attachRequest := AttachRequest{
		Provider:    provider,
		SessionName: "fake-session",
		SessionID:   "fake-session-id",
		Basedir:     "/tmp/fake",
		Model:       "fake-model",
		Status:      "running",
		Effort:      EffortLevel("medium"),
		Incognito:   true,
		Reason:      "reload",
	}
	attachSession, err := got.Attach(ctx, attachRequest)
	if err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	if attachSession != runtime.attachResponse {
		t.Fatalf("Attach response = %p, want %p", attachSession, runtime.attachResponse)
	}
	if runtime.attachRequest != attachRequest {
		t.Fatalf("Attach request = %#v, want %#v", runtime.attachRequest, attachRequest)
	}

	stopRequest := StopRequest{
		Provider:    provider,
		SessionName: "fake-session",
		SessionID:   "fake-session-id",
		TurnID:      "fake-turn",
		Reason:      "test",
	}
	if err := got.Stop(ctx, stopRequest); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if runtime.stopRequest != stopRequest {
		t.Fatalf("Stop request = %#v, want %#v", runtime.stopRequest, stopRequest)
	}

	sendRequest := SendRequest{
		Provider:  provider,
		SessionID: "fake-session-id",
		Text:      "hello",
		Basedir:   "/tmp/fake",
		Model:     "fake-model",
		Effort:    EffortLevel("low"),
	}
	turn, err := got.Send(ctx, sendRequest)
	if err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	if turn != runtime.sendResponse {
		t.Fatalf("Send response = %p, want %p", turn, runtime.sendResponse)
	}
	if runtime.sendRequest != sendRequest {
		t.Fatalf("Send request = %#v, want %#v", runtime.sendRequest, sendRequest)
	}

	streamRequest := StreamRequest{
		Provider:    provider,
		SessionName: "fake-session",
		SessionID:   "fake-session-id",
		TurnID:      "fake-turn",
	}
	events, err := got.Stream(ctx, streamRequest)
	if err != nil {
		t.Fatalf("Stream returned error: %v", err)
	}
	if runtime.streamRequest != streamRequest {
		t.Fatalf("Stream request = %#v, want %#v", runtime.streamRequest, streamRequest)
	}
	event, ok := <-events
	if !ok {
		t.Fatalf("expected stream event")
	}
	if event.Text != "fake text" {
		t.Fatalf("stream event text = %q, want %q", event.Text, "fake text")
	}
}

func TestRegistryKeepsOriginalFactoryOnDuplicateRegistration(t *testing.T) {
	resetRegistryForTest(t)
	provider := session.ProviderID("fake-live")
	original := &fakeLiveRuntime{}
	replacement := &fakeLiveRuntime{}
	Register(provider, func(Deps) LiveRuntime {
		return original
	})
	Register(provider, func(Deps) LiveRuntime {
		return replacement
	})
	got, ok := Get(provider, Deps{})
	if !ok {
		t.Fatalf("expected registered runtime")
	}
	if got != original {
		t.Fatalf("duplicate registration replaced runtime: got %p, want %p", got, original)
	}
}

func resetRegistryForTest(t *testing.T) {
	t.Helper()
	registry.mu.Lock()
	previous := registry.factories
	registry.factories = make(map[session.ProviderID]Factory)
	registry.mu.Unlock()
	t.Cleanup(func() {
		registry.mu.Lock()
		registry.factories = previous
		registry.mu.Unlock()
	})
}

type fakeLiveRuntime struct {
	startRequest  StartRequest
	attachRequest AttachRequest
	stopRequest   StopRequest
	sendRequest   SendRequest
	streamRequest StreamRequest

	startResponse  *LiveSession
	attachResponse *LiveSession
	sendResponse   *LiveTurn
	streamResponse <-chan LiveEvent
}

func (r *fakeLiveRuntime) Start(_ context.Context, req StartRequest) (*LiveSession, error) {
	r.startRequest = req
	return r.startResponse, nil
}

func (r *fakeLiveRuntime) Attach(_ context.Context, req AttachRequest) (*LiveSession, error) {
	r.attachRequest = req
	return r.attachResponse, nil
}

func (r *fakeLiveRuntime) Stop(_ context.Context, req StopRequest) error {
	r.stopRequest = req
	return nil
}

func (r *fakeLiveRuntime) Send(_ context.Context, req SendRequest) (*LiveTurn, error) {
	r.sendRequest = req
	return r.sendResponse, nil
}

func (r *fakeLiveRuntime) Stream(_ context.Context, req StreamRequest) (<-chan LiveEvent, error) {
	r.streamRequest = req
	if r.streamResponse == nil {
		return nil, errors.New("missing stream response")
	}
	return r.streamResponse, nil
}
