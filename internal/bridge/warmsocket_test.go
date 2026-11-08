// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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

// parkedSessionUUID is the session a raw mock names when it answers a pre-select.
const parkedSessionUUID = "11111111-2222-4333-8444-999999999999"

// takenService is the device service a selected local connection sends after its select.
const takenService = "0008shell:ls"

// refillDelay holds a refill's pre-select reply, so a take lands while the refill is in flight.
const refillDelay = 300 * time.Millisecond

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
	return newWarmDeviceWith(t, endpoint, policy, warmParams())
}

// newWarmDeviceWith is newWarmDevice under params.
func newWarmDeviceWith(t *testing.T, endpoint string, policy WSRefreshPolicy, params AttachmentParams) *ActiveDevice {
	t.Helper()
	featureOption := testFeatures(newTestDialer(testWait), policy, params, noopLogger())
	device := NewActiveDevice(featureOption)
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "Test Device - OS 1"}))
	return device
}

// newKeeper builds a keeper outside an attachment, for a test to tune or run by hand.
func newKeeper(device *ActiveDevice, dialer *WSDialer, rotation time.Duration) *warmSocketKeeper {
	return newWarmSocketKeeper(device,
		&featureConfig{dialer: dialer, policy: warmPolicy(rotation), logger: noopLogger()}, testDeviceARN)
}

// startKeeper runs a keeper outside an attachment, so a test can set its retry delay.
func startKeeper(t *testing.T, endpoint string, rotation, retryDelay time.Duration) (*ActiveDevice,
	*warmSocketKeeper) {
	t.Helper()
	return startPingingKeeper(t, endpoint, rotation, retryDelay, 0)
}

// startPingingKeeper is startKeeper pinging the parked socket every pingInterval; zero pings none.
func startPingingKeeper(t *testing.T, endpoint string, rotation, retryDelay, pingInterval time.Duration) (
	*ActiveDevice, *warmSocketKeeper) {
	t.Helper()
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "Test Device - OS 1"}))
	keeper := newKeeper(device, newTestDialer(testWait), rotation)
	keeper.retryDelay = retryDelay
	keeper.cfg.params.ParkPingInterval = pingInterval
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

// awaitPark waits for the keeper to park a socket and returns it.
func awaitPark(t *testing.T, keeper *warmSocketKeeper) *warmSocket {
	t.Helper()
	waitFor(t, func() bool { return parkedSocket(keeper) != nil }, "the keeper never parked a socket")
	return parkedSocket(keeper)
}

// sessionNamed is the mock session minted as sessionUUID, or nil.
func sessionNamed(mock *testsupport.SessionMockWSS, sessionUUID string) *testsupport.MockSession {
	for _, session := range mock.Sessions() {
		if session.UUID == sessionUUID {
			return session
		}
	}
	return nil
}

// answerPreSelect plays the server's side of a pre-select on ws: the session control frame, then
// the select's OKAY. False when the client closed first.
func answerPreSelect(ws *websocket.Conn) bool {
	var request []byte
	if websocket.Message.Receive(ws, &request) != nil {
		return false
	}
	return websocket.Message.Send(ws, testsupport.FrameNewSession(parkedSessionUUID)) == nil &&
		websocket.Message.Send(ws, []byte(statusOkay)) == nil
}

// socketEvents records when each websocket a raw mock serves opens and closes.
type socketEvents struct {
	mu     sync.Mutex
	opened []time.Time
	closed []time.Time
}

// serve answers the pre-select, then blocks until the client closes, recording both instants.
func (events *socketEvents) serve(ws *websocket.Conn) {
	events.mu.Lock()
	events.opened = append(events.opened, time.Now())
	events.mu.Unlock()
	if answerPreSelect(ws) {
		var discard []byte
		websocket.Message.Receive(ws, &discard)
	}
	events.mu.Lock()
	events.closed = append(events.closed, time.Now())
	events.mu.Unlock()
}

func (events *socketEvents) counts() (opened, closed int) {
	events.mu.Lock()
	defer events.mu.Unlock()
	return len(events.opened), len(events.closed)
}

// slowRefillMock answers the first pre-select with OKAY at once and every later one with
// refillReply after refillDelay. Other data echoes.
func slowRefillMock(t *testing.T, refillReply string) *testsupport.SessionMockWSS {
	t.Helper()
	var selects atomic.Int32
	return testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
			if !bytes.Equal(data, []byte(canonicalSelect)) {
				return data
			}
			if selects.Add(1) == 1 {
				return []byte(statusOkay)
			}
			time.Sleep(refillDelay)
			return []byte(refillReply)
		},
	})
}

// fakeWS is a WSConn whose reads a test feeds by hand. It answers each Ping with a pong, unless
// silent withholds it or pingErr fails the write.
type fakeWS struct {
	reads   chan receivedMessage
	closed  chan struct{}
	once    sync.Once
	silent  bool
	pingErr error
	pings   atomic.Int32
	pongs   atomic.Uint64
}

