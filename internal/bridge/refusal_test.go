package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// detachedReason stands in for a detaching verdict's text, which the daemon words.
const detachedReason = "remote access session arn:test no longer exists; start a new session and run 'device add'"

// renewedLabel is the label a RUNNING verdict carries beside its endpoint.
const renewedLabel = "Test Device - OS 2"

// fakeLookup is a RefusalHandler answering one verdict. It records every call. With entered and
// release set, each call announces itself on entered and waits for release, so a test holds a
// lookup and watches what the next refusal does meanwhile.
type fakeLookup struct {
	verdict SessionVerdict
	entered chan struct{}
	release chan struct{}

	mutex sync.Mutex
	arns  []string
}

// heldLookup is a fakeLookup whose calls wait for release.
func heldLookup(verdict SessionVerdict) *fakeLookup {
	return &fakeLookup{verdict: verdict, entered: make(chan struct{}, 2), release: make(chan struct{})}
}

func (lookup *fakeLookup) handler() RefusalHandler {
	return func(arn string) SessionVerdict {
		lookup.mutex.Lock()
		lookup.arns = append(lookup.arns, arn)
		lookup.mutex.Unlock()
		if lookup.entered != nil {
			lookup.entered <- struct{}{}
			<-lookup.release
		}
		return lookup.verdict
	}
}

// calls returns the ARN of every lookup, in order.
func (lookup *fakeLookup) calls() []string {
	lookup.mutex.Lock()
	defer lookup.mutex.Unlock()
	return append([]string(nil), lookup.arns...)
}

// refusingMock refuses the first count fresh sessions (negative: every one) and, once it
// accepts, OKAYs the daemon's select and echoes the rest.
func refusingMock(t *testing.T, count int) *testsupport.SessionMockWSS {
	t.Helper()
	return testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		RefuseNewSessions: count, OnMessage: answerSelectWith("OKAY")})
}

// attachLookupDevice binds a device with attachment features and lookup as the daemon's handler
// at endpoint.
func attachLookupDevice(t *testing.T, endpoint string, lookup *fakeLookup) *ActiveDevice {
	t.Helper()
	device := newForwardingDevice(t, WithRefusalHandler(lookup.handler()))
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "Test Device - OS 1"}))
	return device
}

// runLookupProxy runs a proxy in front of a device bound at endpoint with lookup as the handler.
func runLookupProxy(t *testing.T, endpoint string, lookup *fakeLookup) (proxyAddress string, device *ActiveDevice) {
	t.Helper()
	device = attachLookupDevice(t, endpoint, lookup)
	proxyAddress, _ = runProxy(t, device, testPolicy())
	return proxyAddress, device
}

// readFailThenEOF reads a FAIL status word with its reason, then requires the close.
func readFailThenEOF(t *testing.T, client net.Conn) string {
	t.Helper()
	require.Equal(t, statusFail, string(readN(t, client, statusWordLen)), "the client reads FAIL")
	reasonLength, err := strconv.ParseUint(string(readN(t, client, failLenDigits)), 16, 16)
	require.NoError(t, err)
	reason := string(readN(t, client, int(reasonLength)))
	_, err = client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the connection ends with the refusal")
	return reason
}

// assertDetached requires the device gone with reason as its last detach.
func assertDetached(t *testing.T, device *ActiveDevice, reason string) {
	t.Helper()
	_, present := device.Current()
	assert.False(t, present, "the device must be detached")
	last, detached := device.LastDetach()
	require.True(t, detached, "the detach must be recorded")
	assert.Equal(t, reason, last.Reason)
	assert.WithinDuration(t, time.Now(), last.At, testTimeout)
}

func TestGivenARefusedSessionWhenTheLookupDetachesThenTheClientReadsTheReasonAndNothingIsRedialed(t *testing.T) {
	tests := []struct {
		name string
		// speak plays the client's opening and returns once the daemon has what it needs to dial.
		speak func(t *testing.T, client net.Conn)
	}{
		{name: "a lone request", speak: func(t *testing.T, client net.Conn) {
			writeAll(t, client, []byte("000ehost:get-state"))
		}},
		{name: "a request behind a select", speak: func(t *testing.T, client net.Conn) {
			selectAnyTransport(t, client)
			writeAll(t, client, []byte("000ashell:true"))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := refusingMock(t, -1)
			lookup := &fakeLookup{verdict: SessionVerdict{Detach: true, Reason: detachedReason}}
			proxyAddress, device := runLookupProxy(t, mock.URL, lookup)
			client := dialClient(t, proxyAddress)

			tc.speak(t, client)

			assert.Equal(t, detachedReason, readFailThenEOF(t, client), "adb prints the verdict after error:")
			assertDetached(t, device, detachedReason)
			assert.Equal(t, []string{testDeviceARN}, lookup.calls(), "one lookup, for the attached session")
			assert.Equal(t, 1, mock.ConnectionsAccepted(), "a detach earns no second dial")
		})
	}
}

