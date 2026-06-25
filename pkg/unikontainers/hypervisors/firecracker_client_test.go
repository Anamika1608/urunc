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
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordedRequest captures one API call the fake Firecracker received.
type recordedRequest struct {
	Method string
	Path   string
	Body   []byte
}

// fakeFirecracker is an HTTP server listening on a Unix socket that records the
// requests it receives. It stands in for a real Firecracker API socket so the
// client can be tested without KVM or a real firecracker binary.
type fakeFirecracker struct {
	socketPath string
	server     *http.Server
	mu         sync.Mutex
	requests   []recordedRequest
	status     int // status code to return (defaults to 204, like real Firecracker)
}

func startFakeFirecracker(t *testing.T, status int) *fakeFirecracker {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "fc.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket: %v", err)
	}
	f := &fakeFirecracker{socketPath: socketPath, status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: body})
		f.mu.Unlock()
		w.WriteHeader(f.status)
	})
	f.server = &http.Server{Handler: mux}
	go func() { _ = f.server.Serve(ln) }()
	t.Cleanup(func() { _ = f.server.Close() })
	return f
}

func (f *fakeFirecracker) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

func TestFirecrackerClient_WaitForSocket(t *testing.T) {
	fake := startFakeFirecracker(t, http.StatusNoContent)

	// A live socket should be detected quickly.
	c := newFirecrackerClient(fake.socketPath)
	if err := c.waitForSocket(time.Second); err != nil {
		t.Fatalf("waitForSocket on a ready socket: %v", err)
	}

	// A path with no listener should time out with an error.
	missing := newFirecrackerClient(filepath.Join(t.TempDir(), "missing.sock"))
	if err := missing.waitForSocket(100 * time.Millisecond); err == nil {
		t.Fatal("expected waitForSocket to fail for a missing socket")
	}
}

func TestFirecrackerClient_Configure(t *testing.T) {
	fake := startFakeFirecracker(t, http.StatusNoContent)
	c := newFirecrackerClient(fake.socketPath)

	cfg := &FirecrackerConfig{
		Machine: FirecrackerMachine{VcpuCount: 2, MemSizeMiB: 512},
		Source:  FirecrackerBootSource{ImagePath: "/k/vmlinux", BootArgs: "console=ttyS0"},
		Drives:  []FirecrackerDrive{{DriveID: "rootfs", IsRootDev: true, HostPath: "/r/rootfs.ext4"}},
		NetIfs:  []FirecrackerNet{{IfaceID: "net1", HostIF: "tap0", GuestMAC: "06:00:AC:10:00:02"}},
	}

	if err := c.configure(context.Background(), cfg); err != nil {
		t.Fatalf("configure: %v", err)
	}

	got := fake.recorded()
	// vsock is skipped because UDSPath is empty, so we expect exactly four PUTs,
	// in the order Firecracker requires.
	wantPaths := []string{"/machine-config", "/boot-source", "/drives/rootfs", "/network-interfaces/net1"}
	if len(got) != len(wantPaths) {
		t.Fatalf("got %d requests, want %d: %+v", len(got), len(wantPaths), got)
	}
	for i, want := range wantPaths {
		if got[i].Method != http.MethodPut {
			t.Errorf("request %d: method = %s, want PUT", i, got[i].Method)
		}
		if got[i].Path != want {
			t.Errorf("request %d: path = %s, want %s", i, got[i].Path, want)
		}
	}

	// The config values must survive the trip as JSON.
	var machine FirecrackerMachine
	if err := json.Unmarshal(got[0].Body, &machine); err != nil {
		t.Fatalf("unmarshal machine-config body: %v", err)
	}
	if machine.VcpuCount != 2 || machine.MemSizeMiB != 512 {
		t.Errorf("machine-config body = %+v, want vcpu=2 mem=512", machine)
	}

	var boot FirecrackerBootSource
	if err := json.Unmarshal(got[1].Body, &boot); err != nil {
		t.Fatalf("unmarshal boot-source body: %v", err)
	}
	if boot.ImagePath != "/k/vmlinux" {
		t.Errorf("boot-source kernel = %q, want /k/vmlinux", boot.ImagePath)
	}
}

func TestFirecrackerClient_StartGuest(t *testing.T) {
	fake := startFakeFirecracker(t, http.StatusNoContent)
	c := newFirecrackerClient(fake.socketPath)

	if err := c.startGuest(context.Background()); err != nil {
		t.Fatalf("startGuest: %v", err)
	}

	got := fake.recorded()
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1: %+v", len(got), got)
	}
	if got[0].Method != http.MethodPut || got[0].Path != "/actions" {
		t.Errorf("got %s %s, want PUT /actions", got[0].Method, got[0].Path)
	}

	var action map[string]string
	if err := json.Unmarshal(got[0].Body, &action); err != nil {
		t.Fatalf("unmarshal action body: %v", err)
	}
	if action["action_type"] != "InstanceStart" {
		t.Errorf("action_type = %q, want InstanceStart", action["action_type"])
	}
}

func TestFirecrackerClient_PutSurfacesHTTPError(t *testing.T) {
	// A real Firecracker returns 4xx with a fault message on bad input; the
	// client must turn that into an error rather than silently succeeding.
	fake := startFakeFirecracker(t, http.StatusBadRequest)
	c := newFirecrackerClient(fake.socketPath)

	if err := c.startGuest(context.Background()); err == nil {
		t.Fatal("expected an error for a non-2xx response, got nil")
	}
}
