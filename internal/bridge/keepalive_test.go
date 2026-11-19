package bridge

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// SendKeepAlive lives on WSDialer, so its tests live here. It offers no session UUID, so a
// plain mock is enough.

func TestGivenReachableEndpointWhenKeepAliveSentThenHostDevicesFrameDeliveredOnTheAdbRoute(t *testing.T) {
	gotCh := make(chan []byte, 1)
	routeCh := make(chan string, 1)
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		routeCh <- ws.Request().URL.Query().Get("path")
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err == nil {
			gotCh <- data
		}
		ws.Close()
	})
	dialer := newTestDialer(testWait)

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err)
	assert.Empty(t, serial, "a closed connection yields no serial, not an error")
	select {
	case got := <-gotCh:
		assert.Equal(t, hostDevicesRequest, string(got), "the keep-alive must be an adb host:devices request")
		assert.Equal(t, "adb", <-routeCh, "the probe must reach the adb session handler")
	case <-time.After(5 * time.Second):
		t.Fatal("keep-alive frame never reached the endpoint")
	}
}

func TestGivenUnreachableEndpointWhenKeepAliveSentThenDialErrorReturned(t *testing.T) {
	// Port 1 is not listening, so the dial inside SendKeepAlive fails.
	dialer := newTestDialer(500 * time.Millisecond)

	_, err := dialer.SendKeepAlive(context.Background(), "wss://127.0.0.1:1/adb")

	require.Error(t, err)
}

func TestGivenMalformedEndpointWhenKeepAliveSentThenDialErrorReturned(t *testing.T) {
	// A malformed endpoint fails at url.Parse before any dial, so SendKeepAlive returns
	// a dial error deterministically, with no timing race.
	dialer := newTestDialer(testWait)

	_, err := dialer.SendKeepAlive(context.Background(), "://not-a-url")

	require.Error(t, err)
}

// probeReplyServer builds a mock endpoint that consumes the probe request, then sends
// each of frames as its own binary websocket message.
func probeReplyServer(t *testing.T, frames ...[]byte) string {
	t.Helper()
	return testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		var request []byte
		if err := websocket.Message.Receive(serverConn, &request); err != nil {
			return
		}
		for _, frame := range frames {
			if err := websocket.Message.Send(serverConn, frame); err != nil {
				return
			}
		}
		// Hold the connection open so the reader, not a close, ends the parse.
		var discard []byte
		_ = websocket.Message.Receive(serverConn, &discard)
	})
}

// controlFrameNew renders the V1 session control frame announcing a new session.
func controlFrameNew() []byte {
	return []byte(magicPrefix + "\x00" + "f47ac10b-58cc-4372-a567-0e02b2c3d479")
}

// okayDevicesReply renders the adb host:devices reply naming the given lines.
func okayDevicesReply(payload string) []byte {
	return []byte(fmt.Sprintf("OKAY%04x%s", len(payload), payload))
}

func TestGivenDevicesReplyWhenKeepAliveSentThenSerialParsed(t *testing.T) {
	wsURL := probeReplyServer(t, controlFrameNew(), okayDevicesReply("R3CN30ABCDE\tdevice\n"))
	dialer := newTestDialer(testWait)

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err)
	assert.Equal(t, "R3CN30ABCDE", serial, "the probe must learn the serial from the devices reply")
}

func TestGivenReplySplitAcrossMessagesWhenKeepAliveSentThenSerialParsed(t *testing.T) {
	reply := okayDevicesReply("emulator-5554\tdevice\n")
	wsURL := probeReplyServer(t, controlFrameNew(), reply[:6], reply[6:])
	dialer := newTestDialer(testWait)

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err)
	assert.Equal(t, "emulator-5554", serial, "a reply split across websocket messages must reassemble")
}

func TestGivenReplyWithoutControlFrameWhenKeepAliveSentThenSerialParsed(t *testing.T) {
	// Leniency: if the first message is already the adb reply, parse it anyway.
	wsURL := probeReplyServer(t, okayDevicesReply("R3CN30ABCDE\tdevice\n"))
	dialer := newTestDialer(testWait)

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err)
	assert.Equal(t, "R3CN30ABCDE", serial)
}

