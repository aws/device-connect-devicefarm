package bridge

import (
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// dialMock connects to a mock server with the inbound per-message cap armed, as the
// production dialer does.
func dialMock(t *testing.T, wsURL string) WSConn {
	t.Helper()
	ws, err := newTestDialer(5*time.Second).Dial(context.Background(), wsURL, RouteAdb)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })
	return ws
}

// localTCPPair returns both ends of a loopback TCP connection: appSide stands in for the
// local adb client, pipeSide goes to pipeConn. A real socket, not net.Pipe, for
// production's close semantics: closing your own end during a pending Read yields
// net.ErrClosed on TCP, io.ErrClosedPipe on net.Pipe. Both closed via t.Cleanup.
func localTCPPair(t *testing.T) (appSide, pipeSide net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()

	appSide, err = net.Dial("tcp", listener.Addr().String())
	require.NoError(t, err)
	pipeSide = <-accepted
	require.NotNil(t, pipeSide, "listener failed to accept the loopback connection")

	t.Cleanup(func() {
		appSide.Close()
		pipeSide.Close()
	})
	return appSide, pipeSide
}

// runPipeWithEcho runs pipeConn between localSide and a client websocket on an echo
// server, returning a channel carrying pipeConn's error.
func runPipeWithEcho(t *testing.T, ctx context.Context, localSide net.Conn) <-chan error {
	t.Helper()
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		io.Copy(ws, ws)
	})
	ws := dialMock(t, wsURL)

	errCh := make(chan error, 1)
	go func() { errCh <- pipeConn(ctx, localSide, ws, nil) }()
	return errCh
}

func TestGivenBytesWrittenLocallyWhenPipedThroughEchoServerThenSameBytesReturn(t *testing.T) {
	appSide, pipeSide := localTCPPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errCh := runPipeWithEcho(t, ctx, pipeSide)

	payload := []byte("000chost:version")
	writeAll(t, appSide, payload)

	got := readN(t, appSide, len(payload))
	assert.Equal(t, payload, got)

	appSide.Close()
	assertPipeReturns(t, errCh)
}

func TestGivenMultiMegabyteTransferWhenPipedThroughEchoServerThenCapIsPerMessageNotPerTransfer(t *testing.T) {
	appSide, pipeSide := localTCPPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	errCh := runPipeWithEcho(t, ctx, pipeSide)

	// 5 MiB is well over the 1 MiB per-message cap, but the outbound pump splits it into
	// <=32 KiB messages.
	payload := make([]byte, 5<<20)
	_, err := rand.Read(payload)
	require.NoError(t, err)

	// Background write: appSide must be read concurrently for 5 MiB to drain. The error
	// crosses a channel because require.FailNow only kills its own goroutine.
	writeErr := make(chan error, 1)
	go func() { _, err := appSide.Write(payload); writeErr <- err }()

	got := readN(t, appSide, len(payload))
	assert.Equal(t, payload, got)
	require.NoError(t, <-writeErr, "the full payload must write without error")

	appSide.Close()
	assertPipeReturns(t, errCh)
}

func TestGivenInboundMessageOverCapWhenPipedThenPipeReturnsFrameTooLarge(t *testing.T) {
	oversized := make([]byte, maxInboundMessageBytes+1)
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(ws, oversized)
		io.Copy(io.Discard, ws)
	})
	ws := dialMock(t, wsURL)

	_, pipeSide := localTCPPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- pipeConn(ctx, pipeSide, ws, nil) }()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.ErrorIs(t, err, websocket.ErrFrameTooLarge)
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not tear down after oversized inbound message")
	}
}

func TestGivenRemoteClosesWebsocketWhenPipedThenLocalConnectionTearsDownCleanly(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.Close() // close immediately, sending nothing
	})
	ws := dialMock(t, wsURL)

	appSide, pipeSide := localTCPPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- pipeConn(ctx, pipeSide, ws, nil) }()

	_, err := appSide.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
	assertPipeReturns(t, errCh)
}

func TestGivenContextCancelledWhenNoActivityThenPipeTearsDownWithoutError(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		io.Copy(io.Discard, ws) // stay open and idle until the client tears down
	})
	ws := dialMock(t, wsURL)
	_, pipeSide := localTCPPair(t)

	// A short deadline stands in for SIGTERM or a device remove; the proxied connection
	// itself carries no deadline (see AdbProxyServer.handleConn).
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- pipeConn(ctx, pipeSide, ws, nil) }()

	select {
	case err := <-errCh:
		assert.NoError(t, err, "a context-driven teardown is a clean shutdown")
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not tear down after the context ended")
	}
}

func TestGivenWebsocketClosedWhenPumpOutboundReadsLocalBytesThenReturnsSendError(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		io.Copy(io.Discard, ws)
	})
	ws := dialMock(t, wsURL)
	appSide, pipeSide := localTCPPair(t)

	// The read succeeds; the send onto the closed ws is the error branch.
	require.NoError(t, ws.Close())
	writeAll(t, appSide, []byte("000chost:devices"))

	err := pumpOutbound(pipeSide, ws)

	require.Error(t, err, "sending onto a closed websocket must return an error")
}

func TestGivenLocalConnClosedWhenPumpInboundReceivesMessageThenReturnsWriteError(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(ws, []byte("OKAY"))
		io.Copy(io.Discard, ws)
	})
	ws := dialMock(t, wsURL)
	_, pipeSide := localTCPPair(t)

	// The Receive succeeds; the Write onto the closed conn is the error branch.
	require.NoError(t, pipeSide.Close())

	err := pumpInbound(ws, pipeSide)

	require.Error(t, err, "writing a received message to a closed local conn must return an error")
}

func TestGivenAuxGoroutineWhenPipeReturnsThenAuxIsReleased(t *testing.T) {
	appSide, pipeSide := localTCPPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		io.Copy(ws, ws)
	})
	ws := dialMock(t, wsURL)

	auxReturned := make(chan struct{})
	aux := func(done <-chan struct{}) {
		<-done
		close(auxReturned)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- pipeConn(ctx, pipeSide, ws, aux) }()

	appSide.Close()
	assertPipeReturns(t, errCh)

	select {
	case <-auxReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("aux goroutine was not released when the pipe returned")
	}
}

func writeAll(t *testing.T, conn net.Conn, data []byte) {
	t.Helper()
	_, err := conn.Write(data)
	require.NoError(t, err)
}

func readN(t *testing.T, conn net.Conn, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := io.ReadFull(conn, buf)
	require.NoError(t, err)
	return buf
}

func assertPipeReturns(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		assert.True(t, isBenignCloseError(err), "expected a clean shutdown, got %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("pipe did not return after teardown")
	}
}
