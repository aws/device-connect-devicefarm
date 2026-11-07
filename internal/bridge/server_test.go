package bridge

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// noopLogger discards everything, so tests emit no log noise.
func noopLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// testPolicy disables refreshing (zero Interval), for tests that care only about the byte
// pipe.
func testPolicy() WSRefreshPolicy {
	return WSRefreshPolicy{}
}

// newProxyServer builds an AdbProxyServer on loopback, failing on the constructor error
// (only a nil dialer or device, which no test here intends).
func newProxyServer(t *testing.T, listenPort int, policy WSRefreshPolicy, dialer *WSDialer, device *ActiveDevice) *AdbProxyServer {
	t.Helper()
	server, err := NewAdbProxyServer("127.0.0.1", listenPort, policy, dialer, device, noopLogger())
	require.NoError(t, err)
	return server
}

// deviceFor builds an ActiveDevice already bound to endpoint, standing in for a daemon
// that had a device attached over the control channel.
func deviceFor(endpoint string) *ActiveDevice {
	device := NewActiveDevice()
	device.Set(DeviceInfo{ARN: "arn:test", Endpoint: endpoint, Label: "Test Device - OS 1"})
	return device
}

// startServer runs an AdbProxyServer piping to wsURL on an ephemeral port, returning its
// bound address. Stopped via t.Cleanup.
func startServer(t *testing.T, wsURL string, policy WSRefreshPolicy) (addr string, stopped <-chan error) {
	t.Helper()
	return runProxy(t, deviceFor(wsURL), policy)
}

// runProxy runs an AdbProxyServer in front of device on an ephemeral port, returning its
// bound address. Stopped via t.Cleanup.
func runProxy(t *testing.T, device *ActiveDevice, policy WSRefreshPolicy) (addr string, stopped <-chan error) {
	t.Helper()
	dialer := newTestDialer(5 * time.Second)
	server := newProxyServer(t, 0, policy, dialer, device)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()

	addr = waitForAddr(t, server)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not shut down within timeout")
		}
	})
	return addr, done
}

// startAttachedProxy runs a proxy in front of a device with attachment features. The mock
// OKAYs the daemon's transport select and echoes the rest. Returns the mock to count dials and
// the device to reach the table and budget.
func startAttachedProxy(t *testing.T, policy WSRefreshPolicy) (proxyAddress string,
	mock *testsupport.SessionMockWSS, device *ActiveDevice) {
	t.Helper()
	return startAttachedProxyWithSerial(t, policy, "")
}

// startAttachedProxyWithSerial is startAttachedProxy with the device bound under serial.
func startAttachedProxyWithSerial(t *testing.T, policy WSRefreshPolicy, serial string) (proxyAddress string,
	mock *testsupport.SessionMockWSS, device *ActiveDevice) {
	t.Helper()
	mock = switchReplyMock(t, "OKAY")
	proxyAddress, device = attachProxyTo(t, mock, policy, serial)
	return proxyAddress, mock, device
}

// attachProxyTo runs a proxy in front of a device with attachment features, bound to mock under
// serial.
func attachProxyTo(t *testing.T, mock *testsupport.SessionMockWSS, policy WSRefreshPolicy, serial string) (
	proxyAddress string, device *ActiveDevice) {
	t.Helper()
	device = newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: mock.URL, Label: "Test Device - OS 1", Serial: serial}))
	proxyAddress, _ = runProxy(t, device, policy)
	return proxyAddress, device
}

func waitForAddr(t *testing.T, server *AdbProxyServer) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := server.Addr(); a != nil {
			return a.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server never bound a listen address")
	return ""
}

func TestGivenPortAlreadyInUseWhenListenThenReturnsBindError(t *testing.T) {
	// Listen must surface the bind failure so the daemon fails fast before writing its
	// readiness pidfile, rather than dying once Run tries to bind.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port

	server := newProxyServer(t, port, testPolicy(), newTestDialer(time.Second), NewActiveDevice())

	assert.Error(t, server.Listen(), "binding an in-use port must fail")
}

func TestGivenListenCalledBeforeRunWhenRunThenServesOnThePreBoundListener(t *testing.T) {
	// The daemon binds via Listen (fail fast on a port clash), then Run serves on that
	// same listener.
	wsURL := testsupport.NewSessionEchoWSS(t).URL
	server := newProxyServer(t, 0, testPolicy(), newTestDialer(5*time.Second), deviceFor(wsURL))
	require.NoError(t, server.Listen())
	bound := server.Addr()
	require.NotNil(t, bound, "Addr must be known once Listen has bound")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	assert.Equal(t, bound.String(), waitForAddr(t, server), "the address must not change from Listen to Run")

	client, err := net.Dial("tcp", bound.String())
	require.NoError(t, err)
	defer client.Close()
	payload := []byte("000chost:version")
	writeAll(t, client, payload)
	assert.Equal(t, payload, readN(t, client, len(payload)), "the pre-bound listener must actually serve")
}

