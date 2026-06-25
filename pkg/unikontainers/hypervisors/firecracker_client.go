// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hypervisors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// firecrackerClient drives a running Firecracker process over its HTTP-over-Unix
// API socket: it waits for the socket, configures the microVM and starts the
// guest. It replaces the old "--config-file" flow, where the same configuration
// was written to a JSON file and Firecracker booted on its own.
//
// This type is the API driver only; starting (and owning) the Firecracker
// process is handled by the caller (see Exec).
type firecrackerClient struct {
	socketPath string
	httpClient *http.Client
}

// newFirecrackerClient returns a client that talks to the Firecracker API socket
// at socketPath. The HTTP transport dials that Unix socket for every request, so
// "http://localhost/<endpoint>" requests actually travel over the socket.
func newFirecrackerClient(socketPath string) *firecrackerClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &firecrackerClient{
		socketPath: socketPath,
		httpClient: &http.Client{Transport: transport},
	}
}

// waitForSocket blocks until the Firecracker API socket exists and accepts
// connections, or the timeout elapses. Firecracker creates the socket shortly
// after it starts, so the caller polls here before sending any configuration.
func (c *firecrackerClient) waitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", c.socketPath, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return fmt.Errorf("firecracker API socket %q not ready within %s: %w", c.socketPath, timeout, lastErr)
}

// configure sends the full microVM configuration (the same values that used to
// be written to the JSON config file) over the API socket, in the order
// Firecracker expects, before the guest is started.
func (c *firecrackerClient) configure(ctx context.Context, cfg *FirecrackerConfig) error {
	if err := c.put(ctx, "/machine-config", cfg.Machine); err != nil {
		return err
	}
	if err := c.put(ctx, "/boot-source", cfg.Source); err != nil {
		return err
	}
	for _, drive := range cfg.Drives {
		if err := c.put(ctx, "/drives/"+drive.DriveID, drive); err != nil {
			return err
		}
	}
	for _, iface := range cfg.NetIfs {
		if err := c.put(ctx, "/network-interfaces/"+iface.IfaceID, iface); err != nil {
			return err
		}
	}
	// vsock is only configured when vAccel-over-vsock is enabled (uds_path set).
	if cfg.VSock.UDSPath != "" {
		if err := c.put(ctx, "/vsock", cfg.VSock); err != nil {
			return err
		}
	}
	return nil
}

// startGuest issues the InstanceStart action, which powers on the microVM and
// boots the guest. Firecracker allows this to succeed only once.
func (c *firecrackerClient) startGuest(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// put marshals body to JSON and sends it as an HTTP PUT to the given API path
// over the Unix socket, returning an error for any non-2xx response.
func (c *firecrackerClient) put(ctx context.Context, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send %s request: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned HTTP %d: %s", path, resp.StatusCode, string(respBody))
	}
	return nil
}
