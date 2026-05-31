package logevent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"goodkind.io/clyde/internal/config"
)

func TestFilterPayloadRemovesContextFieldsAndPreservesSafeFields(t *testing.T) {
	raw := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"secret"}],"unknown_debug":{"kept":true},"tools":[{"type":"function"}]}`)

	view := FilterPayload(raw, "application/json")

	if view.Summary.BodyType != "json_object" {
		t.Fatalf("BodyType = %q, want json_object", view.Summary.BodyType)
	}
	if len(view.Fields) != 2 {
		t.Fatalf("Fields length = %d, want 2", len(view.Fields))
	}
	paths := []string{view.Fields[0].Path, view.Fields[1].Path}
	if strings.Join(paths, ",") != "$.model,$.unknown_debug" {
		t.Fatalf("retained paths = %v, want model and unknown_debug", paths)
	}
	if len(view.Removed) != 2 {
		t.Fatalf("Removed length = %d, want 2", len(view.Removed))
	}
	for _, removed := range view.Removed {
		if removed.Path == "$.messages" || removed.Path == "$.tools" {
			continue
		}
		t.Fatalf("unexpected removed path %q", removed.Path)
	}
}

func TestRecorderReportsMissingRequiredLegs(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{}))
	emitter := NewEmitter(logger, RequiredLegs{SurfaceAdapterChat: {LegAdapterIngress, LegAdapterPayload}})
	recorder := emitter.Begin(Identity{RequestID: "req-1"}, Path{Surface: SurfaceAdapterChat, RouteFamily: RouteFamilyChatCompatible})

	recorder.Emit(context.Background(), Event{Path: Path{Leg: LegAdapterIngress, Phase: PhaseStarted}})
	complete := recorder.Complete(context.Background())

	if complete {
		t.Fatal("Complete returned true, want false")
	}
	if !strings.Contains(output.String(), "logging.request.incomplete") {
		t.Fatalf("log output does not include incomplete event: %s", output.String())
	}
	if !strings.Contains(output.String(), "adapter_payload") {
		t.Fatalf("log output does not include missing leg: %s", output.String())
	}
}

func TestRecorderEmitsTypedPayloadFields(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{}))
	emitter := NewEmitter(logger, RequiredLegs{SurfaceAdapterChat: {LegAdapterIngress}})
	recorder := emitter.Begin(Identity{RequestID: "req-2"}, Path{Surface: SurfaceAdapterChat, RouteFamily: RouteFamilyChatCompatible})
	payload := PayloadView{
		Summary: PayloadSummary{BodyType: "json_object", Bytes: 16},
		Fields:  []PayloadField{{Path: "$.model", Value: json.RawMessage(`"gpt-5"`), Bytes: 7}},
	}

	recorder.Emit(context.Background(), Event{
		Time:    time.Unix(1, 0),
		Name:    "logging.request.leg",
		Path:    Path{Leg: LegAdapterIngress, Phase: PhaseStarted},
		Payload: &payload,
	})

	if !strings.Contains(output.String(), "payload_fields") {
		t.Fatalf("log output does not include payload_fields: %s", output.String())
	}
	if !strings.Contains(output.String(), `"component":"adapter"`) {
		t.Fatalf("log output does not include adapter component: %s", output.String())
	}
}

// testFacet is an in-package typed Facet stand-in. It exercises the
// generic Facet contract without importing a provider package (a
// cycle the generic logevent package must avoid).
type testFacet struct {
	Key   string            `json:"-"`
	Attrs []testFacetAttr   `json:"attrs"`
	Hints SinkHints         `json:"-"`
	Extra map[string]string `json:"extra,omitempty"`
}

type testFacetAttr struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (f testFacet) FacetKey() string { return f.Key }

func (f testFacet) FacetAttrs() []slog.Attr {
	attrs := make([]slog.Attr, 0, len(f.Attrs))
	for _, attr := range f.Attrs {
		attrs = append(attrs, slog.String(attr.Key, attr.Value))
	}
	return attrs
}

func (f testFacet) SinkHints() SinkHints { return f.Hints }

func TestFacetsBundleEmitsAttrsAndJSON(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{}))
	emitter := NewEmitter(logger, RequiredLegs{SurfaceAdapterChat: {LegAdapterIngress}})
	recorder := emitter.Begin(Identity{RequestID: "req-facet"}, Path{Surface: SurfaceAdapterChat, RouteFamily: RouteFamilyChatCompatible})

	facet := testFacet{
		Key: "demo",
		Attrs: []testFacetAttr{
			{Key: "model", Value: "gpt-5"},
			{Key: "effort", Value: "high"},
		},
		Hints: SinkHints{NeedsProviderSidecar: true},
		Extra: nil,
	}
	var event Event
	event.Path = Path{Leg: LegAdapterIngress, Phase: PhaseStarted}
	event.Facets.Set(facet)
	recorder.Emit(context.Background(), event)

	logged := output.String()
	for _, want := range []string{`"demo":{"model":"gpt-5","effort":"high"}`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log output missing %q: %s", want, logged)
		}
	}
}

// transportFacet is a Facet stand-in that contributes no extra sink
// hints, standing in for a transport-surface facet in the generic test.
type transportFacet struct{}

func (transportFacet) FacetKey() string        { return "transport" }
func (transportFacet) FacetAttrs() []slog.Attr { return nil }
func (transportFacet) SinkHints() SinkHints {
	return SinkHints{NeedsProviderSidecar: false}
}

func TestDefaultSinksForEventSelectsCentralSinkModel(t *testing.T) {
	var event Event
	event.Identity = Identity{RequestID: "req-3", ChatKey: "chat-1"}
	event.Path = Path{Surface: SurfaceMITMIDE}
	event.Facets.Set(transportFacet{})

	sinks := DefaultSinksForEvent(event)
	want := []SinkName{SinkProcess, SinkConcern, SinkInventory, SinkPerChat}
	if strings.Join(sinkNames(sinks), ",") != strings.Join(sinkNames(want), ",") {
		t.Fatalf("sinks = %v, want %v", sinks, want)
	}
}

// TestSinkWireValuesAreStable locks the on-the-wire "sinks" attr values that the
// slogger filter handlers and the logs-inventory CLI match verbatim. Renaming
// any of these silently breaks end-to-end event routing, so the literals are
// pinned here. The two destinations that share a physical file with the config
// roster are asserted equal to the canonical config sink constants so the two
// taxonomies cannot drift apart on those names.
func TestSinkWireValuesAreStable(t *testing.T) {
	cases := []struct {
		sink SinkName
		want string
	}{
		{sink: SinkProcess, want: "process"},
		{sink: SinkConcern, want: "concern"},
		{sink: SinkPerRequest, want: "per_request"},
		{sink: SinkPerChat, want: "per_chat"},
		{sink: SinkProviderSidecar, want: "provider_sidecar"},
		{sink: SinkInventory, want: "inventory_index"},
	}
	for _, tc := range cases {
		if string(tc.sink) != tc.want {
			t.Fatalf("sink %v wire value = %q, want %q", tc.sink, string(tc.sink), tc.want)
		}
	}
	if string(SinkInventory) != config.LoggingSinkInventory {
		t.Fatalf("SinkInventory = %q, want config.LoggingSinkInventory %q", string(SinkInventory), config.LoggingSinkInventory)
	}
}

func sinkNames(sinks []SinkName) []string {
	names := make([]string, 0, len(sinks))
	for _, sink := range sinks {
		names = append(names, string(sink))
	}
	return names
}

func TestParseIncompletePolicy(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  IncompletePolicy
		ok    bool
	}{
		{name: "warn", input: "warn", want: IncompletePolicyWarn, ok: true},
		{name: "fail_test", input: "fail_test", want: IncompletePolicyFailTest, ok: true},
		{name: "trims_and_lowercases", input: "  FAIL_TEST  ", want: IncompletePolicyFailTest, ok: true},
		{name: "rejects_unknown", input: "abort", want: "", ok: false},
		{name: "rejects_empty", input: "", want: "", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseIncompletePolicy(tc.input)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("policy = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRecorderIncompletePolicyWarnEmitsWithoutCallingHandler(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{}))
	var handlerCalls int
	var handlerReport IncompleteReport
	handler := func(report IncompleteReport) {
		handlerCalls++
		handlerReport = report
	}
	emitter := NewEmitter(logger,
		RequiredLegs{SurfaceAdapterChat: {LegAdapterIngress, LegAdapterPayload}},
		WithIncompletePolicy(IncompletePolicyWarn),
		WithTestingTB(handler),
	)
	recorder := emitter.Begin(Identity{RequestID: "warn-1"}, Path{Surface: SurfaceAdapterChat, RouteFamily: RouteFamilyChatCompatible})

	recorder.Emit(context.Background(), Event{Path: Path{Leg: LegAdapterIngress, Phase: PhaseStarted}})
	complete := recorder.Complete(context.Background())

	if complete {
		t.Fatal("Complete returned true, want false")
	}
	if !strings.Contains(output.String(), "logging.request.incomplete") {
		t.Fatalf("log output does not include incomplete event: %s", output.String())
	}
	if !strings.Contains(output.String(), `"incomplete_policy":"warn"`) {
		t.Fatalf("log output does not include incomplete_policy=warn: %s", output.String())
	}
	if handlerCalls != 0 {
		t.Fatalf("handler called %d times under warn, want 0; report=%+v", handlerCalls, handlerReport)
	}
}

func TestRecorderIncompletePolicyFailTestCallsHandler(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{}))
	var handlerCalls int
	var handlerReport IncompleteReport
	handler := func(report IncompleteReport) {
		handlerCalls++
		handlerReport = report
	}
	emitter := NewEmitter(logger,
		RequiredLegs{SurfaceAdapterChat: {LegAdapterIngress, LegAdapterPayload}},
		WithIncompletePolicy(IncompletePolicyFailTest),
		WithTestingTB(handler),
	)
	recorder := emitter.Begin(Identity{RequestID: "fail-1"}, Path{Surface: SurfaceAdapterChat, RouteFamily: RouteFamilyChatCompatible})

	recorder.Emit(context.Background(), Event{Path: Path{Leg: LegAdapterIngress, Phase: PhaseStarted}})
	complete := recorder.Complete(context.Background())

	if complete {
		t.Fatal("Complete returned true, want false")
	}
	if !strings.Contains(output.String(), "logging.request.incomplete") {
		t.Fatalf("log output does not include incomplete event: %s", output.String())
	}
	if !strings.Contains(output.String(), `"incomplete_policy":"fail_test"`) {
		t.Fatalf("log output does not include incomplete_policy=fail_test: %s", output.String())
	}
	if handlerCalls != 1 {
		t.Fatalf("handler called %d times under fail_test, want 1", handlerCalls)
	}
	if handlerReport.Surface != SurfaceAdapterChat {
		t.Fatalf("handler surface=%q, want %q", handlerReport.Surface, SurfaceAdapterChat)
	}
	if len(handlerReport.Missing) != 1 || handlerReport.Missing[0] != LegAdapterPayload {
		t.Fatalf("handler missing=%v, want [%s]", handlerReport.Missing, LegAdapterPayload)
	}
	if handlerReport.Identity.RequestID != "fail-1" {
		t.Fatalf("handler identity.RequestID=%q, want fail-1", handlerReport.Identity.RequestID)
	}
}

func TestRecorderIncompletePolicyFailTestWithoutHandlerOnlyEmits(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{}))
	emitter := NewEmitter(logger,
		RequiredLegs{SurfaceAdapterChat: {LegAdapterIngress, LegAdapterPayload}},
		WithIncompletePolicy(IncompletePolicyFailTest),
	)
	recorder := emitter.Begin(Identity{RequestID: "fail-2"}, Path{Surface: SurfaceAdapterChat, RouteFamily: RouteFamilyChatCompatible})

	recorder.Emit(context.Background(), Event{Path: Path{Leg: LegAdapterIngress, Phase: PhaseStarted}})
	complete := recorder.Complete(context.Background())

	if complete {
		t.Fatal("Complete returned true, want false")
	}
	if !strings.Contains(output.String(), `"incomplete_policy":"fail_test"`) {
		t.Fatalf("log output does not include incomplete_policy=fail_test: %s", output.String())
	}
}
