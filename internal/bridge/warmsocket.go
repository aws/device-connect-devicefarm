// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// warmSocketMaxAge is the ceiling on a park when the refresh interval is longer: under the load
// balancer's idle timeout, which closes a websocket that carries nothing, and under the server's
// idle limit, so a park the server closed is redialed before the session idles out. A park also
// ends at the jittered interval, so a taker's socket is at most an interval old.
const warmSocketMaxAge = 4 * time.Minute

// warmSocketRetryDelay is how soon the keeper redials after a failed dial; the parked socket
// keeps the server session from going idle. Most failed dials never reach the server's pool,
// so a quick retry risks no eviction. Source: pick.
const warmSocketRetryDelay = 15 * time.Second

// sleepTolerance is how far the wall clock may run ahead of the monotonic clock while a socket
// is parked before the machine counts as having slept. Clock slew stays far below it.
const sleepTolerance = time.Second

// errPreSelectUnanswered means the server closed the pre-select's socket, or kept silent, before
// any frame. At a full pool that is its refusal, and a redial there can evict a live connection.
var errPreSelectUnanswered = errors.New("the server closed the websocket before answering its transport select")

// errPongMissed means the parked socket read no pong between two pings.
var errPongMissed = errors.New("no pong since the last ping")

// warmSocket is an adb websocket dialed early and pre-selected: its first frame, the transport
// select, minted sessionUUID and switched it to the device, so a taker sends its device service
// first. One Receive runs from park, so a server close shows at once; the taker's first Receive
// returns it.
type warmSocket struct {
	WSConn
	sessionUUID string
	// first carries the read started at park, then closes.
	first chan receivedMessage
	// dialedAt is the dial start, a monotonic reading, which stops while a Linux or macOS
	// machine sleeps. dialedWall is the same instant on the wall clock, which does not stop.
	// A taker's connection counts the socket's life from dialedAt.
	dialedAt   time.Time
	dialedWall time.Time
	maxAge     time.Duration
	// firstSendErr is the taker's first Send result, latched once.
	firstSend    sync.Once
	firstSendErr error
	// pinged and pongsAtPing are the last ping's mark. The keeper's goroutine alone uses them.
	pinged      bool
	pongsAtPing uint64
}

func parkSocket(ws WSConn, sessionUUID string, dialedAt time.Time, maxAge time.Duration) *warmSocket {
	socket := &warmSocket{WSConn: ws, sessionUUID: sessionUUID, first: make(chan receivedMessage, 1),
		dialedAt: dialedAt, dialedWall: dialedAt.Round(0), maxAge: maxAge}
	go func() {
		message, err := ws.Receive()
		socket.first <- receivedMessage{message: message, err: err}
		close(socket.first)
	}()
	return socket
}

// ended reports whether the server spoke on the socket or closed it while it was parked.
func (socket *warmSocket) ended() bool {
	return len(socket.first) != 0
}

// withinAge reports whether the socket is younger than its age and the machine has not slept
// since park: after a sleep the network path may be gone with nothing to show it.
func (socket *warmSocket) withinAge() bool {
	now := time.Now()
	age := now.Sub(socket.dialedAt)
	slept := now.Round(0).Sub(socket.dialedWall)-age > sleepTolerance
	return age <= socket.maxAge && !slept
}

func (socket *warmSocket) Receive() ([]byte, error) {
	if result, ok := <-socket.first; ok {
		return result.message, result.err
	}
	return socket.WSConn.Receive()
}

func (socket *warmSocket) Send(data []byte) error {
	err := socket.WSConn.Send(data)
	socket.firstSend.Do(func() { socket.firstSendErr = err })
	return err
}

// firstSendFailed reports whether the taker's first Send failed, which put nothing on the wire:
// the opening may move to a fresh dial. Call it on the goroutine that sent.
func (socket *warmSocket) firstSendFailed() bool {
	return socket.firstSendErr != nil
}

// answerRecorder notes whether the server sent a pre-select any frame, to tell a refused select
// from a socket closed unanswered.
type answerRecorder struct {
	WSConn
	answered atomic.Bool
}

func (recorder *answerRecorder) Receive() ([]byte, error) {
	message, err := recorder.WSConn.Receive()
	if err == nil {
		recorder.answered.Store(true)
	}
	return message, err
}