func newFakeWS() *fakeWS {
	return &fakeWS{reads: make(chan receivedMessage, 4), closed: make(chan struct{})}
}

func (ws *fakeWS) Send([]byte) error { return nil }

func (ws *fakeWS) Ping() error {
	ws.pings.Add(1)
	if ws.pingErr != nil {
		return ws.pingErr
	}
	if !ws.silent {
		ws.pongs.Add(1)
	}
	return nil
}

func (ws *fakeWS) PongsSeen() uint64 { return ws.pongs.Load() }

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
	keeper := newKeeper(NewActiveDevice(), newTestDialer(testWait), warmInterval)
	keeper.parked = socket
	return keeper
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

// openedSocket is what one openConn handed its opening.
type openedSocket struct {
	ws          WSConn
	sessionUUID string
	openedAt    time.Time
	err         error
}

// openThrough runs openConn with an opening that only records what it got: a pre-selected socket
// with its session, or a dial with none.
func openThrough(keeper *warmSocketKeeper, endpoint string) openedSocket {
	var opened openedSocket
	_, opened.err = keeper.openConn(context.Background(), newTestDialer(testWait), endpoint,
		func(ws WSConn, sessionUUID string, openedAt time.Time) (*adbConn, error) {
			opened.ws, opened.sessionUUID, opened.openedAt = ws, sessionUUID, openedAt
			return nil, nil
		})
	return opened
}

// mustOpenThrough is openThrough that fails the test on an error and closes the socket at cleanup.
func mustOpenThrough(t *testing.T, keeper *warmSocketKeeper, endpoint string) openedSocket {
	t.Helper()
	opened := openThrough(keeper, endpoint)
	require.NoError(t, opened.err)
	require.NotNil(t, opened.ws)
	t.Cleanup(func() { opened.ws.Close() })
	return opened
}

func TestGivenAParkedSocketWhenReadThenItsFirstReceiveIsTheReadStartedAtPark(t *testing.T) {
	raw := newFakeWS()
	socket := parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval)
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

func TestGivenAParkedSocketTheServerClosedWhenTakenThenItIsClosedAndNoRefillIsAsked(t *testing.T) {
	raw := newFakeWS()
	socket := parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval)
	raw.reads <- receivedMessage{err: errors.New("EOF")}
	require.Eventually(t, socket.ended, testWait, notifierPoll, "a server close must end the parked socket")
	keeper := keeperHolding(socket)

	assert.Nil(t, keeper.take(context.Background()), "the caller dials its own")
	assert.True(t, raw.isClosed(), "the taker closes the dead socket")
	assert.False(t, refillRequested(keeper), "a socket the server closed asks for no refill before the rotation")
}

func TestGivenAParkedSocketPastItsAgeWhenTakenThenItIsClosedAndTheSlotRefills(t *testing.T) {
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
			socket := parkSocket(raw, parkedSessionUUID, time.Now(), testCase.maxAge)
			socket.dialedWall = socket.dialedWall.Add(-testCase.wallShift)
			socket.dialedAt = socket.dialedAt.Add(-testCase.monotonicShift)
			keeper := keeperHolding(socket)

			assert.Nil(t, keeper.take(context.Background()), "the caller dials its own")
			assert.True(t, raw.isClosed(), "the taker closes the aged socket")
			assert.True(t, refillRequested(keeper), "an aged socket is replaced at once")
		})
	}
}

func TestGivenNoKeeperWhenAConnectionOpensThenItDials(t *testing.T) {
	var keeper *warmSocketKeeper
	mock := testsupport.NewSessionEchoWSS(t)

	beforeDial := time.Now()
	opened := mustOpenThrough(t, keeper, mock.URL)

	assert.Empty(t, opened.sessionUUID, "a dial holds no session yet")
	assert.Equal(t, 1, mock.ConnectionsAccepted())
	assert.False(t, opened.openedAt.Before(beforeDial) || opened.openedAt.After(time.Now()), "a fresh dial's open is its dial start")
}

func TestGivenAParkedSocketWhenTakenThenTheTakerGetsItsDialStart(t *testing.T) {
	// The taker counts the socket's life from its dial, so a long park shortens its first cycle.
	dialedAt := time.Now().Add(-warmInterval / 2)
	keeper := keeperHolding(parkSocket(newFakeWS(), parkedSessionUUID, dialedAt, warmInterval))

	opened := mustOpenThrough(t, keeper, "wss://127.0.0.1:1/adb")

	assert.Equal(t, parkedSessionUUID, opened.sessionUUID, "the parked socket is the one handed out")
	assert.True(t, opened.openedAt.Equal(dialedAt), "a taken socket's open is the dial that parked it")
}