func TestGivenARefusedSessionWhenTheLookupKeepsTheDeviceThenOneRedialRunsAndTheSecondRefusalReachesTheClient(t *testing.T) {
	mock := refusingMock(t, -1)
	lookup := &fakeLookup{} // a transient lookup failure: no endpoint, no detach
	proxyAddress, device := runLookupProxy(t, mock.URL, lookup)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("000ehost:get-state"))

	assert.Equal(t, "the device connection was refused: "+testsupport.NewSessionRefusalReason, readFailThenEOF(t, client),
		"the server's own reason, behind a short lead")
	_, present := device.Current()
	assert.True(t, present, "a kept verdict keeps the device")
	assert.Len(t, lookup.calls(), 1, "the second refusal asks nothing more")
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "exactly one redial")
}

func TestGivenARefusedSessionWhenTheLookupFindsItRunningThenTheRenewedEndpointIsDialedAndTheRequestGoesThrough(t *testing.T) {
	refusing := refusingMock(t, -1)
	accepting := switchReplyMock(t, "OKAY")
	lookup := &fakeLookup{verdict: SessionVerdict{Endpoint: accepting.URL, Label: renewedLabel}}
	proxyAddress, device := runLookupProxy(t, refusing.URL, lookup)
	client := dialClient(t, proxyAddress)

	request := []byte("000dhost:features")
	writeAll(t, client, request)

	assert.Equal(t, string(request), string(readN(t, client, len(request))), "the redial carries the request; the mock echoes it")
	current, present := device.Current()
	require.True(t, present)
	assert.Equal(t, accepting.URL, current.Endpoint, "the RUNNING answer renews the endpoint")
	assert.Equal(t, renewedLabel, current.Label)
	assert.Equal(t, 1, refusing.ConnectionsAccepted(), "the old endpoint got the one refused dial")
	assert.Equal(t, 1, accepting.SessionsOpened(), "the renewed endpoint got the redial")
	assert.Len(t, lookup.calls(), 1)
}

func TestGivenASingleRefusalWhenTheSessionIsRunningThenTheRedialOnTheSameEndpointSucceeds(t *testing.T) {
	mock := refusingMock(t, 1)
	proxyAddress, _ := runLookupProxy(t, mock.URL, &fakeLookup{verdict: SessionVerdict{Endpoint: mock.URL, Label: renewedLabel}})
	client := dialClient(t, proxyAddress)

	request := []byte("000dhost:features")
	writeAll(t, client, request)

	assert.Equal(t, string(request), string(readN(t, client, len(request))))
	assert.Equal(t, 1, mock.NewSessionsRefused())
	assert.Equal(t, 1, mock.SessionsOpened(), "one refused, one accepted")
	assert.Equal(t, 2, mock.ConnectionsAccepted())
}

func TestGivenADialThatFailsWhenTheLookupAnswersThenTheClientReadsTheDetachReasonOrTheDialError(t *testing.T) {
	tests := []struct {
		name        string
		verdict     SessionVerdict
		wantReason  string
		wantPresent bool
	}{
		{name: "detach", verdict: SessionVerdict{Detach: true, Reason: detachedReason}, wantReason: detachedReason},
		{name: "kept", wantReason: "the daemon could not reach the device: dial tcp 127.0.0.1:1:", wantPresent: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Nothing listens on port 1, so every dial fails at once.
			lookup := &fakeLookup{verdict: tc.verdict}
			proxyAddress, device := runLookupProxy(t, "wss://127.0.0.1:1/", lookup)
			client := dialClient(t, proxyAddress)

			writeAll(t, client, []byte("000ehost:get-state"))

			assert.Contains(t, readFailThenEOF(t, client), tc.wantReason)
			_, present := device.Current()
			assert.Equal(t, tc.wantPresent, present)
			assert.Len(t, lookup.calls(), 1)
		})
	}
}

