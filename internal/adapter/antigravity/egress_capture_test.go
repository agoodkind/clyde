package antigravity_test

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/adapter/antigravity"
	"goodkind.io/clyde/internal/mitm/capture"
)

func TestGenerationCaptureRedactsBearer(t *testing.T) {
	const token = "fixture-bearer-value"
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "stream",
			status: http.StatusOK,
			body:   "data: {\"text\":\"answer " + token + "\"}\n\n",
		},
		{
			name:   "rejection",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"quota ` + token + `"}}`,
		},
	} {
		t.Run(
			test.name,
			func(t *testing.T) {
				server := httptest.NewTLSServer(
					http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.Header.Get("Authorization") != "Bearer "+token {
							t.Error("upstream request did not receive the fixture bearer")
						}

						writer.Header().Set("X-Request-Id", "echo-"+token)
						writer.WriteHeader(test.status)
						_, _ = io.WriteString(writer, test.body)
					}),
				)
				defer server.Close()

				path := filepath.Join(t.TempDir(), "capture.db")
				store, err := capture.Open(t.Context(), capture.Config{DBPath: path}, nil)
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { _ = store.Close(t.Context(), "test cleanup") })

				client, err := antigravity.NewClient(antigravity.ClientOptions{
					BaseURL:    server.URL,
					HTTPClient: server.Client(),
					Capture:    store,
				})
				if err != nil {
					t.Fatal(err)
				}

				response, err := client.Stream(
					t.Context(),
					token,
					antigravity.CodeAssistEnvelope{Model: "gemini-test"},
					"fixture-client",
				)
				if test.status == http.StatusOK {
					if err != nil {
						t.Fatal(err)
					}

					raw, readErr := io.ReadAll(response.Body)
					if closeErr := response.Body.Close(); closeErr != nil {
						t.Fatal(closeErr)
					}

					if readErr != nil || string(raw) != test.body {
						t.Fatal("capture changed the streamed response")
					}
				} else {
					var upstream *antigravity.UpstreamError
					if !errors.As(err, &upstream) || upstream.Status != test.status {
						t.Fatalf("upstream rejection = %v, want status %d", err, test.status)
					}
				}

				if err := store.Close(t.Context(), "test"); err != nil {
					t.Fatal(err)
				}

				assertGenerationCapture(t, path, token, test.status)
			},
		)
	}
}

func assertGenerationCapture(t *testing.T, path, token string, wantStatus int) {
	t.Helper()

	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var status int
	var requestHeaders, responseHeaders, upstreamID, responseBody string
	err = db.QueryRowContext(
		t.Context(),
		`
SELECT requests.status, req_headers, resp_headers, upstream_request_id, bodies.data
FROM requests
JOIN bodies ON bodies.request_row_id = requests.id AND bodies.which = 'response'
WHERE requests.client = 'adapter.antigravity' AND requests.path = '/v1internal:streamGenerateContent'
`,
	).Scan(
		&status,
		&requestHeaders,
		&responseHeaders,
		&upstreamID,
		&responseBody,
	)
	if err != nil {
		t.Fatal(err)
	}

	if status != wantStatus {
		t.Fatalf("captured status = %d, want %d", status, wantStatus)
	}

	for _, value := range []string{requestHeaders, responseHeaders, upstreamID, responseBody} {
		if strings.Contains(value, token) {
			t.Fatal("capture retained a raw bearer value")
		}
	}

	if strings.Contains(strings.ToLower(requestHeaders), "authorization") ||
		!strings.Contains(responseBody, "[REDACTED]") {
		t.Fatal("capture did not redact the Authorization header and response body echo")
	}
}
