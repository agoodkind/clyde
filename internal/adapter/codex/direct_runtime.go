package codex

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	adaptermodel "goodkind.io/clyde/internal/adapter/model"
	adapteropenai "goodkind.io/clyde/internal/adapter/openai"
	adapterrender "goodkind.io/clyde/internal/adapter/render"
	"goodkind.io/clyde/internal/correlation"
)

type DirectConfig struct {
	HTTPClient       *http.Client
	BaseURL          string
	WebsocketEnabled bool
	WebsocketURL     string
	Token            string
	AccountID        string
	RequestID        string
	CursorRequestID  string
	Correlation      correlation.Context
	// WorkspacePath is the absolute path to the Cursor-active
	// workspace, used to populate the `workspaces` block in
	// `x-codex-turn-metadata`. Empty when Cursor did not supply a
	// workspace.
	WorkspacePath string
	// WorkspaceProbe runs the small git probe (origin / HEAD /
	// has_changes). Optional. When nil, the workspace block is
	// emitted with the path only and no git fields.
	WorkspaceProbe *WorkspaceProbe
	// SessionCache enables persistent ws session reuse with chained
	// previous_response_id and delta input. Constructed once per
	// Provider. Required: RunDirect refuses to run without it.
	SessionCache     *WebsocketSessionCache
	Log              *slog.Logger
	BodyLog          BodyLogConfig
	BodyLogProvider  BodyLogConfigProvider
	FileLog          FileLogRotationConfig
	ReasoningSummary string
	// InboundThinkingMaterialization picks how round-tripped synthetic
	// thinking envelopes on assistant content are shaped before forwarding
	// upstream. Empty string falls through to the Codex default (drop)
	// inside [BuildRequestWithConfig] / [SanitizeForUpstreamCacheWithStrategy].
	InboundThinkingMaterialization adapterrender.MaterializationStrategy
	// WireCaptureMode controls the optional per-frame log of inbound
	// websocket bodies. Off (default) is safe; the other modes route to
	// adapter.providers.codex.wire_capture for short-retention diagnostics.
	WireCaptureMode WireCaptureMode
	// ReasoningStore receives encrypted_content blobs scraped from
	// response.output_item.done reasoning items. nil disables capture
	// (Phase 3 store off, no store dependency, etc.). Phase 4 writes;
	// Phase 6 reads.
	ReasoningStore ReasoningBlobStore
	// RoundTripEncrypted gates the writer. RoundTripEncryptedRoundTrip
	// (the codex-rs default) persists; RoundTripEncryptedDrop skips.
	// Empty resolves to RoundTripEncryptedRoundTrip per codex-rs.
	RoundTripEncrypted RoundTripEncrypted
}

// ReasoningBlobStore is the narrow Put-only contract DirectConfig needs from
// the encrypted_content cache. It mirrors the subset of
// [reasoningstore.Store] that Phase 4 calls so tests can pass a fake without
// depending on the on-disk implementation.
type ReasoningBlobStore interface {
	Put(ctx context.Context, blob ReasoningBlob) error
}

// ReasoningBlob is the value DirectConfig.ReasoningStore.Put receives. It is
// shape-compatible with reasoningstore.EncryptedBlob; the codex package owns
// its own typed surface so RunDirect does not need to import the storage
// package directly.
type ReasoningBlob struct {
	ChatKey   string
	ItemID    string
	Encrypted string
	CreatedAt int64
}

// RoundTripEncrypted is the closed enum the codex transport honors when the
// dispatcher passes the encrypted_content round-trip lever. Mirrors
// config.CodexRoundTripEncrypted value-for-value so the dispatcher does a
// typed string conversion at the boundary without an import edge.
type RoundTripEncrypted string

// Codex round-trip encrypted_content modes. RoundTripEncryptedRoundTrip is
// the documented codex-rs default; empty resolves to it.
const (
	RoundTripEncryptedRoundTrip RoundTripEncrypted = "round_trip"
	RoundTripEncryptedDrop      RoundTripEncrypted = "drop"
)

// WireCaptureMode is the closed enum the codex transport honors when the
// dispatcher passes a per-provider wire-capture lever. Mirrors
// config.CodexWireCaptureMode value-for-value so the dispatcher does a
// typed string conversion at the boundary without an import edge.
type WireCaptureMode string

// Codex wire-capture modes. WireCaptureOff is the safe default and matches
// an empty configured value.
const (
	WireCaptureOff           WireCaptureMode = "off"
	WireCaptureSummaryOnly   WireCaptureMode = "summary_only"
	WireCaptureReasoningOnly WireCaptureMode = "reasoning_only"
	WireCaptureFull          WireCaptureMode = "full"
)

