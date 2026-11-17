// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// dialTimeout bounds connecting to the control socket, and requestTimeout bounds
// the whole send-request/read-response exchange once connected. The daemon is
// local and serves control commands sequentially (one connection at a time), so
// a command can legitimately wait behind another in-flight one. OpAdd now makes a
// GetRemoteAccessSession call inside the handler, so requestTimeout is 10s: the
// resolve itself is bounded by the daemon's tighter resolveTimeout, so a healthy
// add stays under this ceiling while a wedged or absent daemon still cannot hang
// a CLI command for long. The dial itself is still local and fast.
const (
	dialTimeout    = 5 * time.Second
	requestTimeout = 10 * time.Second
)

// DaemonClient sends a single control request to the daemon over its Unix-domain
// socket and returns the daemon's response.
type DaemonClient struct {
	socketPath string
}

// NewDaemonClient builds a control client targeting the daemon's socket.
func NewDaemonClient(socketPath string) *DaemonClient {
	return &DaemonClient{socketPath: socketPath}
}

// Send opens a connection, writes req, reads one response, closes. Every dial
// failure wraps ErrDaemonNotRunning: no daemon listens at this path. The error
// text keeps the path and the cause.
func (c *DaemonClient) Send(req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, dialTimeout)
	if err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrDaemonNotRunning, err)
	}
	defer conn.Close()

	// Bound the whole send+receive cycle: only the dial was timed out above, so
	// without this a daemon that accepts the connection but never replies (e.g. a
	// hung handler) would block the decode below forever. This mirrors the
	// per-connection deadline the server sets on its side.
	if err := conn.SetDeadline(time.Now().Add(requestTimeout)); err != nil {
		return Response{}, fmt.Errorf(wording.ControlSetDeadlineFormat, err)
	}

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, fmt.Errorf(wording.ControlSendFormat, err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return Response{}, fmt.Errorf(wording.ControlReadReplyFormat, err)
	}
	return resp, nil
}

// ErrDaemonNotRunning wraps every dial failure: no daemon listens at the socket
// path. Match it with errors.Is.
var ErrDaemonNotRunning = errors.New("daemon not running")
