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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

const testFCBinary = "/usr/bin/firecracker"

// TestFirecrackerBuildExecCmdSocket verifies that Firecracker enables its API
// socket only when a socket_path is configured. With no configured path it
// restores the upstream launch mode (--no-api --config-file), which boots the
// guest from the config file without exposing a control socket.
func TestFirecrackerBuildExecCmdSocket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           types.ExecArgs
		mustContain    []string
		mustNotContain []string
	}{
		{
			name: "configured SocketPath renders --api-sock and keeps --config-file",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
				SocketPath:    "/run/urunc/fc.sock",
			},
			mustContain:    []string{"--api-sock /run/urunc/fc.sock", "--config-file"},
			mustNotContain: []string{"--no-api"},
		},
		{
			name: "unset SocketPath restores --no-api and omits --api-sock",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
				ContainerID:   "abc123",
			},
			mustContain:    []string{"--no-api", "--config-file"},
			mustNotContain: []string{"--api-sock"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fc := &Firecracker{binary: FirecrackerBinary, binaryPath: testFCBinary}
			out, err := fc.BuildExecCmd(tt.args, &fakeUnikernel{})
			assert.NoError(t, err)
			assert.NotEmpty(t, out)

			assert.Equal(t, testFCBinary, out[0], "binary path must be the first element")
			joined := strings.Join(out, " ")

			for _, want := range tt.mustContain {
				assert.Contains(t, joined, want, "expected %q to be present", want)
			}
			for _, notWant := range tt.mustNotContain {
				assert.NotContains(t, joined, notWant, "expected %q to be absent", notWant)
			}
		})
	}
}
