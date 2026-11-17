package adapter

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"goodkind.io/clyde/internal/config"
)

func secretReferenceServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)

	return server
}

func newSecretReferenceAdapter(t *testing.T, cfg config.AdapterConfig) *Server {
	t.Helper()
	server, err := New(
		context.Background(),
		cfg,
		config.LoggingConfig{},
		Deps{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}

	return server
}

func TestAdapterSecretReferenceAuthentication(t *testing.T) {
	t.Setenv("CLYDE_ADAPTER_TOKEN", "")
	t.Setenv("CLYDE_TEST_INBOUND_SECRET", "inbound-credential")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("inbound-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{"literal", "env", "file", "override"} {
		t.Run(
			source,
			func(t *testing.T) {
				cfg := baseConfig()
				switch source {
				case "literal":
					cfg.RequireToken = "inbound-credential"
				case "env":
					cfg.RequireTokenEnv = new("CLYDE_TEST_INBOUND_SECRET")
				case "file":
					cfg.RequireTokenFile = &path
				case "override":
					cfg.RequireToken = "other-credential"
					t.Setenv("CLYDE_ADAPTER_TOKEN", "inbound-credential")
				}

				server := newSecretReferenceAdapter(t, cfg)

				for _, token := range []string{"inbound-credential", "wrong", ""} {
					request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
					request.Header.Set("Authorization", "Bearer "+token)
					recorder := httptest.NewRecorder()
					server.mux.ServeHTTP(recorder, request)
					want := http.StatusUnauthorized
					if token == "inbound-credential" {
						want = http.StatusOK
					}

					if recorder.Code != want {
						t.Fatalf("authentication status = %d, want %d", recorder.Code, want)
					}
				}
			},
		)
	}
}

func TestPassthroughSecretReferenceAuthorization(t *testing.T) {
	t.Setenv("CLYDE_ADAPTER_TOKEN", "")
	t.Setenv("CLYDE_TEST_OUTBOUND_SECRET", "outbound-credential")
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("outbound-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, route := range []string{"fallback", "override"} {
		for _, source := range []string{"env", "file", "literal"} {
			t.Run(
				route+"/"+source,
				func(t *testing.T) {
					authorization := make(chan string, 2)
					upstream := secretReferenceServer(
						t,
						http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
							authorization <- request.Header.Get("Authorization")
							writer.Header().Set("Content-Type", "application/json")
							_, _ = writer.Write(
								[]byte(
									`{"id":"test","choices":[{"message":{"content":"ok"}}],"output":[]}`,
								),
							)
						}),
					)
					cfg := passthroughSnapshotConfig(upstream.URL)
					entry := config.AdapterOpenAICompatPassthrough{BaseURL: upstream.URL}
					switch source {
					case "env":
						entry.APIKeyEnv = "CLYDE_TEST_OUTBOUND_SECRET"
					case "file":
						entry.APIKeyFile = &path
					case "literal":
						entry.APIKey = "outbound-credential"
						entry.APIKeyEnv = "CLYDE_TEST_ABSENT_SECRET"
					}

					model := "unknown-secret-probe"
					if route == "fallback" {
						cfg.OpenAICompatPassthrough = entry
					} else {
						model = "snapshot-alias"
						cfg.PassthroughOverrides["snapshot"] = config.AdapterPassthroughOverride{
							BaseURL:    entry.BaseURL,
							APIKey:     entry.APIKey,
							APIKeyEnv:  entry.APIKeyEnv,
							APIKeyFile: entry.APIKeyFile,
						}
					}

					server := newSecretReferenceAdapter(t, cfg)

					for _, endpoint := range []string{"chat/completions", "responses"} {
						body := `{"model":"` + model + `","messages":[{"role":"user","content":"probe"}],"input":"probe"}`
						request := httptest.NewRequest(
							http.MethodPost,
							"/v1/"+endpoint,
							strings.NewReader(body),
						)
						recorder := httptest.NewRecorder()
						server.mux.ServeHTTP(recorder, request)
						if recorder.Code != http.StatusOK {
							t.Fatalf(
								"forward status = %d: %s",
								recorder.Code,
								recorder.Body.String(),
							)
						}

						if <-authorization != "Bearer outbound-credential" {
							t.Fatal("upstream Authorization did not match configured credential")
						}
					}

					if source == "literal" {
						return
					}

					if source == "file" {
						if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
							t.Fatal(err)
						}

						t.Cleanup(func() {
							if err := os.WriteFile(
								path,
								[]byte("outbound-credential\n"),
								0o600,
							); err != nil {
								t.Error(err)
							}
						})
					} else {
						t.Setenv("CLYDE_TEST_OUTBOUND_SECRET", "")
					}

					request := httptest.NewRequest(
						http.MethodPost,
						"/v1/chat/completions",
						strings.NewReader(
							`{"model":"`+model+`","messages":[{"role":"user","content":"probe"}]}`,
						),
					)
					recorder := httptest.NewRecorder()
					server.mux.ServeHTTP(recorder, request)
					if recorder.Code == http.StatusOK ||
						!strings.Contains(recorder.Body.String(), "api_key_") {
						t.Fatalf(
							"invalid reference response = %d: %s",
							recorder.Code,
							recorder.Body.String(),
						)
					}

					select {
					case <-authorization:
						t.Fatal("invalid credential request contacted upstream")
					default:
					}
				},
			)
		}
	}
}

func TestFailedSecretConfigApplyPreservesInboundAuthentication(t *testing.T) {
	t.Setenv("CLYDE_ADAPTER_TOKEN", "")
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("original-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := baseConfig()
	cfg.RequireTokenFile = &path
	server := newSecretReferenceAdapter(t, cfg)
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := server.ApplyConfig(cfg); err == nil {
		t.Fatal("invalid secret configuration succeeded")
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer original-credential")
	recorder := httptest.NewRecorder()
	server.mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("original credential status = %d", recorder.Code)
	}
}
