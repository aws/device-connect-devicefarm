// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/forward"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// openerAt binds one device at endpoint and returns it with an opener over the same
// config and context the attachment's own runs on, but a budget of its own, so a test can
// drive OpenStream with no listener in front of it. That config disables refreshing, so
// every websocket the mock counts is a stream's own.
func openerAt(t *testing.T, endpoint string, opts ...ActiveDeviceOption) (*ActiveDevice, *deviceStreamOpener) {
	t.Helper()
	device := forwardingDeviceAt(t, endpoint, opts...)
	return device, &deviceStreamOpener{device: device, cfg: device.featureConfig,
		arn: testDeviceARN, ctx: attachedFeatures(t, device).ctx,
		budget: &streamBudget{limit: testMaxDeviceStreams}}
}

// forwardingDeviceAt binds one forwarding device at endpoint.
func forwardingDeviceAt(t *testing.T, endpoint string, opts ...ActiveDeviceOption) *ActiveDevice {
	t.Helper()
	device := newForwardingDevice(t, opts...)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "test-device"}))
	return device
}

// runStream hands the opener one accepted connection on its own goroutine, as the accept
// loop does, and returns the application's end of it with a channel closed when the
// stream ends. The target is never an adbd port: the remote denies those, so an
// illustrated success path must not use one. Loopback sockets rather than net.Pipe: the
// accepted side buffers, so a byte the opener should not have written is still readable
// after the close.
func runStream(t *testing.T, opener *deviceStreamOpener) (appConn net.Conn, ended <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	appConn, err = net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { appConn.Close() })
	accepted, err := listener.Accept()
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		opener.OpenStream(accepted, adbwire.AdbSpec{Kind: "tcp", Value: "8080"}, forward.ImmediateTurn())
	}()
	return appConn, done
}

// soleSession waits for the one stream's prologue to reach the mock and returns its
// session, so a test can drive the device side of the stream by hand.
func soleSession(t *testing.T, mock *prologueMock) *testsupport.MockSession {
	t.Helper()
	waitFor(t, func() bool { return len(mock.Frames()) == 2 }, "the stream's prologue never arrived")
	sessions := mock.Sessions()
	require.Len(t, sessions, 1)
	return sessions[0]
}

// readExactly reads length bytes under a deadline, so a stream that delivers nothing fails
// the test instead of hanging it.
func readExactly(t *testing.T, conn net.Conn, length int) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(testWait)))
	receivedBytes := make([]byte, length)
	_, err := io.ReadFull(conn, receivedBytes)
	require.NoError(t, err)
	return string(receivedBytes)
}

// drainClosed returns what is left on the application socket, requiring the socket to be
// closed rather than held open.
func drainClosed(t *testing.T, appConn net.Conn) []byte {
	t.Helper()
	require.NoError(t, appConn.SetReadDeadline(time.Now().Add(testWait)))
	rest, err := io.ReadAll(appConn)
	require.NoError(t, err, "the accepted socket must be closed, not held open")
	return rest
}

// awaitStreamEnd waits for OpenStream to return, telling a stream that ended from one
// still running.
func awaitStreamEnd(t *testing.T, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(testWait):
		t.Fatal("the stream never ended")
	}
}

func TestGivenAnAcceptedForwardedConnectionWhenTheStreamOpensThenBytesRoundTripThroughTheSession(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	device, opener := openerAt(t, mock.URL)

	appConn, ended := runStream(t, opener)

	session := soleSession(t, mock)
	assert.Equal(t, []string{"0012host:transport-any", "0008tcp:8080"}, mock.Frames(),
		"the accepted connection must reach the device as its own prologued session")
	_, err := appConn.Write([]byte("ping"))
	require.NoError(t, err)
	waitFor(t, func() bool { return strings.HasSuffix(string(session.Inbound()), "ping") },
		"the application's bytes never reached the device")
	require.True(t, session.Send([]byte("pong")))
	assert.Equal(t, "pong", readExactly(t, appConn, 4), "the device's bytes must reach the application")

	device.Clear()

	awaitStreamEnd(t, ended)
	waitFor(t, func() bool { return !session.Send([]byte("late")) },
		"the detach must close the stream's own websocket")
}

