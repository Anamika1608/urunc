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

//go:build integration

package hypervisors

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe buffer; firecracker's serial output is written
// from an os/exec goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestFirecrackerSocketBoot_Integration is the Phase-1 socket PoC. It uses the
// real firecrackerClient to drive a REAL Firecracker over its api-sock and boot
// a real guest — proving the runtime reaches the socket and interacts with the
// guest, end to end.
//
// Requirements (the test skips otherwise):
//   - /dev/kvm present and accessible (so run under sudo)
//   - `firecracker` in PATH
//   - FC_KERNEL and FC_ROOTFS env vars pointing at a guest kernel + ext4 rootfs
//
// Run (inside the VM):
//
//	sudo env FC_KERNEL=/home/anamika/fc/vmlinux-6.1.174 \
//	         FC_ROOTFS=/home/anamika/fc/ubuntu-24.04.ext4 \
//	         /usr/local/go/bin/go test -tags integration -v \
//	         -run TestFirecrackerSocketBoot_Integration ./pkg/unikontainers/hypervisors/
func TestFirecrackerSocketBoot_Integration(t *testing.T) {
	kernel := os.Getenv("FC_KERNEL")
	rootfs := os.Getenv("FC_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("set FC_KERNEL and FC_ROOTFS to run the socket-boot integration test")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not available: %v", err)
	}
	fcPath, err := exec.LookPath("firecracker")
	if err != nil {
		t.Skip("firecracker not found in PATH")
	}

	socketPath := filepath.Join(t.TempDir(), "fc.sock")

	// 1) Start REAL Firecracker in api-sock mode, capturing its serial console.
	serial := &syncBuffer{}
	cmd := exec.Command(fcPath, "--api-sock", socketPath, "--no-seccomp")
	cmd.Stdout = serial
	cmd.Stderr = serial
	if err := cmd.Start(); err != nil {
		t.Fatalf("start firecracker: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// 2) Drive it with urunc's client — the same code the runtime will use.
	client := newFirecrackerClient(socketPath)
	if err := client.waitForSocket(5 * time.Second); err != nil {
		t.Fatalf("waitForSocket: %v\n--- firecracker output ---\n%s", err, serial.String())
	}

	cfg := &FirecrackerConfig{
		Machine: FirecrackerMachine{VcpuCount: 1, MemSizeMiB: 512},
		Source: FirecrackerBootSource{
			ImagePath: kernel,
			BootArgs:  "console=ttyS0 reboot=k panic=1 pci=off",
		},
		Drives: []FirecrackerDrive{{
			DriveID:   "rootfs",
			IsRootDev: true,
			HostPath:  rootfs,
		}},
	}

	ctx := context.Background()
	if err := client.configure(ctx, cfg); err != nil {
		t.Fatalf("configure over socket: %v\n--- firecracker output ---\n%s", err, serial.String())
	}
	if err := client.startGuest(ctx); err != nil {
		t.Fatalf("startGuest (InstanceStart): %v\n--- firecracker output ---\n%s", err, serial.String())
	}

	// 3) Prove the guest actually booted by watching the serial console for a
	//    kernel/userspace boot marker.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out := serial.String()
		if strings.Contains(out, "Linux version") || strings.Contains(out, "login:") {
			t.Logf("guest booted over the api-sock\n--- serial (tail) ---\n%s", tailString(out, 1500))
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("guest did not show a boot marker within timeout\n--- firecracker output ---\n%s", serial.String())
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
