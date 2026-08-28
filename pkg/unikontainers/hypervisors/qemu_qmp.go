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
	"fmt"
	"net"
	"time"
)

// qmpDeadline bounds the whole QMP exchange so a stuck monitor cannot block
// the kill path indefinitely.
const qmpDeadline = 5 * time.Second

// SupportsGuestShutdown reports that QEMU can shut the guest down gracefully
// via the QMP system_powerdown command.
func (q *Qemu) SupportsGuestShutdown() bool {
	return true
}

// RequestGuestShutdown connects to QEMU's QMP control socket and asks the
// guest to power down. socketPath is the already-resolved, host-reachable
// path; it is dialed directly. QMP requires a capabilities handshake before
// any command, and system_powerdown's reply is interleaved with an async
// POWERDOWN event, so each response is read until its own "return" arrives.
func (q *Qemu) RequestGuestShutdown(socketPath string) error {
	// Bound the whole attempt (dial plus exchange) by a single absolute
	// deadline, so the worst case stays within one qmpDeadline budget rather
	// than dial timeout plus exchange timeout stacking on top of each other.
	deadline := time.Now().Add(qmpDeadline)

	conn, err := net.DialTimeout("unix", socketPath, time.Until(deadline))
	if err != nil {
		return fmt.Errorf("%w %q: %w", ErrShutdownConnect, socketPath, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("failed to set QMP socket deadline: %w", err)
	}

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	// QEMU sends the QMP greeting unprompted right after connect.
	var greeting map[string]json.RawMessage
	if err := dec.Decode(&greeting); err != nil {
		return fmt.Errorf("%w: %w", ErrShutdownGreeting, err)
	}

	// Capabilities negotiation is mandatory before any other command.
	if err := qmpCommand(enc, dec, "qmp_capabilities", ErrShutdownHandshake); err != nil {
		return err
	}

	// Ask the guest to power down (ACPI power button).
	return qmpCommand(enc, dec, "system_powerdown", ErrShutdownCommand)
}

// qmpCommand sends a QMP command with no arguments and waits for its return.
// stage names which step of the shutdown request this call serves
// (qmp_capabilities is the handshake, system_powerdown is the command
// itself), so a write or read failure can be wrapped with the right one.
func qmpCommand(enc *json.Encoder, dec *json.Decoder, command string, stage error) error {
	if err := enc.Encode(map[string]string{"execute": command}); err != nil {
		return fmt.Errorf("%w: failed to send QMP command %q: %w", stage, command, err)
	}
	return qmpReadReturn(dec, command, stage)
}

// qmpReadReturn reads QMP messages until it sees the command's own reply. Any
// async "event" message is skipped; a "return" ends the wait successfully. A
// read failure means the monitor never answered this stage, so it is wrapped
// with stage; an "error" object means the monitor answered and rejected the
// command, so that is wrapped with ErrShutdownRefused instead.
func qmpReadReturn(dec *json.Decoder, command string, stage error) error {
	for {
		var msg map[string]json.RawMessage
		if err := dec.Decode(&msg); err != nil {
			return fmt.Errorf("%w: failed to read QMP response for %q: %w", stage, command, err)
		}
		if errObj, ok := msg["error"]; ok {
			return fmt.Errorf("%w: QMP command %q failed: %s", ErrShutdownRefused, command, string(errObj))
		}
		if _, ok := msg["return"]; ok {
			return nil
		}
		// Any other message (typically an async "event") is skipped until the
		// command's own "return" arrives.
	}
}
