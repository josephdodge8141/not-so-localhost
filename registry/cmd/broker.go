// Package main runs the distributed Not-So-Localhost app registry.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// BrokerClient provisions exact DNS and tunnel routes for one node.
type BrokerClient struct {
	baseURL        string
	credentialFile string
	nodeSlug       string
	http           *http.Client
	authClaimToken string
}

// NewBrokerClient creates a node-authenticated enrollment broker client.
func NewBrokerClient(baseURL, credentialFile, nodeSlug string) *BrokerClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil
	}
	return &BrokerClient{
		baseURL: baseURL, credentialFile: credentialFile, nodeSlug: nodeSlug,
		http: &http.Client{Timeout: 30 * time.Second}, authClaimToken: os.Getenv("NSL_AUTH_CLAIM_TOKEN"),
	}
}

// EnsureApp provisions one app hostname for this node.
func (c *BrokerClient) EnsureApp(ctx context.Context, appSlug string) error {
	return c.put(ctx, fmt.Sprintf("/v1/nodes/%s/apps/%s", url.PathEscape(c.nodeSlug), url.PathEscape(appSlug)))
}

// SyncApps makes the broker's app hostname set match appSlugs.
func (c *BrokerClient) SyncApps(ctx context.Context, appSlugs []string) error {
	body, err := json.Marshal(map[string][]string{"apps": appSlugs})
	if err != nil {
		return err
	}
	return c.putBody(ctx, fmt.Sprintf("/v1/nodes/%s/apps", url.PathEscape(c.nodeSlug)), body)
}

// EnsureAuth assigns the global auth hostname to this authorized node.
func (c *BrokerClient) EnsureAuth(ctx context.Context) error {
	return c.put(ctx, fmt.Sprintf("/v1/nodes/%s/auth", url.PathEscape(c.nodeSlug)))
}

func (c *BrokerClient) put(ctx context.Context, path string) error {
	return c.putBody(ctx, path, nil)
}

func (c *BrokerClient) putBody(ctx context.Context, path string, body []byte) error {
	credential, err := os.ReadFile(c.credentialFile)
	if err != nil {
		return fmt.Errorf("read node credential: %w", err)
	}
	endpoint := c.baseURL + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(credential)))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if strings.HasSuffix(path, "/auth") && c.authClaimToken != "" {
		request.Header.Set("X-NSL-Auth-Claim", c.authClaimToken)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("broker returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