func TestGivenActiveConnectionWhenDeviceClearedThenConnectionTornDownImmediately(t *testing.T) {
	wsURL := testsupport.NewSessionEchoWSS(t).URL
	device := deviceFor(wsURL)
	// Refreshing off, so the ONLY thing that can tear the connection down in the test
	// window is the Clear.
	server := newProxyServer(t, 0, testPolicy(), newTestDialer(5*time.Second), device)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	addr := waitForAddr(t, server)
	client, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, client.SetDeadline(time.Now().Add(5*time.Second)))

	// Round-trip a whole request so the connection is piped and registered before the
	// Clear: nothing is dialed until the client has asked for something.
	payload := []byte("000chost:devices")
	writeAll(t, client, payload)
	assert.Equal(t, payload, readN(t, client, len(payload)))

	_, ok := device.Clear() // a device remove
	require.True(t, ok)

	// A hang would surface as the read deadline expiring.
	_, readErr := client.Read(make([]byte, 1))
	assert.ErrorIs(t, readErr, io.EOF, "clearing the device must tear down the in-flight connection")
}

func TestGivenServerPipingToEchoWhenClientSendsBytesThenSameBytesReturn(t *testing.T) {
	wsURL := testsupport.NewSessionEchoWSS(t).URL
	addr, _ := startServer(t, wsURL, testPolicy())

	client, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer client.Close()

	payload := []byte("000chost:devices")
	_, err = client.Write(payload)
	require.NoError(t, err)

	got := make([]byte, len(payload))
	_, err = io.ReadFull(client, got)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestGivenNoDeviceAttachedWhenAClientAsksForAnythingButATrackerThenItIsClosedAndTheServerSurvives(t *testing.T) {
	for name, request := range map[string]string{
		"a host query":       hostVersionRequest,
		"a transport select": "000ehost:tport:any0008shell:ls",
	} {
		t.Run(name, func(t *testing.T) {
			// ActiveDevice with nothing bound.
			addr, _ := runProxy(t, NewActiveDevice(), testPolicy())

			refused := dialClient(t, addr)
			writeAll(t, refused, []byte(request))

			_, readErr := refused.Read(make([]byte, 1))
			assert.ErrorIs(t, readErr, io.EOF, "client must be closed with no reply when no device is attached")
			second, err := net.Dial("tcp", addr)
			require.NoError(t, err)
			second.Close()
		})
	}
}

func TestGivenDialFailsForClientWhenClientConnectsThenClientConnectionClosedAndServerSurvives(t *testing.T) {
	// Unreachable endpoint, so every dial fails.
	dialer := newTestDialer(500 * time.Millisecond)
	server := newProxyServer(t, 0, testPolicy(), dialer, deviceFor("wss://127.0.0.1:1/adb"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	addr := waitForAddr(t, server)

	failed, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer failed.Close()
	failed.SetReadDeadline(time.Now().Add(5 * time.Second))
	// The dial is attempted once the client has asked for something.
	writeAll(t, failed, []byte("000chost:devices"))
	_, readErr := failed.Read(make([]byte, 1))
	assert.ErrorIs(t, readErr, io.EOF, "client whose dial failed should be closed")

	second, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	second.Close()
}

func TestGivenPipeEndsWithErrorWhenClientConnectsThenErrorSwallowedAndServerSurvives(t *testing.T) {
	// Handshake first, then an over-cap message, so pipeConn returns ErrFrameTooLarge:
	// handleConn's non-benign branch.
	oversized := make([]byte, maxInboundMessageBytes+1)
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(ws, testsupport.FrameNewSession(testSessionUUID))
		websocket.Message.Send(ws, oversized)
		io.Copy(io.Discard, ws)
	})
	addr, done := startServer(t, wsURL, testPolicy())

	first, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer first.Close()
	first.SetReadDeadline(time.Now().Add(5 * time.Second))
	// A request first: the websocket that sends the oversized frame is dialed only once
	// the client has asked for something.
	writeAll(t, first, []byte("000chost:devices"))
	_, _ = io.Copy(io.Discard, first) // the oversized frame tears the pipe down

	second, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	second.Close()
	select {
	case runErr := <-done:
		t.Fatalf("server exited after a faulted connection: %v", runErr)
	default:
	}
}

func TestGivenActiveConnectionWhenServerShutdownRequestedThenRunReturnsAfterDrain(t *testing.T) {
	wsURL := testsupport.NewSessionEchoWSS(t).URL
	dialer := newTestDialer(5 * time.Second)
	server := newProxyServer(t, 0, testPolicy(), dialer, deviceFor(wsURL))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()

	addr := waitForAddr(t, server)
	client, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	defer client.Close()

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "graceful shutdown returns nil")
	case <-time.After(5 * time.Second):
		t.Fatal("server did not drain and return after shutdown")
	}
}

