package bridge

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// canonicalSelect is the select the daemon fabricates on the device leg, spelled out
// here so these tests pin the bytes rather than the constant that produces them.
const canonicalSelect = "0012host:transport-any"

// detachedReadDeadline bounds a read only a detach should end. It sits below the relay's reply
// wait, so a timeout means the detach did not end it. Source: pick.
const detachedReadDeadline = 2 * time.Second

// transportIDReply is the 8 raw little-endian bytes of syntheticTransportID.
var transportIDReply = string([]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})

// scriptedConn plays a client's bytes at the seam and records every reply, so a test
// needs no reader on the far end and no goroutine to drain one.
type scriptedConn struct {
	reader  io.Reader
	replies bytes.Buffer
}

func newScriptedConn(said string) *scriptedConn {
	return &scriptedConn{reader: bytes.NewReader([]byte(said))}
}

func (conn *scriptedConn) Read(into []byte) (int, error)    { return conn.reader.Read(into) }
func (conn *scriptedConn) Write(data []byte) (int, error)   { return conn.replies.Write(data) }
func (conn *scriptedConn) Close() error                     { return nil }
func (conn *scriptedConn) LocalAddr() net.Addr              { return nil }
func (conn *scriptedConn) RemoteAddr() net.Addr             { return nil }
func (conn *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (conn *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (conn *scriptedConn) SetWriteDeadline(time.Time) error { return nil }

// answerSelectWith answers the daemon's canonical select with reply and echoes the rest.
func answerSelectWith(reply string) func(*testsupport.MockSession, []byte) []byte {
	return func(_ *testsupport.MockSession, data []byte) []byte {
		if bytes.Equal(data, []byte(canonicalSelect)) {
			return []byte(reply)
		}
		return data
	}
}

// switchReplyMock is a session mock that answers the daemon's canonical select with reply.
func switchReplyMock(t *testing.T, reply string) *testsupport.SessionMockWSS {
	t.Helper()
	return testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: answerSelectWith(reply)})
}

// soleSessionInbound returns the bytes the one live session received, or nil while there
// is not exactly one.
func soleSessionInbound(mock *testsupport.SessionMockWSS) []byte {
	sessions := mock.Sessions()
	if len(sessions) != 1 {
		return nil
	}
	return sessions[0].Inbound()
}

// assertSessionReceived waits for the session to hold as many bytes as want, then
// compares them, so a failure shows the bytes rather than a timeout.
func assertSessionReceived(t *testing.T, mock *testsupport.SessionMockWSS, want string) {
	t.Helper()
	waitFor(t, func() bool { return len(soleSessionInbound(mock)) >= len(want) },
		"the session never received the whole request")
	got := soleSessionInbound(mock)
	assert.Equal(t, want, string(got),
		"the session must receive the daemon's own select and then the client's bytes verbatim")
}

func TestGivenATransportSelectWhenAnsweredThenTheReplyShapeFollowsItsSpelling(t *testing.T) {
	// A tport: select waits for the transport id as 8 raw bytes, and a short reply makes the
	// client report a failure to read one. A transport-id select and the older spellings
	// Android Studio sends take a bare OKAY, and 12 bytes would leave the client one reply
	// ahead for the rest of the connection. Any id is accepted because `adb devices -l` prints
	// the remote adb server's own.
	tests := []struct {
		name         string
		request      string
		want         string
		wantSelector adbwire.AdbDeviceSelector
	}{
		{name: "any transport", request: "host:tport:any", want: "OKAY" + transportIDReply,
			wantSelector: adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorAny}},
		{name: "this device's serial", request: "host:tport:serial:dev-1", want: "OKAY" + transportIDReply,
			wantSelector: adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorSerial, Serial: "dev-1"}},
		{name: "a transport id", request: "host:transport-id:97", want: "OKAY",
			wantSelector: adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorTransportID, TransportID: 97}},
		{name: "the older serial spelling", request: "host:transport:dev-1", want: "OKAY",
			wantSelector: adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorSerial, Serial: "dev-1"}},
		{name: "the older any spelling", request: "host:transport-any", want: "OKAY",
			wantSelector: adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorAny}},
		{name: "the older usb spelling", request: "host:transport-usb", want: "OKAY",
			wantSelector: adbwire.AdbDeviceSelector{Kind: adbwire.AdbSelectorUSB}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var reply bytes.Buffer

			selector, claimed, err := answerTransportSelect(&reply, testCase.request, "dev-1")

			require.NoError(t, err)
			assert.Equal(t, testCase.wantSelector, selector,
				"the recorded selector is what a request behind the select is checked against")
			assert.True(t, claimed, "an answered select leaves a second request to read")
			assert.Equal(t, testCase.want, reply.String())
		})
	}
}

