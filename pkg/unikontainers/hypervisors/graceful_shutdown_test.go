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
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tempSocketPath returns a short path under the system temp dir for a unix
// socket, avoiding the ~108 byte sun_path limit that long t.TempDir()/subtest
// names can blow past. The directory is removed when the test finishes.
func tempSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gs")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// qmpFakeServer is a minimal line-JSON QMP server over a unix socket. It sends
// the greeting on accept, answers qmp_capabilities with {"return":{}}, and on
// system_powerdown emits an async {"event":"POWERDOWN"} followed by the
// command's own {"return":{}} (modelling the interleaving real QEMU does,
// fact G29). It records the commands it receives, in order.
type qmpFakeServer struct {
	sockPath string
	listener net.Listener

	mu       sync.Mutex
	commands []string

	// When powerdownError is true, the server answers system_powerdown with a
	// QMP {"error":...} object instead of a return.
	powerdownError bool
}

func newQMPFakeServer(t *testing.T, powerdownError bool) *qmpFakeServer {
	t.Helper()
	sockPath := tempSocketPath(t, "qmp.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	s := &qmpFakeServer{
		sockPath:       sockPath,
		listener:       ln,
		powerdownError: powerdownError,
	}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *qmpFakeServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	// Real QEMU sends the greeting unprompted right after accept (fact G26).
	_ = enc.Encode(map[string]any{
		"QMP": map[string]any{
			"version":      map[string]any{},
			"capabilities": []string{},
		},
	})

	for {
		var cmd map[string]any
		if err := dec.Decode(&cmd); err != nil {
			return
		}
		execute, _ := cmd["execute"].(string)

		s.mu.Lock()
		s.commands = append(s.commands, execute)
		s.mu.Unlock()

		switch execute {
		case "qmp_capabilities":
			_ = enc.Encode(map[string]any{"return": map[string]any{}})
		case "system_powerdown":
			if s.powerdownError {
				_ = enc.Encode(map[string]any{
					"error": map[string]any{"class": "GenericError", "desc": "boom"},
				})
				continue
			}
			// Async event first, command return second (fact G29). A correct
			// client must skip the event and keep reading until the return.
			_ = enc.Encode(map[string]any{
				"timestamp": map[string]any{"seconds": 0, "microseconds": 0},
				"event":     "POWERDOWN",
			})
			_ = enc.Encode(map[string]any{"return": map[string]any{}})
		default:
			_ = enc.Encode(map[string]any{"return": map[string]any{}})
		}
	}
}

func (s *qmpFakeServer) recordedCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.commands))
	copy(out, s.commands)
	return out
}

func TestQemuRequestGuestShutdown(t *testing.T) {
	t.Run("negotiates capabilities then powers down, tolerating the event", func(t *testing.T) {
		s := newQMPFakeServer(t, false)

		q := &Qemu{}
		err := q.RequestGuestShutdown(s.sockPath)
		assert.NoError(t, err)

		assert.Equal(t,
			[]string{"qmp_capabilities", "system_powerdown"},
			s.recordedCommands(),
			"client must send qmp_capabilities before system_powerdown",
		)
	})

	t.Run("surfaces a QMP error object as an error", func(t *testing.T) {
		s := newQMPFakeServer(t, true)

		q := &Qemu{}
		err := q.RequestGuestShutdown(s.sockPath)
		assert.Error(t, err)
	})
}

// TestQemuClientDrainsPowerdownReturn proves the QMP client reads until it sees
// the command's own "return", consuming it off the wire, instead of stopping at
// the async POWERDOWN event. net.Pipe is fully synchronous: every server write
// blocks until the client reads it. A single-read client (one that treated the
// event as the reply and left the return unread) would leave the server's
// return write pending and deadlock the follow-up command; a correct
// read-until-return client drains it and lets the follow-up complete. The
// deadlines turn that deadlock into a clean, prompt failure rather than a hang.
func TestQemuGuestShutdownDrainsPowerdownReturn(t *testing.T) {
	t.Parallel()

	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	require.NoError(t, clientConn.SetDeadline(time.Now().Add(3*time.Second)))
	require.NoError(t, serverConn.SetDeadline(time.Now().Add(3*time.Second)))

	done := make(chan error, 1)
	go func() {
		defer clientConn.Close()
		enc := json.NewEncoder(clientConn)
		dec := json.NewDecoder(clientConn)
		if err := qmpCommand(enc, dec, "system_powerdown"); err != nil {
			done <- err
			return
		}
		// Reachable only if the powerdown "return" was drained; otherwise it
		// deadlocks against the server's still-pending return write.
		done <- qmpCommand(enc, dec, "query-status")
	}()

	senc := json.NewEncoder(serverConn)
	sdec := json.NewDecoder(serverConn)

	var cmd map[string]any
	require.NoError(t, sdec.Decode(&cmd))
	assert.Equal(t, "system_powerdown", cmd["execute"])
	// Async event first, then the command's own return (fact G29).
	require.NoError(t, senc.Encode(map[string]any{"event": "POWERDOWN"}))
	require.NoError(t, senc.Encode(map[string]any{"return": map[string]any{}}))

	require.NoError(t, sdec.Decode(&cmd),
		"follow-up command must arrive, proving the powerdown return was drained")
	assert.Equal(t, "query-status", cmd["execute"])
	require.NoError(t, senc.Encode(map[string]any{"return": map[string]any{}}))

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("qmp client did not drain the powerdown return; follow-up command deadlocked")
	}
}

// clhFakeServer is a fake Cloud Hypervisor REST server speaking HTTP over a
// unix socket. It records the method and path of every request and answers
// with a configured status code.
type clhFakeServer struct {
	sockPath string

	mu       sync.Mutex
	requests [][2]string // {method, path}
}

func newCLHFakeServer(t *testing.T, status int) *clhFakeServer {
	t.Helper()
	sockPath := tempSocketPath(t, "ch.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	s := &clhFakeServer{sockPath: sockPath}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.requests = append(s.requests, [2]string{r.Method, r.URL.Path})
			s.mu.Unlock()
			w.WriteHeader(status)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

func (s *clhFakeServer) recordedRequests() [][2]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][2]string, len(s.requests))
	copy(out, s.requests)
	return out
}

func TestCloudHypervisorRequestGuestShutdown(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"204 succeeds", http.StatusNoContent, false},
		{"500 fails", http.StatusInternalServerError, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newCLHFakeServer(t, tt.status)

			ch := &CloudHypervisor{}
			err := ch.RequestGuestShutdown(s.sockPath)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			assert.Equal(t,
				[][2]string{{http.MethodPut, "/api/v1/vm.power-button"}},
				s.recordedRequests(),
				"expected exactly one PUT /api/v1/vm.power-button",
			)
		})
	}
}

func TestSupportsGuestShutdown(t *testing.T) {
	t.Parallel()

	assert.True(t, (&Qemu{}).SupportsGuestShutdown(), "qemu supports guest shutdown")
	assert.True(t, (&CloudHypervisor{}).SupportsGuestShutdown(), "cloud hypervisor supports guest shutdown")
	assert.False(t, (&HVT{}).SupportsGuestShutdown(), "hvt does not support guest shutdown")
	assert.False(t, (&SPT{}).SupportsGuestShutdown(), "spt does not support guest shutdown")
	assert.False(t, (&Hedge{}).SupportsGuestShutdown(), "hedge does not support guest shutdown")
	assert.Equal(t, runtime.GOARCH == "amd64", (&Firecracker{}).SupportsGuestShutdown(),
		"firecracker supports guest shutdown only on amd64")
}
