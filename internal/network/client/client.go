package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/network/protocol"
)

type Client struct {
	baseURL    string
	nodeToken  string
	httpClient *http.Client
}

func New(baseURL, nodeToken string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), nodeToken: nodeToken, httpClient: httpClient}
}

func (c *Client) Join(ctx context.Context, req protocol.JoinRequest) (protocol.JoinResponse, error) {
	var out protocol.JoinResponse
	if err := c.request(ctx, http.MethodPost, "/v1/join", "", req, &out); err != nil {
		return protocol.JoinResponse{}, err
	}
	return out, nil
}

func (c *Client) Heartbeat(ctx context.Context, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	var out protocol.HeartbeatResponse
	if err := c.request(ctx, http.MethodPost, "/v1/heartbeat", c.nodeToken, req, &out); err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	return out, nil
}

func (c *Client) Leave(ctx context.Context) error {
	return c.request(ctx, http.MethodPost, "/v1/leave", c.nodeToken, map[string]any{}, nil)
}

func (c *Client) Status(ctx context.Context) (protocol.NodeStatusResponse, error) {
	var out protocol.NodeStatusResponse
	if err := c.request(ctx, http.MethodGet, "/v1/status", c.nodeToken, nil, &out); err != nil {
		return protocol.NodeStatusResponse{}, err
	}
	return out, nil
}

func (c *Client) RevalidateLease(ctx context.Context, req protocol.CoordinatorLeaseRevalidateRequest) error {
	return c.request(ctx, http.MethodPost, "/v1/coordinator-lease/revalidate", c.nodeToken, req, nil)
}

func (c *Client) request(ctx context.Context, method, path, token string, body any, out any) error {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload map[string]any
		_ = json.NewDecoder(response.Body).Decode(&payload)
		if message, ok := payload["message"].(string); ok && strings.TrimSpace(message) != "" {
			return fmt.Errorf(message)
		}
		return fmt.Errorf("request failed with status %d", response.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
