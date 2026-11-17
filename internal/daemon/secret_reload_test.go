package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"goodkind.io/clyde/internal/adapter"
	"goodkind.io/clyde/internal/config"
)

func TestInvalidSecretReloadPreservesAdapterAuthentication(t *testing.T) {
	t.Setenv("CLYDE_ADAPTER_TOKEN", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	configPath := config.GlobalConfigPath()
	directory := filepath.Dir(configPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(directory, "token")
	if err := os.WriteFile(keyPath, []byte("original-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.NewConfigWithDefaults()
	cfg.Adapter = hotApplyAdapterConfig()
	cfg.Adapter.RequireTokenFile = &keyPath
	contents, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := adapter.New(t.Context(), cfg.Adapter, cfg.Logging, adapter.Deps{}, log)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.StartOnListener(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	runtime := &runtimeServices{adapter: server}

	for _, scenario := range []string{
		"empty file",
		"blank file",
		"empty file reference",
		"empty environment reference",
	} {
		next := *cfg
		content := "original-credential\n"
		switch scenario {
		case "empty file":
			content = ""
		case "blank file":
			content = " \n"
		case "empty file reference":
			next.Adapter.RequireTokenFile = new("")
		case "empty environment reference":
			next.Adapter.RequireTokenFile = nil
			next.Adapter.RequireTokenEnv = new("")
		}

		if err := os.WriteFile(keyPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		contents, err := toml.Marshal(next)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(configPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := validateReloadListenerConfig(runtime); err == nil ||
			!strings.Contains(err.Error(), "require_token") {
			t.Fatalf("reload did not reject invalid credential: %v", err)
		}

		for _, token := range []string{"original-credential", "wrong"} {
			request, err := http.NewRequestWithContext(
				t.Context(),
				http.MethodGet,
				"http://"+listener.Addr().String()+"/v1/models",
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}

			request.Header.Set("Authorization", "Bearer "+token)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}

			_ = response.Body.Close()
			want := http.StatusUnauthorized
			if token == "original-credential" {
				want = http.StatusOK
			}

			if response.StatusCode != want {
				t.Fatalf(
					"authentication status after rejected reload = %d, want %d",
					response.StatusCode,
					want,
				)
			}
		}
	}
}
