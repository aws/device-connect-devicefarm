package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// warmSocketMaxAge caps how long a socket stays parked: under the server's first-message
// timeout, which closes a silent websocket.
const warmSocketMaxAge = 4 * time.Minute

// sleepTolerance is how far the wall clock may run ahead of the monotonic clock while a socket
// is parked before the machine counts as having slept. Clock slew stays far below it.
const sleepTolerance = time.Second

// warmSocket is an adb websocket dialed early and parked with nothing sent, so it owns no
// session: the server mints one from the taker's first frame. One Receive runs from park, so a
// server close shows at once; the taker's first Receive returns it.
type warmSocket struct {
	WSConn
	// first carries the read started at park, then closes.
	first chan receivedMessage
	// parkedAt carries a monotonic reading, which stops while a Linux or macOS machine sleeps.
	// parkedWall is wall-clock time, which does not stop.
	parkedAt   time.Time
	parkedWall time.Time
	maxAge     time.Duration
}

func parkSocket(ws WSConn, maxAge time.Duration) *warmSocket {
	now := time.Now()
	socket := &warmSocket{WSConn: ws, first: make(chan receivedMessage, 1),
		parkedAt: now, parkedWall: now.Round(0), maxAge: maxAge}
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
	age := now.Sub(socket.parkedAt)
	slept := now.Round(0).Sub(socket.parkedWall)-age > sleepTolerance
	return age <= socket.maxAge && !slept
}

func (socket *warmSocket) Receive() ([]byte, error) {
	if result, ok := <-socket.first; ok {
		return result.message, result.err
	}
	return socket.WSConn.Receive()
}

// takenSocket is a parked socket handed to a taker. A first Send that fails put nothing on the
// wire, so that Send moves to a fresh dial once. After a Send succeeds nothing is sent again:
// the server may have acted on it.
type takenSocket struct {
	redial func() (WSConn, error)
	logger *slog.Logger

	mu      sync.Mutex
	current WSConn // the parked socket, then the fresh dial
	sent    bool
	closed  bool
}

func (socket *takenSocket) Send(data []byte) error {
	socket.mu.Lock()
	current, first := socket.current, !socket.sent
	socket.sent = true
	socket.mu.Unlock()
	err := current.Send(data)
	if err == nil || !first {
		return err
	}
	current.Close()
	socket.logger.Debug("the parked adb websocket failed its first send; dialing a fresh one", "error", err)
	fresh, dialErr := socket.redial()
	if dialErr != nil {
		return fmt.Errorf("send on the parked websocket: %w; dial a fresh one: %w", err, dialErr)
	}
	socket.mu.Lock()
	if socket.closed {
		socket.mu.Unlock()
		fresh.Close()
		return err
	}
	socket.current = fresh
	socket.mu.Unlock()
	return fresh.Send(data)
}

func (socket *takenSocket) Receive() ([]byte, error) {
	socket.mu.Lock()
	current := socket.current
	socket.mu.Unlock()
	return current.Receive()
}

func (socket *takenSocket) Close() error {
	socket.mu.Lock()
	socket.closed = true
	current := socket.current
	socket.mu.Unlock()
	return current.Close()
}

// warmSocketKeeper keeps at most one warm socket parked for one attachment, so the next
// local connection or forwarded stream skips the dial. It dials only at rotation and after a
// take that used or aged out the parked socket, never in a loop: a dial into a full server
// pool is refused and can evict a live connection. After a failed dial, or a parked socket the
// server answered or closed before any take, it parks nothing until its next rotation.
type warmSocketKeeper struct {
	device *ActiveDevice
	cfg    *featureConfig
	arn    string

	mu     sync.Mutex
	parked *warmSocket
	// refill wakes run after a take. Capacity one.
	refill chan struct{}
}

func newWarmSocketKeeper(device *ActiveDevice, cfg *featureConfig, arn string) *warmSocketKeeper {
	return &warmSocketKeeper{device: device, cfg: cfg, arn: arn, refill: make(chan struct{}, 1)}
}

// run parks one socket at a time until ctx ends, then closes whatever is parked. Rotation
// closes the old socket before it dials, so the keeper holds one server slot at most.
func (keeper *warmSocketKeeper) run(ctx context.Context) {
	defer keeper.closeParked()
	// Off with refreshing off, like the swap it rotates beside.
	if keeper.cfg.policy.Interval <= 0 {
		keeper.cfg.logger.Info("no warm websocket is kept: the refresh interval is unset", "device", keeper.arn)
		return
	}
	for {
		// This turn's dial answers every take so far.
		select {
		case <-keeper.refill:
		default:
		}
		maxAge := min(keeper.cfg.policy.nextInterval(), warmSocketMaxAge)
		keeper.parkOne(ctx, maxAge)
		if !keeper.waitForTurn(ctx, maxAge) {
			return
		}
		keeper.closeParked()
	}
}

func (keeper *warmSocketKeeper) parkOne(ctx context.Context, maxAge time.Duration) {
	endpoint, ok := keeper.device.EndpointFor(keeper.arn)
	if !ok {
		return
	}
	ws, err := keeper.cfg.dialer.Dial(ctx, endpoint, RouteAdb)
	if err != nil {
		if ctx.Err() == nil {
			keeper.cfg.logger.Debug("could not dial a warm websocket", "device", keeper.arn, "error", err)
		}
		return
	}
	keeper.mu.Lock()
	keeper.parked = parkSocket(ws, maxAge)
	keeper.mu.Unlock()
}

// waitForTurn waits for the rotation age or a take's refill signal. False means ctx ended.
func (keeper *warmSocketKeeper) waitForTurn(ctx context.Context, maxAge time.Duration) bool {
	rotation := time.NewTimer(maxAge)
	defer rotation.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-rotation.C:
		return true
	case <-keeper.refill:
		return true
	}
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

// take empties the slot and returns its socket if usable. A socket handed out or past its age
// asks for a refill; an empty slot or a socket the server answered or closed asks for none (see
// warmSocketKeeper). A stale socket closes under the keeper's mutex, which closeParked takes
// before run dials, so no dial overlaps it. A parked socket holds nothing unsent, so Close does
// not block.
func (keeper *warmSocketKeeper) take() *warmSocket {
	keeper.mu.Lock()
	defer keeper.mu.Unlock()
	socket := keeper.parked
	keeper.parked = nil
	switch {
	case socket == nil:
		return nil
	case socket.ended():
		socket.Close()
		return nil
	}
	select {
	case keeper.refill <- struct{}{}:
	default:
	}
	if !socket.withinAge() {
		socket.Close()
		return nil
	}
	return socket
}

// adbSocket returns the parked socket when it is usable, else dials on the adb route. A dead
// ctx gets its error, as a dial would return it, and leaves the slot alone. Nil-safe: an
// attachment without a keeper dials.
func (keeper *warmSocketKeeper) adbSocket(ctx context.Context, dialer *WSDialer, endpoint string) (WSConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if keeper != nil {
		if socket := keeper.take(); socket != nil {
			redial := func() (WSConn, error) { return dialer.Dial(ctx, endpoint, RouteAdb) }
			return &takenSocket{redial: redial, logger: keeper.cfg.logger, current: socket}, nil
		}
	}
	return dialer.Dial(ctx, endpoint, RouteAdb)
}
