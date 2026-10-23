package api

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is the HTTP client for the control plane.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func New(server, apiKey string, insecure bool) *Client {
	transport := &http.Transport{}
	if strings.HasPrefix(server, "https://") && insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return &Client{
		baseURL: server,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 10 * time.Second, Transport: transport},
	}
}

// RegisterRequest is the POST /api/v1/nodes body.
type RegisterRequest struct {
	NodeID        string `json:"node_id"`
	CapacityBytes int64  `json:"capacity_bytes"`
	Zone          string `json:"zone"`
	IP            string `json:"ip"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
}

// VolumeConfig is the volume server configuration returned by the server.
type VolumeConfig struct {
	Master     string `json:"master"` // master address, e.g. "192.168.1.9:9333"
	DataCenter string `json:"data_center"`
	Rack       string `json:"rack"`
	MaxVolumes int    `json:"max_volumes"`
	DiskType   string `json:"disk_type"`
	WeedURL    string `json:"weed_url"` // binary URL; empty = use the local binary
	VolumePort int    `json:"volume_port"`
}

// RegisterResponse is the POST /api/v1/nodes response.
type RegisterResponse struct {
	NodeID  string       `json:"node_id"`
	Volume  VolumeConfig `json:"volume"`
	Warning string       `json:"warning,omitempty"`
}

// Register registers the node and returns the volume server configuration.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var out RegisterResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/nodes", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HeartbeatRequest is the POST /api/v1/nodes/{id}/heartbeat body.
type HeartbeatRequest struct {
	Status            string `json:"status"`
	DiskFreeBytes     int64  `json:"disk_free"`
	DiskUsedBytes     int64  `json:"disk_used"`
	VolumeServerAlive bool   `json:"volume_server_alive"`
	VolumesCount      int    `json:"volumes_count"`
	EcShardsCount     int    `json:"ec_shards_count"`
}

// Heartbeat sends the node's heartbeat.
func (c *Client) Heartbeat(ctx context.Context, nodeID string, req HeartbeatRequest) error {
	resp := &struct {
		Error string `json:"error"`
	}{}
	path := fmt.Sprintf("/api/v1/nodes/%s/heartbeat", nodeID)
	if err := c.do(ctx, http.MethodPost, path, req, resp); err != nil {
		return err
	}
	if resp.Error != "" {
		return fmt.Errorf("server: %s", resp.Error)
	}
	return nil
}

// UpdateConfig fetches the updated volume configuration (GET /api/v1/nodes/{id}/config).
func (c *Client) UpdateConfig(ctx context.Context, nodeID string) (*VolumeConfig, error) {
	out := &struct {
		Volume VolumeConfig `json:"volume"`
	}{}
	path := fmt.Sprintf("/api/v1/nodes/%s/config", nodeID)
	if err := c.do(ctx, http.MethodGet, path, nil, out); err != nil {
		return nil, err
	}
	return &out.Volume, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
