// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import "sync"

// socketSet holds the websockets one proxied connection must close on teardown, plus the
// closed latch. Its mutex is NOT adbConn's seam: teardown must not queue behind a refresh.
//
// Never hold this mutex across a socket Close: a Close blocked on I/O would stall an
// attach waiting to register its successor.
//
// The zero value is not usable; construct with newSocketSet.
type socketSet struct {
	mutex  sync.Mutex
	closed bool
	open   map[WSConn]struct{}
}

// newSocketSet returns a set already holding first, the connection's opening websocket.
func newSocketSet(first WSConn) *socketSet {
	return &socketSet{open: map[WSConn]struct{}{first: {}}}
}

// register adds socket to the set, reporting false when the set is already closed. A
// refused caller owns closing socket.
func (s *socketSet) register(socket WSConn) bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closed {
		return false
	}
	s.open[socket] = struct{}{}
	return true
}

// forget drops socket from the set and closes it. Deregistering first stops closeAll from
// closing it twice. An unregistered or nil socket is fine to pass.
func (s *socketSet) forget(socket WSConn) {
	if socket == nil {
		return
	}
	s.mutex.Lock()
	delete(s.open, socket)
	s.mutex.Unlock()
	socket.Close()
}

// closeAll closes every socket in the set and returns one error if any Close failed. It
// always attempts every socket; which failing socket's error surfaces is unspecified.
// Idempotent: it empties the set.
func (s *socketSet) closeAll() error {
	s.mutex.Lock()
	s.closed = true
	sockets := make([]WSConn, 0, len(s.open))
	for socket := range s.open {
		sockets = append(sockets, socket)
	}
	clear(s.open)
	s.mutex.Unlock()

	// Close outside the lock.
	var closeErr error
	for _, socket := range sockets {
		if err := socket.Close(); err != nil {
			closeErr = err
		}
	}
	return closeErr
}

// isClosed reports whether teardown has run.
func (s *socketSet) isClosed() bool {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.closed
}
