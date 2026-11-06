package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// warmInterval is a rotation age far past every test, so only an explicit age test rotates.
const warmInterval = time.Hour

// warmPolicy turns the keeper on with the given rotation age and no jitter.
func warmPolicy(interval time.Duration) WSRefreshPolicy {
	return WSRefreshPolicy{Interval: interval}
}

// warmParams is an attachment with reverse off, so no notifier dials beside the keeper.
func warmParams() AttachmentParams {
	params := forwardingParams()
	params.ReverseEnabled = false
	return params
}

// newWarmDevice attaches a device whose keeper runs under policy against endpoint.
func newWarmDevice(t *testing.T, endpoint string, policy WSRefreshPolicy) *ActiveDevice {
	t.Helper()
	featureOption := testFeatures(newTestDialer(testWait), policy, warmParams(), noopLogger())
	device := NewActiveDevice(featureOption)
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "Test Device - OS 1"}))
	return device
}

// startKeeper runs a keeper outside an attachment, so a test can set its retry delay.
func startKeeper(t *testing.T, endpoint string, rotation, retryDelay time.Duration) (*ActiveDevice,
	*warmSocketKeeper) {
	t.Helper()
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "Test Device - OS 1"}))
	keeper := newWarmSocketKeeper(device,
		&featureConfig{dialer: newTestDialer(testWait), policy: warmPolicy(rotation), logger: noopLogger()},
		testDeviceARN)
	keeper.retryDelay = retryDelay
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		keeper.run(ctx)
		close(stopped)
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	return device, keeper
}

// parkedSocket is the keeper's parked socket, or nil.
func parkedSocket(keeper *warmSocketKeeper) *warmSocket {
	keeper.mu.Lock()
	defer keeper.mu.Unlock()
	return keeper.parked
}

// socketEvents records when each websocket a raw mock serves opens and closes.
type socketEvents struct {
	mu     sync.Mutex
	opened []time.Time
	closed []time.Time
}

// serve blocks until the client closes the websocket, recording both instants.
func (events *socketEvents) serve(ws *websocket.Conn) {
	events.mu.Lock()
	events.opened = append(events.opened, time.Now())
	events.mu.Unlock()
	var discard []byte
	websocket.Message.Receive(ws, &discard)
	events.mu.Lock()
	events.closed = append(events.closed, time.Now())
	events.mu.Unlock()
}

func (events *socketEvents) counts() (opened, closed int) {
	events.mu.Lock()
	defer events.mu.Unlock()
	return len(events.opened), len(events.closed)
}

// fakeWS is a WSConn whose reads a test feeds by hand.
type fakeWS struct {
	reads  chan receivedMessage
	closed chan struct{}
	once   sync.Once
}

func newFakeWS() *fakeWS {
	return &fakeWS{reads: make(chan receivedMessage, 4), closed: make(chan struct{})}
}

func (ws *fakeWS) Send([]byte) error { return nil }

func (ws *fakeWS) Receive() ([]byte, error) {
	select {
	case result := <-ws.reads:
		return result.message, result.err
	case <-ws.closed:
		return nil, errors.New("closed")
	}
}

func (ws *fakeWS) Close() error {
	ws.once.Do(func() { close(ws.closed) })
	return nil
}

func (ws *fakeWS) isClosed() bool {
	select {
	case <-ws.closed:
		return true
	default:
		return false
	}
}

// keeperHolding is a keeper that never runs, its slot holding socket.
func keeperHolding(socket *warmSocket) *warmSocketKeeper {
	keeper := newWarmSocketKeeper(NewActiveDevice(), &featureConfig{logger: noopLogger()}, testDeviceARN)
	keeper.parked = socket
	return keeper
}

// assertDialedOwn asserts ws is the caller's own dial, not a parked socket handed out.
func assertDialedOwn(t *testing.T, ws WSConn) {
	t.Helper()
	_, taken := ws.(*takenSocket)
	assert.False(t, taken, "the parked socket must not be handed out")
}