func TestGivenARequestThatIsNotAPlainSelectWhenAnsweredThenItIsNotClaimed(t *testing.T) {
	tests := []struct {
		name    string
		request string
	}{
		// A prefixed select names its device in the prefix, which this seam does not read,
		// so it goes to the device untouched rather than being answered on a guess.
		{name: "a select behind a device-selecting prefix", request: "host-serial:other-device:tport:any"},
		{name: "an older select behind a device-selecting prefix", request: "host-serial:other-device:transport:dev-1"},
		// The older serial spelling with no serial is no select.
		{name: "the older serial spelling with no serial", request: "host:transport:"},
		// Every other host service is the device's to answer.
		{name: "a host service that selects nothing", request: "host:devices"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var reply bytes.Buffer

			_, claimed, err := answerTransportSelect(&reply, testCase.request, "dev-1")

			assert.False(t, claimed)
			require.NoError(t, err)
			assert.Empty(t, reply.String(), "an unclaimed request is answered by the device, not here")
		})
	}
}

// failingWriter refuses every write, standing in for a client that has gone.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWriteRefused }

func TestGivenAWriterThatFailsWhenASelectIsAnsweredThenTheFailureIsReported(t *testing.T) {
	// Both replies go to a client that may have vanished between its select and the
	// answer, so neither write may be assumed to land.
	tests := []struct {
		name         string
		request      string
		deviceSerial string
	}{
		{name: "an answered select", request: "host:tport:any"},
		{name: "a refused select", request: "host:tport:serial:other-device", deviceSerial: "dev-1"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, claimed, err := answerTransportSelect(failingWriter{}, testCase.request, testCase.deviceSerial)

			assert.False(t, claimed, "an unwritten reply claims nothing")
			assert.ErrorIs(t, err, errWriteRefused)
		})
	}
}

func TestGivenAClientsOpeningWhenItIsReadThenOnlyTheUnansweredBytesArePending(t *testing.T) {
	tests := []struct {
		name         string
		said         string
		deviceSerial string
		noDevice     bool
		wantPending  string
		wantService  string
		wantSelected bool
		wantReplies  string
		wantErr      error
	}{
		{
			// No device, so no select is answered: the select is returned as the service.
			name: "a select with no device", said: "000ehost:tport:any0008shell:ls", noDevice: true,
			wantPending: "000ehost:tport:any0008shell:ls", wantService: "host:tport:any",
		},
		{
			// Both requests in one segment: the second is already buffered when the daemon
			// goes looking for it, and the select it answered must not reach the device.
			name: "a select and its service", said: "000ehost:tport:any0008shell:ls",
			wantPending: "0008shell:ls", wantService: "shell:ls", wantSelected: true,
			wantReplies: "OKAY" + transportIDReply,
		},
		{
			// adb keeps the plain host prefix on the forward family behind its select: the
			// device-bound bytes stay as framed, the service is read bare.
			name: "a select and a host-prefixed forward", said: "000ehost:tport:any001bhost:forward:tcp:0;tcp:9999",
			wantPending: "001bhost:forward:tcp:0;tcp:9999", wantService: "forward:tcp:0;tcp:9999", wantSelected: true,
			wantReplies: "OKAY" + transportIDReply,
		},
		{
			name: "a select and a host-prefixed list-forward", said: "000ehost:tport:any0011host:list-forward",
			wantPending: "0011host:list-forward", wantService: "list-forward", wantSelected: true,
			wantReplies: "OKAY" + transportIDReply,
		},
		{
			name: "a serial select and a host-prefixed killforward-all", said: "0017host:tport:serial:dev-10014host:killforward-all",
			deviceSerial: "dev-1", wantPending: "0014host:killforward-all", wantService: "killforward-all", wantSelected: true,
			wantReplies: "OKAY" + transportIDReply,
		},
		{
			// A host command and no select, as `adb -s dev-1 features` sends: the device gets
			// the client's own bytes and nothing else.
			name: "a host command with no select", said: "001ahost-serial:dev-1:features",
			wantPending: "001ahost-serial:dev-1:features",
			wantService: "host-serial:dev-1:features",
		},
		{
			// An older spelling takes a bare OKAY; the service behind it is read as behind tport:.
			name: "the older any spelling and its service", said: "0012host:transport-any0008shell:ls",
			wantPending: "0008shell:ls", wantService: "shell:ls", wantSelected: true,
			wantReplies: "OKAY",
		},
		{
			// Android Studio's select, then a bare reverse request: read behind the select, so
			// the reverse screens see it.
			name: "the older serial spelling and a reverse request", said: "0014host:transport:dev-1001breverse:forward:tcp:1;tcp:2",
			deviceSerial: "dev-1", wantPending: "001breverse:forward:tcp:1;tcp:2", wantService: "reverse:forward:tcp:1;tcp:2",
			wantSelected: true, wantReplies: "OKAY",
		},
		{
			// Bytes with no length prefix the parser can read are relayed, not refused: the
			// daemon is only a proxy, and its parser fails closed rather than guessing.
			name: "a frame that is not a request", said: "hello world", wantPending: "hello world",
		},
		{
			// The server mints a session from the first frame of any /adb websocket, so a client
			// that asks for nothing must leave the caller nothing to dial for.
			name: "a client that says nothing", said: "", wantErr: io.EOF,
		},
		{
			// A client that selects and then leaves: its select is answered, and it is still
			// owed no session.
			name: "a select and no service", said: "000ehost:tport:any", wantErr: io.EOF,
			wantReplies: "OKAY" + transportIDReply,
		},
		{
			// Once the daemon knows its device's serial, a select naming another one gets
			// adb's own refusal text and no service request follows it.
			name: "a select naming another device", said: "001ehost:tport:serial:other-device",
			deviceSerial: "dev-1", wantErr: errSelectRefused,
			wantReplies: "FAIL001fdevice 'other-device' not found",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			client := newScriptedConn(testCase.said)

			attached := &DeviceInfo{Serial: testCase.deviceSerial}
			if testCase.noDevice {
				attached = nil
			}

			clientOpening, err := readOpening(context.Background(), client, attached)

			if testCase.wantErr != nil {
				require.ErrorIs(t, err, testCase.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, testCase.wantPending, string(clientOpening.pending), "only unanswered bytes may reach the device")
			assert.Equal(t, testCase.wantService, clientOpening.service, "the service is what the client came for")
			assert.Equal(t, testCase.wantSelected, clientOpening.selected)
			assert.Equal(t, testCase.wantReplies, client.replies.String())
		})
	}
}

