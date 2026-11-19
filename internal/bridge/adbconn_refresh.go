package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Every path that holds the seam exclusively across a network round trip lives here. The
// rest of the machine is in adbconn.go.

// errSessionGone is the server's explicit refusal: the named session is gone. It proves
// only that the attempt that got it did not take over; see attachSuccessorRetrying.
var errSessionGone = errors.New("remote session is gone")

// errReattachIndeterminate means the UUID reached the server but the outcome is unknown:
// the ack timed out, named the wrong session, or carried an unexpected status. The
// predecessor may already be superseded.
var errReattachIndeterminate = errors.New("reattach outcome is indeterminate")

// errAdbConnClosed means the connection has been torn down.
var errAdbConnClosed = errors.New("adb connection is closed")

// attachAttempts is one attempt plus one retry. Retrying harder just stalls the adb
// client, since each attempt carries its own timeout.
const attachAttempts = 2

// startRefreshLocked moves the machine into phaseRefreshing and arms the outcome
// channels; exactly one of the two must be closed later. Caller holds the seam.
func (c *adbConn) startRefreshLocked() (sessionUUID string, nextReady, refreshFailed chan struct{}) {
	c.phase = phaseRefreshing
	c.nextReady = make(chan struct{})
	c.refreshFailed = make(chan struct{})
	return c.uuid, c.nextReady, c.refreshFailed
}

// enterDrainingLocked publishes successor as the write target and starts the
// predecessor's drain; reads move over at EOF (commitSwap). Caller holds the seam;
// releases it.
func (c *adbConn) enterDrainingLocked(successor WSConn, nextReady chan struct{}) {
	c.next = successor
	// The predecessor can no longer carry writes (superseded or refused), so the
	// outbound direction moves immediately even though reads stay on it.
	c.writeTo = successor
	c.phase = phaseDraining
	c.consecutiveFailures = 0
	c.drainingSince = time.Now()
	c.seam.Unlock()
	close(nextReady)
}

// refresh replaces the current websocket with a fresh one re-attached to the same session.
// gate is re-checked under the lock; a nil gate always proceeds.
//
// Keep the seam held across the whole dial and ack. A shorter span lets a Send land in a
// websocket the server stopped listening to; it returns nil and the bytes vanish.
func (c *adbConn) refresh(ctx context.Context, gate func() bool) error {
	c.seam.Lock()
	if c.phase != phaseSteady {
		c.seam.Unlock()
		return nil
	}
	if gate != nil && !gate() {
		c.seam.Unlock()
		return nil
	}
	sessionUUID, nextReady, refreshFailed := c.startRefreshLocked()

	// Lock still held: everything up to the publish below is inside the fence.
	successor, err := c.attachSuccessorRetrying(ctx, sessionUUID)
	if err != nil {
		// Teardown first: see unwindIfClosedLocked.
		if c.unwindIfClosedLocked(successor, refreshFailed) {
			return err
		}
		// Test indeterminate before refusal: a refusal after an indeterminate attempt
		// carries both errors, and the fresh-session fallback assumes an unsuperseded
		// predecessor. Capture poisoned under the lock.
		if errors.Is(err, errReattachIndeterminate) {
			poisoned := c.readFrom
			c.failRefreshLocked(refreshFailed)
			c.dropPoisonedPredecessor(poisoned)
			return err
		}
		if errors.Is(err, errSessionGone) {
			// Both take over the held lock and release it.
			if c.terminalSessionGone {
				return c.endOnSessionGoneLocked(refreshFailed, err)
			}
			return c.restartAsNewSessionLocked(ctx, nextReady, refreshFailed)
		}
		c.failRefreshLocked(refreshFailed)
		return err
	}

	// A Close can have landed during the dial. The phase is unreliable here (Close's
	// TryLock failed against this lock), so check the closed latch.
	if c.unwindIfClosedLocked(successor, refreshFailed) {
		return errAdbConnClosed
	}

	c.enterDrainingLocked(successor, nextReady)
	return nil
}

// attachSuccessorRetrying is attachSuccessor plus one retry, used by both attach paths.
// An indeterminate ack is retried too: a host that drops the socket mid-reattach sends no
// frame.
//
// Indeterminacy is sticky. Once a UUID goes out unacked, a later refusal proves only that
// that attempt did not take over, so it is re-wrapped.
func (c *adbConn) attachSuccessorRetrying(ctx context.Context, sessionUUID string) (WSConn, error) {
	var err error
	indeterminate := false
	for attempt := 1; attempt <= attachAttempts; attempt++ {
		var successor WSConn
		successor, err = c.attachSuccessor(ctx, sessionUUID)
		if err == nil {
			return successor, nil
		}
		if errors.Is(err, errReattachIndeterminate) {
			indeterminate = true
		}
		// Retry only failures a second try could answer differently. A cancelled ctx needs no
		// check: the next dial fails at once.
		if errors.Is(err, errSessionGone) || errors.Is(err, errAdbConnClosed) {
			return nil, keepIndeterminateTaint(indeterminate, err)
		}
		if attempt < attachAttempts {
			c.logger.Debug("retrying the websocket attach once",
				"session", sessionUUID, "attempt", attempt, "error", err)
		}
	}
	return nil, keepIndeterminateTaint(indeterminate,
		fmt.Errorf("attach a successor websocket after %d attempts: %w", attachAttempts, err))
}