func TestGivenACancelledContextWhenAConnectionOpensThenTheParkedSocketStaysAndNothingDials(t *testing.T) {
	// A detach cancels a forwarded stream between its endpoint read and the take.
	socket := parkSocket(newFakeWS(), parkedSessionUUID, time.Now(), warmInterval)
	keeper := keeperHolding(socket)
	mock := testsupport.NewSessionEchoWSS(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	openings := 0

	conn, err := keeper.openConn(ctx, newTestDialer(testWait), mock.URL, func(WSConn, string, time.Time) (*adbConn, error) {
		openings++
		return nil, nil
	})

	assert.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, conn)
	assert.Zero(t, openings)
	assert.Same(t, socket, parkedSocket(keeper), "the detach's own cleanup closes the parked socket")
	assert.Zero(t, mock.ConnectionsAccepted())
}

// stalledRefillKeeper is a keeper that never runs, a refill marked in flight that never lands,
// and a wait bound of shortTimeout twice over.
func stalledRefillKeeper() *warmSocketKeeper {
	keeper := newKeeper(NewActiveDevice(), newTestDialer(shortTimeout), warmInterval)
	keeper.cfg.policy.ReattachTimeout = shortTimeout
	keeper.refilling = true
	return keeper
}

// shortTimeout is a dial and reply bound short enough for a test to wait out.
const shortTimeout = 20 * time.Millisecond

// waiterOf reports whether a taker waits on keeper.
func waiterOf(keeper *warmSocketKeeper) bool {
	keeper.mu.Lock()
	defer keeper.mu.Unlock()
	return keeper.waiter != nil
}

// takeAsync runs take on its own goroutine and returns where its result lands.
func takeAsync(ctx context.Context, keeper *warmSocketKeeper) <-chan *warmSocket {
	taken := make(chan *warmSocket, 1)
	go func() { taken <- keeper.take(ctx) }()
	return taken
}

func TestGivenARefillInFlightWhenItOutlastsTheWaitBoundThenTheTakerDials(t *testing.T) {
	keeper := stalledRefillKeeper()
	started := time.Now()

	socket := keeper.take(context.Background())

	assert.Nil(t, socket, "a stalled refill sends the taker to a dial")
	assert.GreaterOrEqual(t, time.Since(started), keeper.refillWait(), "the taker waits out the bound")
	assert.False(t, waiterOf(keeper), "the taker withdraws, so the next one may wait")
}

func TestGivenATakerWaitingForTheRefillWhenItsContextOrTheKeeperEndsThenItGoesToADialAtOnce(t *testing.T) {
	tests := []struct {
		name string
		end  func(keeper *warmSocketKeeper, cancel context.CancelFunc)
	}{
		{name: "the connection is cancelled", end: func(_ *warmSocketKeeper, cancel context.CancelFunc) { cancel() }},
		{name: "the keeper stops at a detach", end: func(keeper *warmSocketKeeper, _ context.CancelFunc) { keeper.stop() }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			keeper := stalledRefillKeeper()
			keeper.cfg.policy.ReattachTimeout = warmInterval
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			taken := takeAsync(ctx, keeper)
			waitFor(t, func() bool { return waiterOf(keeper) }, "the taker never waited for the refill")

			testCase.end(keeper, cancel)

			select {
			case socket := <-taken:
				assert.Nil(t, socket)
			case <-time.After(testWait):
				t.Fatal("the taker kept waiting")
			}
			assert.False(t, waiterOf(keeper), "no waiter is left behind")
		})
	}
}

func TestGivenAnAttachedDeviceWhenIdleThenOneSocketIsParkedPreSelected(t *testing.T) {
	mock := switchReplyMock(t, statusOkay)
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))

	parked := awaitPark(t, attachedFeatures(t, device).warmSockets())
	time.Sleep(notifierSettle)

	assert.Equal(t, 1, mock.ConnectionsAccepted(), "one parked socket, no redial while it lives")
	require.Equal(t, 1, mock.SessionsOpened(), "the pre-select mints the parked socket's session")
	session := mock.Sessions()[0]
	assert.Equal(t, session.UUID, parked.sessionUUID, "the park latches the session its select minted")
	assert.Equal(t, canonicalSelect, string(session.Inbound()), "the park sends the select and nothing more")
}

func TestGivenAParkedSocketWhenASelectedLocalConnectionArrivesThenItSendsOnlyItsServiceAndTheSlotRefills(t *testing.T) {
	mock := switchReplyMock(t, statusOkay)
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
	proxyAddress, _ := runProxy(t, device, warmPolicy(warmInterval))
	keeper := attachedFeatures(t, device).warmSockets()
	first := awaitPark(t, keeper)

	client := dialClient(t, proxyAddress)
	selectAnyTransport(t, client)
	writeAll(t, client, []byte(takenService))

	assert.Equal(t, takenService, string(readN(t, client, len(takenService))), "the device's reply reaches the client")
	session := sessionNamed(mock, first.sessionUUID)
	require.NotNil(t, session, "the connection rides the parked socket's session")
	assert.Equal(t, canonicalSelect+takenService, string(session.Inbound()),
		"the park's select, then the service alone: the take sends no second select")
	waitFor(t, func() bool {
		refill := parkedSocket(keeper)
		return mock.ConnectionsAccepted() == 2 && refill != nil && refill != first
	}, "the connection never consumed the parked socket, or the slot never refilled")
	time.Sleep(notifierSettle)
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "one take, one refill, nothing more")
	assert.Equal(t, 2, mock.SessionsOpened(), "the refill pre-selects a session of its own")
}

