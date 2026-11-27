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
	"time"
)

const availableModelsPath = "/v1internal:fetchAvailableModels"

var errMissingModels = errors.New("model response from Code Assist did not include a catalog")

type availableModelsRequest struct {
	Project string `json:"project"`
}

type availableModelsResponse struct {
	Models map[string]availableModel `json:"models"`
}

type availableModel struct {
	Model string `json:"model"`
}

// AvailableModels lists the account's Gemini wire model IDs.
//
// The adapter intersects these IDs with its advertised local catalog.
//
//	ids, err := backend.AvailableModels(ctx)
func (p *Provider) AvailableModels(ctx context.Context) ([]string, error) {
	token, fingerprint, err := p.authorization(ctx)
	if err != nil {
		return nil, err
	}

	project, err := p.projects.Project(
		ctx,
		Credential{
			AccessToken:  token,
			RefreshToken: "",
			ExpiresAt:    time.Time{},
			Fingerprint:  fingerprint,
		},
	)
	if err != nil {
		return nil, err
	}

	ids, err := p.client.availableModels(ctx, token, p.userAgent, project)
	if err != nil {
		slog.WarnContext(ctx, "adapter.antigravity.model_discovery_failed", "err", err)

		return nil, fmt.Errorf("discover Antigravity account models: %w", err)
	}

	return ids, nil
}

func (client *Client) availableModels(
	ctx context.Context,
	token string,
	userAgent string,
	project string,
) ([]string, error) {
	if token == "" {
		return nil, errMissingAccessToken
	}

	body, err := json.Marshal(availableModelsRequest{Project: project})
	if err != nil {
		return nil, fmt.Errorf("encode Code Assist model request: %w", err)
	}

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.baseURL+availableModelsPath,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("create Code Assist model request: %w", err)
	}

	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", userAgent)

	response, err := client.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send Code Assist model request: %w", err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamErrorBytes))
	if err != nil {
		return nil, fmt.Errorf("read Code Assist model response: %w", err)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, ParseUpstreamError(response.StatusCode, data)
	}

	var result availableModelsResponse
	if err := json.Unmarshal(data, &result); err != nil {
		slog.WarnContext(ctx, "adapter.antigravity.model_response_invalid", "err", err)

		return nil, fmt.Errorf("decode Code Assist model response: %w", err)
	}

	if result.Models == nil {
		return nil, errMissingModels
	}

	ids := make([]string, 0, len(result.Models))
	for id := range result.Models {
		if strings.HasPrefix(id, "gemini-") {
			ids = append(ids, id)
		}
	}

	return ids, nil
}