// keepIndeterminateTaint re-wraps err when an earlier attempt left a UUID unacked. Without
// it a determinate-looking last failure hands a possibly superseded predecessor back to
// service.
func keepIndeterminateTaint(indeterminate bool, err error) error {
	if !indeterminate || errors.Is(err, errReattachIndeterminate) {
		return err
	}
	return fmt.Errorf("%w: after an earlier indeterminate attempt: %w", errReattachIndeterminate, err)
}

// attachSuccessor dials a websocket, sends the session UUID as its whole first
// message, and requires a matching reattach ack. On any failure it closes the
// websocket it dialed, so a caller never inherits a half-attached one.
func (c *adbConn) attachSuccessor(ctx context.Context, sessionUUID string) (WSConn, error) {
	// Teardown first: a latched Close must cost no upgrade. register below catches a Close
	// that lands mid-dial.
	if c.isClosed() {
		return nil, errAdbConnClosed
	}
	// Re-read per attempt: the daemon renews the endpoint every few minutes, and a
	// swap must dial the current one. A miss means the device is gone, so the caller
	// unwinds like any other attach failure.
	endpoint, ok := c.device.EndpointFor(c.arn)
	if !ok {
		return nil, errAdbConnClosed
	}

	successor, err := c.dialer.Dial(ctx, endpoint, c.route)
	if err != nil {
		return nil, fmt.Errorf("dial successor websocket: %w", err)
	}
	if !c.sockets.register(successor) {
		successor.Close()
		return nil, errAdbConnClosed
	}

	// The UUID as its own whole message: that framing is the reconnect signal.
	if err := successor.Send([]byte(sessionUUID)); err != nil {
		c.sockets.forget(successor)
		return nil, fmt.Errorf("send session uuid: %w", err)
	}

	// The UUID is on the wire, so every failure below is indeterminate. Only an explicit
	// ERROR refusal proves nothing took over.
	frame, err := readControlFrameWithin(successor, c.policy.ReattachTimeout)
	if err != nil {
		c.sockets.forget(successor)
		return nil, fmt.Errorf("read reattach ack: %w: %w", errReattachIndeterminate, err)
	}

	switch frame.status {
	case controlStatusReattach:
		if frame.payload != sessionUUID {
			c.sockets.forget(successor)
			return nil, fmt.Errorf("%w: reattach ack names session %q, expected %q",
				errReattachIndeterminate, frame.payload, sessionUUID)
		}
		return successor, nil
	case controlStatusError:
		// The one determinate failure: refused outright, so nothing switched.
		c.sockets.forget(successor)
		return nil, fmt.Errorf("%w: %s", errSessionGone, frame.payload)
	default:
		c.sockets.forget(successor)
		return nil, fmt.Errorf("%w: unexpected control frame status 0x%02x acking a reattach",
			errReattachIndeterminate, frame.status)
	}
}

// endOnSessionGoneLocked ends the connection after a refused reattach on a connection that
// may not open a fresh session. Called with the seam held; releases it.
//
// discardSuccessorLocked, not failRefreshLocked: the latter only counts the failure and
// returns the predecessor to service, so teardown would wait for MaxFailures more cycles.
func (connection *adbConn) endOnSessionGoneLocked(refreshFailed chan struct{}, cause error) error {
	connection.logger.Warn("remote session is gone and this connection may not open a fresh one; ending it",
		"session", connection.uuid, "error", cause)
	// discardSuccessorLocked for the phase flip and the parked refresh outcome; there is no
	// successor to discard, the refused one is already closed. Close then ends the reader.
	connection.discardSuccessorLocked(nil, refreshFailed)
	connection.Close()
	return cause
}