func TestGivenLeftoverAfterThePrologueWhenTheStreamOpensThenItReachesTheApp(t *testing.T) {
	// The second status word and the device's first bytes arrive in one message.
	const greeting = "device-hello"
	mock := newPrologueMock(t, "OKAY", "OKAY"+greeting)
	_, opener := openerAt(t, mock.URL)

	appConn, _ := runStream(t, opener)

	assert.Equal(t, greeting, readExactly(t, appConn, len(greeting)), "leftover bytes must not be dropped")
	session := soleSession(t, mock)
	_, err := appConn.Write([]byte("after"))
	require.NoError(t, err)
	waitFor(t, func() bool { return strings.HasSuffix(string(session.Inbound()), "after") },
		"the pipe must run once the leftover is out")
}

// refusingConn is an accepted connection whose application has stopped taking bytes.
type refusingConn struct{ net.Conn }

func (refusingConn) Write([]byte) (int, error) { return 0, errWriteRefused }

var errWriteRefused = errors.New("write refused")

func TestGivenAnAppThatWillNotTakeTheLeftoverWhenTheStreamOpensThenNothingIsPiped(t *testing.T) {
	// The device's first bytes cannot be delivered, so the stream ends before the pipe
	// starts rather than stranding them.
	mock := newPrologueMock(t, "OKAY", "OKAYdevice-hello")
	_, opener := openerAt(t, mock.URL)
	appConn, accepted := net.Pipe()
	t.Cleanup(func() { appConn.Close() })

	opener.OpenStream(refusingConn{accepted}, adbwire.AdbSpec{Kind: "tcp", Value: "8080"}, forward.ImmediateTurn())

	assert.Equal(t, []string{"0012host:transport-any", "0008tcp:8080"}, mock.Frames(),
		"a stream that could not deliver its first bytes must send nothing more")
}

func TestGivenAFailedOpenWhenAConnectionIsAcceptedThenTheAppSocketClosesWithZeroBytes(t *testing.T) {
	// Framed by hand as every wire expectation here is: FAIL, 4 hex digits, reason.
	mock := newPrologueMock(t, "OKAY", "FAIL0013no such device port")
	_, opener := openerAt(t, mock.URL)

	appConn, ended := runStream(t, opener)

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn), "a refused open must write nothing to the application")
}

func TestGivenADetachedDeviceWhenAConnectionIsAcceptedThenItIsClosedWithoutADial(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	device, opener := openerAt(t, mock.URL)
	// The accept loop hands off connections it took before the detach closed the
	// listener, so the opener is the last thing that can refuse one.
	device.Clear()

	appConn, ended := runStream(t, opener)

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn), "a connection with no device behind it must get zero bytes")
	assert.Zero(t, mock.ConnectionsAccepted(), "a refused connection must not dial")
}

func TestGivenOpenForwardedStreamsWhenTheDeviceIsClearedThenEveryStreamEnds(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY", "OKAY", "OKAY")
	device, opener := openerAt(t, mock.URL)
	firstApp, firstEnded := runStream(t, opener)
	secondApp, secondEnded := runStream(t, opener)
	waitFor(t, func() bool { return len(mock.Frames()) == 4 }, "both streams never opened")
	require.Equal(t, 2, mock.SessionsOpened(), "each forwarded connection is a session of its own")
	// A byte through each pipe: both streams are live here, so the detach below is what
	// ends them.
	for _, appConn := range []net.Conn{firstApp, secondApp} {
		_, err := appConn.Write([]byte("ping"))
		require.NoError(t, err)
	}
	waitFor(t, func() bool { return len(mock.Frames()) == 6 }, "both streams never carried app bytes")

	device.Clear()

	awaitStreamEnd(t, firstEnded)
	awaitStreamEnd(t, secondEnded)
	assert.Empty(t, drainClosed(t, firstApp), "the detach must close the application socket")
	assert.Empty(t, drainClosed(t, secondApp), "the detach must close the application socket")
}

func TestGivenADeviceThatCannotBeDialedWhenAConnectionArrivesThenItIsClosedWithoutBytes(t *testing.T) {
	// Port 1 on loopback needs root to bind, so the dial is refused at once.
	_, opener := openerAt(t, "wss://127.0.0.1:1")

	appConn, ended := runStream(t, opener)

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn), "a stream that never opened must write nothing")
}