func TestGivenAnOpeningWhenItsServiceIsUnwrappedThenOnlyALoneParsableHostPrefixIsStripped(t *testing.T) {
	tests := []struct {
		name    string
		opening opening
		want    string
	}{
		{name: "behind an answered select", want: "reverse:forward:tcp:4;tcp:5",
			opening: opening{service: "reverse:forward:tcp:4;tcp:5", selected: true}},
		// readOpening drops this prefix at read; unwrapping alone does not.
		{name: "a host prefix behind an answered select", want: "host:reverse:forward:tcp:4;tcp:5",
			opening: opening{service: "host:reverse:forward:tcp:4;tcp:5", selected: true}},
		{name: "alone behind a serial", want: "reverse:forward:tcp:4;tcp:5",
			opening: opening{service: "host-serial:emulator-5554:reverse:forward:tcp:4;tcp:5"}},
		{name: "alone behind an unparsable prefix", want: "host-serial::reverse:forward:tcp:4;tcp:5",
			opening: opening{service: "host-serial::reverse:forward:tcp:4;tcp:5"}},
		{name: "alone with no prefix", want: "reverse:forward:tcp:4;tcp:5",
			opening: opening{service: "reverse:forward:tcp:4;tcp:5"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, testCase.opening.unwrappedService())
		})
	}
}

func TestGivenASelectNamingAnotherDeviceWhenAnsweredThenAdbsRefusalEndsTheConnection(t *testing.T) {
	// The client asked for a device this attachment does not hold. It gets adb's own text, so
	// it prints a reason, and the caller is left with nothing to serve.
	var reply bytes.Buffer

	_, claimed, err := answerTransportSelect(&reply, "host:tport:serial:other-device", "dev-1")

	assert.False(t, claimed, "a refused select leaves no second request to read")
	assert.ErrorIs(t, err, errSelectRefused)
	assert.Equal(t, "FAIL001fdevice 'other-device' not found", reply.String())
}

func TestGivenTwoRequestsAndAppBytesInOneFeedWhenScannedThenTheAnsweredOneIsDroppedAndTheRestIsPending(t *testing.T) {
	// The scanner hides only the request the daemon answered itself; every other byte must
	// reach the device exactly as the client framed it.
	var scanner requestScanner
	scanner.feed([]byte("000ehost:tport:any0008shell:lspayload-1"))

	first, err := scanner.readRequest(bytes.NewReader(nil))
	require.NoError(t, err)
	assert.Equal(t, "host:tport:any", first)
	scanner.dropAnswered()

	second, err := scanner.readRequest(bytes.NewReader(nil))
	require.NoError(t, err)
	assert.Equal(t, "shell:ls", second)

	assert.Equal(t, "0008shell:lspayload-1", string(scanner.pending()))
}

func TestGivenASelectSplitAcrossFeedsWhenScannedThenOnlyItsOwnBytesAreDropped(t *testing.T) {
	// A length prefix in one segment and its payload in the next: what the daemon drops is
	// counted from the reader, so it survives any fragmentation.
	var scanner requestScanner
	scanner.feed([]byte("000e"))

	service, err := scanner.readRequest(bytes.NewReader([]byte("host:tport:any0008shell:ls")))
	require.NoError(t, err)
	assert.Equal(t, "host:tport:any", service)

	scanner.dropAnswered()
	assert.Equal(t, "0008shell:ls", string(scanner.pending()))
}

func TestGivenAFrameThatIsNotARequestWhenScannedThenNoServiceAndEveryBytePending(t *testing.T) {
	// The parser fails closed rather than guessing a length, and the daemon is only a proxy,
	// so a non-adb client's bytes are pending in full and no further request is read.
	var scanner requestScanner

	service, err := scanner.readRequest(bytes.NewReader([]byte("hello world")))

	require.NoError(t, err)
	assert.Empty(t, service, "bytes that are not a request name no service")
	assert.Equal(t, "hello world", string(scanner.pending()))
}