func TestGivenNilDialerOrDeviceWhenConstructedThenReturnsError(t *testing.T) {
	dialer := newTestDialer(time.Second)
	device := NewActiveDevice()

	_, dialerErr := NewAdbProxyServer("127.0.0.1", 0, testPolicy(), nil, device, noopLogger())
	assert.Error(t, dialerErr, "a nil dialer must be rejected")

	_, deviceErr := NewAdbProxyServer("127.0.0.1", 0, testPolicy(), dialer, nil, noopLogger())
	assert.Error(t, deviceErr, "a nil device must be rejected")
}

// tooManyConnectionsFail is the framed FAIL a client reads at a spent budget of testMaxDeviceStreams.
const tooManyConnectionsFail = "FAIL0022too many adb connections (limit 2)"

// spendBudget takes every slot of the attachment's budget, as live streams would.
func spendBudget(t *testing.T, device *ActiveDevice) {
	t.Helper()
	budget := attachedFeatures(t, device).budget
	for range testMaxDeviceStreams {
		require.True(t, budget.admit())
	}
}

// relayedHostRequest is a whole host request the daemon relays rather than answers itself.
const relayedHostRequest = "000ehost:get-state"

// openPipedConnection opens one local adb connection and waits for the mock to echo its
// request, so it holds a slot on return.
func openPipedConnection(t *testing.T, proxyAddress string) net.Conn {
	t.Helper()
	client := dialClient(t, proxyAddress)
	request := []byte(relayedHostRequest)
	writeAll(t, client, request)
	require.Equal(t, request, readN(t, client, len(request)), "an admitted connection is piped to the session")
	return client
}

// assertRefusedOverBudget reads the budget's FAIL off client and the close behind it.
func assertRefusedOverBudget(t *testing.T, client net.Conn) {
	t.Helper()
	assert.Equal(t, tooManyConnectionsFail, string(readN(t, client, len(tooManyConnectionsFail))),
		"the client reads the limit as its request's FAIL")
	_, err := client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the daemon closes a connection it refused")
}

func TestGivenASpentBudgetWhenALocalAdbClientConnectsThenItGetsTheLimitFailAndNothingIsDialed(t *testing.T) {
	// The one-request shape: a host service with no select.
	proxyAddress, mock, device := startAttachedProxy(t, testPolicy())
	spendBudget(t, device)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte(relayedHostRequest))

	assertRefusedOverBudget(t, client)
	assertNoWebsocketDialed(t, mock)
}

func TestGivenASpentBudgetWhenTheSelectWasAnsweredThenTheFailAnswersTheServiceRequest(t *testing.T) {
	// The two-request shape: the select is answered at the seam, so the FAIL must answer the
	// service behind it, whose status word the client is waiting for.
	proxyAddress, mock, device := startAttachedProxy(t, testPolicy())
	spendBudget(t, device)
	client := dialClient(t, proxyAddress)

	selectAnyTransport(t, client)
	writeAll(t, client, []byte("0006shell:"))

	assertRefusedOverBudget(t, client)
	assertNoWebsocketDialed(t, mock)
}

func TestGivenASpentBudgetWhenAForwardRequestArrivesThenItIsStillAnsweredLocally(t *testing.T) {
	// A forward request dials nothing, so it spends no slot and a full budget cannot refuse it.
	proxyAddress, mock, device := startAttachedProxy(t, testPolicy())
	spendBudget(t, device)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte(installForward))

	assert.NotZero(t, readForwardSuccessPort(t, client), "the forward family is answered ahead of the budget")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenASpentBudgetWhenTheOpeningIsNotARequestThenZeroBytesAndNoDial(t *testing.T) {
	// No reply shape is known for bytes with no length prefix, so the refusal is the close.
	proxyAddress, mock, device := startAttachedProxy(t, testPolicy())
	spendBudget(t, device)
	client := dialClient(t, proxyAddress)

	writeAll(t, client, []byte("hello world"))

	assert.Empty(t, drainClosed(t, client), "a refused non-request opener must get zero bytes")
	assertNoWebsocketDialed(t, mock)
}

