package testsupport

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// The reverse mock is a second implementation of the ?path=adbreverse contract, so it gets its
// own tests, for the reason given at the top of sessionwss_test.go.

const (
	// testWait bounds every wait for something the mock must do promptly.
	testWait = 2 * time.Second
	// testReadDeadline fails a blocked read instead of hanging the test.
	testReadDeadline = 5 * testWait
	// testPollInterval is the Eventually tick.
	testPollInterval = 5 * time.Millisecond
)

// testToken is a canonical UUIDv4, as a REVERSE_OPEN token must be.
const testToken = "11111111-2222-4333-8444-555555555555"

// dialReverse dials the mock on the reverse route, as the production dialer does by setting the
// endpoint's path query parameter.
func dialReverse(t *testing.T, mock *ReverseMockWSS) *websocket.Conn {
	t.Helper()
	ws := dialTestWSS(t, fmt.Sprintf("%s?path=%s", mock.URL, routeAdbReverse))
	// Bounded, so a mock that stops sending fails a test instead of hanging it.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(testReadDeadline)))
	return ws
}

// dialReadyNotifier dials, sends the hello and requires the READY ack. Both are literal wire
// bytes (fixture vectors notifier-hello and notifier-ready), so a drifted builder fails here.
func dialReadyNotifier(t *testing.T, mock *ReverseMockWSS) *websocket.Conn {
	t.Helper()
	ws := dialReverse(t, mock)
	require.NoError(t, websocket.Message.Send(ws, []byte("\x00DFADB\x01{\"notifierHello\":{}}")))
	require.Equal(t, []byte("\x00DFADB\x01{\"notifierReady\":{}}"), firstFrame(t, ws))
	return ws
}

// assertRefused checks the mock answered ws with an ERROR carrying reason, closed the socket,
// and counted that one refusal under the same reason.
func assertRefused(t *testing.T, mock *ReverseMockWSS, ws *websocket.Conn, reason string) {
	t.Helper()
	assert.Contains(t, assertControlFrame(t, firstFrame(t, ws), controlStatusError), reason)
	var afterTheError []byte
	assert.ErrorIs(t, websocket.Message.Receive(ws, &afterTheError), io.EOF,
		"a refusal must close the socket, not merely stop answering")
	require.Len(t, mock.Refusals(), 1)
	assert.Contains(t, mock.Refusals()[0], reason)
}

func TestGivenANotifierHelloWhenSentThenTheMockAcksItAndCountsTheNotifier(t *testing.T) {
	mock := NewReverseMockWSS(t)

	dialReadyNotifier(t, mock)

	assert.Equal(t, 1, mock.NotifiersAccepted())
	assert.Empty(t, mock.Refusals())
}

func TestGivenALiveNotifierWhenASecondHelloArrivesThenItIsRefusedAndTheFirstKeepsDelivery(t *testing.T) {
	mock := NewReverseMockWSS(t)
	first := dialReadyNotifier(t, mock)
	second := dialReverse(t, mock)

	require.NoError(t, websocket.Message.Send(second, FrameEmptyArm(controlStatusNotifierHello)))

	assertRefused(t, mock, second, "notifier already registered")
	assert.Equal(t, 1, mock.NotifiersAccepted())
	require.True(t, mock.Announce(testToken, 8080))
	assert.Equal(t, testToken, assertControlFrame(t, firstFrame(t, first), controlStatusReverseOpen))
}

