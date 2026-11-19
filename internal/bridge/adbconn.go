package bridge

import (
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// refreshPhase is the state of one proxied connection's websocket-refresh machine.
//
// x/net discards the close code, so the phase is the only signal that an inbound EOF is an
// expected clean cut rather than a failure.
type refreshPhase int

const (
	// phaseHandshake means the next inbound message must be the session control frame.
	phaseHandshake refreshPhase = iota
	// phaseFreshHandshake is phaseHandshake on a websocket from the fresh-session
	// fallback. Kept distinct because only this one can wedge, so only this one is
	// guarded by enforceWSDeadline.
	phaseFreshHandshake
	// phaseSteady means inbound messages are raw ADB bytes for the local adb client.
	phaseSteady
	// phaseRefreshing means a successor is being established, so an EOF on the current
	// websocket is expected: the server cuts it as a consequence of the reattach, which
	// can land before the client has read the ack.
	phaseRefreshing
	// phaseDraining means the successor is acked and live for writes; the predecessor
	// is being drained to EOF before reads move over.
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
//     websocket. readFrom waits for the predecessor's EOF.
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
	// readSince is when the current read target entered service: what wsDeadline
	// measures.
	readSince time.Time
	// drainingSince bounds a predecessor's time to EOF. A drain that never ends
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

// newAdbConn wraps an already-dialed websocket as a session's first. The control frame
// has not been read yet: the first Receive consumes it. route must be the one first was
// dialed on.
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
		c.seam.RUnlock()
		return errAdbConnClosed
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
		c.seam.RUnlock()

		if phase == phaseClosed || source == nil {
			return nil, errAdbConnClosed
		}

		// Read with no lock held: this parks forever on an idle connection.
		message, err := source.Receive()
		if err != nil {
			// A codec fault, not a dead socket: a resume would skip the over-cap
			// payload and desynchronize the adb stream.
			if errors.Is(err, websocket.ErrFrameTooLarge) {
				return nil, err
			}
			// An expected clean cut: wait for the successor and read it instead.
			if c.awaitSuccessor(err) {
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
// Only NEW is expected here; anything else fails closed.
func (c *adbConn) consumeControlFrame(message []byte) error {
	frame, err := parseControlFrame(message)
	if err != nil {
		return fmt.Errorf("read session control frame: %w", err)
	}

	switch frame.status {
	case controlStatusNew:
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
	case controlStatusError:
		return fmt.Errorf("remote refused the adb session: %s", frame.payload)
	default:
		return fmt.Errorf("unexpected control frame status 0x%02x opening a new session", frame.status)
	}
}

// awaitSuccessor reports whether readErr is the expected end of a websocket a refresh is
// replacing, blocking until the successor is published. The RLock cannot be taken until
// refresh releases the fence, so an early reader blocks instead of reading the cut as a
// failure: the old websocket's close can overtake the ack sent on the new one.
func (c *adbConn) awaitSuccessor(readErr error) bool {
	c.seam.RLock()
	phase := c.phase
	nextReady := c.nextReady
	refreshFailed := c.refreshFailed
	c.seam.RUnlock()

	if phase != phaseRefreshing && phase != phaseDraining {
		return false
	}
	if !isBenignCloseError(readErr) {
		return false
	}
	if nextReady == nil || refreshFailed == nil {
		return false
	}

	select {
	case <-nextReady:
		return c.commitSwap()
	case <-refreshFailed:
		// No successor is coming, so this really is the end of the connection.
		return false
	}
}

// commitSwap advances reads onto the successor once the predecessor has drained to EOF,
// closes the predecessor, and returns to steady state. Reports whether the swap happened;
// a Close that landed first makes it a no-op.
func (c *adbConn) commitSwap() bool {
	c.seam.Lock()
	if c.phase != phaseDraining || c.next == nil {
		c.seam.Unlock()
		return false
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
	return true
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

// sessionUUID returns the current session's UUID, or "" before one is announced.
func (c *adbConn) sessionUUID() string {
	c.seam.RLock()
	defer c.seam.RUnlock()
	return c.uuid
}

// enforceWSDeadline closes the current websocket if it outlived deadline, reporting
// whether it did. Zero or negative disables it. An anomaly guard, not a cap.
//
// The lock is exclusive: deciding and re-arming the clock must be one step, or every later
// poll re-decides the same expiry.
func (c *adbConn) enforceWSDeadline(deadline time.Duration) bool {
	if deadline <= 0 {
		return false
	}

	c.seam.Lock()
	phase := c.phase
	stale := c.readFrom
	age := time.Since(c.readSince)
	// commitSwap re-arms readSince, so a fresh handshake's age is its own websocket's.
	staleRead := (phase == phaseSteady || phase == phaseFreshHandshake) && age >= deadline
	// drainingSince is zero until the first refresh, so the IsZero guard is load-bearing.
	stalledDrain := phase == phaseDraining && !c.drainingSince.IsZero() &&
		time.Since(c.drainingSince) >= drainStallMultiple*deadline

	if !staleRead && !stalledDrain {
		c.seam.Unlock()
		return false
	}
	// Re-arm whichever clock justified this firing. The reader owns advancing readFrom.
	if stalledDrain {
		c.drainingSince = time.Now()
	} else {
		c.readSince = time.Now()
	}
	// No uuid left to re-attach to, so the reader cannot recover. Close the phase here or
	// every later poll re-fires on a dead socket.
	if phase == phaseFreshHandshake {
		c.phase = phaseClosed
	}
	c.seam.Unlock()

	// Keep every log below the unlock: sessionUUID takes seam.RLock and RWMutex is not
	// reentrant.
	switch {
	case stalledDrain:
		c.logger.Warn("predecessor websocket never reached EOF after a refresh; dropping it",
			"deadline", drainStallMultiple*deadline, "session", c.sessionUUID())
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