func RunDirect(
	ctx context.Context,
	cfg DirectConfig,
	req adapteropenai.ChatRequest,
	model adaptermodel.ResolvedModel,
	effort string,
	emit func(adapterrender.Event) error,
) (RunResult, error) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	ConfigureCodexFileLogger(cfg.FileLog)
	if !cfg.WebsocketEnabled {
		return NewRunResult("stop"), errCodexWebsocketDisabled
	}
	transportPayload := BuildRequestWithConfig(req, model, effort, RequestBuilderConfig{
		ReasoningSummary:               cfg.ReasoningSummary,
		InboundThinkingMaterialization: cfg.InboundThinkingMaterialization,
	})
	// WARNING: this is the websocket session identity, not the
	// prompt_cache_key. Codex uses prompt_cache_key for upstream cache
	// partitioning, but websocket previous_response_id reuse is only safe
	// when keyed by a real Cursor/Codex conversation/thread id. Content or
	// account-derived cache keys can be shared by unrelated fresh chats.
	conversationID := strings.TrimSpace(transportPayload.WebsocketSessionKey)
	if conversationID != "" {
		installationID, _ := LoadInstallationID()
		turnMeta := NewTurnMetadata(conversationID, "")
		if ws := strings.TrimSpace(cfg.WorkspacePath); ws != "" {
			entry := TurnMetadataWorkspace{}
			if cfg.WorkspaceProbe != nil {
				entry = cfg.WorkspaceProbe.Probe(ws)
			}
			turnMeta = turnMeta.WithWorkspace(ws, entry)
		}
		turnMetaJSON, _ := turnMeta.MarshalCompact()
		transportPayload.ClientMetadata = ClientMetadataWithTurn(installationID, CodexWindowID(conversationID), turnMetaJSON)
	}
	wsReq := ResponseCreateRequestFromHTTP(transportPayload)
	wsCfg := WebsocketTransportConfig{
		URL:             cfg.WebsocketURL,
		Token:           cfg.Token,
		AccountID:       cfg.AccountID,
		RequestID:       cfg.RequestID,
		CursorRequestID: cfg.CursorRequestID,
		Correlation:     cfg.Correlation,
		Alias:           model.Alias,
		ConversationID:  conversationID,
		TurnState:       NewTurnState(),
		TurnMetadata:    transportPayload.ClientMetadata[CodexTurnMetadataHeader],
		BodyLog:         cfg.BodyLog,
		BodyLogProvider: cfg.BodyLogProvider,
		SessionCache:    cfg.SessionCache,
		Log:             cfg.Log,
		WireCaptureMode: cfg.WireCaptureMode,
	}
	result, runErr := RunWebsocketTransportEvents(ctx, wsCfg, wsReq, emit)
	captureReasoningBlobs(ctx, cfg, result)
	return result, runErr
}

// captureReasoningBlobs walks result.OutputItems for response.output_item.done
// reasoning items that carry encrypted_content and persists them to
// cfg.ReasoningStore. Skipped silently when the store is nil, the chat key
// is empty (no per-chat partition), or the round-trip lever is set to drop.
//
// The persistence is best-effort. Any error is logged at WARN and the request
// completes normally; the upstream turn already succeeded.
func captureReasoningBlobs(ctx context.Context, cfg DirectConfig, result RunResult) {
	if cfg.ReasoningStore == nil {
		return
	}
	chatKey := strings.TrimSpace(cfg.Correlation.ChatKey)
	if chatKey == "" {
		return
	}
	mode := cfg.RoundTripEncrypted
	if mode == "" {
		mode = RoundTripEncryptedRoundTrip
	}
	if mode != RoundTripEncryptedRoundTrip {
		return
	}
	now := time.Now().UnixNano()
	for _, item := range result.OutputItems {
		if !reasoningItemHasEncryptedContent(item) {
			continue
		}
		blob := ReasoningBlob{
			ChatKey:   chatKey,
			ItemID:    strings.TrimSpace(stringField(item, "id")),
			Encrypted: strings.TrimSpace(stringField(item, "encrypted_content")),
			CreatedAt: now,
		}
		if blob.ItemID == "" || blob.Encrypted == "" {
			continue
		}
		if err := cfg.ReasoningStore.Put(ctx, blob); err != nil {
			cfg.Log.WarnContext(ctx, "adapter.codex.reasoningstore.put_failed",
				"component", "adapter",
				"subcomponent", "codex",
				"chat_key", blob.ChatKey,
				"item_id", blob.ItemID,
				"err", err.Error(),
			)
		}
	}
}

// reasoningItemHasEncryptedContent reports whether the upstream output item
// is a reasoning item carrying a non-empty encrypted_content field. Other
// item types (message, function_call, etc.) are ignored.
func reasoningItemHasEncryptedContent(item map[string]any) bool {
	if item == nil {
		return false
	}
	if strings.TrimSpace(stringField(item, "type")) != "reasoning" {
		return false
	}
	return strings.TrimSpace(stringField(item, "encrypted_content")) != ""
}

// stringField pulls a string-typed map value or returns empty when missing
// or wrong-typed. Used for the opaque-shape OutputItems map walk.
func stringField(item map[string]any, key string) string {
	if item == nil {
		return ""
	}
	raw, ok := item[key]
	if !ok {
		return ""
	}
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	return s
}

var errCodexWebsocketDisabled = errors.New("codex websocket transport is disabled but no HTTPS fallback exists")
