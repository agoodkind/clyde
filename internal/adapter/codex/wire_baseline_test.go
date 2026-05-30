package codex

import (
	"errors"
	"path/filepath"
	"testing"

	"goodkind.io/clyde/internal/mitm"
)

// learnedCodexBaselineValues are the verbatim codex-cli identity values
// the test baseline carries. They deliberately differ from the
// compiled-in constants (CodexOriginatorValue, UserAgent(),
// responsesWebsocketsV2BetaHeaderValue) so a test can prove the egress
// used the baseline rather than the fallback.
const (
	learnedOriginator   = "codex_cli_rs"
	learnedOpenAIBeta   = "responses_websockets=2099-12-31"
	learnedUserAgent    = "codex_cli_rs/9.9.9 (Mac OS Unknown; arm64) testterm"
	learnedBetaFeatures = "feature-a,feature-b"
	learnedAttestation  = "att-token-learned-123"
)

// writeTestCodexWireBaseline writes a minimal but realistic v2 MITM
// baseline carrying one codex-cli flavor to a temp dir and returns its
// path. It uses the real [mitm.WriteSnapshotV2TOML] writer so the load
// path under test matches production exactly.
func writeTestCodexWireBaseline(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	snap := mitm.SnapshotV2{
		Upstream: mitm.V2Upstream{
			Name:        "codex-cli",
			Version:     "",
			CapturedAt:  "2026-05-29T00:00:00Z",
			RecordCount: 2,
		},
		Flavors: []mitm.FlavorShape{testCodexFlavorShape()},
	}
	out, err := mitm.WriteSnapshotV2TOML(snap, dir)
	if err != nil {
		t.Fatalf("WriteSnapshotV2TOML: %v", err)
	}
	if filepath.Base(out) != "reference-v2.toml" {
		t.Fatalf("unexpected baseline filename %q", out)
	}
	return out
}

func testCodexFlavorShape() mitm.FlavorShape {
	return mitm.FlavorShape{
		Slug:        "codex-cli-9f3c21aa",
		RecordCount: 2,
		Methods:     []string{"POST"},
		Paths:       []string{"/responses"},
		Signature: mitm.V2Signature{
			UserAgent:       learnedUserAgent,
			BetaFingerprint: learnedBetaFeatures,
			BodyKeys:        []string{"model", "instructions", "input", "tools", "stream"},
		},
		Headers:            testCodexFlavorHeaders(),
		BillingAttestation: "",
		Body: mitm.V2Body{
			BodyType: "object",
			Fields: []mitm.V2Field{
				{Name: "model", Kind: mitm.V2FieldKindString, Presence: mitm.V2HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "input", Kind: mitm.V2FieldKindArray, Presence: mitm.V2HeaderPresenceRequired, OccurrenceRate: 1.0},
				{Name: "instructions", Kind: mitm.V2FieldKindString, Presence: mitm.V2HeaderPresenceOptional, OccurrenceRate: 0.5},
				{Name: "stream", Kind: mitm.V2FieldKindBool, Presence: mitm.V2HeaderPresenceRequired, OccurrenceRate: 1.0},
			},
		},
	}
}

func testCodexFlavorHeaders() []mitm.V2Header {
	constant := func(name, value string) mitm.V2Header {
		return mitm.V2Header{
			Name:           name,
			Classification: mitm.V2HeaderClassConstant,
			Presence:       mitm.V2HeaderPresenceRequired,
			ObservedValues: []string{value},
			Pattern:        "",
			OccurrenceRate: 1.0,
		}
	}
	return []mitm.V2Header{
		constant("originator", learnedOriginator),
		constant("openai-beta", learnedOpenAIBeta),
		constant("user-agent", learnedUserAgent),
		constant("x-codex-beta-features", learnedBetaFeatures),
		constant("x-oai-attestation", learnedAttestation),
		// A secret header rides through masked; the loader does not
		// surface it into any identity field.
		constant("authorization", "<redacted>"),
	}
}

// TestCodexWireBaselineProjectsIdentity proves the loader projects the
// originator, openai-beta, user-agent, beta-features, attestation, and
// body field order from a reference-v2.toml.
func TestCodexWireBaselineProjectsIdentity(t *testing.T) {
	path := writeTestCodexWireBaseline(t)
	loader := NewWireBaselineLoader()
	identity, err := loader.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if identity.Originator != learnedOriginator {
		t.Errorf("Originator=%q want %q", identity.Originator, learnedOriginator)
	}
	if identity.OpenAIBeta != learnedOpenAIBeta {
		t.Errorf("OpenAIBeta=%q want %q", identity.OpenAIBeta, learnedOpenAIBeta)
	}
	if identity.UserAgent != learnedUserAgent {
		t.Errorf("UserAgent=%q want %q", identity.UserAgent, learnedUserAgent)
	}
	if identity.BetaFeatures != learnedBetaFeatures {
		t.Errorf("BetaFeatures=%q want %q", identity.BetaFeatures, learnedBetaFeatures)
	}
	if identity.Attestation != learnedAttestation {
		t.Errorf("Attestation=%q want %q", identity.Attestation, learnedAttestation)
	}
	wantBodyOrder := []string{"model", "instructions", "input", "tools", "stream"}
	if len(identity.BodyFields) != len(wantBodyOrder) {
		t.Fatalf("BodyFields=%v want %v", identity.BodyFields, wantBodyOrder)
	}
	for i, name := range wantBodyOrder {
		if identity.BodyFields[i] != name {
			t.Errorf("BodyFields[%d]=%q want %q", i, identity.BodyFields[i], name)
		}
	}
	wantRequired := []string{"input", "model", "stream"}
	if len(identity.BodyFieldsRequired) != len(wantRequired) {
		t.Fatalf("BodyFieldsRequired=%v want %v", identity.BodyFieldsRequired, wantRequired)
	}
	for i, name := range wantRequired {
		if identity.BodyFieldsRequired[i] != name {
			t.Errorf("BodyFieldsRequired[%d]=%q want %q", i, identity.BodyFieldsRequired[i], name)
		}
	}
}