func TestGivenTwoRefusalsAtOnceWhenTheLookupDetachesThenTheLookupsRunOneAtATimeAndBothClientsReadTheReason(t *testing.T) {
	mock := refusingMock(t, -1)
	lookup := heldLookup(SessionVerdict{Detach: true, Reason: detachedReason})
	proxyAddress, device := runLookupProxy(t, mock.URL, lookup)
	first, second := dialClient(t, proxyAddress), dialClient(t, proxyAddress)

	writeAll(t, first, []byte("000ehost:get-state"))
	<-lookup.entered
	writeAll(t, second, []byte("000ehost:get-state"))
	waitFor(t, func() bool { return mock.ConnectionsAccepted() == 2 }, "the second client's dial never reached the mock")
	assert.Never(t, func() bool { return len(lookup.entered) > 0 }, 200*time.Millisecond, 10*time.Millisecond,
		"the second lookup must wait for the first")
	close(lookup.release)

	assert.Equal(t, detachedReason, readFailThenEOF(t, first))
	assert.Equal(t, detachedReason, readFailThenEOF(t, second), "the detach by the first lookup does not silence the second client")
	assert.Equal(t, []string{testDeviceARN, testDeviceARN}, lookup.calls(), "two lookups, none deduplicated")
	assertDetached(t, device, detachedReason)
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "one dial each, no redial")
}

func TestGivenASocketParkedDuringTheLookupWhenTheSessionIsRunningElsewhereThenTheRedialDialsTheRenewedEndpoint(t *testing.T) {
	refusing := refusingMock(t, -1)
	accepting := switchReplyMock(t, "OKAY")
	lookup := heldLookup(SessionVerdict{Endpoint: accepting.URL, Label: renewedLabel})
	// A keeper that parks: the refused dial's take asks for a refill, which parks a socket at the
	// old endpoint while the lookup runs.
	warmPolicy := testPolicy()
	warmPolicy.Interval = time.Hour
	device := NewActiveDevice(testFeatures(newTestDialer(testWait), warmPolicy, warmParams(), noopLogger()),
		WithRefusalHandler(lookup.handler()))
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: refusing.URL, Label: "Test Device - OS 1"}))
	keeper := attachedFeatures(t, device).warm
	proxyAddress, _ := runProxy(t, device, testPolicy())
	waitFor(t, func() bool { return parkedSocket(keeper) != nil }, "the keeper never parked a socket")
	client := dialClient(t, proxyAddress)

	request := []byte("000dhost:features")
	writeAll(t, client, request)
	<-lookup.entered
	// The take emptied the slot; a socket parked now is the refill, dialed at the old endpoint.
	waitFor(t, func() bool { return parkedSocket(keeper) != nil }, "the refill never parked a second socket")
	close(lookup.release)

	assert.Equal(t, string(request), string(readN(t, client, len(request))), "the redial reached the renewed endpoint")
	assert.Equal(t, 1, accepting.SessionsOpened(), "a fresh dial at the renewed endpoint")
	assert.Equal(t, 1, refusing.NewSessionsRefused(), "the socket parked at the old endpoint never spoke")
}

// holdingListener accepts and holds every connection without a byte, and reports each accept: a
// websocket dial to it blocks until its context ends.
func holdingListener(t *testing.T) (address string, accepted <-chan struct{}) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	accepts := make(chan struct{}, 8)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			t.Cleanup(func() { conn.Close() })
			accepts <- struct{}{}
		}
	}()
	return listener.Addr().String(), accepts
}

func TestGivenADialInFlightWhenTheDaemonDetachesTheDeviceThenTheClientReadsTheRecordedReason(t *testing.T) {
	address, accepted := holdingListener(t)
	lookup := &fakeLookup{}
	proxyAddress, device := runLookupProxy(t, "wss://"+address+"/", lookup)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("000ehost:get-state"))
	<-accepted
	// The refresh tick found the session gone while this connection's dial hung.
	_, cleared := device.ClearMatching(testDeviceARN, detachedReason)
	require.True(t, cleared)

	assert.Equal(t, detachedReason, readFailThenEOF(t, client), "the cut dial reads the detach's reason")
	assert.Empty(t, lookup.calls(), "a canceled connection asks nothing itself")
}