func TestGivenATakenSocketWhenTheDevicesFirstReplyArrivesThenTheClientGetsItByteForByte(t *testing.T) {
	tests := []struct {
		name  string
		reply string
	}{
		{name: "a refusal", reply: "FAIL0009no device"},
		{name: "a status word coalesced with output", reply: statusOkay + "hello"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
				OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
					if bytes.Equal(data, []byte(canonicalSelect)) {
						return []byte(statusOkay)
					}
					return []byte(testCase.reply)
				},
			})
			device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
			proxyAddress, _ := runProxy(t, device, warmPolicy(warmInterval))
			first := awaitPark(t, attachedFeatures(t, device).warmSockets())
			client := dialClient(t, proxyAddress)
			selectAnyTransport(t, client)

			writeAll(t, client, []byte(takenService))

			assert.Equal(t, testCase.reply, string(readN(t, client, len(testCase.reply))))
			assert.NotNil(t, sessionNamed(mock, first.sessionUUID), "the reply came over the parked session")
		})
	}
}

func TestGivenATakenSocketWhenTheServerClosesItBeforeTheFirstReplyThenTheConnectionEndsWithNoReattach(t *testing.T) {
	// At a full server pool the close can be an eviction, and a reattach dial there can evict a
	// live connection.
	var reattaches atomic.Int32
	endpoint := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		var first []byte
		if websocket.Message.Receive(ws, &first) != nil {
			return
		}
		if isUUIDv4(string(first)) {
			reattaches.Add(1)
			ws.Close()
			return
		}
		if websocket.Message.Send(ws, testsupport.FrameNewSession(parkedSessionUUID)) != nil ||
			websocket.Message.Send(ws, []byte(statusOkay)) != nil {
			return
		}
		var service []byte
		websocket.Message.Receive(ws, &service)
		ws.Close()
	})
	// adb reads the coded close as its error line; a forwarded stream has no adb protocol to carry it.
	tests := []struct {
		name  string
		open  func(t *testing.T, device *ActiveDevice) net.Conn
		reply string
	}{
		{name: "a selected local connection", reply: "FAIL" + string(framed("Device Farm closed the connection to the device (close code 1000)")),
			open: func(t *testing.T, device *ActiveDevice) net.Conn {
				proxyAddress, _ := runProxy(t, device, fastPolicy())
				client := dialClient(t, proxyAddress)
				selectAnyTransport(t, client)
				writeAll(t, client, []byte(takenService))
				return client
			}},
		{name: "a forwarded connection", open: func(t *testing.T, device *ActiveDevice) net.Conn {
			conn := dialRule(t, installRule(t, attachedFeatures(t, device), "tcp:8080"))
			require.NoError(t, conn.SetDeadline(time.Now().Add(testTimeout)))
			return conn
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			reattaches.Store(0)
			device := newWarmDevice(t, endpoint, warmPolicy(warmInterval))
			awaitPark(t, attachedFeatures(t, device).warmSockets())

			conn := testCase.open(t, device)

			assert.Equal(t, testCase.reply, string(drainClosed(t, conn)), "the connection ends with no device bytes")
			time.Sleep(notifierSettle)
			assert.Zero(t, reattaches.Load(), "a session that never answered is not resumed")
		})
	}
}

func TestGivenAParkedSocketWhenAForwardedConnectionArrivesThenItSendsOnlyTheDeviceService(t *testing.T) {
	// Replies in frame order: the park's select, the stream's service, the refill's select.
	mock := newPrologueMock(t, statusOkay, statusOkay, statusOkay)
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
	features := attachedFeatures(t, device)
	keeper := features.warmSockets()
	first := awaitPark(t, keeper)

	dialRule(t, installRule(t, features, "tcp:8080"))

	waitFor(t, func() bool {
		session := sessionNamed(mock.SessionMockWSS, first.sessionUUID)
		return session != nil && string(session.Inbound()) == canonicalSelect+"0008tcp:8080"
	}, "the stream's device service never followed the park's select on the parked socket")
	waitFor(t, func() bool {
		refill := parkedSocket(keeper)
		return mock.ConnectionsAccepted() == 2 && refill != nil && refill != first
	}, "the stream never consumed the parked socket, or the slot never refilled")
}

func TestGivenAParkedSocketWhenAHostRequestIsRelayedThenItDialsAndTheParkStays(t *testing.T) {
	// A selected session cannot serve a host request.
	mock := switchReplyMock(t, statusOkay)
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
	proxyAddress, _ := runProxy(t, device, warmPolicy(warmInterval))
	keeper := attachedFeatures(t, device).warmSockets()
	first := awaitPark(t, keeper)

	client := openPipedConnection(t, proxyAddress)
	defer client.Close()

	assert.Same(t, first, parkedSocket(keeper), "the parked socket stays for a device connection")
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "the host request dials its own")
	assert.Equal(t, 2, mock.SessionsOpened(), "the host request mints its own session")
}

