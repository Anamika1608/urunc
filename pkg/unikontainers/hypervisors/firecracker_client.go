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
	"sync"
	"time"
)

// firecrackerClient drives a running Firecracker process over its HTTP-over-Unix
// API socket, one configuration resource at a time, so the caller can send each
// piece of configuration as soon as it becomes available.
//
// The client establishes a single connection in connect() and keeps it alive
// for all requests. This matters because the caller may change its root
// (pivot_root/chroot) between configuration stages: the socket path stops
// being resolvable from the new root, but the already-open connection keeps
// working, since open file descriptors survive a root change.
//
// This type is the API driver only; starting (and owning) the Firecracker
// process is handled by the caller (see SpawnSocketVMM).
type firecrackerClient struct {
	socketPath string
	httpClient *http.Client

	dialMu   sync.Mutex
	heldConn net.Conn
}

// newFirecrackerClient returns a client that talks to the Firecracker API socket
// at socketPath. "http://localhost/<endpoint>" requests actually travel over the
// socket. Call connect() before issuing any request.
func newFirecrackerClient(socketPath string) *firecrackerClient {
	c := &firecrackerClient{socketPath: socketPath}
	transport := &http.Transport{
		DialContext: c.dialContext,
		// Keep the single connection alive for the whole staged boot: it is
		// established before the caller may change root, and the socket path
		// is not resolvable afterwards, so an idle close between stages
		// would break every later stage.
		IdleConnTimeout:     0,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
	}
	c.httpClient = &http.Client{Transport: transport}
	return c
}

// dialContext hands the transport the connection pre-established by connect(),
// and falls back to dialing the socket path directly if that connection was
// already consumed (which only works while the path is still resolvable).
func (c *firecrackerClient) dialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()
	if c.heldConn != nil {
		conn := c.heldConn
		c.heldConn = nil
		return conn, nil
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.socketPath)
}

// connect blocks until the Firecracker API socket exists and accepts
// connections, or the timeout elapses. Firecracker creates the socket shortly
// after it starts. The successful connection is kept and reused for all
// requests (see the type comment for why).
func (c *firecrackerClient) connect(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", c.socketPath, 50*time.Millisecond)
		if err == nil {
			c.dialMu.Lock()
			c.heldConn = conn
			c.dialMu.Unlock()
			return nil
		}
		lastErr = err
		// Firecracker binds the socket within ~1ms of starting, so poll
		// tightly: a 1ms interval captures nearly all of the readiness
		// latency a coarser interval would waste, without busy-spinning.
		time.Sleep(1 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = context.DeadlineExceeded
	}
	return fmt.Errorf("firecracker API socket %q not ready within %s: %w", c.socketPath, timeout, lastErr)
}

// putMachineConfig configures vCPUs and memory. This needs nothing but the
// container spec, so it can be sent as the very first stage.
func (c *firecrackerClient) putMachineConfig(ctx context.Context, machine FirecrackerMachine) error {
	return c.put(ctx, "/machine-config", machine)
}

// putNetworkIface attaches one network interface. Firecracker opens the tap
// device during this call, so it must not be sent before the tap exists.
func (c *firecrackerClient) putNetworkIface(ctx context.Context, iface FirecrackerNet) error {
	return c.put(ctx, "/network-interfaces/"+iface.IfaceID, iface)
}

// putDrives attaches the guest's block devices.
func (c *firecrackerClient) putDrives(ctx context.Context, drives []FirecrackerDrive) error {
	for _, drive := range drives {
		if err := c.put(ctx, "/drives/"+drive.DriveID, drive); err != nil {
			return err
		}
	}
	return nil
}

// putBootSource configures the kernel image, boot arguments and initrd.
func (c *firecrackerClient) putBootSource(ctx context.Context, source FirecrackerBootSource) error {
	return c.put(ctx, "/boot-source", source)
}

// putVSock configures the vsock device. No-op when unset (empty uds_path).
func (c *firecrackerClient) putVSock(ctx context.Context, vsock FirecrackerVSockDev) error {
	if vsock.UDSPath == "" {
		return nil
	}
	return c.put(ctx, "/vsock", vsock)
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