func TestGivenADialInFlightWhenTheUserRemovesTheDeviceThenTheClientClosesWithZeroBytes(t *testing.T) {
	address, accepted := holdingListener(t)
	lookup := &fakeLookup{}
	proxyAddress, device := runLookupProxy(t, "wss://"+address+"/", lookup)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("000ehost:get-state"))
	<-accepted
	_, cleared := device.Clear()
	require.True(t, cleared)

	assert.Empty(t, drainClosed(t, client), "a remove by the user leaves no reason to give")
	assert.Empty(t, lookup.calls())
}

func TestGivenAParkedSocketWhoseLateDialFailsWhenClassifiedThenItIsARefusedOpening(t *testing.T) {
	// The taken socket's first send fails and its fresh dial fails too: the dial failure sits behind
	// two wraps, where the client's text and the lookup must still find it.
	lateDial := &dialFailure{cause: fmt.Errorf("websocket dial wss://127.0.0.1:1 route adb: %w",
		errors.New("dial tcp 127.0.0.1:1: connect: connection refused"))}
	err := fmt.Errorf("send the client's request: %w", fmt.Errorf("send on the parked websocket: %w; dial a fresh one: %w",
		io.ErrClosedPipe, lateDial))

	assert.True(t, isRefusedOpening(err))
	assert.Equal(t, "the daemon could not reach the device: dial tcp 127.0.0.1:1: connect: connection refused", refusalText(err))
	assert.False(t, isRefusedOpening(io.ErrClosedPipe), "a failure of the exchange asks nothing")
}

func TestGivenTheDeviceRefusesTheSelectWhenTheClientSentARequestThenItReadsAdbsFailAndNothingIsLookedUp(t *testing.T) {
	// The device's own adb server answered the daemon's select with FAIL: adb's refusal, not the
	// server's, so it reaches the client as is and asks the daemon nothing.
	mock := switchReplyMock(t, "FAIL000edevice offline")
	lookup := &fakeLookup{verdict: SessionVerdict{Detach: true, Reason: detachedReason}}
	proxyAddress, device := runLookupProxy(t, mock.URL, lookup)
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("000ashell:true"))

	assert.Equal(t, "device offline", readFailThenEOF(t, client), "adb's reason, verbatim")
	assert.Empty(t, lookup.calls(), "a device refusal is not a session refusal")
	_, present := device.Current()
	assert.True(t, present)
	assert.Equal(t, 1, mock.SessionsOpened(), "the session was minted; adb refused inside it")
}

func TestGivenNoRefusalHandlerWhenASessionIsRefusedThenTheClientClosesWithZeroBytesAndNoRedial(t *testing.T) {
	mock := refusingMock(t, -1)
	proxyAddress, device := attachProxyTo(t, mock, testPolicy(), "")
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("000ehost:get-state"))

	assert.Empty(t, drainClosed(t, client), "no handler: the close alone, as before")
	_, present := device.Current()
	assert.True(t, present)
	assert.Equal(t, 1, mock.ConnectionsAccepted())
}

func TestGivenAServerRefusingEveryFreshSessionWhenHostVersionIsAskedThenTheCachedReplyAnswersWithNoLookup(t *testing.T) {
	mock := hostReplyMock(t, cannedHostReplies)
	lookup := &fakeLookup{verdict: SessionVerdict{Detach: true, Reason: detachedReason}}
	device := NewActiveDevice(WithAttachmentFeatures(newTestDialer(testWait), testPolicy(), warmParams(), noopLogger()),
		WithRefusalHandler(lookup.handler()))
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL, Label: "test-device"}))
	proxyAddress, _ := runProxy(t, device, testPolicy())
	waitFor(t, func() bool { return mock.SessionsOpened() == len(prefetchedHostRequests) }, "the attach's prefetch never finished")
	mock.RefuseNewSessions(-1)

	assert.Equal(t, serverVersionReply, askHost(t, proxyAddress, hostVersionRequest), "the copy answers")

	assert.Empty(t, lookup.calls(), "a cached answer asks nothing")
	_, present := device.Current()
	assert.True(t, present)
	assert.Zero(t, mock.NewSessionsRefused(), "no fresh session was dialed")
}

// lookupOpener is openerAt plus a refusal handler and the attachment's features, as the attachment
// builds its own opener.
func lookupOpener(t *testing.T, endpoint string, lookup *fakeLookup) (*ActiveDevice, *deviceStreamOpener) {
	t.Helper()
	device, opener := openerAt(t, endpoint, WithRefusalHandler(lookup.handler()))
	opener.features = attachedFeatures(t, device)
	return device, opener
}