func TestGivenLocalConnectionsFillingTheBudgetWhenOneEndsThenItsSlotAdmitsTheNext(t *testing.T) {
	proxyAddress, mock, device := startAttachedProxy(t, testPolicy())
	budget := attachedFeatures(t, device).budget
	first := openPipedConnection(t, proxyAddress)
	for range testMaxDeviceStreams - 1 {
		openPipedConnection(t, proxyAddress)
	}
	refused := dialClient(t, proxyAddress)
	writeAll(t, refused, []byte(relayedHostRequest))
	assertRefusedOverBudget(t, refused)
	require.Equal(t, testMaxDeviceStreams, mock.ConnectionsAccepted(), "the refused connection must not dial")

	require.NoError(t, first.Close())
	waitFor(t, func() bool { return budget.inUse.Load() == testMaxDeviceStreams-1 },
		"the ended connection never freed its slot")

	openPipedConnection(t, proxyAddress)
}

func TestGivenLocalConnectionsFillingTheBudgetWhenAForwardedConnectionArrivesThenItClosesWithZeroBytes(t *testing.T) {
	// One budget for both: local connections can take every slot, and a forwarded connection
	// then gets the opener's zero-byte close, not a dial that would spend the server's headroom.
	proxyAddress, mock, device := startAttachedProxy(t, testPolicy())
	for range testMaxDeviceStreams {
		openPipedConnection(t, proxyAddress)
	}

	appConn := dialRule(t, installRule(t, attachedFeatures(t, device), "tcp:8080"))

	assert.Empty(t, drainClosed(t, appConn), "a forwarded connection above the shared budget must get zero bytes")
	assert.Equal(t, testMaxDeviceStreams, mock.ConnectionsAccepted(), "a refused forwarded connection must not dial")
}

func TestGivenADialThatFailsWhenTheConnectionEndsThenItsSlotReturns(t *testing.T) {
	// Unreachable endpoint, so the dial fails after the slot is taken. The client's EOF comes
	// from the deferred close behind the release, so the slot is back once it reads it.
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: "wss://127.0.0.1:1/adb"}))
	budget := attachedFeatures(t, device).budget
	proxyAddress, _ := runProxy(t, device, testPolicy())
	failed := dialClient(t, proxyAddress)

	writeAll(t, failed, []byte(relayedHostRequest))

	assert.Empty(t, drainClosed(t, failed), "a failed dial closes the client with no bytes")
	assert.Zero(t, budget.inUse.Load(), "a failed dial must free its slot")
}

func TestGivenALocalConnectionThatSwapsThenTheBudgetStaysFlat(t *testing.T) {
	// A swap dials inside the session, never through the accept loop, so it takes no second slot.
	proxyAddress, mock, device := startAttachedProxy(t, fastPolicy())
	budget := attachedFeatures(t, device).budget
	client := openPipedConnection(t, proxyAddress)

	waitFor(t, func() bool { return mock.Reattaches() >= 1 }, "the connection never swapped")

	assert.Equal(t, int64(1), budget.inUse.Load(), "a swap must not spend a second slot")
	require.NoError(t, client.Close())
	waitFor(t, func() bool { return budget.inUse.Load() == 0 }, "the ended connection never freed its slot")
}

func TestGivenRepeatedRefusalsWhenLoggedThenOneWarnNamesTheLimit(t *testing.T) {
	// The seam is asked directly, so the log is read without a proxy in front of it.
	_, forwarding := forwardingFor(t)
	logs := &strings.Builder{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	clientOpening := opening{service: "host:devices"}

	for range 2 {
		client := newScriptedConn("")
		refuseOverBudget(client, clientOpening, forwarding, logger)
		assert.Equal(t, tooManyConnectionsFail, client.replies.String(), "every refusal gets the FAIL")
	}

	warn := `level=WARN msg="refused a local adb connection: every device-stream slot is taken"`
	assert.Equal(t, 1, strings.Count(logs.String(), warn), "refusals inside one interval must log once")
	assert.Contains(t, logs.String(), "maxDeviceStreams=2", "the Warn must name the limit")
}

// The proxy server must mint no session a proxied connection did not ask for: each spends a
// per-host slot that a refresh's transient second socket also needs. No attachment features
// here, so no host reply fetch or parked socket runs.

func TestGivenAttachedDeviceWithNoProxiedConnectionWhenIdleThenTheBridgeMintsNoSession(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	addr, _ := startServer(t, mock.URL, testPolicy())

	// A device is attached and the listener is up, but no local adb client ever connects.
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