func TestGivenFailReplyWhenKeepAliveSentThenEmptySerialAndNoError(t *testing.T) {
	wsURL := probeReplyServer(t, controlFrameNew(), []byte("FAIL0014something went wrong"))
	dialer := newTestDialer(testWait)

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err, "a refused probe reply must not fail the keep-alive")
	assert.Empty(t, serial)
}

func TestGivenAnUnparsableReplyLengthWhenKeepAliveSentThenEmptySerialAndNoError(t *testing.T) {
	wsURL := probeReplyServer(t, controlFrameNew(), []byte("OKAYzzzzR3CN30ABCDE\tdevice\n"))
	dialer := newTestDialer(testWait)

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err, "a garbled length must not fail the keep-alive")
	assert.Empty(t, serial)
}

func TestGivenAReplyThatNeverCompletesWhenKeepAliveSentThenEmptySerialAndNoError(t *testing.T) {
	// A length the server never satisfies must stop accumulating at the cap.
	header := []byte("OKAYffff")
	filler := bytes.Repeat([]byte("x"), maxProbeReplyBytes+1-len(header))
	wsURL := probeReplyServer(t, controlFrameNew(), header, filler)
	dialer := newTestDialer(testWait)
	dialer.probeReplyTimeout = time.Hour // only the byte cap may end this read

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err)
	assert.Empty(t, serial)
}

func TestGivenAReplyWithNoDeviceRowWhenKeepAliveSentThenEmptySerialAndNoError(t *testing.T) {
	wsURL := probeReplyServer(t, controlFrameNew(), okayDevicesReply("no devices attached\n"))
	dialer := newTestDialer(testWait)

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err)
	assert.Empty(t, serial, "a line with no state column is not a device row")
}

func TestGivenSilentServerWhenKeepAliveSentThenEmptySerialAfterTimeout(t *testing.T) {
	wsURL := probeReplyServer(t) // consumes the request, sends nothing back
	dialer := newTestDialer(testWait)
	dialer.probeReplyTimeout = 100 * time.Millisecond

	serial, err := dialer.SendKeepAlive(context.Background(), wsURL)

	require.NoError(t, err, "a silent server must not fail the keep-alive; the send did its job")
	assert.Empty(t, serial)
}

// The bridge layer must mint no session a proxied connection did not ask for: each
// spends a per-host slot that a refresh's transient second socket also needs. The
// daemon's keep-alive probe does open one per probe; that is the daemon's business.

func TestGivenAttachedDeviceWithNoProxiedConnectionWhenIdleThenTheBridgeMintsNoSession(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	addr, _ := startServer(t, mock.URL, testPolicy())

	// A device is attached and the listener is up, but no local adb client ever
	// connects. The bridge must not reach the endpoint on its own. (startServer wires
	// only the bridge, so the daemon's keep-alive loop is deliberately not running.)
	require.NotEmpty(t, addr)
	time.Sleep(300 * time.Millisecond)

	assert.Zero(t, mock.SessionsOpened(),
		"the bridge must not mint a session for an idle attached device")
}

func TestGivenProxiedConnectionWhenIdleThenExactlyOneSessionExistsPerConnection(t *testing.T) {
	// A connection that goes quiet must hold exactly the one session it opened: nothing
	// extra from the bridge, and no extra session from the refresher until it swaps.
	mock := testsupport.NewSessionEchoWSS(t)
	policy := testPolicy()
	// An interval far longer than the test window, so any additional session would
	// have to come from something other than a refresh.
	policy.Interval = time.Hour
	addr, _ := startServer(t, mock.URL, policy)

	client, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer client.Close()

	payload := []byte("000chost:version")
	writeAll(t, client, payload)
	assert.Equal(t, payload, readN(t, client, len(payload)))

	time.Sleep(300 * time.Millisecond)

	assert.Equal(t, 1, mock.SessionsOpened(), "one proxied connection must mean exactly one session")
	assert.Zero(t, mock.Reattaches(), "no refresh was due, so nothing may re-attach")
	assert.Equal(t, []string{"adb"}, mock.RoutesDialed(), "an accepted adb client is proxied on the adb route")
}
