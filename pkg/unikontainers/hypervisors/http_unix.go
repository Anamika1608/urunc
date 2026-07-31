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
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// socketRequestTimeout bounds a single control-socket REST request so a stuck
// monitor cannot block the kill path indefinitely.
const socketRequestTimeout = 5 * time.Second

// unixSocketRequest performs an HTTP request to a monitor's REST API served
// over a unix socket. The socket path is dialed directly; the URL host is a
// placeholder net/http requires. A non-2xx response is returned as an error.
// body may be nil for requests without a payload.
func unixSocketRequest(socketPath, method, urlPath string, body []byte) error {
	client := &http.Client{
		Timeout: socketRequestTimeout,
		Transport: &http.Transport{
			// A single one-shot request; keep no idle unix connection in the
			// pool, so nothing lingers if this helper is ever called from a
			// long-lived process.
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://unix"+urlPath, reqBody)
	if err != nil {
		return fmt.Errorf("failed to build %s %s request: %w", method, urlPath, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s over %q failed: %w", method, urlPath, socketPath, err)
	}
	defer resp.Body.Close()

	// The body is only used to enrich an error string, so cap the read.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned status %d: %s",
			method, urlPath, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}
