package mitm

import (
	"strings"
	"testing"
)

func TestDiffReportSummaryMarksDrift(t *testing.T) {
	clean := DiffReport{Upstream: "claude-code"}
	if clean.HasDiverged() {
		t.Fatalf("empty report HasDiverged=true want false")
	}
	if strings.Contains(clean.SummaryString(), "DRIFT") {
		t.Fatalf("clean summary marked DRIFT: %q", clean.SummaryString())
	}

	drifted := DiffReport{Upstream: "claude-code", MissingFlavors: []string{"flav-x"}}
	if !drifted.HasDiverged() {
		t.Fatalf("report with missing flavor HasDiverged=false want true")
	}
	if !strings.Contains(drifted.SummaryString(), "DRIFT") {
		t.Fatalf("drifted summary missing DRIFT marker: %q", drifted.SummaryString())
	}
}