// stalledReader gives no bytes and no error, which no net.Conn does.
type stalledReader struct{}

func (stalledReader) Read([]byte) (int, error) { return 0, nil }

func TestGivenAReaderThatMakesNoProgressWhenARequestIsReadThenItStopsInsteadOfSpinning(t *testing.T) {
	var scanner requestScanner

	_, err := scanner.readRequest(stalledReader{})

	assert.ErrorIs(t, err, io.ErrNoProgress)
}

func TestGivenAnAnsweredSelectWhenTheRequestIsRelayedThenTheDaemonsOwnSwitchPrecedesIt(t *testing.T) {
	// The client's select was consumed at the seam and cannot be replayed: a second status
	// word would leave the client one reply ahead for the rest of the connection.
	mock := switchReplyMock(t, "OKAY")
	session := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})
	client := newScriptedConn("")

	err := relayRequest(context.Background(), client, session, []byte("0008shell:ls"), true)

	require.NoError(t, err)
	assertSessionReceived(t, mock, canonicalSelect+"0008shell:ls")
	assert.Empty(t, client.replies.String(), "the switch's reply is the daemon's, not the client's")
}

func TestGivenNoAnsweredSelectWhenTheRequestIsRelayedThenTheDeviceGetsItWithoutASwitch(t *testing.T) {
	mock := switchReplyMock(t, "OKAY")
	session := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})
	client := newScriptedConn("")

	err := relayRequest(context.Background(), client, session, []byte("0025host-serial:dev-1:forward:tcp:1;tcp:2"), false)

	require.NoError(t, err)
	assertSessionReceived(t, mock, "0025host-serial:dev-1:forward:tcp:1;tcp:2")
}

func TestGivenBytesCoalescedWithTheSwitchReplyWhenRelayedThenTheyReachTheClient(t *testing.T) {
	// The remote pumps the adb server with partial reads, so the switch reply can arrive
	// with the client's first bytes stuck to it. They belong to the client and must go
	// out before the pipe puts its own first read in front of them.
	mock := switchReplyMock(t, "OKAYcoalesced")
	session := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})
	client := newScriptedConn("")

	err := relayRequest(context.Background(), client, session, []byte("0008shell:ls"), true)

	require.NoError(t, err)
	assert.Equal(t, "coalesced", client.replies.String())
	assertSessionReceived(t, mock, canonicalSelect+"0008shell:ls")
}

func TestGivenTheDeviceRefusesTheSwitchWhenRelayedThenTheClientGetsAdbsRefusal(t *testing.T) {
	// The remote adb server lost the device. The client is waiting for the status word of
	// its own request, so it gets the device's FAIL and prints the real reason instead of
	// reporting a hangup.
	mock := switchReplyMock(t, "FAIL000edevice offline")
	session := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})
	client := newScriptedConn("")

	err := relayRequest(context.Background(), client, session, []byte("0008shell:ls"), true)

	require.Error(t, err)
	assert.Equal(t, "FAIL000edevice offline", client.replies.String())
	assert.Equal(t, canonicalSelect, string(soleSessionInbound(mock)),
		"a refused switch must not be followed by the client's request")
}

func TestGivenADetachWhileTheSwitchIsPendingWhenRelayingThenItEndsAtOnce(t *testing.T) {
	// Nothing pumps either leg during the relay, so a `device remove` has to reach it
	// there. Otherwise the wait runs out the whole reply timeout and the client's request
	// still reaches a device the daemon has released.
	testDone := make(chan struct{})
	defer close(testDone)
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, _ []byte) []byte {
			<-testDone // the switch is never answered
			return nil
		},
	})
	session := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	relayed := make(chan error, 1)
	go func() {
		relayed <- relayRequest(ctx, newScriptedConn(""), session, []byte("0008shell:ls"), true)
	}()

	waitFor(t, func() bool { return bytes.Equal(soleSessionInbound(mock), []byte(canonicalSelect)) },
		"the daemon never played its own switch")
	cancel()

	select {
	case err := <-relayed:
		assert.NoError(t, err, "a cancel is end-of-life, not a fault")
	case <-time.After(testWait):
		t.Fatal("the cancel left the relay waiting for a reply that never comes")
	}
}

func TestGivenAReleasedWatcherWhenItsContextEndsThenTheTargetStaysOpen(t *testing.T) {
	// A caller releases, then may cancel at once; a watcher still starting would see both
	// channels ready, so the release must outlast it. synctest.Wait runs any watcher the release
	// left behind before the write, so a release that did not wait for it fails here.
	synctest.Test(t, func(t *testing.T) {
		for range 20 {
			clientEnd, daemonEnd := net.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			release := closeOnCancel(ctx, daemonEnd)

			release()
			cancel()
			synctest.Wait()

			written := make(chan error, 1)
			go func() { _, err := daemonEnd.Write([]byte("x")); written <- err }()
			_, err := clientEnd.Read(make([]byte, 1))
			require.NoError(t, err, "the released target must still carry bytes")
			require.NoError(t, <-written)
			_ = clientEnd.Close()
			_ = daemonEnd.Close()
		}
	})
}

