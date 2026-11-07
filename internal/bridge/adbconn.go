// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// Close codes the server sends on /adb that change what the client does.
const (
	// supersededCloseCode is the server's close of a reattach's predecessor, sent after its
	// last write there.
	supersededCloseCode = 1000
	// sessionEndedCloseCode is the server's close once a session can no longer be resumed, for
	// example when the device side of the adb stream closes.
	sessionEndedCloseCode = 4000
)

// isCloseWithCode reports whether err is a peer close frame that carries code.
func isCloseWithCode(err error, code int) bool {
	var closeError *wsCloseError
	return errors.As(err, &closeError) && closeError.code == code
}

// errSwapCut means a reattach's predecessor ended without supersededCloseCode. The server writes
// there until that close, so some bytes may be lost.
var errSwapCut = errors.New("the old websocket of a swap did not end with a clean close, so data may have been lost")

// errClosedAfterCut is errAdbConnClosed after a swap cut. It wraps errSwapCut, so the pipe does
// not log the cut a second time.
var errClosedAfterCut = fmt.Errorf("%w: %w", errAdbConnClosed, errSwapCut)

// refreshPhase is the state of one proxied connection's websocket-refresh machine.
//
// The phase says if an inbound end is expected. In a reattach's drain, the close code also says
// if the end is clean.
type refreshPhase int

const (
	// phaseHandshake means the next inbound message must be the session control frame.
	phaseHandshake refreshPhase = iota
	// phaseFreshHandshake is phaseHandshake on a websocket from the fresh-session
	// fallback. Kept distinct because only this one can wedge, so only this one is
	// guarded by enforceWSDeadline.
	phaseFreshHandshake
	// phaseSelected means the park minted the session and switched it to the device, and no
	// reply has come back yet. A failed read here ends the connection with no resume: at a
	// full server pool the close may be an eviction, and a reattach dial there can evict a
	// live connection.
	phaseSelected
	// phaseSteady means inbound messages are raw ADB bytes for the local adb client.
	phaseSteady
	// phaseRefreshing means a successor is being established, so an end of the current
	// websocket is expected: the server closes it on the reattach, and the close can arrive
	// before the client reads the ack.
	phaseRefreshing
	// phaseDraining means the successor is acked and live for writes; the predecessor
	// is being drained to its end before reads move over.
	phaseDraining
	// phaseClosed means the connection is torn down.
	phaseClosed
)

// adbConn is one proxied connection's view of a durable server-side ADB session. It
// implements WSConn, so pipeConn's pumps never see the websocket change.
//
// Two invariants, both with silent failure modes:
//  1. Single reader. Only pipeConn's inbound pump calls Receive; a second reader would
//     reorder the drain.
//  2. Two pointers. writeTo moves at ack time: the server drops writes to a superseded
//     websocket. readFrom waits for the predecessor's end.
//
// The zero value is not usable; construct with newAdbConn.
type adbConn struct {
	dialer *WSDialer
	// device and arn resolve the endpoint to dial. Read per dial, never cached: the
	// daemon renews the time-limited endpoint while this connection is open (see
	// ActiveDevice.EndpointFor). Read with the seam held, so the lock order is seam
	// then ActiveDevice.
	device *ActiveDevice
	arn    string
	// route is the server-side route every dial names, so a swap or fresh-session
	// fallback lands on the same handler as the first websocket.
	route    Route
	policy   WSRefreshPolicy
	activity *activityTracker
	logger   *slog.Logger
	// terminalSessionGone ends the connection on a refused reattach instead of opening a
	// fresh session. Set at construction and never written after, so it needs no lock.
	terminalSessionGone bool
	// refusals is asked what became of the session after a refused reattach ends the
	// connection. Nil asks nothing. Set at construction, like terminalSessionGone.
	refusals *refusalGate
	// preSelected means the park already minted the first websocket's session and switched it
	// to the device, so the opening skips the select. Set at construction.
	preSelected bool

	// sockets is the teardown set and the closed latch, with its own mutex: teardown
	// must not queue behind a refresh's dial-and-ack.
	sockets *socketSet

	// seam fences the outbound direction and publishes the pointer swap. Held
	// exclusively from before the reattach UUID goes out until writeTo has moved.
	// Never hold it across a blocking Receive: a read parks forever when idle.
	//
	// Guards this whole field group; the swap is atomic only if they move together.
	seam     sync.RWMutex
	readFrom WSConn
	writeTo  WSConn
	next     WSConn
	phase    refreshPhase
	uuid     string
	// nextNeedsHandshake marks a successor that owes its own control frame (the
	// fresh-session fallback). commitSwap consults it to pick the resume phase.
	nextNeedsHandshake bool
	// endedOnCut marks a connection endOnSwapCutLocked closed; see closedErrLocked.
	endedOnCut bool
	// readSince is when the current read target entered service: what wsDeadline
	// measures.
	readSince time.Time
	// drainingSince bounds how long a predecessor may take to end. A drain that never ends
	// leaves the connection half-duplex.
	drainingSince time.Time
	// nextReady and refreshFailed are recreated per refresh; which one closes is the
	// outcome.
	nextReady           chan struct{}
	refreshFailed       chan struct{}
	consecutiveFailures int

	// uuidReady gates the refresher: nothing to re-attach to before a session exists.
	// Not seam-guarded. The Once is for the fresh-session fallback's second uuid.
	uuidReady     chan struct{}
	uuidReadyOnce sync.Once
}

