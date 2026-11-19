package testsupport

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"
)

// testTimeout bounds every blocking wait so a hung test fails instead of stalling the run.
const testTimeout = 5 * time.Second

// dialTestWSS opens a client websocket to a mock server URL with a localhost
// Origin, matching what the production dialer sends. It trusts the test CA.
func dialTestWSS(t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	cfg, err := websocket.NewConfig(wsURL, "http://localhost/")
	require.NoError(t, err)
	cfg.TlsConfig = &tls.Config{RootCAs: TestRoots()}
	ws, err := cfg.DialContext(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })
	return ws
}

func TestGivenEchoHandlerWhenClientSendsThenSameBytesReturnAndURLHasAdbPath(t *testing.T) {
	wsURL := NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		io.Copy(ws, ws)
	})
	// The helper advertises the /adb path the real endpoint carries.
	assert.Contains(t, wsURL, "/adb")

	ws := dialTestWSS(t, wsURL)
	payload := []byte("000chost:devices")
	require.NoError(t, websocket.Message.Send(ws, payload))

	var got []byte
	require.NoError(t, websocket.Message.Receive(ws, &got))
	assert.Equal(t, payload, got)
}

func TestGivenHandlerThatClosesImmediatelyWhenClientReceivesThenObservesEOF(t *testing.T) {
	wsURL := NewMockWSS(t, func(ws *websocket.Conn) {
		ws.Close()
	})

	ws := dialTestWSS(t, wsURL)
	var got []byte
	err := websocket.Message.Receive(ws, &got)

	assert.ErrorIs(t, err, io.EOF, "a client must observe EOF when the mock closes immediately")
}

func TestGivenReachableTargetWhenPipingMockRelaysThenBytesRoundTripThroughTarget(t *testing.T) {
	// A trivial TCP echo server stands in for the backing adb server.
	target := startTCPEcho(t)
	wsURL := NewMockWSSPipingTo(t, target)

	ws := dialTestWSS(t, wsURL)
	ws.PayloadType = websocket.BinaryFrame
	payload := []byte("000chost:version")
	require.NoError(t, websocket.Message.Send(ws, payload))

	var got []byte
	require.NoError(t, websocket.Message.Receive(ws, &got))
	assert.Equal(t, payload, got, "bytes must round-trip through the backing target")
}

func TestGivenUnreachableTargetWhenPipingMockAcceptsThenClientObservesClose(t *testing.T) {
	// Port 1 is not listening, so the relay cannot reach a target and closes the
	// websocket; the client must observe a clean close rather than hang.
	wsURL := NewMockWSSPipingTo(t, "127.0.0.1:1")

	ws := dialTestWSS(t, wsURL)
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var got []byte
	err := websocket.Message.Receive(ws, &got)

	require.Error(t, err, "the client must see the relay close when the target is unreachable")
}

func TestGivenAnAcceptCounterWhenAConnectionArrivesThenItIsCountedAndClosed(t *testing.T) {
	// A counter that never counts makes every "nothing dialed" assertion vacuous.
	counterAddress, accepted := NewAcceptCounter(t)

	conn, err := net.Dial("tcp", counterAddress)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(testTimeout)))
	_, err = conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the counter must close what it accepts")
	assert.Equal(t, 1, accepted())
}

func TestGivenAServerMockWhenEachRouteIsDialedThenItReachesItsOwnMock(t *testing.T) {
	sessions, reverse := NewServerMockWSS(t, SessionMockConfig{})
	require.Equal(t, sessions.URL, reverse.URL, "one endpoint serves both routes")

	session := dialTestWSS(t, sessions.URL+"?path=adb")
	session.PayloadType = websocket.BinaryFrame
	require.NoError(t, websocket.Message.Send(session, []byte("000chost:version")))
	assertControlFrame(t, firstFrame(t, session), controlStatusNew)
	dialReadyNotifier(t, reverse)

	assert.Equal(t, []string{"adb"}, sessions.RoutesDialed(), "the reverse route must not reach the session mock")
	assert.Equal(t, 1, reverse.NotifiersAccepted())
	assert.Empty(t, reverse.Refusals())
}

// startTCPEcho starts a loopback TCP echo server and returns its address. It is
// torn down via t.Cleanup.
func startTCPEcho(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}