func TestGivenABadFirstFrameWhenSentThenItIsRefusedAndCounted(t *testing.T) {
	cases := []struct {
		name       string
		send       func(t *testing.T, ws *websocket.Conn)
		wantReason string
	}{
		{"adb bytes before the hello", func(t *testing.T, ws *websocket.Conn) {
			require.NoError(t, websocket.Message.Send(ws, []byte("000chost:version")))
		}, "first frame was not an empty NOTIFIER_HELLO"},
		{"a hello carrying a member", func(t *testing.T, ws *websocket.Conn) {
			require.NoError(t, websocket.Message.Send(ws, FrameWithBody(`{"notifierHello":{"extra":1}}`)))
		}, "first frame was not an empty NOTIFIER_HELLO"},
		{"a well-formed frame of another arm", func(t *testing.T, ws *websocket.Conn) {
			require.NoError(t, websocket.Message.Send(ws, FrameEmptyArm(controlStatusPing)))
		}, "first frame was not an empty NOTIFIER_HELLO"},
		// x/net hands a text frame to a []byte receive unchanged, so it is refused on its
		// content rather than on its opcode, which is all the daemon can see either.
		{"a text frame", func(t *testing.T, ws *websocket.Conn) {
			require.NoError(t, websocket.Message.Send(ws, "hello"))
		}, "first frame was not an empty NOTIFIER_HELLO"},
		{"a connector hello with no parked connection", func(t *testing.T, ws *websocket.Conn) {
			require.NoError(t, websocket.Message.Send(ws, FrameConnectorHello(testToken)))
		}, "unknown or already redeemed reverse token"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mock := NewReverseMockWSS(t)
			ws := dialReverse(t, mock)

			testCase.send(t, ws)

			assertRefused(t, mock, ws, testCase.wantReason)
			assert.Zero(t, mock.NotifiersAccepted())
			assert.NotContains(t, mock.Refusals()[0], testToken,
				"a refusal reason must not echo a bearer token")
		})
	}
}

func TestGivenADialWithoutTheReverseRouteWhenServedThenItIsRefusedAndCounted(t *testing.T) {
	mock := NewReverseMockWSS(t)

	ws := dialTestWSS(t, mock.URL)
	// Bounded like dialReverse: a mock that refuses without closing must fail, not hang.
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(testReadDeadline)))

	// The reason quotes the routes; the JSON writer must carry the quotes through.
	assertRefused(t, mock, ws, `dialed on route "", not "adbreverse"`)
}

func TestGivenAReadyNotifierWhenItPingsThenTheMockPongsAndCountsIt(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReadyNotifier(t, mock)

	// Fixture vectors ping and pong, as literal bytes.
	require.NoError(t, websocket.Message.Send(ws, []byte("\x00DFADB\x01{\"ping\":{}}")))

	require.Equal(t, []byte("\x00DFADB\x01{\"pong\":{}}"), firstFrame(t, ws))
	assert.Equal(t, 1, mock.PingsSeen())
}

func TestGivenSwallowedPongsWhenTheNotifierPingsThenItIsCountedWithNoAnswer(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReadyNotifier(t, mock)
	mock.SwallowPongs(true)

	require.NoError(t, websocket.Message.Send(ws, FrameEmptyArm(controlStatusPing)))

	require.Eventually(t, func() bool { return mock.PingsSeen() == 1 }, testWait, testPollInterval)
	// A sentinel proves the absence positively: the next frame in is this one, not a PONG.
	// Fixture vector unknown-arm.
	sentinel := FrameEmptyArm("hello")
	require.True(t, mock.SendRaw(sentinel))
	assert.Equal(t, sentinel, firstFrame(t, ws))
}

func TestGivenAReadyNotifierWhenItSendsAnythingButAPingThenItIsRefusedAndCounted(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReadyNotifier(t, mock)

	require.NoError(t, websocket.Message.Send(ws, []byte("000chost:version")))

	assertRefused(t, mock, ws, "not a PING")
	assert.Zero(t, mock.PingsSeen())
}

func TestGivenAReadyNotifierWhenAnnouncedThenTheReverseOpenCarriesTheTokenAndPort(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReadyNotifier(t, mock)

	require.True(t, mock.Announce(fixtureUUID, 9090))

	assert.Equal(t, frameVectorBytes(t, "reverse-open-port-9090"), firstFrame(t, ws))
}

func TestGivenAReadyNotifierWhenSendRawIsUsedThenTheExactBytesArrive(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReadyNotifier(t, mock)
	// An arm no correct server sends, which is the point of SendRaw (fixture vector unknown-arm).
	frame := FrameEmptyArm("hello")

	require.True(t, mock.SendRaw(frame))

	assert.Equal(t, frame, firstFrame(t, ws))
}

