// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package testsupport

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"

	"golang.org/x/net/websocket"
)

// routeAdbReverse is the ?path= value the reverse route is dialed with. NewMockWSS bakes /adb
// into the URL it hands back, so the route travels in the query string.
const routeAdbReverse = "adbreverse"

// connectorSessionUUID is the session every redeemed connector is minted under. Fixed: no test
// needs two connectors told apart by their session.
const connectorSessionUUID = "22222222-3333-4444-8555-666666666666"

// spliceDirections sizes serveConnector's done channel: one slot per io.Copy goroutine, so the
// second to finish never blocks.
const spliceDirections = 2

// ReverseMockWSS stands in for the server's /adbreverse endpoint. As a notifier endpoint: one
// notifier at a time, NOTIFIER_HELLO required as the first frame, NOTIFIER_READY as the ack,
// PING counted and echoed with PONG, and every protocol violation refused with an ERROR frame
// and a close counted under its reason. It refuses rather than tolerates on purpose: a lax double
// would let a daemon that sends a data frame before its hello, opens a second notifier, or
// keeps talking after the ack pass its own tests.
//
// As a connector endpoint it redeems the token of a connection parked with Connect and splices
// that connection to the websocket. Any other token is refused.
//
// Deliberate divergences from the server: no 30 s first-frame reaper; no 180 s no-PING watchdog
// (so a redial after a half-open drop always succeeds here, where the server would refuse it
// until that timer clears the stale registration); a redeemed connector's session ends with its
// websocket, so every reattach is refused as gone; a token is matched as bytes, not checked as a
// uuid or tied to a live notifier; nothing caps concurrent connectors; and x/net's 1000 close
// code where the server sends 1011 (the daemon treats both alike). The exception: a connector
// whose splice ends gets SessionEndedCloseCode, as from the server when its device end closes.
//
// Counters span the mock's whole life, redials included. Assert them with Eventually or with
// a floor, never as an exact count reached at an exact moment: a predecessor frees the
// notifier slot only when its own goroutine observes the close, so a fast redial can be
// refused once.
type ReverseMockWSS struct {
	URL string

	stateMutex        sync.Mutex
	notifiersAccepted int
	notifiersClosed   int
	pingsSeen         int
	swallowPongs      bool
	refusals          []string
	// notifier holds the single notifier slot from its hello onward; notifierReady turns it
	// into an announce target, set with the ack under one lock.
	notifier      *websocket.Conn
	notifierReady bool
	// connectors holds the device end of each parked connection, by token. A token is deleted once
	// redeemed, so a replay is refused like an unknown one.
	connectors         map[string]net.Conn
	connectorsAccepted int
	connectorsRefused  int
	refuseConnector    string
	// closeNewCode and closeNewReason close every later websocket after its first frame; zero code:
	// off. See CloseNewConnections.
	closeNewCode   int
	closeNewReason string
}

// NewReverseMockWSS starts the mock endpoint, torn down via t.Cleanup.
func NewReverseMockWSS(t *testing.T) *ReverseMockWSS {
	t.Helper()
	mock := newReverseMock()
	mock.URL = NewMockWSS(t, mock.serve)
	return mock
}

// newReverseMock is a reverse mock with no endpoint yet.
func newReverseMock() *ReverseMockWSS {
	return &ReverseMockWSS{connectors: make(map[string]net.Conn)}
}

// NotifiersAccepted returns how many notifiers completed the hello exchange, their READY ack on
// the wire. A redial shows up here, so a test proves a death by a count rather than by waiting
// for nothing to arrive.
func (mock *ReverseMockWSS) NotifiersAccepted() int {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	return mock.notifiersAccepted
}

// NotifiersClosed returns how many accepted notifiers have since ended: the positive
// observable that a detach or a kill reached the socket.
func (mock *ReverseMockWSS) NotifiersClosed() int {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	return mock.notifiersClosed
}

// PingsSeen returns how many PING frames arrived, across every notifier this mock served.
func (mock *ReverseMockWSS) PingsSeen() int {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	return mock.pingsSeen
}

// Refusals returns the reason of every websocket this mock refused, in order.
func (mock *ReverseMockWSS) Refusals() []string {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	return append([]string(nil), mock.refusals...)
}

// SwallowPongs stops the mock answering PING, so a caller's missed-pong deadline fires while
// the socket stays open. PINGs are still counted.
func (mock *ReverseMockWSS) SwallowPongs(swallow bool) {
	mock.stateMutex.Lock()
	mock.swallowPongs = swallow
	mock.stateMutex.Unlock()
}

// Announce sends one REVERSE_OPEN to the live notifier, as the server does for a parked
// device-originated connection. Reports whether it went anywhere.
func (mock *ReverseMockWSS) Announce(token string, customerPort int) bool {
	return mock.SendRaw(FrameReverseOpen(token, customerPort))
}

