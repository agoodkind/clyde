package codex

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"goodkind.io/clyde/codexwire"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/clock"
	"goodkind.io/gklog/correlation"
)

// prewarmWebsocketTurn issues a generate=false probe on conn, captures
// the response_id as previous_response_id for the real frame, and on
// failure closes the conn and re-dials with auth refresh. The returned
// conn is the connection the caller should use for the real frame;
// when the re-dial fires the original conn is closed inside the helper
// and the caller is responsible for installing a defer on the new conn.
// FallbackToHTTP signals the caller to return ErrWebsocketFallbackToHTTP.
func prewarmWebsocketTurn(
	ctx context.Context,
	conn *websocket.Conn,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
) (*websocket.Conn, WebsocketTransportConfig, ResponseCreateWsRequest, prewarmTelemetry, error) {
	warmupResult, warmupErr := probeWebsocketTurn(ctx, conn, cfg, payload)
	if warmupErr == nil && strings.TrimSpace(warmupResult.ResponseID) != "" {
		payload = WithPreviousResponseID(payload, warmupResult.ResponseID, []codexwire.InputItem{})

		return conn, cfg, payload, prewarmTelemetry{
			PrewarmUsed:      true,
			ConnectionReused: true,
			PrewarmFailed:    false,
			FallbackToHTTP:   false,
		}, nil
	}

	_ = conn.Close()
	newConn, statusCode, refreshedCfg, err := dialResponsesWebsocketWithAuthRefresh(ctx, cfg)
	cfg = refreshedCfg
	if statusCode == http.StatusUpgradeRequired {
		logWebsocketPrepared(
			ctx,
			cfg,
			payload,
			TransportTelemetry{
				FallbackToHTTP:         true,
				WebsocketPrewarmFailed: true,
				RequestID:              "",
				CursorRequestID:        "",
				Correlation: correlation.
					Context{
					TraceID:            "",
					SpanID:             "",
					ParentSpanID:       "",
					RequestID:          "",
					IdentityAttributes: nil,
				},

				Alias:                    "",
				UpstreamModel:            "",
				Transport:                "",
				ServiceTier:              "",
				PromptCacheKey:           "",
				ClientMetadata:           nil,
				InputCount:               0,
				ToolCount:                0,
				NativeShellCount:         0,
				NativeCustomCount:        0,
				FunctionToolCount:        0,
				WebsocketWarmup:          false,
				WebsocketPrewarmUsed:     false,
				WebsocketConnectionReuse: false,
				PreviousResponseID:       "",
				TurnStatePresent:         false,
				ContextWindowError:       false,
			},
		)

		return nil, cfg, payload, prewarmTelemetry{
			PrewarmFailed:    true,
			FallbackToHTTP:   true,
			PrewarmUsed:      false,
			ConnectionReused: false,
		}, nil
	}

	if err != nil {
		return nil, cfg, payload, prewarmTelemetry{
			PrewarmFailed:    true,
			PrewarmUsed:      false,
			ConnectionReused: false,
			FallbackToHTTP:   false,
		}, err
	}

	return newConn, cfg, payload, prewarmTelemetry{
		PrewarmFailed:    true,
		PrewarmUsed:      false,
		ConnectionReused: false,
		FallbackToHTTP:   false,
	}, nil
}

