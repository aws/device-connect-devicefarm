// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// countingSocket records its closes. The set never sends or receives, and a real
// websocket cannot report close counts.
type countingSocket struct {
	closes   int
	closeErr error
}

func (s *countingSocket) Send([]byte) error        { return nil }
func (s *countingSocket) Receive() ([]byte, error) { return nil, nil }

func (s *countingSocket) Close() error {
	s.closes++
	return s.closeErr
}

// closeOnceSocket errors from its second Close on, as a real websocket does, so a
// double-close is visible in closeAll's return value.
type closeOnceSocket struct {
	closes        int
	errAfterFirst error
}

func (s *closeOnceSocket) Send([]byte) error        { return nil }
func (s *closeOnceSocket) Receive() ([]byte, error) { return nil, nil }

func (s *closeOnceSocket) Close() error {
	s.closes++
	if s.closes > 1 {
		return s.errAfterFirst
	}
	return nil
}

func TestGivenClosedSetWhenSocketRegisteredThenRefusedSoCallerOwnsIt(t *testing.T) {
	// Accepting would leave a socket nothing closes.
	first := &countingSocket{}
	set := newSocketSet(first)
	require.NoError(t, set.closeAll())

	late := &countingSocket{}

	assert.False(t, set.register(late), "a closed set must refuse a new socket")
	assert.Zero(t, late.closes, "the set must not close a socket it refused; the caller owns it")
}

func TestGivenOpenSetWhenSocketRegisteredThenClosedAllByTeardown(t *testing.T) {
	first := &countingSocket{}
	set := newSocketSet(first)
	successor := &countingSocket{}
	require.True(t, set.register(successor))

	require.NoError(t, set.closeAll())

	assert.Equal(t, 1, first.closes, "the opening socket must be closed by teardown")
	assert.Equal(t, 1, successor.closes, "a registered successor must be closed by teardown")
}

func TestGivenRegisteredSocketWhenForgottenThenClosedOnceAndDeregistered(t *testing.T) {
	// Deregistering before closing stops teardown from closing the same socket twice and
	// reporting a spurious error.
	retired := &countingSocket{}
	set := newSocketSet(retired)

	set.forget(retired)
	require.Equal(t, 1, retired.closes, "forget must close the socket it retires")

	require.NoError(t, set.closeAll())

	assert.Equal(t, 1, retired.closes, "teardown must not close an already-retired socket again")
}

func TestGivenUnregisteredSocketWhenForgottenThenStillClosed(t *testing.T) {
	// attachSuccessor forgets a socket whose register was refused.
	set := newSocketSet(&countingSocket{})
	stranger := &countingSocket{}

	set.forget(stranger)

	assert.Equal(t, 1, stranger.closes, "forget must close a socket even if it was never registered")
}

func TestGivenNilSocketWhenForgottenThenNoPanic(t *testing.T) {
	// The refresh paths pass whatever they captured under the lock, which is nil on a
	// connection that never got a successor.
	set := newSocketSet(&countingSocket{})

	assert.NotPanics(t, func() { set.forget(nil) }, "forgetting nil must be a no-op")
}

func TestGivenFailingSocketsWhenClosedAllThenEveryOneClosedAndErrorReturned(t *testing.T) {
	// Both sockets fail, so an early return leaves one unclosed regardless of map order.
	first := &countingSocket{closeErr: errors.New("first close failed")}
	second := &countingSocket{closeErr: errors.New("second close failed")}
	set := newSocketSet(first)
	require.True(t, set.register(second))

	err := set.closeAll()

	assert.Error(t, err, "a failing Close must surface")
	assert.Equal(t, 1, first.closes, "every socket must be closed despite a failure")
	assert.Equal(t, 1, second.closes, "a Close error must not stop the set from closing the rest")
}

func TestGivenClosedSetWhenClosedAgainThenIdempotentAndNoError(t *testing.T) {
	// Idempotence comes from closeAll emptying the set.
	socket := &closeOnceSocket{errAfterFirst: errors.New("socket already closed")}
	set := newSocketSet(socket)
	require.NoError(t, set.closeAll(), "the first teardown must report nothing")

	assert.NoError(t, set.closeAll(), "a second teardown must report nothing")
	assert.Equal(t, 1, socket.closes, "a second teardown must not re-close anything")
	assert.True(t, set.isClosed())
}

func TestGivenConcurrentRegisterAndCloseWhenRacedThenEverySocketClosedExactlyOnce(t *testing.T) {
	// Every socket is closed exactly once: by closeAll if it made the set, by the refused
	// caller if not. forget is not raced against closeAll: that double-close is benign.
	set := newSocketSet(&countingSocket{})
	sockets := make([]*countingSocket, 50)
	for i := range sockets {
		sockets[i] = &countingSocket{}
	}

	var wg sync.WaitGroup
	for _, socket := range sockets {
		wg.Add(1)
		go func(s *countingSocket) {
			defer wg.Done()
			if !set.register(s) {
				s.Close() // refused because closeAll already ran: caller owns it
			}
		}(socket)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		set.closeAll()
	}()
	wg.Wait()

	// Deliberately NO final sweep: it would close orphans itself and mask the bug. A
	// register ignoring the closed latch leaves a socket at zero closes, caught here.
	for i, socket := range sockets {
		assert.Equal(t, 1, socket.closes, "socket %d must be closed exactly once", i)
	}
}

// reenteringSocket calls back into the set from inside its own Close, proving rule 2 on
// socketSet: neither forget nor closeAll holds the mutex across a Close.
type reenteringSocket struct {
	set       *socketSet
	closes    int
	reentered bool
}

func (s *reenteringSocket) Send([]byte) error        { return nil }
func (s *reenteringSocket) Receive() ([]byte, error) { return nil, nil }

func (s *reenteringSocket) Close() error {
	s.closes++
	s.set.isClosed() // takes the mutex; never returns if the caller still holds it
	s.reentered = true
	return nil
}

func TestGivenSocketWhoseCloseTakesTheMutexWhenForgottenThenNoSelfDeadlock(t *testing.T) {
	// A re-entering Close probes the lock scope directly. If forget held the mutex across
	// the Close this hangs to the test timeout instead of failing.
	set := newSocketSet(&countingSocket{})
	socket := &reenteringSocket{set: set}
	require.True(t, set.register(socket))

	set.forget(socket)

	assert.Equal(t, 1, socket.closes)
	assert.True(t, socket.reentered, "the socket's Close must be able to take the set's mutex")
}

func TestGivenSocketWhoseCloseTakesTheMutexWhenClosedAllThenNoSelfDeadlock(t *testing.T) {
	// Same probe on the teardown path, which snapshots under the mutex and closes after
	// releasing it. A version closing inside the lock hangs here.
	set := newSocketSet(&countingSocket{})
	socket := &reenteringSocket{set: set}
	require.True(t, set.register(socket))

	require.NoError(t, set.closeAll())

	assert.Equal(t, 1, socket.closes)
	assert.True(t, socket.reentered, "teardown must not hold the set's mutex across a socket Close")
}

func TestGivenRealWebsocketWhenForgottenThenTheUnderlyingConnectionCloses(t *testing.T) {
	// A second Close on a live x/net connection errors, which is what teardown would report
	// if forget left it registered.
	mock := testsupport.NewSessionEchoWSS(t)
	socket := dialMock(t, mock.URL)
	set := newSocketSet(socket)

	set.forget(socket)

	assert.NoError(t, set.closeAll(), "teardown must report nothing for an already-retired socket")
	assert.Error(t, socket.Close(), "the underlying websocket must really be closed")
}