func TestGivenAForwardedStreamWhenTheDeviceSendsAnOverCapMessageThenTheStreamEnds(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	_, opener := openerAt(t, mock.URL)
	appConn, ended := runStream(t, opener)
	session := soleSession(t, mock)

	// Unchecked: the client rejects the frame from its header and closes, so the device
	// side's own write may fail. The stream's end is the assertion.
	session.Send(make([]byte, maxInboundMessageBytes+1))

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn), "an over-cap message must not reach the application")
}

func TestGivenTheDeviceStreamCapWhenOneMoreConnectionArrivesThenItIsClosedWithoutADial(t *testing.T) {
	mock := newPrologueMock(t, slices.Repeat([]string{"OKAY"}, 2*(testMaxDeviceStreams+1))...)
	_, opener := openerAt(t, mock.URL)
	firstApp, firstEnded := runStream(t, opener)
	for range testMaxDeviceStreams - 1 {
		runStream(t, opener)
	}
	waitFor(t, func() bool { return len(mock.Frames()) == 2*testMaxDeviceStreams },
		"the streams that fill the cap never opened")

	appConn, ended := runStream(t, opener)

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn), "a connection above the cap must get zero bytes")
	assert.Equal(t, testMaxDeviceStreams, mock.ConnectionsAccepted(),
		"a connection above the cap must not dial")

	// Ending a stream frees its slot; the next accept opens.
	require.NoError(t, firstApp.Close())
	awaitStreamEnd(t, firstEnded)
	runStream(t, opener)
	waitFor(t, func() bool { return len(mock.Frames()) == 2*testMaxDeviceStreams+2 },
		"the freed slot never admitted a stream")
}

func TestGivenFailedOpensWhenEachEndsThenItsSlotReturns(t *testing.T) {
	refusal := []string{"OKAY", "FAIL0013no such device port"}
	mock := newPrologueMock(t, slices.Repeat(refusal, testMaxDeviceStreams+1)...)
	_, opener := openerAt(t, mock.URL)

	for range testMaxDeviceStreams + 1 {
		_, ended := runStream(t, opener)
		awaitStreamEnd(t, ended)
	}

	assert.Equal(t, testMaxDeviceStreams+1, mock.ConnectionsAccepted(),
		"a failed open must free its slot")
}

// socketRemote is the device socket the order tests forward to.
const socketRemote = "localabstract:scrcpy"

// heldReplyMock answers the transport select with OKAY. It answers every device service with
// OKAY except the first: it holds that reply until release is called, then sends firstReply.
// It records each device service as it arrives and the held reply as it leaves, in one order.
type heldReplyMock struct {
	*testsupport.SessionMockWSS
	release func()

	eventsMutex sync.Mutex
	events      []string
}

func newHeldReplyMock(t *testing.T, firstReply string) *heldReplyMock {
	t.Helper()
	released := make(chan struct{})
	var releaseOnce sync.Once
	mock := &heldReplyMock{release: func() { releaseOnce.Do(func() { close(released) }) }}
	mock.SessionMockWSS = testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
			switch string(data) {
			case string(deviceTransportRequest):
				return []byte("OKAY")
			case string(framed(socketRemote)):
				if mock.record("service") > 1 {
					return []byte("OKAY")
				}
				<-released
				mock.record("reply")
				return []byte(firstReply)
			}
			return nil
		},
	})
	// Registered after the mock's own cleanup, so it runs first: a held reply must not keep
	// the server from closing.
	t.Cleanup(mock.release)
	return mock
}

// record appends event and returns how many events there are.
func (mock *heldReplyMock) record(event string) int {
	mock.eventsMutex.Lock()
	defer mock.eventsMutex.Unlock()
	mock.events = append(mock.events, event)
	return len(mock.events)
}

// Events returns the recorded device services and the held reply, in order.
func (mock *heldReplyMock) Events() []string {
	mock.eventsMutex.Lock()
	defer mock.eventsMutex.Unlock()
	return slices.Clone(mock.events)
}

