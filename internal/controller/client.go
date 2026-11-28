package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds controller HTTP requests when the caller's context has no deadline.
const DefaultTimeout = 10 * time.Second

const maxResponseBytes = 4 << 20

// Client talks to a controller-v1 server over HTTP+JSON.
type Client struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Health calls GET /v1/health.
func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var out HealthResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/health", false, nil, &out); err != nil {
		return HealthResponse{}, err
	}
	return out, nil
}

// PutState calls PUT /v1/hosts/{id}/state with the host bearer token.
func (c *Client) PutState(ctx context.Context, id string, req PutStateRequest) error {
	if id == "" {
		return errors.New("controller: host id is required")
	}
	if req.ID == "" {
		req.ID = id
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("controller: encode put state: %w", err)
	}
	return c.doJSON(ctx, http.MethodPut, "/v1/hosts/"+url.PathEscape(id)+"/state", true, body, nil)
}

// Hosts calls GET /v1/hosts with the client bearer token.
func (c *Client) Hosts(ctx context.Context) (HostsResponse, error) {
	var out HostsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/hosts", true, nil, &out); err != nil {
		return HostsResponse{}, err
	}
	return out, nil
}

// Usage calls GET /v1/usage with the client bearer token.
func (c *Client) Usage(ctx context.Context) (UsageResponse, error) {
	var out UsageResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/usage", true, nil, &out); err != nil {
		return UsageResponse{}, err
	}
	return out, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, auth bool, reqBody []byte, dst any) error {
	base := strings.TrimRight(strings.TrimSpace(c.URL), "/")
	if base == "" {
		return errors.New("controller: URL is required")
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}

	var bodyReader io.Reader
	if reqBody != nil {
		bodyReader = bytes.NewReader(reqBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, bodyReader)
	if err != nil {
		return fmt.Errorf("controller: build request: %w", err)
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth && c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = defaultHTTPClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("controller: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("controller: read %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var er ErrorResponse
		if json.Unmarshal(respBytes, &er) == nil && (er.Error != "" || er.Message != "") {
			return &APIError{Status: resp.StatusCode, Code: er.Error, Message: er.Message}
		}
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(respBytes))}
	}

	if dst != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.Unmarshal(respBytes, dst); err != nil {
			return fmt.Errorf("controller: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

var defaultHTTPClient = &http.Client{Timeout: DefaultTimeout}