// warmSocketKeeper keeps at most one pre-selected socket parked for one attachment, so the next
// device connection skips the dial and the transport select. It dials at rotation, after a take
// that used or aged out the parked socket, after a parked socket missed its pong, and retryDelay
// after a failed dial or a refused select. After the server closes a pre-select unanswered, or
// answers or closes a parked socket, it waits for the next rotation: a dial into a full server
// pool is refused and can evict a live connection, so it must not loop.
type warmSocketKeeper struct {
	device *ActiveDevice
	cfg    *featureConfig
	arn    string
	// retryDelay is warmSocketRetryDelay; a field so tests can shorten it.
	retryDelay time.Duration

	mu     sync.Mutex
	parked *warmSocket
	// refilling is true from a take's refill request or a park's start until that park lands, so
	// a taker right behind a take waits too.
	refilling bool
	// waiter is the one taker waiting for the refill in flight; it gets the socket, or nil.
	waiter chan *warmSocket
	// refill wakes run after a take. Capacity one.
	refill chan struct{}
}

func newWarmSocketKeeper(device *ActiveDevice, cfg *featureConfig, arn string) *warmSocketKeeper {
	return &warmSocketKeeper{device: device, cfg: cfg, arn: arn, retryDelay: warmSocketRetryDelay,
		refill: make(chan struct{}, 1)}
}

// run parks one socket at a time until ctx ends, then sends a waiting taker to a dial and closes
// whatever is parked. Rotation closes the old socket before it dials, so the keeper holds one
// server slot at most.
func (keeper *warmSocketKeeper) run(ctx context.Context) {
	defer keeper.stop()
	// Off with refreshing off, like the swap it rotates beside.
	if keeper.cfg.policy.Interval <= 0 {
		keeper.cfg.logger.Info("no warm websocket is kept: the refresh interval is unset", "device", keeper.arn)
		return
	}
	retrying := false
	for {
		// This turn's dial answers every take so far.
		select {
		case <-keeper.refill:
		default:
		}
		maxAge := min(keeper.cfg.policy.nextInterval(), warmSocketMaxAge)
		turn := maxAge
		var failedDial *dialFailure
		err := keeper.parkOne(ctx, maxAge)
		if IsSessionStopping(err) {
			return
		}
		switch {
		case err == nil:
			retrying = false
		case errors.As(err, &failedDial):
			turn = min(keeper.retryDelay, maxAge)
			keeper.logDialFailure(ctx, err, retrying)
			retrying = true
		case errors.Is(err, errPreSelectUnanswered):
			retrying = false
			keeper.logRefusedPreSelect(ctx, err, "parking again at the next rotation")
		default:
			// Answered, so admitted: a retry evicts nothing.
			turn = min(keeper.retryDelay, maxAge)
			retrying = false
			keeper.logRefusedPreSelect(ctx, err, "retrying")
		}
		if !keeper.waitForTurn(ctx, turn) {
			return
		}
		keeper.closeParked()
	}
}

// parkOne dials and pre-selects one socket, then lands it. A failed dial is a dialFailure; a
// socket closed before any frame is errPreSelectUnanswered; any other error is the server's
// refusal of the select.
func (keeper *warmSocketKeeper) parkOne(ctx context.Context, maxAge time.Duration) error {
	keeper.mu.Lock()
	keeper.refilling = true
	keeper.mu.Unlock()
	endpoint, ok := keeper.device.EndpointFor(keeper.arn)
	if !ok {
		keeper.land(nil)
		return nil
	}
	dialedAt := time.Now()
	ws, err := keeper.cfg.dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		keeper.land(nil)
		return &dialFailure{cause: err}
	}
	recorder := &answerRecorder{WSConn: ws}
	release := closeOnCancel(ctx, ws)
	sessionUUID, leftover, err := mintSelectedSession(recorder, keeper.cfg.policy.normalized().ReattachTimeout)
	release()
	if err == nil && len(leftover) > 0 {
		// Nothing follows the select's OKAY until a service goes out.
		err = fmt.Errorf("%d unexpected bytes after the transport select's OKAY", len(leftover))
	}
	if err != nil {
		ws.Close()
		// Detach before the waiter wakes: its context ends, so it dials nothing.
		keeper.device.detachIfSessionStopping(keeper.arn, err)
		keeper.land(nil)
		if !recorder.answered.Load() {
			return fmt.Errorf("%w: %w", errPreSelectUnanswered, err)
		}
		return err
	}
	keeper.cfg.logger.Debug("parked a pre-selected adb websocket", "device", keeper.arn, "session", sessionUUID)
	keeper.land(parkSocket(ws, sessionUUID, dialedAt, maxAge))
	return nil
}