// refillRequested reports whether a take signalled the keeper, draining the signal.
func refillRequested(keeper *warmSocketKeeper) bool {
	select {
	case <-keeper.refill:
		return true
	default:
		return false
	}
}

func TestGivenAParkedSocketWhenReadThenItsFirstReceiveIsTheReadStartedAtPark(t *testing.T) {
	raw := newFakeWS()
	socket := parkSocket(raw, warmInterval)
	require.False(t, socket.ended(), "an unread parked socket has not ended")

	raw.reads <- receivedMessage{message: []byte("first")}
	raw.reads <- receivedMessage{message: []byte("second")}

	first, err := socket.Receive()
	require.NoError(t, err)
	second, err := socket.Receive()
	require.NoError(t, err)
	assert.Equal(t, "first", string(first), "the parked read is delivered first")
	assert.Equal(t, "second", string(second), "later reads come off the websocket")
}

func TestGivenAParkedSocketTheServerClosedWhenTakenThenItIsClosedAndTheCallerDials(t *testing.T) {
	raw := newFakeWS()
	socket := parkSocket(raw, warmInterval)
	raw.reads <- receivedMessage{err: errors.New("EOF")}
	require.Eventually(t, socket.ended, testWait, notifierPoll, "a server close must end the parked socket")
	keeper := keeperHolding(socket)
	mock := testsupport.NewSessionEchoWSS(t)

	ws, err := keeper.adbSocket(context.Background(), newTestDialer(testWait), mock.URL)

	require.NoError(t, err)
	defer ws.Close()
	assertDialedOwn(t, ws)
	assert.True(t, raw.isClosed(), "the taker closes the dead socket")
	assert.Equal(t, 1, mock.ConnectionsAccepted(), "the caller dials its own")
	assert.False(t, refillRequested(keeper), "a socket the server closed asks for no refill before the rotation")
}

func TestGivenAParkedSocketPastItsAgeWhenTakenThenTheCallerDialsAndTheSlotRefills(t *testing.T) {
	tests := []struct {
		name   string
		maxAge time.Duration
		// wallShift and monotonicShift move the park back on each clock.
		wallShift, monotonicShift time.Duration
	}{
		{name: "older than its age", maxAge: time.Minute, wallShift: 2 * time.Minute, monotonicShift: 2 * time.Minute},
		// The monotonic clock stops while the machine sleeps; the wall clock does not.
		{name: "the machine slept since park", maxAge: warmInterval, wallShift: 2 * time.Minute},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// The pending read never returns, as after a suspend that killed the path unseen.
			raw := newFakeWS()
			socket := parkSocket(raw, testCase.maxAge)
			socket.parkedWall = socket.parkedWall.Add(-testCase.wallShift)
			socket.parkedAt = socket.parkedAt.Add(-testCase.monotonicShift)
			keeper := keeperHolding(socket)
			mock := testsupport.NewSessionEchoWSS(t)

			ws, err := keeper.adbSocket(context.Background(), newTestDialer(testWait), mock.URL)

			require.NoError(t, err)
			defer ws.Close()
			assertDialedOwn(t, ws)
			assert.True(t, raw.isClosed(), "the taker closes the aged socket")
			assert.Equal(t, 1, mock.ConnectionsAccepted(), "the caller dials its own")
			assert.True(t, refillRequested(keeper), "an aged socket is replaced at once")
		})
	}
}

func TestGivenNoKeeperWhenASocketIsAskedForThenTheCallerDials(t *testing.T) {
	var keeper *warmSocketKeeper
	mock := testsupport.NewSessionEchoWSS(t)

	ws, err := keeper.adbSocket(context.Background(), newTestDialer(testWait), mock.URL)

	require.NoError(t, err)
	defer ws.Close()
	assert.Equal(t, 1, mock.ConnectionsAccepted())
}

func TestGivenAnAttachedDeviceWhenIdleThenOneSocketIsParkedAndNoSessionMinted(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))

	waitFor(t, func() bool { return parkedSocket(attachedFeatures(t, device).warmSockets()) != nil },
		"the keeper never parked a socket")
	time.Sleep(notifierSettle)

	assert.Equal(t, 1, mock.ConnectionsAccepted(), "one parked socket, no redial while it lives")
	assert.Zero(t, mock.SessionsOpened(), "a parked socket sends nothing, so the server mints no session")
}