// dialSocketRule installs a rule to socketRemote on device and returns its local address.
func dialSocketRule(t *testing.T, device *ActiveDevice) string {
	t.Helper()
	port := installRule(t, attachedFeatures(t, device), socketRemote)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// dialHeldFirst dials the rule and waits until the mock holds the reply to that connection's
// device service. It returns the application socket.
func dialHeldFirst(t *testing.T, mock *heldReplyMock, address string) net.Conn {
	t.Helper()
	first := dialClient(t, address)
	waitFor(t, func() bool { return len(mock.Events()) == 1 }, "the first device service never arrived")
	return first
}

// proveSecondWaitsForTheFirstReply dials the rule again, after dialHeldFirst. It waits until
// the mock's session count is sessions: the second connection has then minted its session.
// It holds the reply long enough for an unordered second open to pass the first, releases the
// reply, and checks the order the mock saw.
func proveSecondWaitsForTheFirstReply(t *testing.T, mock *heldReplyMock, address string, sessions int) {
	t.Helper()
	dialClient(t, address)
	waitFor(t, func() bool { return mock.SessionsOpened() == sessions },
		"the second connection never got a selected session")

	assert.Never(t, func() bool { return len(mock.Events()) > 1 }, notifierSettle, notifierPoll,
		"the second device service must wait for the first one's status word")
	mock.release()
	waitFor(t, func() bool { return len(mock.Events()) == 3 }, "the second device service never followed")
	assert.Equal(t, []string{"service", "reply", "service"}, mock.Events())
}

func TestGivenTwoForwardedConnectionsWhenTheFirstOpensSlowerThenTheDeviceServicesLeaveInAcceptOrder(t *testing.T) {
	mock := newHeldReplyMock(t, "OKAY")
	address := dialSocketRule(t, forwardingDeviceAt(t, mock.URL))
	dialHeldFirst(t, mock, address)

	proveSecondWaitsForTheFirstReply(t, mock, address, 2)
}

func TestGivenTheFirstConnectionOnTheParkedSocketWhenTheSecondOpensThenItDialsAndWaitsForTheFirstStatusWord(t *testing.T) {
	mock := newHeldReplyMock(t, statusOkay)
	device := newWarmDevice(t, mock.URL, warmPolicy(warmInterval))
	keeper := attachedFeatures(t, device).warmSockets()
	parked := awaitPark(t, keeper)
	address := dialSocketRule(t, device)
	dialHeldFirst(t, mock, address)
	waitFor(t, func() bool {
		refill := parkedSocket(keeper)
		return refill != nil && refill != parked
	}, "the slot never refilled behind the first connection")
	refill := parkedSocket(keeper)

	// Three sessions: the park, the refill behind the first's take, the second's own dial.
	proveSecondWaitsForTheFirstReply(t, mock, address, 3)

	assert.Equal(t, canonicalSelect+string(framed(socketRemote)),
		string(sessionNamed(mock.SessionMockWSS, parked.sessionUUID).Inbound()),
		"the first connection rides the parked socket")
	assert.Same(t, refill, parkedSocket(keeper),
		"a connection that must wait leaves the parked socket to one that sends at once")
}

func TestGivenAFailedFirstOpenWhenTheSecondWaitsThenItProceeds(t *testing.T) {
	mock := newHeldReplyMock(t, "FAIL0013no such device port")
	address := dialSocketRule(t, forwardingDeviceAt(t, mock.URL))
	first := dialHeldFirst(t, mock, address)

	proveSecondWaitsForTheFirstReply(t, mock, address, 2)

	assert.Empty(t, drainClosed(t, first), "the refused first connection must get zero bytes")
}

func TestGivenABudgetRefusalWhenTheNextConnectionWaitsThenItIsReleased(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	device := forwardingDeviceAt(t, mock.URL)
	address := dialSocketRule(t, device)
	budget := attachedFeatures(t, device).budget
	for range testMaxDeviceStreams {
		require.True(t, budget.admit())
	}
	assert.Empty(t, drainClosed(t, dialClient(t, address)), "a connection past the cap must get zero bytes")
	for range testMaxDeviceStreams {
		budget.release()
	}

	dialClient(t, address)

	waitFor(t, func() bool { return len(mock.Frames()) == 2 }, "the refused connection's turn never released the next one")
	assert.Equal(t, []string{"0012host:transport-any", string(framed(socketRemote))}, mock.Frames())
}