func TestGivenADetachWhileTheOpeningIsReadWhenItIsCancelledThenTheReadEndsAtOnce(t *testing.T) {
	// net.Pipe, not a scripted reader: a read of it blocks the way a read of a client's
	// socket does, so only closing it under the read can end the wait.
	client, daemon := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	opened := make(chan error, 1)
	go func() {
		_, err := readOpening(ctx, daemon, &DeviceInfo{})
		opened <- err
	}()

	cancel()

	select {
	case err := <-opened:
		require.Error(t, err, "a socket closed under the read cannot yield a request")
	case <-time.After(testWait):
		t.Fatal("the cancel left the opening read blocked")
	}
}

func TestGivenAContextPastItsDeadlineWhenRelayingThenTheDeadlineIsReportedNotSwallowed(t *testing.T) {
	// Only a cancel is end-of-life. The connection context carries no deadline, so one that
	// fires is a fault the operator must see.
	mock := switchReplyMock(t, "OKAY")
	session := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	err := relayRequest(ctx, newScriptedConn(""), session, []byte("0008shell:ls"), true)

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Zero(t, mock.SessionsOpened(), "nothing may reach a device once the context has ended")
}

func TestGivenAnAlreadyCancelledContextWhenRelayingThenNothingReachesTheDevice(t *testing.T) {
	// The watcher that closes the legs is a goroutine, so a cancel that landed before the
	// relay started must be seen by the relay itself.
	mock := switchReplyMock(t, "OKAY")
	session := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := relayRequest(ctx, newScriptedConn(""), session, []byte("0008shell:ls"), true)

	assert.NoError(t, err, "a cancel is end-of-life, not a fault")
	// Like the server, the mock mints a session from a first frame, so zero sessions is
	// proof that no byte was sent.
	assert.Zero(t, mock.SessionsOpened(), "nothing may reach a device the daemon has released")
}

// dialClient connects to a running proxy with a deadline on both directions, so a hang
// fails the test instead of stalling it.
func dialClient(t *testing.T, proxyAddress string) net.Conn {
	t.Helper()
	client, err := net.Dial("tcp", proxyAddress)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	require.NoError(t, client.SetDeadline(time.Now().Add(testTimeout)))
	return client
}

// selectAnyTransport sends `host:tport:any` and reads the seam's reply, so the daemon reads the
// request behind it next.
func selectAnyTransport(t *testing.T, client net.Conn) {
	t.Helper()
	writeAll(t, client, []byte("000ehost:tport:any"))
	require.Equal(t, "OKAY"+transportIDReply, string(readN(t, client, 12)), "the select is answered first")
}

// startSelectAwareProxy runs a proxy in front of a mock that answers the daemon's own
// switch, and returns a client already connected to it.
func startSelectAwareProxy(t *testing.T) (net.Conn, *testsupport.SessionMockWSS) {
	t.Helper()
	mock := switchReplyMock(t, "OKAY")
	proxyAddress, _ := startServer(t, mock.URL, testPolicy())
	return dialClient(t, proxyAddress), mock
}

// assertNoWebsocketDialed pins the absence of a dial on a count, over a window long
// enough for one to show up.
func assertNoWebsocketDialed(t *testing.T, mock *testsupport.SessionMockWSS) {
	t.Helper()
	assert.Never(t, func() bool { return mock.ConnectionsAccepted() > 0 }, 200*time.Millisecond,
		10*time.Millisecond, "no websocket may be dialed")
}

func TestGivenATportSelectWhenReceivedThenOkayAndEightBytesAndNoDial(t *testing.T) {
	// The reply `adb forward` waits for: OKAY plus the transport id as 8 raw
	// bytes. A short one makes the client report a failure to read the transport id.
	client, mock := startSelectAwareProxy(t)

	writeAll(t, client, []byte("000ehost:tport:any"))

	want := "OKAY" + transportIDReply
	assert.Equal(t, want, string(readN(t, client, len(want))), "a tport: select is answered with the id")
	assert.Equal(t, 0, mock.ConnectionsAccepted(),
		"the daemon answers the select itself, so nothing is dialed for it")
}

func TestGivenATransportIDSelectWhenReceivedThenBareOkayAndNoDial(t *testing.T) {
	// `adb -t 97` selects with transport-id:<n>, which takes a bare OKAY. Reading the echo
	// of the second request straight after the 4 bytes proves no payload followed them.
	client, mock := startSelectAwareProxy(t)

	writeAll(t, client, []byte("0014host:transport-id:97"))
	assert.Equal(t, "OKAY", string(readN(t, client, 4)), "a transport-id select is answered with a bare OKAY")
	assert.Equal(t, 0, mock.ConnectionsAccepted(), "answering a select dials nothing")

	writeAll(t, client, []byte("000chost:devices"))
	assert.Equal(t, "000chost:devices", string(readN(t, client, 16)),
		"the next bytes the client reads are its own service's reply, not a transport id")
	assertSessionReceived(t, mock, canonicalSelect+"000chost:devices")
}

