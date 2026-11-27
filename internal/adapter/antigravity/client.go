package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"goodkind.io/clyde/internal/clock"
	"goodkind.io/clyde/internal/mitm/capture"
	"goodkind.io/gklog/correlation"
)

var (
	errInvalidCodeAssistOrigin = errors.New(
		"invalid Code Assist base URL: use an HTTPS or loopback HTTP origin",
	)
	errMissingAccessToken = errors.New("antigravity access token is empty")
)

const (
	streamGenerateContentPath  = "/v1internal:streamGenerateContent"
	streamGenerateContentQuery = "alt=sse"
	maxUpstreamErrorBytes      = 1 << 20
)

// Client sends typed GenerateContent envelopes to the Code Assist API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	capture    *capture.Store
}

// ClientOptions configures the Code Assist endpoint and HTTP transport.
type ClientOptions struct {
	BaseURL    string
	HTTPClient *http.Client
	Capture    *capture.Store
}

// NewClient creates a Code Assist client.
//
// It validates the configured origin and disables redirects.
//
//	client, err := NewClient(options)
func NewClient(options ClientOptions) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, errInvalidCodeAssistOrigin
	}

	hostname := parsed.Hostname()
	loopbackHTTP := parsed.Scheme == "http" && (hostname == "localhost" || hostname == "::1")
	if hostname == "" || parsed.User != nil || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !loopbackHTTP) {
		return nil, errInvalidCodeAssistOrigin
	}

	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	directClient := *httpClient
	directClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return &Client{baseURL: baseURL, httpClient: &directClient, capture: options.Capture}, nil
}

// Stream posts a Gemini request and returns the HTTP response.
//
// The caller owns the response body on success. HTTP failures return a
// classified [UpstreamError].
//
//	response, err := client.Stream(ctx, token, envelope, userAgent)
func (client *Client) Stream(
	ctx context.Context,
	accessToken string,
	envelope CodeAssistEnvelope,
	userAgent string,
) (*http.Response, error) {
	if accessToken == "" {
		return nil, errMissingAccessToken
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		slog.WarnContext(ctx, "adapter.antigravity.request_encode_failed")

		return nil, fmt.Errorf("encode Code Assist request: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.baseURL+streamGenerateContentPath+"?"+streamGenerateContentQuery,
		bytes.NewReader(body),
	)
	if err != nil {
		slog.WarnContext(ctx, "adapter.antigravity.request_create_failed")

		return nil, fmt.Errorf("create Code Assist request: %w", err)
	}

	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", userAgent)
	started := clock.Now()
	response, err := client.httpClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "adapter.antigravity.request_send_failed")

		return nil, fmt.Errorf("send Code Assist request: %w", err)
	}

	if client.capture.RecordingEnabled() {
		captureGeneration(
			client.capture,
			correlation.FromContext(ctx),
			request,
			response,
			body,
			envelope.Request.SessionID,
			started,
		)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()

		rejection, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes))
		if err != nil {
			slog.WarnContext(ctx, "adapter.antigravity.rejection_read_failed")

			return nil, fmt.Errorf("read Code Assist rejection: %w", err)
		}

		return nil, ParseUpstreamError(response.StatusCode, rejection)
	}

	return response, nil
}
