// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testsupport provides in-process test doubles shared across the
// devicefarm-connect unit and integration tests. It is importable from any package's
// _test.go without dragging production dependencies along. It imports "testing"
// on purpose: it is test infrastructure and is never referenced by production
// code paths.
package testsupport

import (
	"io"
	"net"
	"sync/atomic"
	"testing"

	"golang.org/x/net/websocket"
)

// NewMockWSS starts an in-process websocket server driven by the given handler and
// returns its wss:// URL. Its loopback leaf chains to TestRoots. The server is torn down
// via t.Cleanup. The default websocket.Handler rejects a null Origin, but the production
// dialer always sends one, so no custom handshake hook is needed.
func NewMockWSS(t *testing.T, handler func(*websocket.Conn)) string {
	t.Helper()
	return startTLSWSS(t, handler, loadAuthority().loopbackLeaf)
}

// NewServerMockWSS serves both routes on one URL, as the server does: ?path=adbreverse at a
// ReverseMockWSS, every other path at a SessionMockWSS. For an attachment that dials both: the
// notifier, then its attach clear.
func NewServerMockWSS(t *testing.T, cfg SessionMockConfig) (*SessionMockWSS, *ReverseMockWSS) {
	t.Helper()
	sessions, reverse := newSessionMock(cfg), newReverseMock()
	endpoint := NewMockWSS(t, func(ws *websocket.Conn) {
		if ws.Request().URL.Query().Get("path") == routeAdbReverse {
			reverse.serve(ws)
			return
		}
		sessions.serve(ws)
	})
	sessions.URL, reverse.URL = endpoint, endpoint
	return sessions, reverse
}

// NewAcceptCounter listens on loopback, closes each accepted connection and counts it,
// for a test that must prove no dial happened.
func NewAcceptCounter(t *testing.T) (address string, accepted func() int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testsupport: listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	var count atomic.Int32
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			count.Add(1)
			conn.Close()
		}
	}()
	return listener.Addr().String(), func() int { return int(count.Load()) }
}

// NewMockWSSPipingTo starts an in-process websocket server that, for each
// accepted connection, opens a fresh TCP connection to targetAddr and copies
// bytes verbatim in both directions. It models the remote adb-pipe endpoint: a
// websocket front that hands raw bytes to a real adb server. Every relayed TCP
// connection is closed when either direction ends.
func NewMockWSSPipingTo(t *testing.T, targetAddr string) string {
	t.Helper()
	return NewMockWSS(t, func(ws *websocket.Conn) {
		target, err := net.Dial("tcp", targetAddr)
		if err != nil {
			// Cannot reach the backing adb server; close the ws so the client
			// observes a clean failure rather than hanging.
			ws.Close()
			return
		}
		// Binary frames so an adb server's binary output is never coerced to text.
		ws.PayloadType = websocket.BinaryFrame
		done := make(chan struct{}, 2)
		go func() { io.Copy(target, ws); done <- struct{}{} }()
		go func() { io.Copy(ws, target); done <- struct{}{} }()
		<-done
		ws.Close()
		target.Close()
	})
}