func TestGivenAParkedSocketWhenALocalConnectionArrivesThenItRidesTheParkedSocketAndTheSlotRefills(t *testing.T) {
	mock := switchReplyMock(t, "OKAY")
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
	proxyAddress, _ := runProxy(t, device, warmPolicy(warmInterval))
	keeper := attachedFeatures(t, device).warmSockets()
	waitFor(t, func() bool { return parkedSocket(keeper) != nil }, "the keeper never parked a socket")
	first := parkedSocket(keeper)

	client := openPipedConnection(t, proxyAddress)
	defer client.Close()

	assert.Equal(t, 1, mock.SessionsOpened(), "the connection's first frame minted its session on the parked socket")
	waitFor(t, func() bool {
		refill := parkedSocket(keeper)
		return mock.ConnectionsAccepted() == 2 && refill != nil && refill != first
	}, "the connection never consumed the parked socket, or the slot never refilled")
	time.Sleep(notifierSettle)
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "one take, one refill, nothing more")
	assert.Equal(t, 1, mock.SessionsOpened(), "the refill sends nothing")
}

func TestGivenAParkedSocketWhenAForwardedConnectionArrivesThenItRidesTheParkedSocket(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
	features := attachedFeatures(t, device)
	keeper := features.warmSockets()
	waitFor(t, func() bool { return parkedSocket(keeper) != nil }, "the keeper never parked a socket")
	first := parkedSocket(keeper)

	dialRule(t, installRule(t, features, "tcp:8080"))

	waitFor(t, func() bool { return mock.SessionsOpened() == 1 }, "the opener never reached the device")
	// The device service follows the select's reply, so it can land after the session opens.
	waitFor(t, func() bool { return string(mock.Sessions()[0].Inbound()) == "0012host:transport-any0008tcp:8080" },
		"the prologue is the parked socket's first frames")
	waitFor(t, func() bool {
		refill := parkedSocket(keeper)
		return mock.ConnectionsAccepted() == 2 && refill != nil && refill != first
	}, "the stream never consumed the parked socket, or the slot never refilled")
}

func TestGivenTheServerClosesEveryParkedSocketWhenNoTakeComesThenTheKeeperWaitsForTheRotation(t *testing.T) {
	// A full server pool refuses this way, and a redial there can evict a live connection, so
	// the short retry after a failed dial must not apply.
	events := &socketEvents{}
	endpoint := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		events.mu.Lock()
		events.opened = append(events.opened, time.Now())
		events.mu.Unlock()
		ws.Close()
	})
	startKeeper(t, endpoint, warmInterval, time.Millisecond)

	waitFor(t, func() bool { opened, _ := events.counts(); return opened == 1 }, "the keeper never dialed")
	time.Sleep(notifierSettle)

	opened, _ := events.counts()
	assert.Equal(t, 1, opened, "the keeper waits for a take or a rotation, not the retry delay")
}

func TestGivenTheServerClosesEveryParkedSocketWhenTakesComeThenTheKeeperDialsOncePerRotation(t *testing.T) {
	// A full server pool refuses a socket and can evict a live one. A refill per take would do
	// both per take; the keeper waits for its rotation instead.
	var keeperDials atomic.Int32
	endpoint := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		keeperDials.Add(1)
		ws.Close()
	})
	const rotation = 200 * time.Millisecond
	device := newWarmDevice(t, endpoint, warmPolicy(rotation))
	keeper := attachedFeatures(t, device).warmSockets()
	caller := testsupport.NewSessionEchoWSS(t)
	waitFor(t, func() bool { return keeperDials.Load() == 1 }, "the keeper never dialed")
	started := time.Now()

	takes := 0
	for time.Since(started) < 3*rotation {
		// Only a slot the server emptied: a take of a live socket is a real take, which refills.
		waitFor(t, func() bool { socket := parkedSocket(keeper); return socket == nil || socket.ended() },
			"the server never closed the parked socket")
		ws, err := keeper.adbSocket(context.Background(), newTestDialer(testWait), caller.URL)
		require.NoError(t, err)
		assertDialedOwn(t, ws)
		ws.Close()
		takes++
		time.Sleep(rotation / 10)
	}

	rotations := int(time.Since(started) / rotation)
	assert.LessOrEqual(t, int(keeperDials.Load()), 1+rotations, "at most one keeper dial per rotation, however many takes come")
	assert.Greater(t, int(keeperDials.Load()), 1, "a rotation still tries once")
	assert.Equal(t, takes, caller.ConnectionsAccepted(), "each take dials once, for the caller")
}