// TestCodexWireBaselineMtimeCache proves a second load with no file
// change returns the cached identity (same path, no re-read needed).
func TestCodexWireBaselineMtimeCache(t *testing.T) {
	path := writeTestCodexWireBaseline(t)
	loader := NewWireBaselineLoader()
	first, err := loader.Load(path)
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := loader.Load(path)
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if first.Slug != second.Slug || first.UserAgent != second.UserAgent {
		t.Fatalf("cached identity diverged: first=%+v second=%+v", first, second)
	}
}

// TestCodexWireBaselineMissingReturnsSentinel proves a missing baseline
// yields ErrCodexBaselineMissing (the soft fall-back signal, not a hard
// error).
func TestCodexWireBaselineMissingReturnsSentinel(t *testing.T) {
	loader := NewWireBaselineLoader()
	_, err := loader.Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if !errors.Is(err, ErrCodexBaselineMissing) {
		t.Fatalf("err=%v want ErrCodexBaselineMissing", err)
	}
}

// TestCodexWireBaselineEmptyPathReturnsSentinel proves an empty path
// yields ErrCodexBaselineMissing rather than a stat panic.
func TestCodexWireBaselineEmptyPathReturnsSentinel(t *testing.T) {
	loader := NewWireBaselineLoader()
	_, err := loader.Load("")
	if !errors.Is(err, ErrCodexBaselineMissing) {
		t.Fatalf("err=%v want ErrCodexBaselineMissing", err)
	}
}

// TestCodexHeadersUseLearnedIdentity proves that when a baseline
// identity is present the outbound websocket headers carry the learned
// originator, openai-beta, user-agent, and beta-features, and replay the
// captured x-oai-attestation header.
func TestCodexHeadersUseLearnedIdentity(t *testing.T) {
	path := writeTestCodexWireBaseline(t)
	identity, err := NewWireBaselineLoader().Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:            "req-1",
		ConversationID:       "conv-1",
		Token:                "tok",
		InstallationID:       "",
		WindowID:             "",
		BetaFeatures:         identity.BetaFeatures,
		TurnState:            NewTurnState(),
		TurnMetadata:         "",
		IncludeTimingMetrics: false,
		Originator:           identity.Originator,
		UserAgent:            identity.UserAgent,
		OpenAIBeta:           identity.OpenAIBeta,
		Attestation:          identity.Attestation,
	})
	if got := header.Get(CodexOriginatorHeader); got != learnedOriginator {
		t.Errorf("%s=%q want %q", CodexOriginatorHeader, got, learnedOriginator)
	}
	if got := header.Get(openAIBetaHeader); got != learnedOpenAIBeta {
		t.Errorf("%s=%q want %q", openAIBetaHeader, got, learnedOpenAIBeta)
	}
	if got := header.Get("User-Agent"); got != learnedUserAgent {
		t.Errorf("User-Agent=%q want %q", got, learnedUserAgent)
	}
	if got := header.Get(CodexBetaFeaturesHeader); got != learnedBetaFeatures {
		t.Errorf("%s=%q want %q", CodexBetaFeaturesHeader, got, learnedBetaFeatures)
	}
	if got := header.Get(codexAttestationHeader); got != learnedAttestation {
		t.Errorf("%s=%q want %q", codexAttestationHeader, got, learnedAttestation)
	}
}

// TestCodexHeadersColdStartFallBackToConstants proves that with no
// baseline identity (a zero-value WireIdentity, the cold-start case) the
// outbound headers fall back to the compiled-in constants and omit the
// x-oai-attestation header that Clyde cannot mint.
func TestCodexHeadersColdStartFallBackToConstants(t *testing.T) {
	var coldStart WireIdentity
	header := BuildResponsesWebsocketHeaders(ResponsesWebsocketHeaderConfig{
		RequestID:            "req-1",
		ConversationID:       "conv-1",
		Token:                "tok",
		InstallationID:       "",
		WindowID:             "",
		BetaFeatures:         coldStart.BetaFeatures,
		TurnState:            NewTurnState(),
		TurnMetadata:         "",
		IncludeTimingMetrics: false,
		Originator:           coldStart.Originator,
		UserAgent:            coldStart.UserAgent,
		OpenAIBeta:           coldStart.OpenAIBeta,
		Attestation:          coldStart.Attestation,
	})
	if got := header.Get(CodexOriginatorHeader); got != CodexOriginatorValue {
		t.Errorf("%s=%q want fallback %q", CodexOriginatorHeader, got, CodexOriginatorValue)
	}
	if got := header.Get(openAIBetaHeader); got != responsesWebsocketsV2BetaHeaderValue {
		t.Errorf("%s=%q want fallback %q", openAIBetaHeader, got, responsesWebsocketsV2BetaHeaderValue)
	}
	if got := header.Get("User-Agent"); got != UserAgent() {
		t.Errorf("User-Agent=%q want fallback %q", got, UserAgent())
	}
	if got := header.Get(CodexBetaFeaturesHeader); got != "" {
		t.Errorf("%s=%q want empty on cold start", CodexBetaFeaturesHeader, got)
	}
	if got := header.Get(codexAttestationHeader); got != "" {
		t.Errorf("%s=%q want omitted on cold start (clyde does not mint attestation)", codexAttestationHeader, got)
	}
}