// adbConnOption configures an adbConn at construction.
type adbConnOption func(*adbConn)

// withTerminalSessionGone makes a refused reattach end the connection instead of opening a
// fresh session. For a stream whose session cannot be replaced: a fresh session's adb
// connection is back in host mode, so the next application byte would reach the remote's
// screener as a request, be denied on a non-hex header, and the remote would write a FAIL
// and a reason into the application's own byte stream.
func withTerminalSessionGone() adbConnOption {
	return func(connection *adbConn) { connection.terminalSessionGone = true }
}

// withRefusalGate makes a refused reattach that ends the connection ask gate what became of the
// session, so a dead session detaches the device instead of refusing every later stream.
func withRefusalGate(gate *refusalGate) adbConnOption {
	return func(connection *adbConn) { connection.refusals = gate }
}

// withSelectedSession starts the connection on a pre-selected socket, sessionUUID latched so
// the refresher can reattach once the first reply proves the session. An empty sessionUUID
// changes nothing: the connection reads its own control frame.
func withSelectedSession(sessionUUID string) adbConnOption {
	return func(connection *adbConn) {
		if sessionUUID == "" {
			return
		}
		connection.preSelected = true
		connection.uuid = sessionUUID
		connection.phase = phaseSelected
		connection.uuidReadyOnce.Do(func() { close(connection.uuidReady) })
	}
}

// newAdbConn wraps an already-dialed websocket as a session's first. Without
// withSelectedSession the control frame is unread: the first Receive consumes it.
// route must be the one first was dialed on.
func newAdbConn(dialer *WSDialer, device *ActiveDevice, arn string, route Route, first WSConn, policy WSRefreshPolicy, activity *activityTracker, logger *slog.Logger, opts ...adbConnOption) *adbConn {
	if logger == nil {
		logger = slog.Default()
	}
	conn := &adbConn{
		dialer: dialer,
		device: device,
		arn:    arn,
		route:  route,
		// Normalized once here so every read below is a plain field.
		policy:    policy.normalized(),
		activity:  activity,
		logger:    logger,
		sockets:   newSocketSet(first),
		readFrom:  first,
		writeTo:   first,
		phase:     phaseHandshake,
		readSince: time.Now(),
		uuidReady: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(conn)
	}
	return conn
}

// Send writes data to whichever websocket the server is currently listening to.
//
// The read lock is held across the underlying Send, so a refresher waits for an in-flight
// write. Safe only because sendDeadline bounds every write.
func (c *adbConn) Send(data []byte) error {
	c.seam.RLock()
	target := c.writeTo
	phase := c.phase
	if phase == phaseClosed || target == nil {
		closedErr := c.closedErrLocked()
		c.seam.RUnlock()
		return closedErr
	}
	c.activity.beginWrite()
	// Deferred, so the counted span covers the mark below too: a poll landing between the
	// unlock and the mark would otherwise read the pre-write gap.
	defer c.activity.endWrite()
	err := target.Send(data)
	c.seam.RUnlock()

	if err != nil {
		return err
	}
	c.activity.mark()
	return nil
}

