package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProjectManagerDiscoversAndCachesByFingerprint(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(
		http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requestCount++
			if request.URL.Path != "/v1internal:loadCodeAssist" {
				t.Errorf("path = %q", request.URL.Path)
			}

			if request.Header.Get("Authorization") != "Bearer access-token" {
				t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
			}

			var body struct {
				Metadata struct {
					IDEType string `json:"ideType"`
				} `json:"metadata"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
			}

			if body.Metadata.IDEType != "ANTIGRAVITY" {
				t.Errorf("ideType = %q", body.Metadata.IDEType)
			}

			_, _ = writer.Write(
				[]byte(
					`{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"project-123"}`,
				),
			)
		}),
	)
	defer server.Close()
	manager := NewProjectManager(ProjectManagerOptions{
		HTTPClient: server.Client(),
		BaseURL:    server.URL,
		Project:    "",
		Logger:     nil,
	})
	credential := Credential{AccessToken: "access-token", Fingerprint: "one"}

	for range 2 {
		project, err := manager.Project(context.Background(), credential)
		if err != nil || project != "project-123" {
			t.Fatalf("Project = %q, %v", project, err)
		}
	}

	if requestCount != 1 {
		t.Fatalf("loadCodeAssist calls = %d, want 1", requestCount)
	}

	credential.Fingerprint = "two"
	if _, err := manager.Project(context.Background(), credential); err != nil {
		t.Fatalf("Project after fingerprint change: %v", err)
	}

	if requestCount != 2 {
		t.Fatalf("loadCodeAssist calls after fingerprint change = %d, want 2", requestCount)
	}
}

func TestProjectManagerOverrideSkipsDiscovery(t *testing.T) {
	manager := NewProjectManager(ProjectManagerOptions{
		HTTPClient: nil,
		BaseURL:    "",
		Project:    "configured-project",
		Logger:     nil,
	})
	project, err := manager.Project(context.Background(), Credential{})
	if err != nil || project != "configured-project" {
		t.Fatalf("Project = %q, %v", project, err)
	}
}

func TestProjectManagerRequiresAntigravityOnboarding(t *testing.T) {
	for _, responseBody := range []string{
		`{"cloudaicompanionProject":"project-123"}`,
		`{"currentTier":{"id":"free-tier"}}`,
	} {
		t.Run(
			responseBody,
			func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
						_, _ = writer.Write([]byte(responseBody))
					}),
				)
				defer server.Close()
				manager := NewProjectManager(ProjectManagerOptions{
					HTTPClient: server.Client(),
					BaseURL:    server.URL,
					Project:    "",
					Logger:     nil,
				})
				if _, err := manager.Project(
					context.Background(),
					Credential{Fingerprint: "one"},
				); err == nil {
					t.Fatal("Project succeeded without both currentTier and project")
				}
			},
		)
	}
}