// SendRaw sends exact bytes to the live notifier, for the frames a correct server would never
// send. Reports whether they went anywhere.
func (mock *ReverseMockWSS) SendRaw(frame []byte) bool {
	mock.stateMutex.Lock()
	target := mock.notifier
	if !mock.notifierReady {
		target = nil
	}
	mock.stateMutex.Unlock()
	if target == nil {
		return false
	}
	// Sent outside the lock. x/net serializes writes per connection, so this cannot
	// interleave with the handler's own PONG.
	return websocket.Message.Send(target, frame) == nil
}

// HoldNotifierSlot marks the notifier slot taken by a rival this mock never serves, so every
// hello is refused with the registered payload until the returned release runs.
func (mock *ReverseMockWSS) HoldNotifierSlot() (release func()) {
	rival := &websocket.Conn{}
	mock.stateMutex.Lock()
	mock.notifier = rival
	mock.stateMutex.Unlock()
	return func() {
		mock.stateMutex.Lock()
		defer mock.stateMutex.Unlock()
		if mock.notifier == rival {
			mock.notifier = nil
		}
	}
}

// DropNotifier closes the live notifier from the server side, the way a restart or a pool
// eviction does: the client's next read fails instead of carrying an ERROR frame, which is the
// only way to drive a caller's Receive-error path. Reports whether a notifier was there to drop.
func (mock *ReverseMockWSS) DropNotifier() bool {
	mock.stateMutex.Lock()
	target := mock.notifier
	if !mock.notifierReady {
		target = nil
	}
	mock.stateMutex.Unlock()
	if target == nil {
		return false
	}
	// The slot frees when the serving goroutine sees the close, not here.
	return target.Close() == nil
}

// Connect parks a device connection under token, as the server does when a device app dials
// adbd's reverse listener, and returns the device end, which a redeeming connector splices to.
// Announce the token to have the daemon redeem it. Closing this end drops the connector's
// websocket, indistinguishable from a pool eviction. The pipe is unbuffered: a reader must be
// ready on the device end too.
func (mock *ReverseMockWSS) Connect(token string) net.Conn {
	device, parked := net.Pipe()
	mock.stateMutex.Lock()
	mock.connectors[token] = parked
	mock.stateMutex.Unlock()
	return device
}

// ConnectorsAccepted returns how many connectors redeemed a token.
func (mock *ReverseMockWSS) ConnectorsAccepted() int {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	return mock.connectorsAccepted
}

// ConnectorsRefused returns how many connector helloes were refused, replays included.
func (mock *ReverseMockWSS) ConnectorsRefused() int {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	return mock.connectorsRefused
}

// CloseNewConnections closes every later websocket, notifier or connector, with closeCode and reason
// after its first frame, as the relay does for a stopping session. A zero
// closeCode turns it off.
func (mock *ReverseMockWSS) CloseNewConnections(closeCode int, reason string) {
	mock.stateMutex.Lock()
	mock.closeNewCode, mock.closeNewReason = closeCode, reason
	mock.stateMutex.Unlock()
}

// RefuseConnector makes every later redemption of a parked token fail with reason, standing in for
// a server at its connector cap: the connection stays parked.
func (mock *ReverseMockWSS) RefuseConnector(reason string) {
	mock.stateMutex.Lock()
	mock.refuseConnector = reason
	mock.stateMutex.Unlock()
}

// serve handles one websocket for its whole lifetime.
func (mock *ReverseMockWSS) serve(ws *websocket.Conn) {
	defer ws.Close()
	if route := ws.Request().URL.Query().Get("path"); route != routeAdbReverse {
		mock.refuse(ws, fmt.Sprintf("dialed on route %q, not %q", route, routeAdbReverse))
		return
	}

	first, err := receive(ws, nil)
	if err != nil {
		// The client hung up before its hello. A text frame does not land here: x/net's
		// Message codec ignores the opcode on a []byte receive, so a text hello is refused
		// below on its content, which is all the daemon can see either.
		return
	}
	mock.stateMutex.Lock()
	closeCode, closeReason := mock.closeNewCode, mock.closeNewReason
	mock.stateMutex.Unlock()
	if closeCode != 0 {
		closeWithCode(ws, closeCode, closeReason)
		return
	}
	if token := connectorTokenOf(first); token != "" {
		mock.serveConnector(ws, token)
		return
	}
	if len(first) == sessionUUIDLen && uuidV4Pattern.Match(first) {
		// A connector reattaching. This mock keeps no session across websockets, so it answers as
		// a server whose session is gone: the one refusal a connector must end on rather than
		// carry on under a fresh session.
		mock.refuse(ws, "unknown or expired session")
		return
	}
	if !bytes.Equal(first, FrameEmptyArm(controlStatusNotifierHello)) {
		mock.refuse(ws, "first frame was not an empty NOTIFIER_HELLO")
		return
	}
	if !mock.claim(ws) {
		mock.refuse(ws, "notifier already registered")
		return
	}
	defer mock.release(ws)

	if !mock.sendReady(ws) {
		return
	}

	for {
		message, err := receive(ws, nil)
		if err != nil {
			return
		}
		if !bytes.Equal(message, FrameEmptyArm(controlStatusPing)) {
			// A ready notifier sends nothing but PING. The reason stays payload-free: a
			// connector's first frame carries a bearer token.
			mock.refuse(ws, "a ready notifier sent a frame that is not a PING")
			return
		}
		if !mock.countPing() {
			continue
		}
		if websocket.Message.Send(ws, FrameEmptyArm(controlStatusPong)) != nil {
			return
		}
	}
}

