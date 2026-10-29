//go:build live

package live

import (
	"database/sql"
	_ "embed"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/optional_capture_counts.sql
var optionalCaptureCountsSQL string

func TestLiveAdapterEgressRecordingWithoutMITM(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	writeAdapterOnlyCaptureConfig(t, h, upstream.baseURL(), time.Now().Add(time.Hour).Format(time.RFC3339))
	h.boot(t)
	probe := uniqueProbeID(t, "adapter-egress-without-mitm")
	response := h.postResponses(t, h.cfg.AdapterPort, responsesRequestBody(probe, false))
	if response.statusCode != http.StatusOK || !strings.Contains(response.body, probe) {
		t.Fatalf("forwarded response status = %d, contains probe = %t", response.statusCode, strings.Contains(response.body, probe))
	}
	_ = upstream.waitForRequest(t, 10*time.Second)
	databasePath := filepath.Join(h.stateRoot, "mitm", "capture.db")
	deadline := time.Now().Add(5 * time.Second)
	for {
		captures, err := readResponsesCaptures(databasePath, probe)
		if err == nil && len(captures) == 1 {
			if captures[0].client != "adapter.passthrough" {
				t.Fatalf("capture client = %s, want adapter.passthrough", captures[0].client)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("adapter egress capture did not commit: captures=%d, error=%v", len(captures), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertOptionalCaptureCounts(t, h, 1)
}

func TestLiveResponsesDefaultRecordingDisabled(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	writeAdapterOnlyCaptureConfig(t, h, upstream.baseURL(), "")
	h.boot(t)
	probe := uniqueProbeID(t, "recording-disabled")
	response := h.postResponses(t, h.cfg.AdapterPort, responsesRequestBody(probe, false))
	if response.statusCode != http.StatusOK || !strings.Contains(response.body, probe) {
		t.Fatalf("forwarded response status = %d, contains probe = %t", response.statusCode, strings.Contains(response.body, probe))
	}
	_ = upstream.waitForRequest(t, 10*time.Second)
	upstream.releaseStreams()
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, responsesRequestBody(probe, true))
	completed := stream.waitForCompletion(t, 10*time.Second)
	if !strings.Contains(completed, "response.completed") || !strings.Contains(completed, probe) {
		t.Fatal("Responses stream did not complete with recording disabled")
	}
	assertOptionalCaptureCounts(t, h, 0)
}

func TestLiveRecordingDeadlinePreservesActiveStream(t *testing.T) {
	upstream := startResponsesUpstream(t)
	h := newHarness(t)
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, upstream.baseURL())
	deadline := time.Now().Add(15 * time.Second).Truncate(time.Second)
	writeOptionalCaptureDeadline(t, h, deadline.Format(time.RFC3339))
	h.boot(t)
	probe := uniqueProbeID(t, "recording-before-expiration")
	response := h.postResponses(t, h.cfg.AdapterPort, responsesRequestBody(probe, false))
	if response.statusCode != http.StatusOK {
		t.Fatalf("response status = %d", response.statusCode)
	}
	_ = upstream.waitForRequest(t, 10*time.Second)
	assertResponsesCaptures(t, h.waitForResponsesCaptures(t, probe, 10*time.Second), probe, false)
	stream := h.startResponsesStream(t, h.cfg.AdapterPort, responsesRequestBody(probe, true))
	if first := stream.waitForFirstFrame(t, 10*time.Second); !strings.Contains(first, "response.created") {
		t.Fatal("Responses stream did not start before capture expiration")
	}
	_ = upstream.waitForRequest(t, 10*time.Second)
	if !time.Now().Before(deadline) {
		t.Fatal("test did not establish a stream before expiration")
	}
	timer := time.NewTimer(time.Until(deadline) + 100*time.Millisecond)
	defer timer.Stop()
	<-timer.C
	upstream.releaseStreams()
	completed := stream.waitForCompletion(t, 10*time.Second)
	if !strings.Contains(completed, "response.completed") {
		t.Fatal("capture expiration interrupted the active stream")
	}
	response = h.postResponses(t, h.cfg.AdapterPort, responsesRequestBody(probe, false))
	if response.statusCode != http.StatusOK {
		t.Fatalf("response after expiration status = %d", response.statusCode)
	}
	assertOptionalCaptureCounts(t, h, 2)
}

func writeAdapterOnlyCaptureConfig(t *testing.T, h *harness, upstreamURL, deadline string) {
	t.Helper()
	h.writeCombinedAdapterMITMConfig(t, h.cfg.AdapterPort, upstreamURL)
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(data), "[mitm]\nenabled_default = true", "[mitm]\nenabled_default = false", 1)
	if updated == string(data) {
		t.Fatal("test configuration does not enable MITM")
	}
	withoutIngress := strings.Replace(updated, "capture_ingress = true", "capture_ingress = false", 1)
	if withoutIngress == updated {
		t.Fatal("test configuration does not enable adapter ingress capture")
	}
	if err := os.WriteFile(h.configPath, []byte(withoutIngress), 0o600); err != nil {
		t.Fatal(err)
	}
	writeOptionalCaptureDeadline(t, h, deadline)
}

func writeOptionalCaptureDeadline(t *testing.T, h *harness, deadline string) {
	t.Helper()
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var output []string
	inCaptureStore := false
	found := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "[") {
			inCaptureStore = line == "[mitm.capture_store]"
			if inCaptureStore {
				found = true
				output = append(output, line)
				if deadline != "" {
					output = append(output, fmt.Sprintf("record_until = %q", deadline))
				}
				continue
			}
		}
		if inCaptureStore && strings.HasPrefix(strings.TrimSpace(line), "record_until") {
			continue
		}
		output = append(output, line)
	}
	if !found {
		t.Fatal("capture store settings are missing")
	}
	if err := os.WriteFile(h.configPath, []byte(strings.Join(output, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertOptionalCaptureCounts(t *testing.T, h *harness, expectedRequests int) {
	t.Helper()
	path := filepath.Join(h.stateRoot, "mitm", "capture.db")
	database, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	var requests, bodies, decoded, tools int
	if err := database.QueryRow(optionalCaptureCountsSQL).Scan(&requests, &bodies, &decoded, &tools); err != nil {
		t.Fatal(err)
	}
	if requests != expectedRequests || bodies != 2*expectedRequests || decoded != 0 || tools != 0 {
		t.Fatalf("capture counts: requests=%d bodies=%d decoded=%d tools=%d; expected requests=%d", requests, bodies, decoded, tools, expectedRequests)
	}
}