func TestGivenAFailedDialWhenTheKeeperRunsThenItRedialsAtTheRetryDelayUntilADialSucceeds(t *testing.T) {
	// The rotation is an hour, so only the retry delay can bring the later dials.
	unreachable, failedDials := testsupport.NewAcceptCounter(t)
	device, keeper := startKeeper(t, "wss://"+unreachable, warmInterval, 20*time.Millisecond)
	waitFor(t, func() bool { return failedDials() >= 3 }, "a failed dial must be retried at the retry delay")

	events := &socketEvents{}
	require.True(t, device.UpdateEndpoint(testDeviceARN, testsupport.NewMockWSS(t, events.serve), "Test Device - OS 1"))

	waitFor(t, func() bool { return parkedSocket(keeper) != nil }, "a retry never parked on the reachable endpoint")
	dialsAtPark := failedDials()
	time.Sleep(notifierSettle)
	opened, _ := events.counts()
	assert.Equal(t, 1, opened, "a parked socket waits for its rotation")
	assert.Equal(t, dialsAtPark, failedDials(), "the retries end once a dial succeeds")
}

func TestGivenARotationShorterThanTheRetryDelayWhenDialsFailThenTheKeeperRedialsEachRotation(t *testing.T) {
	unreachable, failedDials := testsupport.NewAcceptCounter(t)

	startKeeper(t, "wss://"+unreachable, 20*time.Millisecond, time.Hour)

	waitFor(t, func() bool { return failedDials() >= 3 }, "the rotation must cap the retry delay")
}

func TestGivenAParkedSocketWhenItAgesThenItClosesBeforeTheReplacementDials(t *testing.T) {
	events := &socketEvents{}
	endpoint := testsupport.NewMockWSS(t, events.serve)
	newWarmDevice(t, endpoint, warmPolicy(50*time.Millisecond))

	waitFor(t, func() bool { opened, _ := events.counts(); return opened >= 2 }, "an aged socket was never replaced")

	events.mu.Lock()
	defer events.mu.Unlock()
	require.NotEmpty(t, events.closed, "the aged socket must close")
	assert.False(t, events.opened[1].Before(events.closed[0]),
		"the replacement dials after the aged socket closes, so the keeper holds one server slot")
}

func TestGivenARefillSignalFromBeforeTheParkWhenTheKeeperRunsThenOnlyTheParkAnswersIt(t *testing.T) {
	// A take between a rotation and the next park signals for the dial that park makes anyway.
	events := &socketEvents{}
	endpoint := testsupport.NewMockWSS(t, events.serve)
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "Test Device - OS 1"}))
	keeper := newWarmSocketKeeper(device,
		&featureConfig{dialer: newTestDialer(testWait), policy: warmPolicy(warmInterval), logger: noopLogger()},
		testDeviceARN)
	keeper.refill <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go keeper.run(ctx)

	waitFor(t, func() bool { opened, _ := events.counts(); return opened == 1 }, "the keeper never parked a socket")
	time.Sleep(notifierSettle)
	opened, _ := events.counts()
	assert.Equal(t, 1, opened, "a signal from before the park dials nothing more")
}