func TestGivenASelectThenAReverseRequestWhenDeclinedThenTheCanonicalSelectPrecedesTheVerbatimRequest(t *testing.T) {
	// Reverse on (the fixture's setting), so reverse:* is not answered here: it reaches the device
	// untouched, behind the switch the daemon fabricates for itself. The client's own select is
	// never replayed.
	client, mock := startSelectAwareProxy(t)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("001breverse:forward:tcp:4;tcp:5"))

	assertSessionReceived(t, mock, canonicalSelect+"001breverse:forward:tcp:4;tcp:5")
	assert.Equal(t, 1, mock.SessionsOpened(), "one declined request opens one session")
}

func TestGivenASelectThenADeviceServiceWhenDeclinedThenTheServiceReachesTheSessionOnce(t *testing.T) {
	// The positive control: `adb shell` selects a transport too, so every device-directed
	// command goes down this path.
	client, mock := startSelectAwareProxy(t)

	writeAll(t, client, []byte("000ehost:tport:any"))
	readN(t, client, 12)
	writeAll(t, client, []byte("0008shell:ls"))

	assertSessionReceived(t, mock, canonicalSelect+"0008shell:ls")
}

func TestGivenASelectAndItsServiceInOneWriteWhenReceivedThenOnlyTheServiceReachesTheSession(t *testing.T) {
	// Both requests land in one segment, so the second one is already buffered when the
	// daemon goes looking for it and no further read may be needed to find it.
	client, mock := startSelectAwareProxy(t)

	writeAll(t, client, []byte("000ehost:tport:any0008shell:ls"))

	assert.Equal(t, 12, len(readN(t, client, 12)), "the select is still answered first")
	assertSessionReceived(t, mock, canonicalSelect+"0008shell:ls")
}

func TestGivenAHostCommandWithNoSelectWhenItArrivesThenNoSelectIsAddedToTheSession(t *testing.T) {
	// A host command and no select, as `adb -s dev-1 features` sends: the device leg carries
	// the client's own bytes and nothing else.
	client, mock := startSelectAwareProxy(t)

	writeAll(t, client, []byte("001ahost-serial:dev-1:features"))

	assertSessionReceived(t, mock, "001ahost-serial:dev-1:features")
}

func TestGivenAnOlderSelectSpellingThatMatchesWhenReceivedThenBareOkayAndTheServiceRelaysBehindTheCanonicalSelect(t *testing.T) {
	// Android Studio selects with these and reads a bare OKAY. Reading the echo of the second
	// request straight after the 4 bytes proves no id followed them; the session sees the
	// daemon's own select, never the client's.
	tests := []struct {
		name          string
		deviceSerial  string
		selectRequest string
	}{
		{name: "the serial spelling", deviceSerial: "dev-1", selectRequest: "0014host:transport:dev-1"},
		{name: "the any spelling", deviceSerial: "dev-1", selectRequest: "0012host:transport-any"},
		{name: "the usb spelling", deviceSerial: "dev-1", selectRequest: "0012host:transport-usb"},
		{name: "the local spelling", deviceSerial: "emulator-5554", selectRequest: "0014host:transport-local"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			proxyAddress, mock, _ := startAttachedProxyWithSerial(t, testPolicy(), testCase.deviceSerial)
			client := dialClient(t, proxyAddress)

			writeAll(t, client, []byte(testCase.selectRequest))
			assert.Equal(t, "OKAY", string(readN(t, client, 4)), "an older spelling is answered with a bare OKAY")
			assert.Equal(t, 0, mock.ConnectionsAccepted(), "answering a select dials nothing")

			writeAll(t, client, []byte("0008shell:ls"))
			assert.Equal(t, "0008shell:ls", string(readN(t, client, 12)),
				"the next bytes the client reads are its own service's echo, not a transport id")
			assertSessionReceived(t, mock, canonicalSelect+"0008shell:ls")
		})
	}
}

func TestGivenAnOlderSelectSpellingThatMissesWhenReceivedThenAdbsFailAndTheCloseAndNoDial(t *testing.T) {
	// The select is resolved here, so an Android Studio select for a device this attachment
	// does not hold fails one hop early, with adb's text.
	tests := []struct {
		name          string
		deviceSerial  string
		selectRequest string
		wantReply     string
	}{
		{name: "another serial", deviceSerial: "dev-1", selectRequest: "001bhost:transport:other-device",
			wantReply: "FAIL001fdevice 'other-device' not found"},
		{name: "usb against an emulator", deviceSerial: "emulator-5554", selectRequest: "0012host:transport-usb",
			wantReply: "FAIL0010no devices found"},
		{name: "local against a usb device", deviceSerial: "dev-1", selectRequest: "0014host:transport-local",
			wantReply: "FAIL0012no emulators found"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			proxyAddress, mock, _ := startAttachedProxyWithSerial(t, testPolicy(), testCase.deviceSerial)
			client := dialClient(t, proxyAddress)

			writeAll(t, client, []byte(testCase.selectRequest))

			assert.Equal(t, testCase.wantReply, string(readN(t, client, len(testCase.wantReply))))
			_, err := client.Read(make([]byte, 1))
			assert.ErrorIs(t, err, io.EOF, "the daemon closes a connection it refused")
			assert.Zero(t, mock.ConnectionsAccepted(), "a refused select dials nothing")
		})
	}
}