func TestGivenARefusedDeviceStreamWhenTheLookupDetachesThenTheAppSocketClosesEmptyAndTheDeviceDetaches(t *testing.T) {
	mock := refusingMock(t, -1)
	lookup := &fakeLookup{verdict: SessionVerdict{Detach: true, Reason: detachedReason}}
	device, opener := lookupOpener(t, mock.URL, lookup)

	appConn, ended := runStream(t, opener)

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn), "a device stream carries no adb protocol for text")
	assertDetached(t, device, detachedReason)
	assert.Equal(t, []string{testDeviceARN}, lookup.calls())
	assert.Equal(t, 1, mock.ConnectionsAccepted())
}

func TestGivenARefusedDeviceStreamWhenTheLookupKeepsTheDeviceThenItIsDialedOnceMore(t *testing.T) {
	mock := refusingMock(t, -1)
	lookup := &fakeLookup{}
	device, opener := lookupOpener(t, mock.URL, lookup)

	appConn, ended := runStream(t, opener)

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn))
	_, present := device.Current()
	assert.True(t, present)
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "exactly one redial")
}

func TestGivenADeviceStreamWhoseReattachIsRefusedWhenTheLookupSaysEndedThenTheDeviceDetaches(t *testing.T) {
	mock := newPrologueMockWith(t, testsupport.SessionMockConfig{RejectReattach: true}, "OKAY", "OKAY")
	lookup := &fakeLookup{verdict: SessionVerdict{Detach: true, Reason: detachedReason}}
	device := attachLookupDevice(t, mock.URL, lookup)
	conn, _, err := openDeviceStream(context.Background(), newTestDialer(testWait), device, testDeviceARN,
		attachedFeatures(t, device), adbwire.AdbSpec{Kind: "tcp", Value: "8080"}, fastPolicy(), testWait,
		newActivityTracker(nil), noopLogger())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.ErrorIs(t, conn.refresh(context.Background(), nil), ErrSessionGone)

	assert.True(t, conn.isClosed(), "the stream ends as before")
	waitFor(t, func() bool { _, present := device.Current(); return !present }, "the lookup never detached the device")
	assertDetached(t, device, detachedReason)
	assert.Equal(t, []string{testDeviceARN}, lookup.calls(), "the refused reattach asks about the session")
}

func TestGivenADeviceStreamWhoseWebsocketDiesWhenTheResumeIsRefusedThenTheLookupRuns(t *testing.T) {
	mock := newPrologueMockWith(t, testsupport.SessionMockConfig{RejectReattach: true}, "OKAY", "OKAY")
	lookup := &fakeLookup{verdict: SessionVerdict{Detach: true, Reason: detachedReason}}
	device := attachLookupDevice(t, mock.URL, lookup)
	conn, _, err := openDeviceStream(context.Background(), newTestDialer(testWait), device, testDeviceARN,
		attachedFeatures(t, device), adbwire.AdbSpec{Kind: "tcp", Value: "8080"}, fastPolicy(), testWait,
		newActivityTracker(nil), noopLogger())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	sessions := mock.Sessions()
	require.Len(t, sessions, 1)

	// The relay's close while the session lives: the stream resumes reactively, and the mock refuses.
	sessions[0].DropAttached(testsupport.InternalErrorCloseCode)
	_, err = conn.Receive()

	require.Error(t, err, "a refused resume ends the stream")
	waitFor(t, func() bool { _, present := device.Current(); return !present }, "the lookup never detached the device")
	assertDetached(t, device, detachedReason)
	assert.Equal(t, []string{testDeviceARN}, lookup.calls(), "the refused resume asks about the session")
}

func TestGivenRefusalErrorsWhenWordedForAdbThenTheServersReasonOrTheDialErrorFollowsTheLead(t *testing.T) {
	assert.Equal(t, "the device connection was refused: device is busy",
		refusalText(fmt.Errorf("open the session: %w", &sessionRefusal{reason: "device is busy"})),
		"the server's reason alone, not the wraps around it")
	assert.Equal(t, "the daemon could not reach the device: dial tcp: connection refused",
		refusalText(&dialFailure{cause: errors.New("dial tcp: connection refused")}), "a bare cause stands as is")
}