// assertTakeDoesNotWait asserts a take on keeper's empty slot returns at once: no refill is
// marked in flight.
func assertTakeDoesNotWait(t *testing.T, keeper *warmSocketKeeper) {
	t.Helper()
	started := time.Now()
	assert.Nil(t, keeper.take(context.Background()), "nothing is parked")
	assert.Less(t, time.Since(started), notifierSettle, "no refill is left marked in flight")
}

func TestGivenTheServerRefusesThePreSelectWhenTheKeeperParksThenNothingIsParkedAndItRetriesAtTheRetryDelay(t *testing.T) {
	// The server answered, so it admitted the socket: a retry evicts nothing.
	tests := []struct {
		name string
		mock func(t *testing.T) *testsupport.SessionMockWSS
	}{
		{name: "the device's adb server answers FAIL", mock: func(t *testing.T) *testsupport.SessionMockWSS {
			return switchReplyMock(t, "FAIL0009no device")
		}},
		{name: "the server refuses the session", mock: func(t *testing.T) *testsupport.SessionMockWSS {
			return testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RefuseNewSessions: -1})
		}},
		{name: "bytes follow the select's OKAY", mock: func(t *testing.T) *testsupport.SessionMockWSS {
			return switchReplyMock(t, statusOkay+"junk")
		}},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			mock := testCase.mock(t)
			_, keeper := startKeeper(t, mock.URL, warmInterval, shortTimeout)

			waitFor(t, func() bool { return mock.ConnectionsAccepted() >= 3 }, "a refused pre-select is retried")

			assert.Nil(t, parkedSocket(keeper), "a refused pre-select parks nothing")
			assertTakeDoesNotWait(t, keeper)
		})
	}
}

func TestGivenTheServerClosesThePreSelectUnansweredWhenTheKeeperParksThenItWaitsForTheRotation(t *testing.T) {
	// A full server pool refuses this way, and a redial there can evict a live connection, so the
	// short retry delay must not apply.
	var accepted atomic.Int32
	endpoint := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		accepted.Add(1)
		ws.Close()
	})
	_, keeper := startKeeper(t, endpoint, warmInterval, time.Millisecond)

	waitFor(t, func() bool { return accepted.Load() == 1 }, "the keeper never dialed")
	time.Sleep(notifierSettle)

	assert.Equal(t, int32(1), accepted.Load(), "the keeper waits for the rotation, not the retry delay")
	assert.Nil(t, parkedSocket(keeper), "a closed pre-select parks nothing")
	assertTakeDoesNotWait(t, keeper)
}

func TestGivenTheServerClosesAParkedSocketWhenNoTakeComesThenTheKeeperWaitsForTheRotation(t *testing.T) {
	var accepted atomic.Int32
	endpoint := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		accepted.Add(1)
		if answerPreSelect(ws) {
			ws.Close()
		}
	})
	_, keeper := startKeeper(t, endpoint, warmInterval, time.Millisecond)
	parked := awaitPark(t, keeper)

	require.Eventually(t, parked.ended, testWait, notifierPoll, "the server's close must end the parked socket")
	time.Sleep(notifierSettle)

	assert.Equal(t, int32(1), accepted.Load(), "a close the keeper has not seen through a take dials nothing")
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
		opened := mustOpenThrough(t, keeper, caller.URL)
		assert.Empty(t, opened.sessionUUID, "no pre-selected socket survives the server")
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

	awaitPark(t, keeper)
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
	keeper := newKeeper(device, newTestDialer(testWait), warmInterval)
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
	awaitPark(t, attachedFeatures(t, device).warmSockets())

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

func TestGivenATakenSocketWhenTheDeviceNeverAnswersThenTheRequestFailsWithinOneRoundTripAndIsNotResent(t *testing.T) {
	// A parked socket can die unseen after its pre-select; the daemon must not wait on it forever.
	// The send succeeded, so the server may have acted on it: no second socket may carry it.
	var requestCarriers atomic.Int32
	silent := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		if !answerPreSelect(ws) {
			return
		}
		var discard []byte
		for websocket.Message.Receive(ws, &discard) == nil {
			requestCarriers.Add(1)
		}
	})
	device := newWarmDevice(t, silent, warmPolicy(warmInterval))
	policy := warmPolicy(warmInterval)
	policy.ReattachTimeout = 100 * time.Millisecond
	proxyAddress, _ := runProxy(t, device, policy)
	awaitPark(t, attachedFeatures(t, device).warmSockets())

	client := dialClient(t, proxyAddress)
	selectAnyTransport(t, client)
	writeAll(t, client, []byte(takenService))

	assert.Empty(t, drainClosed(t, client), "no reply within the round trip ends the connection")
	time.Sleep(notifierSettle)
	assert.Equal(t, int32(1), requestCarriers.Load(), "the request rides the parked socket only")
}

