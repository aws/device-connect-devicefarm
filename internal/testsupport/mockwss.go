// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testsupport provides in-process test doubles shared across the
// devicefarm-connect unit and integration tests. It is importable from any package's
// _test.go without dragging production dependencies along. It imports "testing"
// on purpose: it is test infrastructure and is never referenced by production
// code paths.
package testsupport

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// routeControl is the server's status route; the add reads the session's time left there.
const routeControl = "control"

// NewServerMockWSS serves the server's routes on one URL: ?path=adbreverse at a ReverseMockWSS,
// ?path=control at a StatusMockWSS, every other path at a SessionMockWSS.
func NewServerMockWSS(t *testing.T, cfg SessionMockConfig) (*SessionMockWSS, *ReverseMockWSS, *StatusMockWSS) {
	t.Helper()
	sessions, reverse, status := newSessionMock(cfg), newReverseMock(), newStatusMock()
	endpoint := NewMockWSS(t, func(ws *websocket.Conn) {
		switch ws.Request().URL.Query().Get("path") {
		case routeAdbReverse:
			reverse.serve(ws)
		case routeControl:
			status.serve(ws)
		default:
			sessions.serve(ws)
		}
	})
	sessions.URL, reverse.URL, status.URL = endpoint, endpoint, endpoint
	return sessions, reverse, status
}

// DefaultStatusTimeLeft is the session time left a StatusMockWSS reports until a test sets its own.
const DefaultStatusTimeLeft = 140 * time.Minute

// StatusMockWSS stands in for the server's status route: it answers each status request with the
// server's reply shape, a JSON object whose value member is JSON text carrying timeLeft in
// seconds, sent as one text frame.
type StatusMockWSS struct {
	URL string

	mu       sync.Mutex
	timeLeft time.Duration
	silent   bool
	refuse   bool
	body     []byte
	requests [][]byte
	open     int
}

func newStatusMock() *StatusMockWSS {
	return &StatusMockWSS{timeLeft: DefaultStatusTimeLeft}
}

// SetTimeLeft sets the time left later replies report.
func (mock *StatusMockWSS) SetTimeLeft(timeLeft time.Duration) {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.timeLeft = timeLeft
}

// StaySilent makes the mock read requests and answer nothing, as a server in another mode does.
func (mock *StatusMockWSS) StaySilent() {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.silent = true
}

// RefuseDials makes the mock close each websocket at open, unanswered.
func (mock *StatusMockWSS) RefuseDials() {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.refuse = true
}

// AnswerWith makes the mock answer every request with exactly body.
func (mock *StatusMockWSS) AnswerWith(body []byte) {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	mock.body = append([]byte(nil), body...)
}

// Requests returns every frame the route received, in order, across every websocket.
func (mock *StatusMockWSS) Requests() [][]byte {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	copied := make([][]byte, len(mock.requests))
	for index, request := range mock.requests {
		copied[index] = append([]byte(nil), request...)
	}
	return copied
}

// OpenSockets returns how many websockets the route holds open now.
func (mock *StatusMockWSS) OpenSockets() int {
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return mock.open
}

// serve answers each inbound frame with the status reply, the canned body, or silence.
func (mock *StatusMockWSS) serve(ws *websocket.Conn) {
	mock.mu.Lock()
	mock.open++
	mock.mu.Unlock()
	defer func() {
		mock.mu.Lock()
		mock.open--
		mock.mu.Unlock()
		ws.Close()
	}()
	mock.mu.Lock()
	refuse := mock.refuse
	mock.mu.Unlock()
	if refuse {
		return
	}
	for {
		request, err := receive(ws, nil)
		if err != nil {
			return
		}
		mock.mu.Lock()
		mock.requests = append(mock.requests, request)
		reply, silent := mock.statusReply(), mock.silent
		mock.mu.Unlock()
		if silent {
			continue
		}
		// A text frame, as the server sends it.
		if websocket.Message.Send(ws, string(reply)) != nil {
			return
		}
	}
}

// statusReply builds the double-encoded reply, or the canned body. Call with mu held.
func (mock *StatusMockWSS) statusReply() []byte {
	if mock.body != nil {
		return append([]byte(nil), mock.body...)
	}
	inner := strconv.Quote(fmt.Sprintf(`{"status": "running", "timeLeft": %.1f}`, mock.timeLeft.Seconds()))
	return []byte(`{"status": "SUCCESS", "message": null, "value": ` + inner + `}`)
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