func TestGivenAParkedSocketWhenTheDeviceIsClearedThenTheSocketCloses(t *testing.T) {
	events := &socketEvents{}
	endpoint := testsupport.NewMockWSS(t, events.serve)
	device := newWarmDevice(t, endpoint, warmPolicy(warmInterval))
	waitFor(t, func() bool { opened, _ := events.counts(); return opened == 1 }, "the keeper never parked a socket")

	_, cleared := device.Clear()

	require.True(t, cleared)
	waitFor(t, func() bool { _, closed := events.counts(); return closed == 1 }, "the detach must close the parked socket")
}

func TestGivenRefreshingOffWhenTheDeviceAttachesThenNoSocketIsParked(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	newWarmDevice(t, mock.URL, WSRefreshPolicy{})

	assert.Never(t, func() bool { return mock.ConnectionsAccepted() > 0 }, notifierSettle, notifierPoll,
		"with refreshing off no socket is parked")
}

func TestGivenATakenSocketWhenTheServerNeverAnswersThenTheRequestFailsWithinOneRoundTripAndIsNotResent(t *testing.T) {
	// A parked socket can die with nothing to show it; the daemon must not wait on it forever.
	// The send succeeded, so the server may have acted on it: no second socket may carry it.
	var requestCarriers atomic.Int32
	silent := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		var discard []byte
		if websocket.Message.Receive(ws, &discard) != nil {
			return
		}
		requestCarriers.Add(1)
		for websocket.Message.Receive(ws, &discard) == nil {
		}
	})
	device := newWarmDevice(t, silent, warmPolicy(warmInterval))
	policy := warmPolicy(warmInterval)
	policy.ReattachTimeout = 100 * time.Millisecond
	proxyAddress, _ := runProxy(t, device, policy)
	keeper := attachedFeatures(t, device).warmSockets()
	waitFor(t, func() bool { return parkedSocket(keeper) != nil }, "the keeper never parked a socket")

	client := dialClient(t, proxyAddress)
	writeAll(t, client, []byte(relayedHostRequest))

	assert.Empty(t, drainClosed(t, client), "no control frame within the round trip ends the connection")
	time.Sleep(notifierSettle)
	assert.Equal(t, int32(1), requestCarriers.Load(), "the request rides the parked socket only")
}

// failingSendWS is a fakeWS whose Sends fail from the failFrom-th on, counting each try.
type failingSendWS struct {
	*fakeWS
	failFrom int
	sends    int
}

func (ws *failingSendWS) Send([]byte) error {
	ws.sends++
	if ws.sends >= ws.failFrom {
		return errors.New("broken pipe")
	}
	return nil
}

func TestGivenATakenSocketWhoseSendFailsWhenSentThenOnlyAFailedFirstSendMovesToAFreshDial(t *testing.T) {
	request := []byte("000chost:version")
	tests := []struct {
		name      string
		failFrom  int
		wantSends int
		wantDials int
	}{
		// A failed first send put nothing on the wire, so a fresh dial carries it.
		{name: "the first send fails", failFrom: 1, wantSends: 1, wantDials: 1},
		// After a sent frame the server may have acted, so the request is never replayed.
		{name: "a later send fails", failFrom: 2, wantSends: 2},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := &failingSendWS{fakeWS: newFakeWS(), failFrom: testCase.failFrom}
			mock := testsupport.NewSessionEchoWSS(t)
			ws, err := keeperHolding(parkSocket(raw, warmInterval)).adbSocket(context.Background(),
				newTestDialer(testWait), mock.URL)
			require.NoError(t, err)
			defer ws.Close()

			var sendErr error
			for range testCase.wantSends {
				sendErr = ws.Send(request)
			}

			assert.Equal(t, testCase.wantSends, raw.sends, "sends tried on the parked socket")
			assert.Equal(t, testCase.wantDials, mock.ConnectionsAccepted(), "fresh dials")
			if testCase.wantDials == 0 {
				assert.Error(t, sendErr, "the failed send is the caller's to handle")
				return
			}
			require.NoError(t, sendErr)
			assert.True(t, raw.isClosed(), "the failed parked socket closes")
			opening, err := ws.Receive()
			require.NoError(t, err)
			frame, err := unmarshalControlFrame(opening)
			require.NoError(t, err)
			assert.Equal(t, armNewSession, frame.arm, "the fresh dial opens a new session")
			echo, err := ws.Receive()
			require.NoError(t, err)
			assert.Equal(t, request, echo, "the request rides the fresh dial")
			assert.Equal(t, 1, mock.SessionsOpened(), "exactly one session")
		})
	}
}