// Receive returns the next raw ADB message for the local adb client. It never returns a
// control frame; forwarding one desynchronizes the adb client's framing.
//
// Assumes a single caller.
func (c *adbConn) Receive() ([]byte, error) {
	for {
		c.seam.RLock()
		source := c.readFrom
		phase := c.phase
		sessionUUID := c.uuid
		closedErr := c.closedErrLocked()
		c.seam.RUnlock()

		if phase == phaseClosed || source == nil {
			return nil, closedErr
		}

		// Read with no lock held: this parks forever on an idle connection.
		message, err := source.Receive()
		if err != nil {
			// A codec fault, not a dead socket: a resume would skip the over-cap
			// payload and desynchronize the adb stream.
			if errors.Is(err, websocket.ErrFrameTooLarge) {
				return nil, err
			}
			// Any phase: no successor or resume can reach an ended session. Close latches the
			// teardown so the refresher dials nothing; it never waits on the seam a refresh holds.
			if isCloseWithCode(err, sessionEndedCloseCode) {
				c.logger.Debug("the server ended the adb session", "session", sessionUUID, "error", err)
				c.Close()
				return nil, err
			}
			// A refresh's predecessor may have ended: read the successor, or end on errSwapCut.
			swapped, swapErr := c.awaitSuccessor(err)
			if swapErr != nil {
				return nil, swapErr
			}
			if swapped {
				continue
			}
			// An unexpected failure with a session to resume: one best-effort attempt.
			if c.reconnectReactively(err) {
				continue
			}
			return nil, err
		}

		if phase == phaseHandshake || phase == phaseFreshHandshake {
			if handshakeErr := c.consumeControlFrame(message); handshakeErr != nil {
				return nil, handshakeErr
			}
			continue
		}
		if phase == phaseSelected {
			c.seam.Lock()
			if c.phase == phaseSelected {
				c.phase = phaseSteady
			}
			c.seam.Unlock()
		}

		c.activity.mark()
		return message, nil
	}
}

// errHandshakeConsumed means the control frame is not this caller's to read: it was already
// consumed, or the phase is a refresh's, whose frame the refresher takes.
var errHandshakeConsumed = errors.New("no session control frame is pending")

// awaitHandshake consumes the session control frame with one read bounded by within: nil
// for NEW, the refusal for ERROR. For a caller that must know the session exists before it
// sends or dials anything else, when no data message is coming to carry the answer. Receive
// cannot serve that: it consumes the frame and then blocks on to the first data message.
//
// Send the connection's first frame BEFORE calling this. The server mints the session from
// that frame, so awaiting first parks until the read times out. Any error closes the
// connection.
func (connection *adbConn) awaitHandshake(within time.Duration) error {
	connection.seam.RLock()
	source := connection.readFrom
	phase := connection.phase
	connection.seam.RUnlock()

	// Fail closed rather than read: outside the opening handshake the next inbound message
	// belongs to the stream, and stealing it desynchronizes its reader.
	if phase != phaseHandshake {
		return errHandshakeConsumed
	}

	message, err := receiveWithin(source, within)
	if err == nil {
		err = connection.consumeControlFrame(message)
	}
	if err != nil {
		// Nothing to retry: no session was named. Closing here also retires the socket a
		// read timeout shut behind the socket set's back.
		connection.Close()
	}
	return err
}