// restartAsNewSessionLocked opens a brand-new session after a refused reattach. Called
// with the seam held; releases it. Lossy: in-flight adb stream state is gone, so it warns.
func (c *adbConn) restartAsNewSessionLocked(ctx context.Context, nextReady, refreshFailed chan struct{}) error {
	// Teardown first: a dead connection gets no dial and no Warn.
	if c.unwindIfClosedLocked(nil, refreshFailed) {
		return errAdbConnClosed
	}
	c.logger.Warn("remote session is gone; opening a fresh one (in-flight adb stream state is lost)",
		"session", c.uuid)

	// Current endpoint, not a captured one: see attachSuccessor. A miss unwinds on the
	// same path as a failed dial.
	endpoint, ok := c.device.EndpointFor(c.arn)
	if !ok {
		if c.unwindIfClosedLocked(nil, refreshFailed) {
			return errAdbConnClosed
		}
		c.failRefreshLocked(refreshFailed)
		return errAdbConnClosed
	}

	successor, err := c.dialer.Dial(ctx, endpoint, c.route)
	if err != nil {
		// Teardown first. Nothing to discard: the dial is what failed.
		if c.unwindIfClosedLocked(nil, refreshFailed) {
			return errAdbConnClosed
		}
		c.failRefreshLocked(refreshFailed)
		return fmt.Errorf("dial fresh session websocket: %w", err)
	}
	// Both checks are needed. register refuses an already-closed set; isClosed catches a
	// Close landing just after register succeeded, which would install a dead socket as
	// writeTo. No test can drive that window.
	if !c.sockets.register(successor) || c.isClosed() {
		c.discardSuccessorLocked(successor, refreshFailed)
		return errAdbConnClosed
	}
	// The old UUID is dead; the fresh session's control frame latches a new one. uuidReady
	// stays closed.
	c.uuid = ""
	// Makes commitSwap resume in phaseFreshHandshake, so Receive consumes the fresh
	// session's own control frame instead of forwarding it to the adb client.
	c.nextNeedsHandshake = true
	predecessor := c.readFrom
	c.enterDrainingLocked(successor, nextReady)

	// The client must close the predecessor: the server refused the reattach, so it will
	// never cut this websocket and the parked reader would wait forever.
	c.sockets.forget(predecessor)
	return nil
}

// unwindIfClosedLocked reports whether the connection was torn down. Called with the seam
// held; releases it only on a true return.
//
// Ask isClosed, do not match the error: a Close during the ack wait arrives as
// errReattachIndeterminate, not errAdbConnClosed.
func (c *adbConn) unwindIfClosedLocked(successor WSConn, refreshFailed chan struct{}) bool {
	if !c.isClosed() {
		return false
	}
	c.discardSuccessorLocked(successor, refreshFailed)
	return true
}

// failRefreshLocked returns to steady state after a failed refresh, counts the failure, and
// releases any parked reader. The predecessor stays in service. It does not re-arm
// readSince; wsDeadline budgets for that. Called with the seam held; releases it.
func (c *adbConn) failRefreshLocked(refreshFailed chan struct{}) {
	if c.phase == phaseRefreshing {
		c.phase = phaseSteady
	}
	c.consecutiveFailures++
	c.seam.Unlock()
	close(refreshFailed)
}

// discardSuccessorLocked ends a refresh with phaseClosed: discards the successor (nil is
// fine) and releases any parked reader. Called with the seam held; releases it. For a Close
// that raced the attach, whose TryLock failed against this lock, and for a refused reattach
// that is terminal for the connection.
func (c *adbConn) discardSuccessorLocked(successor WSConn, refreshFailed chan struct{}) {
	c.phase = phaseClosed
	c.seam.Unlock()
	c.sockets.forget(successor)
	close(refreshFailed)
}

// dropPoisonedPredecessor closes the websocket that was in service when a reattach outcome
// became unknown, so the reader sees an error instead of silence.
//
// Takes the socket as a parameter, not a re-read of c.readFrom: this runs after the seam is
// released, so readFrom may already have advanced.
func (c *adbConn) dropPoisonedPredecessor(poisoned WSConn) {
	c.logger.Warn("reattach outcome unknown; dropping the predecessor websocket rather than risk silently discarded writes",
		"session", c.sessionUUID())
	c.sockets.forget(poisoned)
}

// reconnectReactively resumes the session after an unexpected websocket failure in steady
// state, retrying once. Reports whether reads may continue.
//
// Lossy: the server drops the chunk in flight when a websocket dies abnormally, so up to
// one read chunk is missing.
func (c *adbConn) reconnectReactively(readErr error) bool {
	c.seam.Lock()
	if c.phase != phaseSteady {
		c.seam.Unlock()
		return false
	}
	sessionUUID, nextReady, refreshFailed := c.startRefreshLocked()

	c.logger.Debug("adb websocket failed unexpectedly; attempting to resume the session",
		"session", sessionUUID, "error", readErr)

	ctx, cancel := context.WithTimeout(context.Background(), c.policy.reactiveTimeout())
	defer cancel()

	// Lock held across the dial and ack, as in refresh, so a Send waits instead of failing
	// mid-recovery.
	successor, err := c.attachSuccessorRetrying(ctx, sessionUUID)
	if err != nil {
		c.logger.Debug("could not resume the adb session", "error", err)
		// Teardown first, as in refresh.
		if c.unwindIfClosedLocked(successor, refreshFailed) {
			return false
		}
		c.failRefreshLocked(refreshFailed)
		return false
	}

	if c.unwindIfClosedLocked(successor, refreshFailed) {
		return false
	}
	dead := c.readFrom
	c.readFrom = successor
	c.readSince = time.Now()
	c.writeTo = successor
	c.phase = phaseSteady
	c.consecutiveFailures = 0
	c.seam.Unlock()
	// Publish to release a Close waiting on the outcome; reads already moved, nothing to
	// drain.
	close(nextReady)

	c.sockets.forget(dead)
	c.logger.Debug("resumed the adb session on a new websocket", "session", sessionUUID)
	return true
}