func TestGivenARequestPipelinedWithAppBytesWhenDeclinedThenBothReachTheSessionInOrder(t *testing.T) {
	// A client that writes its service and its first payload bytes together: the bytes
	// read past the request must reach the session before the pipe's own first read.
	client, mock := startSelectAwareProxy(t)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("0008shell:lspayload-1"))

	assertSessionReceived(t, mock, canonicalSelect+"0008shell:lspayload-1")
}

func TestGivenASelectAndNoSecondRequestWhenTheClientClosesThenNoSessionIsMinted(t *testing.T) {
	// A client that selects and then leaves: the server mints a session from the first frame
	// of any /adb websocket, so dialing one here would hold a session per abandoned command.
	client, mock := startSelectAwareProxy(t)

	selectAnyTransport(t, client)
	require.NoError(t, client.Close())

	assertNoWebsocketDialed(t, mock)
}

func TestGivenANonSmartSocketFirstFrameWhenReceivedThenItReachesTheSessionVerbatim(t *testing.T) {
	// Bytes with no length prefix the parser can read are relayed, not refused: the
	// daemon is only a proxy, and its parser fails closed rather than guessing.
	mock := testsupport.NewSessionEchoWSS(t)
	proxyAddress, _ := startServer(t, mock.URL, testPolicy())
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("hello world"))

	assertSessionReceived(t, mock, "hello world")
}

func TestGivenAClientThatClosesBeforeItsFirstRequestWhenAcceptedThenNoSessionIsMinted(t *testing.T) {
	mock := switchReplyMock(t, "OKAY")
	proxyAddress, _ := startServer(t, mock.URL, testPolicy())

	silent, err := net.Dial("tcp", proxyAddress)
	require.NoError(t, err)
	require.NoError(t, silent.Close())
	assertNoWebsocketDialed(t, mock)

	// The listener still dials for a client that does ask for something, so the absence
	// above belongs to the silent client and not to a dead proxy.
	writeAll(t, dialClient(t, proxyAddress), []byte("000chost:devices"))
	assertSessionReceived(t, mock, "000chost:devices")
}

func TestGivenBytesCoalescedWithTheSwitchReplyWhenDeclinedThenTheyReachTheClientBeforeThePipe(t *testing.T) {
	// The bytes behind the switch reply belong to the client and must go out before the
	// pipe puts its own first read in front of them.
	mock := switchReplyMock(t, "OKAYcoalesced")
	proxyAddress, _ := startServer(t, mock.URL, testPolicy())
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("0008shell:ls"))

	assert.Equal(t, "coalesced0008shell:ls", string(readN(t, client, 21)),
		"the bytes behind the switch reply must precede everything the pipe carries")
}

func TestGivenTheDeviceRefusesTheSwitchWhenDeclinedThenTheServerSurvives(t *testing.T) {
	// A refused switch closes that client and nothing else.
	mock := switchReplyMock(t, "FAIL000edevice offline")
	proxyAddress, stopped := startServer(t, mock.URL, testPolicy())
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("0008shell:ls"))

	assert.Equal(t, "FAIL000edevice offline", string(readN(t, client, 22)))
	select {
	case runErr := <-stopped:
		t.Fatalf("the server exited after a refused switch: %v", runErr)
	default:
	}
}

func TestGivenADetachWhileTheSwitchIsPendingWhenDecliningThenTheConnectionEndsAtOnce(t *testing.T) {
	// The relay's close-on-cancel has to be reached from a `device remove`, or the client
	// waits out the whole reply timeout and its command still reaches a released device.
	testDone := make(chan struct{})
	defer close(testDone)
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
			if bytes.Equal(data, []byte(canonicalSelect)) {
				<-testDone // the switch is never answered
			}
			return nil
		},
	})
	device := deviceFor(mock.URL)
	server := newProxyServer(t, 0, testPolicy(), newTestDialer(testTimeout), device)
	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-serverDone
	})

	client := dialClient(t, waitForAddr(t, server))
	selectAnyTransport(t, client)
	writeAll(t, client, []byte("0008shell:ls"))
	waitFor(t, func() bool {
		return bytes.Equal(soleSessionInbound(mock), []byte(canonicalSelect))
	}, "the daemon never played its own switch")

	_, ok := device.Clear() // a device remove
	require.True(t, ok)

	// The client's deadline is far shorter than the wait for a reply that never comes.
	require.NoError(t, client.SetDeadline(time.Now().Add(detachedReadDeadline)))
	_, readErr := client.Read(make([]byte, 1))
	assert.ErrorIs(t, readErr, io.EOF, "the detach must end the connection instead of waiting")
}

// reverseOffFail is the framed FAIL every reverse request reads while reverse is off.
const reverseOffFail = "FAIL0047adb reverse is off for this client; restart it with --allow-adb-reverse"

