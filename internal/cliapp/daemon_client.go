package cliapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/powerformer/looper/pkg/api"
)

type DaemonAPIClient struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

type DaemonAPIClientOptions struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

type DaemonAPIError struct {
	Message   string
	Code      api.ErrorCode
	Status    int
	RequestID string
}

func (e *DaemonAPIError) Error() string {
	return e.Message
}

func NewDaemonAPIClient(options DaemonAPIClientOptions) *DaemonAPIClient {
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &DaemonAPIClient{
		baseURL:    strings.TrimRight(options.BaseURL, "/"),
		token:      options.Token,
		httpClient: httpClient,
	}
}

func (c *DaemonAPIClient) Get(ctx context.Context, path string, out any) error {
	return c.request(ctx, http.MethodGet, path, nil, out)
}

func (c *DaemonAPIClient) Post(ctx context.Context, path string, body any, out any) error {
	return c.request(ctx, http.MethodPost, path, body, out)
}

func (c *DaemonAPIClient) request(ctx context.Context, method, path string, body any, out any) error {
	var requestBody io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		requestBody = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, requestBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.token))
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("looperd is not reachable: %w", err)
	}
	defer response.Body.Close()

	return decodeAPIResponse(response, out)
}

func decodeAPIResponse(response *http.Response, out any) error {
	var envelope api.Envelope[json.RawMessage]
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return &DaemonAPIError{
			Message: fmt.Sprintf("Request failed with status %d", response.StatusCode),
			Status:  response.StatusCode,
		}
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.OK || envelope.Data == nil {
		message := fmt.Sprintf("Request failed with status %d", response.StatusCode)
		var code api.ErrorCode
		if envelope.Error != nil {
			if envelope.Error.Message != "" {
				message = envelope.Error.Message
			}
			code = envelope.Error.Code
		}

		return &DaemonAPIError{
			Message:   message,
			Code:      code,
			Status:    response.StatusCode,
			RequestID: envelope.RequestID,
		}
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(*envelope.Data, out); err != nil {
		return fmt.Errorf("decode response data: %w", err)
	}

	return nil
}
