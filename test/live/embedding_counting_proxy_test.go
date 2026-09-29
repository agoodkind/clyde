//go:build live

package live

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// embeddingCounts is the number of embedding requests and embedding inputs a
// live run sent to the embedding endpoint.
type embeddingCounts struct {
	Requests int
	Inputs   int
}

// embeddingCountingProxy forwards every request unchanged to the real
// embedding endpoint and counts the embedding requests and their inputs. A
// live run reports the counts as its embedding cost.
type embeddingCountingProxy struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	counts embeddingCounts
}

// embeddingInputs is the input field of an OpenAI-compatible embeddings
// request, which is one string or an array of strings.
type embeddingInputs struct {
	Input json.RawMessage `json:"input"`
}

// startEmbeddingCountingProxy listens on an IPv6 loopback port and forwards
// each request to the scheme and host of upstream with the request path
// unchanged.
func startEmbeddingCountingProxy(t *testing.T, upstream string) *embeddingCountingProxy {
	t.Helper()

	target, err := url.Parse(upstream)
	if err != nil {
		t.Fatalf("parse embedding endpoint %q: %v", upstream, err)
	}
	proxy := &embeddingCountingProxy{t: t, server: nil, mu: sync.Mutex{}, counts: embeddingCounts{Requests: 0, Inputs: 0}}
	forward := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(&url.URL{Scheme: target.Scheme, Host: target.Host})
		},
	}
	listenConfig := net.ListenConfig{}
	listener, err := listenConfig.Listen(t.Context(), "tcp", "[::1]:0")
	if err != nil {
		t.Fatalf("listen for the embedding counting proxy: %v", err)
	}
	proxy.server = httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/embeddings") {
			body, readErr := io.ReadAll(request.Body)
			if readErr != nil {
				http.Error(writer, "read embedding request", http.StatusBadRequest)
				return
			}
			proxy.record(body)
			request.Body = io.NopCloser(bytes.NewReader(body))
		}
		forward.ServeHTTP(writer, request)
	}))
	proxy.server.Listener = listener
	proxy.server.Start()
	t.Cleanup(proxy.server.Close)
	return proxy
}

// baseURL returns the proxy URL followed by upstreamPath.
func (proxy *embeddingCountingProxy) baseURL(upstreamPath string) string {
	return proxy.server.URL + upstreamPath
}

// record counts one embedding request and its inputs. A body with an input
// field that is neither a string nor an array of strings fails the test,
// because the counts would otherwise misreport the embedding cost.
func (proxy *embeddingCountingProxy) record(body []byte) {
	inputCount, err := countEmbeddingInputs(body)
	if err != nil {
		proxy.t.Errorf("embedding counting proxy: %v", err)
		return
	}
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	proxy.counts.Requests++
	proxy.counts.Inputs += inputCount
}

// countEmbeddingInputs returns the number of inputs in an OpenAI-compatible
// embeddings request body: 1 for a string input and the array length for a
// string array input.
func countEmbeddingInputs(body []byte) (int, error) {
	var decoded embeddingInputs
	if err := json.Unmarshal(body, &decoded); err != nil {
		return 0, fmt.Errorf("decode embedding request body: %w", err)
	}
	var single string
	if err := json.Unmarshal(decoded.Input, &single); err == nil {
		return 1, nil
	}
	var inputs []string
	if err := json.Unmarshal(decoded.Input, &inputs); err != nil {
		return 0, fmt.Errorf("decode embedding request input as a string or a string array: %w", err)
	}
	return len(inputs), nil
}

// snapshot returns the counts so far.
func (proxy *embeddingCountingProxy) snapshot() embeddingCounts {
	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	return proxy.counts
}