// reverseFamily holds one request of each form in adb's reverse family, plus a malformed one.
var reverseFamily = []string{
	"reverse:forward:tcp:4;tcp:5",
	"reverse:forward:norebind:tcp:4;tcp:5",
	"reverse:killforward:tcp:4",
	"reverse:killforward-all",
	"reverse:list-forward",
	"reverse:forward:bogus",
}

// startReverseOffProxy runs a proxy in front of an attachment with reverse off. The mock OKAYs the
// daemon's select, so a relayed service reaches it; every dial shows in its connection count.
func startReverseOffProxy(t *testing.T) (proxyAddress string, mock *testsupport.SessionMockWSS,
	device *ActiveDevice) {
	t.Helper()
	mock = switchReplyMock(t, "OKAY")
	params := forwardingParams()
	params.ReverseEnabled = false
	device = newAttachedDevice(t, params)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: mock.URL, Label: "Test Device - OS 1", Serial: "dev-1"}))
	proxyAddress, _ = runProxy(t, device, testPolicy())
	return proxyAddress, mock, device
}

// assertRefusedReverseOff reads the reverse-off FAIL off client and the close behind it. The
// close means the connection's handler returned, so no dial can follow the count.
func assertRefusedReverseOff(t *testing.T, client net.Conn, mock *testsupport.SessionMockWSS) {
	t.Helper()
	assert.Equal(t, reverseOffFail, string(readN(t, client, len(reverseOffFail))),
		"the client reads the refusal as its request's FAIL")
	_, err := client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the daemon closes a connection it refused")
	assert.Zero(t, mock.ConnectionsAccepted(), "a refused reverse request must dial nothing")
}

func TestGivenReverseOffWhenAReverseRequestArrivesThenTheDaemonFailsItLocallyAndDialsNothing(t *testing.T) {
	// Shapes a client puts the family in: behind the `any` select adb sends, behind the serial
	// select Android Studio sends, and alone, with and without a host prefix, as a raw client may.
	shapes := []struct {
		name string
		send func(t *testing.T, client net.Conn, service string)
	}{
		{name: "behind host:tport:any", send: func(t *testing.T, client net.Conn, service string) {
			selectAnyTransport(t, client)
			writeAll(t, client, framed(service))
		}},
		{name: "behind host:transport:<serial>", send: func(t *testing.T, client net.Conn, service string) {
			writeAll(t, client, framed("host:transport:dev-1"))
			require.Equal(t, "OKAY", string(readN(t, client, 4)), "the select is answered first, with no id")
			writeAll(t, client, framed(service))
		}},
		{name: "alone behind host:", send: func(t *testing.T, client net.Conn, service string) {
			writeAll(t, client, framed("host:"+service))
		}},
		{name: "alone with no prefix", send: func(t *testing.T, client net.Conn, service string) {
			writeAll(t, client, framed(service))
		}},
	}
	for _, shape := range shapes {
		for _, service := range reverseFamily {
			t.Run(shape.name+" "+service, func(t *testing.T) {
				proxyAddress, mock, _ := startReverseOffProxy(t)
				client := dialClient(t, proxyAddress)

				shape.send(t, client, service)

				assertRefusedReverseOff(t, client, mock)
			})
		}
	}
}

func TestGivenReverseOffAndASpentBudgetWhenAReverseRequestArrivesThenTheReverseOffFailAnswersIt(t *testing.T) {
	// The refusal sits before the budget: a full budget must not change the answer.
	proxyAddress, mock, device := startReverseOffProxy(t)
	spendBudget(t, device)
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, framed("reverse:forward:tcp:4;tcp:5"))

	assertRefusedReverseOff(t, client, mock)
}

func TestGivenReverseOffAndAFullReverseTableWhenAnInstallArrivesThenTheReverseOffFailAnswersIt(t *testing.T) {
	// The refusal sits before the rule screen: a full table must not change the answer.
	proxyAddress, mock, device := startReverseOffProxy(t)
	rules := attachedFeatures(t, device).reverseRules
	for index := range testMaxReverseRules {
		request, full := rules.begin(adbwire.AdbActionForward, tcpSpec(index+1), 0)
		require.False(t, full, "the table must have room for the rule")
		request.settle(true)
	}
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, framed("reverse:forward:tcp:9;tcp:5"))

	assertRefusedReverseOff(t, client, mock)
}

func TestGivenReverseOffWhenAForwardRequestArrivesThenItIsStillAnsweredLocally(t *testing.T) {
	// Only the reverse family is refused: forwarding keeps its local answer and its listener.
	proxyAddress, mock, _ := startReverseOffProxy(t)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte(installForward))

	assert.NotZero(t, readForwardSuccessPort(t, client), "the forward family is answered with reverse off")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenReverseOffWhenADeviceServiceArrivesThenItIsStillRelayed(t *testing.T) {
	// `adb shell` and every other device service are untouched by the reverse setting.
	proxyAddress, mock, _ := startReverseOffProxy(t)
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("0008shell:ls"))

	assertSessionReceived(t, mock, canonicalSelect+"0008shell:ls")
}