// serveConnector redeems token and splices ws to the connection parked under it, for as long as
// both live. The ack is NEW plus a session uuid, so a connector is an ordinary durable session
// from there on. A refusal reaches the wire before anything is spliced, which is what lets a test
// prove no local port was dialed.
func (mock *ReverseMockWSS) serveConnector(ws *websocket.Conn, token string) {
	parked, reason := mock.redeem(token)
	if parked == nil {
		mock.refuse(ws, reason)
		return
	}
	defer parked.Close()
	if websocket.Message.Send(ws, FrameNewSession(connectorSessionUUID)) != nil {
		return
	}
	// Binary frames, and message boundaries carry no meaning: the daemon reframes from the byte
	// stream. Whichever direction ends first ends both and the session, so the daemon reads the
	// session-ended close, sent before the deferred Close's own, and no ERROR frame.
	ws.PayloadType = websocket.BinaryFrame
	done := make(chan struct{}, spliceDirections)
	go func() { io.Copy(parked, ws); done <- struct{}{} }()
	go func() { io.Copy(ws, parked); done <- struct{}{} }()
	<-done
	ws.WriteClose(SessionEndedCloseCode)
}

// redeem takes the connection a token names, or returns the reason it was refused.
func (mock *ReverseMockWSS) redeem(token string) (net.Conn, string) {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	parked, ok := mock.connectors[token]
	if !ok {
		mock.connectorsRefused++
		return nil, "unknown or already redeemed reverse token"
	}
	if mock.refuseConnector != "" {
		mock.connectorsRefused++
		return nil, mock.refuseConnector
	}
	delete(mock.connectors, token)
	mock.connectorsAccepted++
	return parked, ""
}

// claim takes the single notifier slot for ws, or reports false when another holds it. The
// slot is taken at the hello, so a second hello is refused before any ack.
func (mock *ReverseMockWSS) claim(ws *websocket.Conn) bool {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	if mock.notifier != nil {
		return false
	}
	mock.notifier = ws
	return true
}

// sendReady puts the READY ack on the wire and, under the same lock, counts ws and makes it the
// announce target. One step, both ways: an announce cannot overtake the ack, and a client that
// has read READY cannot see Announce refuse or the count lag. Holding mock.stateMutex across one
// short write to a fresh socket cannot block; every other sender releases mock.stateMutex before it writes.
func (mock *ReverseMockWSS) sendReady(ws *websocket.Conn) bool {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	if websocket.Message.Send(ws, FrameEmptyArm(controlStatusNotifierReady)) != nil {
		return false
	}
	if mock.notifier == ws {
		mock.notifierReady = true
		mock.notifiersAccepted++
	}
	return true
}

// release frees the slot ws holds and counts the end of a notifier that had reached ready. The
// guard is defensive: a superseded socket must not disturb a successor.
func (mock *ReverseMockWSS) release(ws *websocket.Conn) {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	if mock.notifier != ws {
		return
	}
	mock.notifier = nil
	if mock.notifierReady {
		mock.notifierReady = false
		mock.notifiersClosed++
	}
}

// countPing records one PING and reports whether the mock should answer it.
func (mock *ReverseMockWSS) countPing() bool {
	mock.stateMutex.Lock()
	defer mock.stateMutex.Unlock()
	mock.pingsSeen++
	return !mock.swallowPongs
}

// refuse counts the reason and announces it, as the server does; serve's defer closes the
// socket. Counting first is what makes a test's read of the ERROR frame and of Refusals
// order-independent.
func (mock *ReverseMockWSS) refuse(ws *websocket.Conn, reason string) {
	mock.stateMutex.Lock()
	mock.refusals = append(mock.refusals, reason)
	mock.stateMutex.Unlock()
	websocket.Message.Send(ws, FrameError(reason))
}