func TestGivenAFailedFirstSendWhenTheFreshSocketCannotTakeOverThenTheSendFailsAndNoSocketLeaks(t *testing.T) {
	tests := []struct {
		name string
		// redial stands in for the fresh dial; it may close socket first.
		redial          func(socket *takenSocket, fresh *fakeWS) (WSConn, error)
		wantFreshClosed bool
	}{
		{name: "the fresh dial fails", redial: func(*takenSocket, *fakeWS) (WSConn, error) {
			return nil, errors.New("connection refused")
		}},
		{name: "the taker closes the socket during the dial", redial: func(socket *takenSocket, fresh *fakeWS) (WSConn, error) {
			socket.Close()
			return fresh, nil
		}, wantFreshClosed: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			parked := &failingSendWS{fakeWS: newFakeWS(), failFrom: 1}
			fresh := newFakeWS()
			socket := &takenSocket{logger: noopLogger(), current: parked}
			socket.redial = func() (WSConn, error) { return testCase.redial(socket, fresh) }

			err := socket.Send([]byte("000chost:version"))

			assert.Error(t, err)
			assert.True(t, parked.isClosed(), "the failed parked socket closes")
			assert.Same(t, parked, socket.current, "no fresh socket takes over")
			assert.Equal(t, testCase.wantFreshClosed, fresh.isClosed(), "a socket dialed after the close must not leak")
		})
	}
}

func TestGivenACancelledContextWhenASocketIsAskedForThenTheParkedSocketStaysAndNothingDials(t *testing.T) {
	// A detach cancels a forwarded stream between its endpoint read and the take.
	socket := parkSocket(newFakeWS(), warmInterval)
	keeper := keeperHolding(socket)
	mock := testsupport.NewSessionEchoWSS(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ws, err := keeper.adbSocket(ctx, newTestDialer(testWait), mock.URL)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, ws)
	assert.Same(t, socket, parkedSocket(keeper), "the detach's own cleanup closes the parked socket")
	assert.Zero(t, mock.ConnectionsAccepted())
}

// gatedCloseWS is a fakeWS whose Close blocks until the test opens the gate.
type gatedCloseWS struct {
	*fakeWS
	closeEntered chan struct{}
	closeGate    chan struct{}
}

func (ws *gatedCloseWS) Close() error {
	close(ws.closeEntered)
	<-ws.closeGate
	return ws.fakeWS.Close()
}

func TestGivenAStaleSocketClosingWhenTheKeeperRotatesThenItsPreDialStepWaitsForTheClose(t *testing.T) {
	// At a full server pool a dial that overlaps the stale socket can evict a live connection.
	raw := &gatedCloseWS{fakeWS: newFakeWS(), closeEntered: make(chan struct{}), closeGate: make(chan struct{})}
	socket := parkSocket(raw, warmInterval)
	raw.reads <- receivedMessage{err: errors.New("EOF")}
	require.Eventually(t, socket.ended, testWait, notifierPoll)
	keeper := keeperHolding(socket)
	mock := testsupport.NewSessionEchoWSS(t)
	go func() {
		if ws, err := keeper.adbSocket(context.Background(), newTestDialer(testWait), mock.URL); err == nil {
			ws.Close()
		}
	}()
	<-raw.closeEntered

	rotated := make(chan struct{})
	go func() {
		keeper.closeParked()
		close(rotated)
	}()

	select {
	case <-rotated:
		t.Fatal("the keeper's pre-dial step must wait for the stale socket's close")
	case <-time.After(notifierSettle):
	}
	close(raw.closeGate)
	select {
	case <-rotated:
	case <-time.After(testWait):
		t.Fatal("the pre-dial step never resumed after the close")
	}
}