// runWebsocketFreshDial is the legacy path. Dial a fresh websocket,
// optionally warm up, send one frame, close. Preserved so tests and
// non-cache callers do not break. Tagged for removal once all
// callers route through runWebsocketWithCache.
func runWebsocketFreshDial(
	ctx context.Context,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
	emit func(adapterrender.Event) error,
) (RunResult, bool, error) {
	conn, statusCode, refreshedCfg, err := dialResponsesWebsocketWithAuthRefresh(ctx, cfg)
	cfg = refreshedCfg
	if statusCode == http.StatusUpgradeRequired {
		logWebsocketPrepared(
			ctx,
			cfg,
			payload,
			TransportTelemetry{
				FallbackToHTTP:  true,
				RequestID:       "",
				CursorRequestID: "",
				Correlation: correlation.
					Context{
					TraceID:            "",
					SpanID:             "",
					ParentSpanID:       "",
					RequestID:          "",
					IdentityAttributes: nil,
				},

				Alias:                    "",
				UpstreamModel:            "",
				Transport:                "",
				ServiceTier:              "",
				PromptCacheKey:           "",
				ClientMetadata:           nil,
				InputCount:               0,
				ToolCount:                0,
				NativeShellCount:         0,
				NativeCustomCount:        0,
				FunctionToolCount:        0,
				WebsocketWarmup:          false,
				WebsocketPrewarmUsed:     false,
				WebsocketPrewarmFailed:   false,
				WebsocketConnectionReuse: false,
				PreviousResponseID:       "",
				TurnStatePresent:         false,
				ContextWindowError:       false,
			},
		)

		return NewRunResult("stop"), false, ErrWebsocketFallbackToHTTP
	}

	if err != nil {
		return NewRunResult("stop"), false, err
	}

	defer func(c *websocket.Conn) { _ = c.Close() }(conn)

	prewarmUsed := false
	prewarmFailed := false
	connectionReused := false
	if cfg.Prewarm && strings.TrimSpace(payload.PreviousResponseID) == "" {
		newConn, newCfg, newPayload, tel, prewarmErr := prewarmWebsocketTurn(
			ctx,
			conn,
			cfg,
			payload,
		)
		cfg = newCfg
		payload = newPayload
		prewarmUsed = tel.PrewarmUsed
		prewarmFailed = tel.PrewarmFailed
		connectionReused = tel.ConnectionReused
		if tel.FallbackToHTTP {
			return NewRunResult("stop"), false, ErrWebsocketFallbackToHTTP
		}

		if prewarmErr != nil {
			return NewRunResult("stop"), false, prewarmErr
		}

		if newConn != nil && newConn != conn {
			conn = newConn
			defer func(c *websocket.Conn) { _ = c.Close() }(conn)
		}
	}

	logWebsocketPrepared(
		ctx,
		cfg,
		payload,
		websocketFreshDialTelemetry(prewarmUsed, prewarmFailed, connectionReused),
	)

	return writeAndParseWebsocketRequest(ctx, conn, cfg, payload, emit, false)
}

func probeWebsocketTurn(
	ctx context.Context,
	conn *websocket.Conn,
	cfg WebsocketTransportConfig,
	payload ResponseCreateWsRequest,
) (RunResult, error) {
	warmup := WithWarmupGenerateFalse(payload)
	warmup.Tools = []codexwire.ToolSpec{}
	logWebsocketPrepared(
		ctx,
		cfg,
		warmup,
		TransportTelemetry{
			WebsocketWarmup: true,
			RequestID:       "",
			CursorRequestID: "",
			Correlation: correlation.
				Context{
				TraceID:            "",
				SpanID:             "",
				ParentSpanID:       "",
				RequestID:          "",
				IdentityAttributes: nil,
			},

			Alias:                    "",
			UpstreamModel:            "",
			Transport:                "",
			ServiceTier:              "",
			PromptCacheKey:           "",
			ClientMetadata:           nil,
			InputCount:               0,
			ToolCount:                0,
			NativeShellCount:         0,
			NativeCustomCount:        0,
			FunctionToolCount:        0,
			WebsocketPrewarmUsed:     false,
			WebsocketPrewarmFailed:   false,
			WebsocketConnectionReuse: false,
			PreviousResponseID:       "",
			TurnStatePresent:         false,
			FallbackToHTTP:           false,
			ContextWindowError:       false,
		},
	)
	prewarmTimeout := cfg.PrewarmTimeout
	if prewarmTimeout <= 0 {
		prewarmTimeout = defaultWebsocketPrewarmTimeout
	}

	_ = conn.SetReadDeadline(clock.Now().Add(prewarmTimeout))
	warmupResult, _, warmupErr := writeAndParseWebsocketRequest(
		ctx,
		conn,
		cfg,
		warmup,
		func(adapterrender.Event) error {
			return nil
		},
		true,
	)
	_ = conn.SetReadDeadline(time.Time{})

	return warmupResult, warmupErr
}

func websocketFreshDialTelemetry(
	prewarmUsed bool,
	prewarmFailed bool,
	connectionReused bool,
) TransportTelemetry {
	return TransportTelemetry{
		WebsocketPrewarmUsed:     prewarmUsed,
		WebsocketPrewarmFailed:   prewarmFailed,
		WebsocketConnectionReuse: connectionReused,
		RequestID:                "",
		CursorRequestID:          "",
		Correlation: correlation.
			Context{
			TraceID:            "",
			SpanID:             "",
			ParentSpanID:       "",
			RequestID:          "",
			IdentityAttributes: nil,
		},

		Alias:              "",
		UpstreamModel:      "",
		Transport:          "",
		ServiceTier:        "",
		PromptCacheKey:     "",
		ClientMetadata:     nil,
		InputCount:         0,
		ToolCount:          0,
		NativeShellCount:   0,
		NativeCustomCount:  0,
		FunctionToolCount:  0,
		WebsocketWarmup:    false,
		PreviousResponseID: "",
		TurnStatePresent:   false,
		FallbackToHTTP:     false,
		ContextWindowError: false,
	}
}