// land ends the refill in flight: socket goes to the waiting taker, else to the slot. Nil, a
// failed refill, sends the waiter to its own dial. A waiter that gets a socket asks for the next
// refill, as any take does.
func (keeper *warmSocketKeeper) land(socket *warmSocket) {
	keeper.mu.Lock()
	defer keeper.mu.Unlock()
	keeper.refilling = false
	if waiter := keeper.waiter; waiter != nil {
		keeper.waiter = nil
		waiter <- socket
		if socket != nil {
			keeper.requestRefillLocked()
		}
		return
	}
	if socket != nil {
		keeper.parked = socket
	}
}

// stop ends the keeper: it sends the waiter to a dial and closes the parked socket.
func (keeper *warmSocketKeeper) stop() {
	keeper.mu.Lock()
	waiter := keeper.waiter
	keeper.waiter, keeper.refilling = nil, false
	keeper.mu.Unlock()
	if waiter != nil {
		waiter <- nil
	}
	keeper.closeParked()
}

// logDialFailure warns once per run of failed dials, then logs the retries at Debug: with no
// parked socket the remote session can idle out.
func (keeper *warmSocketKeeper) logDialFailure(ctx context.Context, err error, retrying bool) {
	if ctx.Err() != nil {
		return
	}
	level := slog.LevelWarn
	if retrying {
		level = slog.LevelDebug
	}
	keeper.cfg.logger.Log(ctx, level, "could not dial a warm websocket; retrying", "device", keeper.arn, "error", err)
}

// logRefusedPreSelect logs a pre-select the server refused at Debug: commands still dial their own.
func (keeper *warmSocketKeeper) logRefusedPreSelect(ctx context.Context, err error, next string) {
	if ctx.Err() != nil {
		return
	}
	keeper.cfg.logger.Debug("the warm websocket's transport select failed; "+next, "device", keeper.arn, "error", err)
}

// waitForTurn pings the parked socket every ParkPingInterval and returns when turn passes, a take
// signals a refill, or a ping drop empties the slot. False means ctx ended.
func (keeper *warmSocketKeeper) waitForTurn(ctx context.Context, turn time.Duration) bool {
	timer := time.NewTimer(turn)
	defer timer.Stop()
	var pingTicks <-chan time.Time
	if interval := keeper.cfg.params.ParkPingInterval; interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		pingTicks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-keeper.refill:
			return true
		case <-pingTicks:
			if keeper.pingParked() {
				return true
			}
		}
	}
}

// pingParked pings the parked socket and reports whether it dropped a dead one. It drops a socket
// that read no pong since the last ping, or whose ping write failed. An empty slot, a socket the
// server answered or closed, and one that cannot ping wait for the rotation. The ping runs
// outside mu, which every take takes.
func (keeper *warmSocketKeeper) pingParked() (dropped bool) {
	keeper.mu.Lock()
	socket := keeper.parked
	keeper.mu.Unlock()
	if socket == nil || socket.ended() {
		return false
	}
	conn, ok := socket.WSConn.(pinger)
	if !ok {
		return false
	}
	pongs := conn.PongsSeen()
	if socket.pinged && pongs == socket.pongsAtPing {
		return keeper.dropParked(socket, errPongMissed)
	}
	socket.pinged, socket.pongsAtPing = true, pongs
	if err := conn.Ping(); err != nil {
		return keeper.dropParked(socket, err)
	}
	return false
}

// dropParked closes socket and empties the slot, reporting whether it did. A socket taken
// meanwhile is its taker's. One the server answered or closed meanwhile waits for the rotation:
// at a full pool that close is a refusal, and a redial there can evict a live connection.
func (keeper *warmSocketKeeper) dropParked(socket *warmSocket, cause error) bool {
	if socket.ended() {
		return false
	}
	keeper.mu.Lock()
	held := keeper.parked == socket
	if held {
		keeper.parked = nil
	}
	keeper.mu.Unlock()
	if !held {
		return false
	}
	socket.Close()
	keeper.cfg.logger.Debug("dropped the parked adb websocket; dialing a new one",
		"device", keeper.arn, "error", cause)
	return true
}