// consumeControlFrame decodes the first server frame and latches the session it names.
// Only newSession is expected here; anything else fails closed.
func (c *adbConn) consumeControlFrame(message []byte) error {
	frame, err := unmarshalControlFrame(message)
	if err != nil {
		return fmt.Errorf("read session control frame: %w", err)
	}

	switch frame.arm {
	case armNewSession:
		c.seam.Lock()
		// A Close racing the handshake wins: do not revive the phase.
		if c.phase == phaseClosed {
			c.seam.Unlock()
			return errAdbConnClosed
		}
		c.uuid = frame.payload
		c.phase = phaseSteady
		c.seam.Unlock()
		c.uuidReadyOnce.Do(func() { close(c.uuidReady) })
		c.logger.Debug("adb session established", "session", frame.payload)
		return nil
	case armError:
		return &sessionRefusal{reason: frame.payload}
	default:
		return fmt.Errorf("unexpected control frame %s opening a new session", frame.arm)
	}
}

// awaitSuccessor reports whether reads moved to the successor after readErr ended a websocket a
// refresh is replacing. It blocks until the refresh outcome is known; commitSwap judges the end.
// The RLock waits for refresh to release the fence, so an early reader blocks and does not read
// the end as a failure: the old websocket's close can overtake the ack on the new one. A cut
// swap returns errSwapCut.
func (c *adbConn) awaitSuccessor(readErr error) (swapped bool, cutErr error) {
	c.seam.RLock()
	phase := c.phase
	nextReady := c.nextReady
	refreshFailed := c.refreshFailed
	c.seam.RUnlock()

	if phase != phaseRefreshing && phase != phaseDraining {
		return false, nil
	}
	if nextReady == nil || refreshFailed == nil {
		return false, nil
	}

	select {
	case <-nextReady:
		return c.commitSwap(readErr)
	case <-refreshFailed:
		// No successor is coming, so this really is the end of the connection.
		return false, nil
	}
}

// commitSwap moves reads to the successor after the predecessor ended with readErr, closes the
// predecessor, and enters phaseSteady, or phaseFreshHandshake for the fresh-session fallback.
// Reports whether the swap happened; a Close that landed first makes it a no-op.
//
// A reattach's predecessor must end with supersededCloseCode. Any other end may have cut bytes
// short, so the connection ends with errSwapCut. The client closes the fallback's predecessor
// itself, so any benign end commits.
func (c *adbConn) commitSwap(readErr error) (swapped bool, cutErr error) {
	c.seam.Lock()
	// No drain left, or teardown closed the predecessor: not a cut.
	if c.phase != phaseDraining || c.next == nil || c.isClosed() {
		c.seam.Unlock()
		return false, nil
	}
	switch {
	case !c.nextNeedsHandshake && !isCloseWithCode(readErr, supersededCloseCode):
		return false, c.endOnSwapCutLocked(readErr)
	case c.nextNeedsHandshake && !isBenignCloseError(readErr):
		// A reset may have cut the fallback's predecessor short: no swap, the read error ends the pipe.
		c.seam.Unlock()
		return false, nil
	}
	drained := c.readFrom
	c.readFrom = c.next
	c.readSince = time.Now()
	// Every entry into phaseDraining already pointed writeTo here.
	c.writeTo = c.next
	c.next = nil
	// phaseFreshHandshake, not phaseHandshake: only that one is visible to the guard.
	if c.nextNeedsHandshake {
		c.nextNeedsHandshake = false
		c.phase = phaseFreshHandshake
	} else {
		c.phase = phaseSteady
	}
	c.seam.Unlock()

	c.sockets.forget(drained)
	c.logger.Debug("adb session websocket refreshed", "session", c.sessionUUID())
	return true, nil
}

// endOnSwapCutLocked ends the connection with errSwapCut wrapping cause, logged at Warn. Caller
// holds the seam; this releases it.
func (c *adbConn) endOnSwapCutLocked(cause error) error {
	sessionUUID := c.uuid
	err := fmt.Errorf("%w: %w", errSwapCut, cause)
	c.phase = phaseClosed
	c.endedOnCut = true
	c.seam.Unlock()
	c.logger.Warn("ending the adb connection", "session", sessionUUID, "error", err)
	c.Close()
	return err
}

// Close closes every websocket the connection opened, which unblocks parked pumps.
//
// Does not take the seam: a refresh can hold it for a whole attach and teardown must not
// wait. The phase flips only if TryLock succeeds; otherwise the refresh unwinds itself.
func (c *adbConn) Close() error {
	err := c.sockets.closeAll()
	if c.seam.TryLock() {
		c.phase = phaseClosed
		c.seam.Unlock()
	}
	return err
}

