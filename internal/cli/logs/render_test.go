package logs

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestRenderInventoryPrintsMetadataTable(t *testing.T) {
	generated := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	modified := time.Date(2026, 5, 6, 11, 30, 0, 0, time.UTC)
	currentInventory := inventory{
		StateRoot:         "/tmp/clyde-state",
		Generated:         generated,
		Mode:              inventoryModeIndexed,
		RawCaptureEnabled: false,
		CleanupEnabled:    true,
		Categories: []categorySummary{
			{
				Category:           categoryMITMCaptureIndexes,
				Sink:               "mitm_capture_index",
				Source:             inventorySourceIndexed,
				RawCaptureEnabled:  false,
				CleanupEnabled:     true,
				Count:              1,
				TotalBytes:         2048,
				LatestModified:     modified,
				RepresentativePath: "mitm/capture.jsonl",
				LargestFiles: []fileSummary{
					{
						RelativePath: "mitm/capture.jsonl",
						SizeBytes:    2048,
						Modified:     modified,
					},
				},
			},
			{
				Category: categoryUncategorizedLogs,
			},
		},
	}
	var output bytes.Buffer
	if err := writeInventoryTable(&output, currentInventory); err != nil {
		t.Fatalf("writeInventoryTable returned error: %v", err)
	}
	rendered := output.String()
	for _, want := range []string{
		"State root: /tmp/clyde-state",
		"Mode: indexed",
		"Raw capture: false",
		"Cleanup: true",
		"Category",
		"MITM capture indexes",
		"1",
		"2.0 KiB",
		"mitm/capture.jsonl (2.0 KiB)",
		"Uncategorized logs",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered table missing %q:\n%s", want, rendered)
		}
	}
}
