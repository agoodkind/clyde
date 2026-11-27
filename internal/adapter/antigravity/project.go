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
	"strings"
	"sync"
)

const projectResponseLimit = 1 << 20

var (
	errLoadCodeAssistStatus = errors.New("loadCodeAssist returned an error status")
	errNoCodeAssistProject  = errors.New(
		"antigravity account has no Code Assist project; sign in to Antigravity once",
	)
)

// ProjectManager discovers and caches the Code Assist project for each login.
type ProjectManager struct {
	mu          sync.Mutex
	client      *http.Client
	logger      *slog.Logger
	baseURL     string
	override    string
	project     string
	fingerprint string
}

// ProjectManagerOptions configures project discovery.
type ProjectManagerOptions struct {
	HTTPClient *http.Client
	BaseURL    string
	Project    string
	Logger     *slog.Logger
}

// NewProjectManager creates a Code Assist project resolver.
//
// The resolver calls loadCodeAssist when Project is empty.
//
//	options := ProjectManagerOptions{Project: "my-project"}
func NewProjectManager(options ProjectManagerOptions) *ProjectManager {
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}

	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://daily-cloudcode-pa.googleapis.com"
	}

	return &ProjectManager{
		mu:          sync.Mutex{},
		client:      client,
		logger:      logger,
		baseURL:     baseURL,
		override:    strings.TrimSpace(options.Project),
		project:     "",
		fingerprint: "",
	}
}

type loadCodeAssistRequest struct {
	Metadata struct {
		IDEType string `json:"ideType"`
	} `json:"metadata"`
}

type loadCodeAssistResponse struct {
	CloudAICodeProject string `json:"cloudaicompanionProject"`
	CurrentTier        *struct {
		ID string `json:"id"`
	} `json:"currentTier"`
}

// Project returns the configured override or discovers the account project.
//
// Discovery caches the result by credential fingerprint and rejects accounts
// without a tier or project.
//
//	project, err := manager.Project(ctx, credential)
func (manager *ProjectManager) Project(ctx context.Context, credential Credential) (string, error) {
	if manager.override != "" {
		return manager.override, nil
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.project != "" && manager.fingerprint == credential.Fingerprint {
		return manager.project, nil
	}

	var requestBody loadCodeAssistRequest
	requestBody.Metadata.IDEType = "ANTIGRAVITY"
	encodedBody, err := json.Marshal(requestBody)
	if err != nil {
		return manager.failed(ctx, fmt.Errorf("encode loadCodeAssist request: %w", err))
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		manager.baseURL+"/v1internal:loadCodeAssist",
		bytes.NewReader(encodedBody),
	)
	if err != nil {
		return manager.failed(ctx, fmt.Errorf("create loadCodeAssist request: %w", err))
	}

	request.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := manager.client.Do(request)
	if err != nil {
		return manager.failed(ctx, fmt.Errorf("call loadCodeAssist: %w", err))
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, projectResponseLimit))
	if err != nil {
		return manager.failed(ctx, fmt.Errorf("read loadCodeAssist response: %w", err))
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return manager.failed(
			ctx,
			fmt.Errorf("%w: HTTP %d", errLoadCodeAssistStatus, response.StatusCode),
		)
	}

	var result loadCodeAssistResponse
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return manager.failed(ctx, fmt.Errorf("decode loadCodeAssist response: %w", err))
	}

	if result.CurrentTier == nil || result.CloudAICodeProject == "" {
		return manager.failed(ctx, errNoCodeAssistProject)
	}

	manager.project = result.CloudAICodeProject
	manager.fingerprint = credential.Fingerprint

	return manager.project, nil
}

func (manager *ProjectManager) failed(ctx context.Context, err error) (string, error) {
	manager.logger.ErrorContext(
		ctx,
		"adapter.antigravity.project.discovery_failed",
		"concern",
		"adapter.providers.antigravity.request",
		"component",
		"adapter",
		"subcomponent",
		"antigravity_project",
		"err",
		err,
	)

	return "", err
}