// closedErrLocked is what Send and Receive return once the connection is closed. Caller holds
// the seam.
func (c *adbConn) closedErrLocked() error {
	if c.endedOnCut {
		return errClosedAfterCut
	}
	return errAdbConnClosed
}

// sessionUUID returns the current session's UUID, or "" before one is announced.
func (c *adbConn) sessionUUID() string {
	c.seam.RLock()
	defer c.seam.RUnlock()
	return c.uuid
}

// enforceWSDeadline closes the current websocket if it outlived deadline, or ends the
// connection if a drain never ended, reporting whether it fired. Zero or negative disables
// it. An anomaly guard, not a cap.
//
// Decides under the read lock and takes the exclusive lock only to act. During a drain a Send
// can park on the successor while it holds the read lock. A Lock on every poll would queue
// behind that Send, RWMutex would block the reader's next RLock, and the predecessor would stop
// draining. Decides again under the exclusive lock and re-arms the clock in the same step: the
// reader can commit a swap between the two locks.
func (c *adbConn) enforceWSDeadline(deadline time.Duration) bool {
	if deadline <= 0 {
		return false
	}

	c.seam.RLock()
	staleRead, stalledDrain := c.deadlinePassedLocked(deadline)
	c.seam.RUnlock()
	if !staleRead && !stalledDrain {
		return false
	}

	c.seam.Lock()
	staleRead, stalledDrain = c.deadlinePassedLocked(deadline)
	if !staleRead && !stalledDrain {
		c.seam.Unlock()
		return false
	}
	phase := c.phase
	stale := c.readFrom
	age := time.Since(c.readSince)
	// The drain may have lost bytes, so end the connection, as for any unclean end.
	if stalledDrain {
		c.endOnSwapCutLocked(fmt.Errorf("no clean close within %s", drainStallMultiple*deadline))
		return true
	}
	// Re-arm the clock that justified this firing. The reader owns advancing readFrom.
	c.readSince = time.Now()
	// No uuid left to re-attach to, so the reader cannot recover. Close the phase here or
	// every later poll re-fires on a dead socket.
	if phase == phaseFreshHandshake {
		c.phase = phaseClosed
	}
	c.seam.Unlock()

	// Keep every log below the unlock: sessionUUID takes seam.RLock and RWMutex is not
	// reentrant.
	switch {
	case phase == phaseFreshHandshake:
		c.logger.Warn("fresh session never sent its control frame; dropping the connection",
			"age", age, "deadline", deadline)
	default:
		c.logger.Warn("websocket outlived its refresh cycle; dropping it to force a reconnect",
			"age", age, "deadline", deadline, "session", c.sessionUUID())
	}
	// Close, do not swap: the parked reader owns resuming the session.
	c.sockets.forget(stale)
	return true
}

// deadlinePassedLocked reports whether the read target outlived deadline (staleRead) or a drain
// outlived drainStallMultiple times deadline (stalledDrain). Caller holds the seam, shared or
// exclusive.
func (c *adbConn) deadlinePassedLocked(deadline time.Duration) (staleRead, stalledDrain bool) {
	// commitSwap re-arms readSince, so a fresh handshake's age is its own websocket's.
	staleRead = (c.phase == phaseSteady || c.phase == phaseFreshHandshake) && time.Since(c.readSince) >= deadline
	// drainingSince is zero until the first refresh, so the IsZero guard is load-bearing.
	stalledDrain = c.phase == phaseDraining && !c.drainingSince.IsZero() &&
		time.Since(c.drainingSince) >= drainStallMultiple*deadline
	return staleRead, stalledDrain
}

// failureCount returns how many refreshes have failed consecutively; a success resets it.
func (c *adbConn) failureCount() int {
	c.seam.RLock()
	defer c.seam.RUnlock()
	return c.consecutiveFailures
}

// isClosed reports whether the connection has been torn down.
func (c *adbConn) isClosed() bool {
	return c.sockets.isClosed()
}