func TestGivenNoNotifierWhenAnnouncedThenNothingIsDelivered(t *testing.T) {
	mock := NewReverseMockWSS(t)

	assert.False(t, mock.Announce(testToken, 8080))
}

func TestGivenALiveNotifierWhenDroppedFromTheServerThenTheClientsReadFails(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReadyNotifier(t, mock)

	require.True(t, mock.DropNotifier(), "a live notifier must be droppable")

	var message []byte
	assert.Error(t, websocket.Message.Receive(ws, &message),
		"a server-side drop must fail the client's read, not deliver a frame")
	require.Eventually(t, func() bool { return mock.NotifiersClosed() == 1 }, testWait,
		testPollInterval, "the mock must count the notifier it dropped")
	assert.False(t, mock.DropNotifier(), "there is nothing left to drop")
}

func TestGivenADroppedNotifierWhenItClosesThenTheMockSeesItAndAcceptsASuccessor(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReadyNotifier(t, mock)

	require.NoError(t, ws.Close())

	require.Eventually(t, func() bool { return mock.NotifiersClosed() == 1 }, testWait,
		testPollInterval, "the mock must see a dropped notifier and free the slot")
	dialReadyNotifier(t, mock)
	assert.Equal(t, 2, mock.NotifiersAccepted())
}

func TestGivenAParkedConnectionWhenItsTokenIsRedeemedThenBytesRoundTripBothWays(t *testing.T) {
	mock := NewReverseMockWSS(t)
	device := mock.Connect(testToken)
	require.NoError(t, device.SetDeadline(time.Now().Add(testWait)))
	ws := dialReverse(t, mock)

	require.NoError(t, websocket.Message.Send(ws, FrameConnectorHello(testToken)))

	assert.Len(t, assertControlFrame(t, firstFrame(t, ws), controlStatusNew), sessionUUIDLen)
	require.NoError(t, websocket.Message.Send(ws, []byte("to the device")))
	inbound := make([]byte, len("to the device"))
	_, err := io.ReadFull(device, inbound)
	require.NoError(t, err)
	assert.Equal(t, "to the device", string(inbound))
	_, err = device.Write([]byte("from the device"))
	require.NoError(t, err)
	assert.Equal(t, []byte("from the device"), firstFrame(t, ws))
	assert.Equal(t, 1, mock.ConnectorsAccepted())
	assert.Zero(t, mock.ConnectorsRefused())
}

func TestGivenARefusingMockWhenATokenIsRedeemedThenTheReasonIsAnnouncedAndCounted(t *testing.T) {
	mock := NewReverseMockWSS(t)
	mock.Connect(testToken)
	mock.RefuseConnector("the connector pool is at its cap")
	ws := dialReverse(t, mock)

	require.NoError(t, websocket.Message.Send(ws, FrameConnectorHello(testToken)))

	assertRefused(t, mock, ws, "the connector pool is at its cap")
	assert.Equal(t, 1, mock.ConnectorsRefused())
	assert.Zero(t, mock.ConnectorsAccepted())
}

func TestGivenAUUIDAsTheFirstFrameWhenSentOnTheReverseRouteThenTheSessionIsRefusedAsGone(t *testing.T) {
	mock := NewReverseMockWSS(t)
	ws := dialReverse(t, mock)

	require.NoError(t, websocket.Message.Send(ws, []byte(testToken)))

	assertRefused(t, mock, ws, "unknown or expired session")
}

func TestGivenAHeldSlotWhenAHelloArrivesThenItIsRefusedUntilTheHoldIsReleased(t *testing.T) {
	mock := NewReverseMockWSS(t)
	release := mock.HoldNotifierSlot()
	refused := dialReverse(t, mock)
	require.NoError(t, websocket.Message.Send(refused, FrameEmptyArm(controlStatusNotifierHello)))
	assertRefused(t, mock, refused, "notifier already registered")

	release()
	dialReadyNotifier(t, mock)

	assert.Equal(t, 1, mock.NotifiersAccepted(), "the released slot takes the next hello")
}