func TestGivenATakenPreSelectedConnectionWhenItRefreshesThenItReattachesToTheParkedSession(t *testing.T) {
	mock := switchReplyMock(t, statusOkay)
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
	proxyAddress, _ := runProxy(t, device, fastPolicy())
	first := awaitPark(t, attachedFeatures(t, device).warmSockets())
	client := dialClient(t, proxyAddress)
	selectAnyTransport(t, client)
	writeAll(t, client, []byte(takenService))
	require.Equal(t, takenService, string(readN(t, client, len(takenService))))

	waitFor(t, func() bool { return mock.Reattaches() >= 1 }, "the taken connection never refreshed")
	probe := []byte("after the swap")
	writeAll(t, client, probe)

	assert.Equal(t, probe, readN(t, client, len(probe)), "the stream survives the swap")
	session := sessionNamed(mock, first.sessionUUID)
	require.NotNil(t, session, "the reattach resumes the session the park minted")
	assert.Equal(t, canonicalSelect+takenService+string(probe), string(session.Inbound()),
		"every byte lands on the parked session, in order")
	assert.Empty(t, session.Dropped(), "no byte lands on a superseded websocket")
}

func TestGivenARefillInFlightWhenOneTakerFindsTheSlotEmptyThenItWaitsAndRidesTheRefill(t *testing.T) {
	mock := slowRefillMock(t, statusOkay)
	_, keeper := startKeeper(t, mock.URL, warmInterval, warmInterval)
	first := awaitPark(t, keeper)

	taken := mustOpenThrough(t, keeper, mock.URL)
	waited := mustOpenThrough(t, keeper, mock.URL)

	assert.Equal(t, first.sessionUUID, taken.sessionUUID, "the first take rides the park")
	assert.NotEmpty(t, waited.sessionUUID, "the second take rides the refill instead of dialing")
	assert.NotEqual(t, first.sessionUUID, waited.sessionUUID, "the refill holds a session of its own")
	next := awaitPark(t, keeper)
	assert.NotEqual(t, waited.sessionUUID, next.sessionUUID, "the waiter's take asks for the next refill")
}

func TestGivenARefillInFlightWhenTwoTakersFindTheSlotEmptyThenOneWaitsAndTheOtherDials(t *testing.T) {
	mock := slowRefillMock(t, statusOkay)
	_, keeper := startKeeper(t, mock.URL, warmInterval, warmInterval)
	awaitPark(t, keeper)
	mustOpenThrough(t, keeper, mock.URL)

	results := make(chan openedSocket, 2)
	for range 2 {
		go func() { results <- openThrough(keeper, mock.URL) }()
	}
	var preSelected, dialed int
	for range 2 {
		opened := <-results
		require.NoError(t, opened.err)
		t.Cleanup(func() { opened.ws.Close() })
		if opened.sessionUUID != "" {
			preSelected++
		} else {
			dialed++
		}
	}

	assert.Equal(t, 1, preSelected, "one taker waits for the refill")
	assert.Equal(t, 1, dialed, "the other dials, as before")
}

func TestGivenARefillInFlightWhenTheRefillFailsThenTheWaiterDials(t *testing.T) {
	mock := slowRefillMock(t, "FAIL0009no device")
	_, keeper := startKeeper(t, mock.URL, warmInterval, warmInterval)
	awaitPark(t, keeper)
	mustOpenThrough(t, keeper, mock.URL)
	started := time.Now()

	waited := mustOpenThrough(t, keeper, mock.URL)

	assert.Empty(t, waited.sessionUUID, "a failed refill sends the waiter to a dial")
	assert.GreaterOrEqual(t, time.Since(started), refillDelay/2, "the taker waited for the refill first")
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

func TestGivenATakenSocketWhoseSendFailsWhenOpenedThenOnlyAFailedFirstSendMovesToAFreshDial(t *testing.T) {
	request := []byte(takenService)
	tests := []struct {
		name      string
		failFrom  int
		wantSends int
		wantDials int
	}{
		// A failed first send put nothing on the wire, so a fresh dial carries the opening.
		{name: "the first send fails", failFrom: 1, wantSends: 1, wantDials: 1},
		// After a sent frame the server may have acted, so the opening is never replayed.
		{name: "a later send fails", failFrom: 2, wantSends: 2},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := &failingSendWS{fakeWS: newFakeWS(), failFrom: testCase.failFrom}
			mock := testsupport.NewSessionEchoWSS(t)
			keeper := keeperHolding(parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval))
			var openedFor []string
			opening := func(ws WSConn, sessionUUID string, openedAt time.Time) (*adbConn, error) {
				openedFor = append(openedFor, sessionUUID)
				for range testCase.wantSends {
					if err := ws.Send(request); err != nil {
						ws.Close()
						return nil, err
					}
				}
				return newAdbConn(nil, nil, testDeviceARN, RouteAdb, ws, openedAt, WSRefreshPolicy{}, newActivityTracker(nil),
					noopLogger(), withSelectedSession(sessionUUID)), nil
			}

			conn, err := keeper.openConn(context.Background(), newTestDialer(testWait), mock.URL, opening)

			assert.Equal(t, testCase.wantSends, raw.sends, "sends tried on the parked socket")
			assert.Equal(t, testCase.wantDials, mock.ConnectionsAccepted(), "fresh dials")
			if testCase.wantDials == 0 {
				assert.Error(t, err, "the failed send is the caller's to handle")
				assert.Equal(t, []string{parkedSessionUUID}, openedFor, "the opening ran once, on the parked socket")
				return
			}
			require.NoError(t, err)
			defer conn.Close()
			assert.True(t, raw.isClosed(), "the failed parked socket closes")
			assert.Equal(t, []string{parkedSessionUUID, ""}, openedFor, "the opening moved to a fresh dial")
			echo, err := conn.Receive()
			require.NoError(t, err)
			assert.Equal(t, request, echo, "the opening rides the fresh dial")
			assert.Equal(t, 1, mock.SessionsOpened(), "exactly one session")
		})
	}
}

