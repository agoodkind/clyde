package anthropic

import (
	"context"
	"errors"
	"testing"

	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterprovider "goodkind.io/clyde/internal/adapter/provider"
	adapterresolver "goodkind.io/clyde/internal/adapter/resolver"
)

func TestProviderIDIsAnthropic(t *testing.T) {
	p := NewProvider(adapterprovider.Deps{}, ProviderOptions{})
	if got := p.ID(); got != adapterresolver.ProviderAnthropic {
		t.Errorf("ID() = %q, want %q", got, adapterresolver.ProviderAnthropic)
	}
}

func TestProviderExecuteRequiresPrepare(t *testing.T) {
	p := NewProvider(adapterprovider.Deps{}, ProviderOptions{})
	req := adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderAnthropic,
		Model:    "claude-test",
		Effort:   adapterresolver.EffortMedium,
		OpenAI:   adapteropenai.ChatRequest{Stream: true},
	}
	_, err := p.Execute(context.Background(), req, nil)
	var execErr *ExecuteError
	if !errors.As(err, &execErr) || execErr.Code != "anthropic_prepare_unconfigured" {
		t.Fatalf("Execute() err = %v, want anthropic_prepare_unconfigured", err)
	}
}

func TestProviderExecuteCollectBuildsFinalResponse(t *testing.T) {
	t.Parallel()
	provider := NewProvider(adapterprovider.Deps{}, ProviderOptions{
		Prepare: func(_ context.Context, req adapterresolver.ResolvedRequest, reqID string) (PreparedRequest, error) {
			resolved := req
			return PreparedRequest{
				RequestID: reqID,
				Resolved:  &resolved,
			}, nil
		},
		ExecutePrepared: func(_ context.Context, req PreparedRequest, _ adapterprovider.EventWriter) (adapterprovider.Result, error) {
			resp := &adapteropenai.ChatResponse{
				ID:                req.RequestID,
				Object:            "chat.completion",
				Model:             "claude-test",
				SystemFingerprint: "fp-test",
				Choices: []adapteropenai.ChatChoice{{
					Index: 0,
					Message: adapteropenai.ChatMessage{
						Role:    "assistant",
						Content: []byte(`"ok"`),
					},
					FinishReason: "stop",
				}},
				Usage: &adapteropenai.Usage{
					PromptTokens:     3,
					CompletionTokens: 2,
					TotalTokens:      5,
				},
			}
			return adapterprovider.Result{
				FinalResponse:     resp,
				Usage:             *resp.Usage,
				FinishReason:      "stop",
				SystemFingerprint: "fp-test",
			}, nil
		},
	})
	req := adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderAnthropic,
		Model:    "claude-test",
		Effort:   adapterresolver.EffortMedium,
		OpenAI: adapteropenai.ChatRequest{
			Model: "claude-test",
		},
	}

	result, err := provider.Execute(WithRequestID(context.Background(), "req-provider"), req, nil)
	if err != nil {
		t.Fatalf("Execute() err = %v", err)
	}
	if result.FinalResponse == nil {
		t.Fatalf("FinalResponse is nil")
	}
	if result.FinalResponse.ID != "req-provider" {
		t.Fatalf("response id = %q want req-provider", result.FinalResponse.ID)
	}
	if got := result.FinalResponse.Choices[0].Message.Content; string(got) != `"ok"` {
		t.Fatalf("content = %s want %q", got, `"ok"`)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q want stop", result.FinishReason)
	}
	if result.Usage.PromptTokens != 3 || result.Usage.CompletionTokens != 2 {
		t.Fatalf("usage = %+v want prompt=3 completion=2", result.Usage)
	}
}

func TestProviderExecuteStreamUsesStreamCallback(t *testing.T) {
	t.Parallel()

	called := false
	provider := NewProvider(adapterprovider.Deps{}, ProviderOptions{
		Prepare: func(_ context.Context, req adapterresolver.ResolvedRequest, reqID string) (PreparedRequest, error) {
			resolved := req
			return PreparedRequest{
				RequestID: reqID,
				Request:   Request{Stream: req.OpenAI.Stream},
				Resolved:  &resolved,
			}, nil
		},
		ExecutePrepared: func(_ context.Context, req PreparedRequest, _ adapterprovider.EventWriter) (adapterprovider.Result, error) {
			called = true
			if req.RequestID != "req-stream" {
				t.Fatalf("reqID = %q want req-stream", req.RequestID)
			}
			return adapterprovider.Result{FinishReason: "stop"}, nil
		},
	})
	req := adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderAnthropic,
		Model:    "claude-test",
		Effort:   adapterresolver.EffortMedium,
		OpenAI:   adapteropenai.ChatRequest{Stream: true},
	}

	result, err := provider.Execute(WithRequestID(context.Background(), "req-stream"), req, nil)
	if err != nil {
		t.Fatalf("Execute() err = %v", err)
	}
	if !called {
		t.Fatalf("stream callback was not called")
	}
	if result.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q want stop", result.FinishReason)
	}
}

func TestProviderExecutePreparedUsesPreparedCallback(t *testing.T) {
	t.Parallel()

	called := false
	provider := NewProvider(adapterprovider.Deps{}, ProviderOptions{
		ExecutePrepared: func(_ context.Context, req PreparedRequest, _ adapterprovider.EventWriter) (adapterprovider.Result, error) {
			called = true
			if req.RequestID != "req-native" {
				t.Fatalf("reqID = %q want req-native", req.RequestID)
			}
			return adapterprovider.Result{FinishReason: "stop"}, nil
		},
	})

	result, err := provider.ExecutePrepared(context.Background(), PreparedRequest{
		RequestID: "req-native",
		Request:   Request{Model: "claude-test", Stream: true},
		Resolved: &adapterresolver.ResolvedRequest{
			Provider: adapterresolver.ProviderAnthropic,
			Model:    "claude-test",
			Alias:    "claude-test",
		},
	}, nil)
	if err != nil {
		t.Fatalf("ExecutePrepared() err = %v", err)
	}
	if !called {
		t.Fatalf("prepared callback was not called")
	}
	if result.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q want stop", result.FinishReason)
	}
}

func TestProviderPrepareUsesConfiguredPreparation(t *testing.T) {
	t.Parallel()

	called := false
	provider := NewProvider(adapterprovider.Deps{}, ProviderOptions{
		Prepare: func(_ context.Context, req adapterresolver.ResolvedRequest, requestID string) (PreparedRequest, error) {
			called = true
			if requestID != "req-responses" {
				t.Fatalf("request id = %q, want req-responses", requestID)
			}
			resolved := req
			return PreparedRequest{RequestID: requestID, Resolved: &resolved}, nil
		},
	})

	prepared, err := provider.Prepare(WithRequestID(context.Background(), "req-responses"), adapterresolver.ResolvedRequest{
		Provider: adapterresolver.ProviderAnthropic,
		Model:    "claude-test",
	})
	if err != nil {
		t.Fatalf("Prepare() err = %v", err)
	}
	if !called {
		t.Fatal("preparation callback was not called")
	}
	if prepared.RequestID != "req-responses" {
		t.Fatalf("prepared request id = %q, want req-responses", prepared.RequestID)
	}
}

// satisfiesProviderInterface fails to compile if Provider does not
// satisfy the upstream-agnostic adapter/provider.Provider contract.
// It is the cheapest available guarantee that a future change to the
// Provider type does not silently regress its registry compatibility.
func TestProviderSatisfiesInterface(t *testing.T) {
	var _ adapterprovider.Provider = (*Provider)(nil)
}