func (keeper *warmSocketKeeper) closeParked() {
	keeper.mu.Lock()
	socket := keeper.parked
	keeper.parked = nil
	keeper.mu.Unlock()
	if socket != nil {
		socket.Close()
	}
}

// requestRefillLocked wakes run for a refill, which is in flight from here. Caller holds mu.
func (keeper *warmSocketKeeper) requestRefillLocked() {
	keeper.refilling = true
	select {
	case keeper.refill <- struct{}{}:
	default:
	}
}

// take empties the slot and returns its socket if usable. A socket handed out or past its age
// asks for a refill; one the server answered or closed asks for none (see warmSocketKeeper). On
// an empty slot, a taker waits for the refill in flight unless another already waits: that
// refill started first, on the same path. A stale socket closes under the keeper's mutex, which
// closeParked takes before run dials, so no dial overlaps it. A parked socket holds nothing
// unsent, so Close does not block. Nil-safe.
func (keeper *warmSocketKeeper) take(ctx context.Context) *warmSocket {
	if keeper == nil {
		return nil
	}
	keeper.mu.Lock()
	socket := keeper.parked
	keeper.parked = nil
	if socket == nil {
		if !keeper.refilling || keeper.waiter != nil {
			keeper.mu.Unlock()
			return nil
		}
		arrival := make(chan *warmSocket, 1)
		keeper.waiter = arrival
		keeper.mu.Unlock()
		return keeper.awaitRefill(ctx, arrival)
	}
	defer keeper.mu.Unlock()
	if socket.ended() {
		socket.Close()
		return nil
	}
	keeper.requestRefillLocked()
	if !socket.withinAge() {
		socket.Close()
		return nil
	}
	return socket
}

// refillWait bounds a taker's wait for the refill in flight: one dial and one round trip, the
// refill's own work. A failed refill ends the wait at once; the bound ends a stalled one.
func (keeper *warmSocketKeeper) refillWait() time.Duration {
	return keeper.cfg.dialer.dialTimeout + keeper.cfg.policy.normalized().ReattachTimeout
}

// awaitRefill waits up to refillWait for the refill in flight. Nil: the refill failed, the wait
// ran out, or ctx ended.
func (keeper *warmSocketKeeper) awaitRefill(ctx context.Context, arrival chan *warmSocket) *warmSocket {
	timer := time.NewTimer(keeper.refillWait())
	defer timer.Stop()
	select {
	case socket := <-arrival:
		return socket
	case <-timer.C:
	case <-ctx.Done():
	}
	keeper.mu.Lock()
	withdrawn := keeper.waiter == arrival
	if withdrawn {
		keeper.waiter = nil
	}
	keeper.mu.Unlock()
	if withdrawn {
		return nil
	}
	// land handed the socket over as the wait ended.
	return <-arrival
}

// connOpening wraps ws as a connection's first websocket and plays the connection's opening on
// it. sessionUUID names the session a pre-selected socket already holds; empty for a fresh dial.
// openedAt is when ws was dialed, a parked socket's by the keeper, so the connection counts the
// socket's whole life from it.
type connOpening func(ws WSConn, sessionUUID string, openedAt time.Time) (*adbConn, error)

// openConn plays opening on the socket take hands out, else on a fresh adb-route dial. A failed
// first Send on the taken socket put nothing on the wire, so the opening moves to a fresh dial
// once, counted from that dial. After a Send succeeds nothing is sent again: the server may have
// acted on it. A ctx dead on entry returns its error, as a dial would, and leaves the slot alone.
// A failed dial is a dialFailure. Nil-safe: without a keeper it dials.
func (keeper *warmSocketKeeper) openConn(ctx context.Context, dialer *WSDialer, endpoint string,
	opening connOpening) (*adbConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	socket := keeper.take(ctx)
	if err := ctx.Err(); err != nil {
		if socket != nil {
			socket.Close()
		}
		return nil, err
	}
	if socket != nil {
		keeper.cfg.logger.Debug("took the parked adb websocket", "device", keeper.arn, "session", socket.sessionUUID)
		conn, err := opening(socket, socket.sessionUUID, socket.dialedAt)
		if err == nil || !socket.firstSendFailed() {
			return conn, err
		}
		keeper.cfg.logger.Debug("the parked adb websocket failed its first send; dialing a fresh one", "error", err)
	}
	dialedAt := time.Now()
	ws, err := dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		return nil, &dialFailure{cause: err}
	}
	return opening(ws, "", dialedAt)
}