func TestGivenAFailedFirstSendWhenTheFreshDialFailsThenTheOpeningFailsAsADialFailure(t *testing.T) {
	raw := &failingSendWS{fakeWS: newFakeWS(), failFrom: 1}
	keeper := keeperHolding(parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval))
	unreachable, _ := testsupport.NewAcceptCounter(t)
	openings := 0

	_, err := keeper.openConn(context.Background(), newTestDialer(testWait), "wss://"+unreachable,
		func(ws WSConn, _ string, _ time.Time) (*adbConn, error) {
			openings++
			return nil, ws.Send([]byte(takenService))
		})

	var failure *dialFailure
	assert.ErrorAs(t, err, &failure, "the refusal gate sees a failed dial")
	assert.Equal(t, 1, raw.sends, "the parked socket is tried once")
	assert.Equal(t, 1, openings, "no opening runs without a socket")
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
	socket := parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval)
	raw.reads <- receivedMessage{err: errors.New("EOF")}
	require.Eventually(t, socket.ended, testWait, notifierPoll)
	keeper := keeperHolding(socket)
	go keeper.take(context.Background())
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

// parkPingInterval is short enough that a test sees several pings, wide enough that a loaded
// machine answers one inside it.
const parkPingInterval = 60 * time.Millisecond

// pingingKeeperHolding is keeperHolding pinging its slot every parkPingInterval.
func pingingKeeperHolding(socket *warmSocket) *warmSocketKeeper {
	keeper := keeperHolding(socket)
	keeper.cfg.params.ParkPingInterval = parkPingInterval
	return keeper
}

// silentAfterPreSelect answers the pre-select of the first websocket it serves, then reads every
// frame without answering a ping. Every later websocket closes at once, unanswered.
func silentAfterPreSelect(t *testing.T) (endpoint string, accepted *atomic.Int32) {
	t.Helper()
	accepted = &atomic.Int32{}
	endpoint = testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		if accepted.Add(1) > 1 {
			ws.Close()
			return
		}
		if !answerPreSelect(ws) {
			return
		}
		// Raw frame reads: x/net answers a ping only inside HandleFrame.
		for {
			frame, err := ws.NewFrameReader()
			if err != nil {
				return
			}
			io.Copy(io.Discard, frame)
		}
	})
	return endpoint, accepted
}

func TestGivenAParkedSocketWhenThePingIntervalPassesThenItIsPingedAndStaysParked(t *testing.T) {
	mock := switchReplyMock(t, statusOkay)
	_, keeper := startPingingKeeper(t, mock.URL, warmInterval, warmInterval, parkPingInterval)
	first := awaitPark(t, keeper)

	waitFor(t, func() bool { return mock.PingsSeen() >= 3 }, "the parked socket was never pinged on its interval")

	assert.Same(t, first, parkedSocket(keeper), "an answered ping keeps the socket parked")
	assert.Equal(t, 1, mock.ConnectionsAccepted(), "answered pings cost no redial")
}

func TestGivenNoPingIntervalWhenASocketIsParkedThenNoPingIsSent(t *testing.T) {
	mock := switchReplyMock(t, statusOkay)
	_, keeper := startKeeper(t, mock.URL, warmInterval, warmInterval)
	awaitPark(t, keeper)

	time.Sleep(notifierSettle)

	assert.Zero(t, mock.PingsSeen())
}

