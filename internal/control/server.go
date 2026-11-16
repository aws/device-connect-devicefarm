// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/aws/device-connect-devicefarm/internal/wording"
)

// connDeadline bounds the whole read-handle-write cycle for one control
// connection. Connections are served inline (the device state is the
// serialization point), so without a deadline a client that connects and never
// finishes writing its request would block the accept loop and wedge every
// subsequent control command for the life of the daemon. The OpAdd handler
// resolves the session and fetches the device serial inline; both sit under the
// daemon's tighter resolveTimeout, leaving room for the local read and write, and
// matching the client's requestTimeout. It is a package var, not a const, so tests
// can shrink it to exercise the hung-client path quickly.
var connDeadline = 10 * time.Second

// chmod sets the bound socket's mode. It is a package var so a test can make the
// mode change fail and prove Listen then serves nothing.
var chmod = os.Chmod

// Handler processes one control request and returns the response to send back.
// The daemon supplies it; keeping the transport ignorant of operation semantics
// lets the daemon own the device state and decide which ops stop it.
type Handler func(Request) Response

// LocalSocketServer accepts control connections on a Unix-domain socket and dispatches
// each request to a Handler. Binding (Listen) is split from the accept loop
// (Serve) so the daemon can bind the socket before announcing readiness, which
// removes the race where the CLI connects before the listener exists.
type LocalSocketServer struct {
	socketPath string
	logger     *slog.Logger
	listener   net.Listener
}

// NewLocalSocketServer builds a control server bound to socketPath once Listen is
// called. A nil logger defaults to slog.Default.
func NewLocalSocketServer(socketPath string, logger *slog.Logger) *LocalSocketServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &LocalSocketServer{socketPath: socketPath, logger: logger}
}

// Listen binds the Unix-domain socket and sets its mode to 0600. A stale socket
// file from a daemon that exited uncleanly is removed first so binding does not
// fail with "address already in use"; this is safe because RunningPID has already
// confirmed no live daemon owns it. Call Serve to start accepting, and Close to
// release the socket.
func (s *LocalSocketServer) Listen() error {
	if err := removeStaleSocket(s.socketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on control socket %q: %w", s.socketPath, err)
	}
	// net.Listen creates the socket file under the umask (0755 with the usual
	// 022), and connect(2) needs write permission on it. The 0700 runtime dir is
	// the first gate; 0600 on the file is the second. listener.Close unlinks the
	// file on failure.
	if err := chmod(s.socketPath, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("chmod control socket %q: %w", s.socketPath, err)
	}
	s.listener = listener
	return nil
}

// Serve accepts control connections until ctx is cancelled, dispatching each to
// handler. It returns nil on a clean shutdown (ctx cancelled). One bad connection
// is logged and never takes the loop down.
func (s *LocalSocketServer) Serve(ctx context.Context, handler Handler) error {
	go func() {
		<-ctx.Done()
		s.listener.Close()
	}()

	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on control socket: %w", err)
		}
		s.handleConn(conn, handler)
	}
}

// Close stops the listener, which unlinks the socket file. Serve closes the
// listener on ctx cancel, so a later Close sees net.ErrClosed and treats it as
// done. Nil before Listen.
func (s *LocalSocketServer) Close() error {
	if s.listener == nil {
		return nil
	}
	if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close control socket %q: %w", s.socketPath, err)
	}
	return nil
}

// handleConn reads one request, invokes the handler, and writes one response.
// Control commands are sequential and trivially cheap, so connections are served
// inline rather than per-goroutine; a malformed request gets an error response
// and the connection is closed.
func (s *LocalSocketServer) handleConn(conn net.Conn, handler Handler) {
	defer conn.Close()

	// Bound the whole exchange so a client that stalls mid-request (or never
	// writes) cannot wedge this inline accept loop. See connDeadline.
	_ = conn.SetDeadline(time.Now().Add(connDeadline))

	var req Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		s.logger.Warn("malformed control request", "error", err)
		_ = json.NewEncoder(conn).Encode(Response{OK: false, Error: wording.RefusalMalformedRequest,
			Fixes: []string{wording.FixMalformedRequest}})
		return
	}

	resp := handler(req)
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		s.logger.Warn("failed to write control response", "error", err)
	}
}

// removeStaleSocket deletes a leftover socket file so a re-bind does not fail. A
// missing file is fine; any other type at the path is left alone and surfaces as
// the bind error.
func removeStaleSocket(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale control socket %q: %w", path, err)
	}
	return nil
}
