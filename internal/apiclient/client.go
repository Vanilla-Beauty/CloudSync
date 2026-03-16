package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/cloudsync/cloudsync/internal/ipc"
)

// Client communicates with cloudsyncd over a Unix Domain Socket.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates an API client that connects to socketPath.
func NewClient(socketPath string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   10 * time.Second,
		},
		baseURL: "http://cloudsyncd",
	}
}

// Ping checks if the daemon is running. Returns nil if running.
func (c *Client) Ping() error {
	resp, err := c.httpClient.Get(c.baseURL + "/status")
	if err != nil {
		return fmt.Errorf("daemon is not running — use 'cloudsync start'")
	}
	resp.Body.Close()
	return nil
}

// StatusResponse mirrors daemon.StatusResponse
type StatusResponse struct {
	Running    bool   `json:"running"`
	DaemonPID  int    `json:"daemon_pid"`
	MountCount int    `json:"mount_count"`
	Version    string `json:"version"`
}

// Status returns the daemon status.
func (c *Client) Status() (*StatusResponse, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/status")
	if err != nil {
		return nil, fmt.Errorf("daemon is not running — use 'cloudsync start'")
	}
	defer resp.Body.Close()
	var s StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return &s, nil
}

// ListMounts returns all active mounts.
func (c *Client) ListMounts() ([]ipc.MountRecord, error) {
	resp, err := c.httpClient.Get(c.baseURL + "/mounts")
	if err != nil {
		return nil, fmt.Errorf("daemon is not running — use 'cloudsync start'")
	}
	defer resp.Body.Close()

	var body struct {
		Mounts []ipc.MountRecord `json:"mounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode mounts: %w", err)
	}
	return body.Mounts, nil
}

// AddMount tells the daemon to start watching localPath → remotePrefix.
// Set downloadFirst=true to pull remote files before the initial upload scan.
// bucket overrides the default daemon bucket; empty string uses the default.
func (c *Client) AddMount(localPath, remotePrefix string, downloadFirst bool, bucket string) (*ipc.MountRecord, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"local_path":     localPath,
		"remote_prefix":  remotePrefix,
		"download_first": downloadFirst,
		"bucket":         bucket,
	})
	resp, err := c.httpClient.Post(c.baseURL+"/mounts", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("daemon is not running — use 'cloudsync start'")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return nil, fmt.Errorf("add mount failed: %s", e["error"])
	}
	var rec ipc.MountRecord
	if err := json.NewDecoder(resp.Body).Decode(&rec); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &rec, nil
}

// RemoveMount tells the daemon to stop watching localPath.
// Set deleteRemote=true to also delete remote objects.
func (c *Client) RemoveMount(localPath string, deleteRemote bool) error {
	body, _ := json.Marshal(map[string]interface{}{
		"local_path":    localPath,
		"delete_remote": deleteRemote,
	})
	req, _ := http.NewRequest(http.MethodDelete, c.baseURL+"/mounts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("daemon is not running — use 'cloudsync start'")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("remove mount failed: %s", e["error"])
	}
	return nil
}