func TestGivenAParkedSocketWhosePongIsMissedWhenTheNextPingIsDueThenItClosesAndANewSocketParksWithNoTake(t *testing.T) {
	// Through an attachment, so the ping interval travels in the attachment's params.
	mock := switchReplyMock(t, statusOkay)
	mock.SwallowPongs(true)
	params := warmParams()
	params.ParkPingInterval = parkPingInterval
	device := newWarmDeviceWith(t, mock.URL, warmPolicy(warmInterval), params)
	keeper := attachedFeatures(t, device).warmSockets()
	first := awaitPark(t, keeper)

	waitFor(t, func() bool {
		replacement := parkedSocket(keeper)
		return first.ended() && replacement != nil && replacement != first
	}, "an unanswered ping never replaced the parked socket")

	assert.GreaterOrEqual(t, mock.PingsSeen(), 1, "the drop follows an unanswered ping")
	assert.GreaterOrEqual(t, mock.ConnectionsAccepted(), 2, "the replacement is a new dial")
}

func TestGivenAParkedSocketWhosePingWriteFailsWhenPingedThenItClosesAndTheTurnEndsAtOnce(t *testing.T) {
	// The turn's end is where run closes the slot and dials the replacement.
	raw := newFakeWS()
	raw.pingErr = errors.New("broken pipe")
	keeper := pingingKeeperHolding(parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval))
	started := time.Now()

	nextTurn := keeper.waitForTurn(context.Background(), warmInterval)

	assert.True(t, nextTurn)
	assert.Less(t, time.Since(started), testWait, "the failed ping ends the turn, not the rotation")
	assert.True(t, raw.isClosed(), "the socket whose ping failed closes")
	assert.Nil(t, parkedSocket(keeper), "the slot empties")
}

func TestGivenTheServerClosedAParkedSocketWhenThePingIntervalPassesThenNoPingIsSentAndTheKeeperWaitsForTheRotation(t *testing.T) {
	raw := newFakeWS()
	socket := parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval)
	raw.reads <- receivedMessage{err: errors.New("EOF")}
	require.Eventually(t, socket.ended, testWait, notifierPoll, "a server close must end the parked socket")
	keeper := pingingKeeperHolding(socket)
	started := time.Now()

	keeper.waitForTurn(context.Background(), notifierSettle)

	assert.GreaterOrEqual(t, time.Since(started), notifierSettle, "only the turn's own end ends the wait")
	assert.Zero(t, raw.pings.Load(), "a socket the server closed is not pinged")
	assert.Same(t, socket, parkedSocket(keeper), "the next take or rotation handles it")
	assert.False(t, keeper.dropParked(socket, errPongMissed), "a socket the server closed is never dropped")
	assert.False(t, raw.isClosed())
}

func TestGivenAMissedPongWhenTheReplacementsPreSelectIsUnansweredThenTheKeeperWaitsForTheRotation(t *testing.T) {
	// The replacement meets a full pool's refusal: a close before any frame. The short retry delay
	// must not apply.
	endpoint, accepted := silentAfterPreSelect(t)
	startPingingKeeper(t, endpoint, warmInterval, time.Millisecond, parkPingInterval)

	waitFor(t, func() bool { return accepted.Load() == 2 }, "an unanswered ping never brought a replacement dial")
	time.Sleep(notifierSettle)

	assert.Equal(t, int32(2), accepted.Load(), "after the unanswered pre-select the keeper waits for the rotation")
}

func TestGivenASocketTakenBetweenTwoPingsWhenItsPongIsMissingThenTheTakenSocketIsNotClosed(t *testing.T) {
	raw := newFakeWS()
	raw.silent = true
	socket := parkSocket(raw, parkedSessionUUID, time.Now(), warmInterval)
	keeper := pingingKeeperHolding(socket)
	require.False(t, keeper.pingParked(), "the first ping only marks the socket")

	taken := keeper.take(context.Background())

	require.Same(t, socket, taken)
	assert.Equal(t, int32(1), raw.pings.Load(), "the take sends no ping")
	assert.False(t, keeper.pingParked(), "an empty slot is not a missed pong")
	assert.False(t, keeper.dropParked(socket, errPongMissed), "a drop leaves a taken socket alone")
	assert.False(t, raw.isClosed(), "the taker owns the socket")
}

// pinglessWS hides fakeWS's pinger methods, so a parked socket over it is not a pinger.
type pinglessWS struct {
	ws *fakeWS
}

func (plain *pinglessWS) Send(data []byte) error   { return plain.ws.Send(data) }
func (plain *pinglessWS) Receive() ([]byte, error) { return plain.ws.Receive() }
func (plain *pinglessWS) Close() error             { return plain.ws.Close() }

func TestGivenAParkedSocketThatCannotPingWhenTheIntervalPassesThenItWaitsForTheRotation(t *testing.T) {
	raw := newFakeWS()
	socket := parkSocket(&pinglessWS{ws: raw}, parkedSessionUUID, time.Now(), warmInterval)
	keeper := pingingKeeperHolding(socket)

	assert.False(t, keeper.pingParked())
	assert.False(t, keeper.pingParked(), "a socket that cannot ping is never a missed pong")

	assert.Zero(t, raw.pings.Load())
	assert.False(t, raw.isClosed())
	assert.Same(t, socket, parkedSocket(keeper))
}
