package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// testReverseToken is one parked connection's token. UUIDv4 because a REVERSE_OPEN carrying
// anything else is refused before a connector is ever opened.
const testReverseToken = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

// localTarget stands in for the customer's own server on a loopback port.
type localTarget struct {
	port     int
	listener *net.TCPListener
}

// startLocalTarget listens on an ephemeral IPv4 loopback port until the test ends.
func startLocalTarget(t *testing.T) *localTarget {
	t.Helper()
	return startLocalTargetOn(t, "127.0.0.1", 0)
}

// startLocalTargetOn listens on host:port until the test ends; port 0 takes a free one. A failed
// ::1 bind skips the test: the host may have no IPv6 loopback, or the port may be taken there.
func startLocalTargetOn(t *testing.T, host string, port int) *localTarget {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil && host == "::1" {
		t.Skipf("cannot listen on [::1]:%d here: %v", port, err)
	}
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	tcpListener := listener.(*net.TCPListener)
	return &localTarget{port: tcpListener.Addr().(*net.TCPAddr).Port, listener: tcpListener}
}

// releasedPort returns a free IPv4 loopback port with nothing listening on it.
func releasedPort(t *testing.T) int {
	t.Helper()
	target := startLocalTarget(t)
	require.NoError(t, target.listener.Close())
	return target.port
}

// awaitAccepted returns the connection the connector dialed, failing the test if it never did.
func (target *localTarget) awaitAccepted(t *testing.T) net.Conn {
	t.Helper()
	require.NoError(t, target.listener.SetDeadline(time.Now().Add(testWait)))
	conn, err := target.listener.Accept()
	require.NoError(t, err, "the connector never dialed the customer's local port")
	return conn
}

// requireNoDial requires that nothing reached the target inside a settle window. A dial the
// listener never accepted still completes into the backlog, so it is caught here too.
func (target *localTarget) requireNoDial(t *testing.T) {
	t.Helper()
	require.NoError(t, target.listener.SetDeadline(time.Now().Add(notifierSettle)))
	_, err := target.listener.Accept()
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "nothing may dial the customer's local port")
}

// openConnectorTo opens one connector against mock for a bound device, on a goroutine of its own
// as an announcement does, and returns the channel its outcome arrives on.
func openConnectorTo(t *testing.T, mock *testsupport.ReverseMockWSS, customerPort int,
	policy WSRefreshPolicy) <-chan error {
	t.Helper()
	device := deviceAt(t, mock.URL)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &featureConfig{dialer: newTestDialer(testWait), policy: policy, logger: noopLogger()}
	ended := make(chan error, 1)
	go func() {
		ended <- openConnector(ctx, cfg, device, testDeviceARN, testReverseToken, customerPort)
	}()
	return ended
}

// openConnectorOnce opens one connector against endpoint and returns its outcome. An empty endpoint
// leaves the device detached, the one failure that reaches no network at all.
func openConnectorOnce(t *testing.T, endpoint string, customerPort int) error {
	t.Helper()
	device := NewActiveDevice()
	if endpoint != "" {
		require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "test-device"}))
	}
	cfg := &featureConfig{dialer: newTestDialer(testWait), policy: testPolicy(), logger: noopLogger()}
	return openConnector(context.Background(), cfg, device, testDeviceARN, testReverseToken, customerPort)
}

// assertClosed asserts conn was closed by its peer rather than left open with nothing behind it.
// The deadline only bounds the wait, so its error is not the assertion: a pipe whose peer has
// already closed refuses to take one and still reads EOF.
func assertClosed(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(testWait))
	_, err := conn.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, what)
}

func TestGivenAnAnnouncedOpenWhenTheConnectorIsDialedThenTheHelloCarriesTheTokenAsItsFirstFrame(t *testing.T) {
	mock := serverReverseMock(t)
	mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	_, supervisor, _ := runSupervisor(t, mock, notifierParams(parkedKeepAlive, notifierPoll), target.port)
	require.Eventually(t, supervisor.notifierReady, testWait, notifierPoll, "no notifier to announce on")

	require.True(t, mock.Announce(testReverseToken, target.port))

	// The mock redeems a token only from a first-frame CONNECTOR_HELLO and refuses everything else
	// by reason, so a redemption with no refusals proves it.
	require.Eventually(t, func() bool { return mock.ConnectorsAccepted() == 1 }, testWait,
		notifierPoll, "the announced token must be redeemed by a connector")
	assert.Empty(t, mock.Refusals())
}

func TestGivenAListeningLocalTargetWhenAConnectorOpensThenBytesRoundTripBothWays(t *testing.T) {
	mock := serverReverseMock(t)
	deviceConn := mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	openConnectorTo(t, mock, target.port, testPolicy())

	targetConn := target.awaitAccepted(t)
	go io.Copy(targetConn, targetConn)

	payload := testsupport.ExpectedSequence(64 << 10)
	echoed := make([]byte, len(payload))
	read := make(chan error, 1)
	// Read while writing: the device end is unbuffered, so both directions must run at once.
	go func() { _, err := io.ReadFull(deviceConn, echoed); read <- err }()

	// Bound the write, whose own error carries whether the splice happened.
	_ = deviceConn.SetWriteDeadline(time.Now().Add(testWait))
	_, err := deviceConn.Write(payload)
	require.NoError(t, err, "the parked connection must be spliced to the connector")
	require.NoError(t, await(t, read, "the echoed bytes to come back"))
	assert.Equal(t, payload, echoed)
}

func TestGivenNothingListeningLocallyWhenAConnectorOpensThenItIsRefusedAndTheWebsocketCloses(t *testing.T) {
	mock := serverReverseMock(t)
	deviceConn := mock.Connect(testReverseToken)
	port := releasedPort(t)

	err := await(t, openConnectorTo(t, mock, port, testPolicy()),
		"the connector to end")

	require.Error(t, err, "a connector with nothing to splice to must fail")
	assert.Equal(t, 1, mock.ConnectorsAccepted(), "the hello is admitted before the local dial")
	assertClosed(t, deviceConn, "the device end must learn the splice failed")
}

func TestGivenATargetOnTheIPv6LoopbackOnlyWhenAConnectorOpensThenTheDialFallsBackToIt(t *testing.T) {
	mock := serverReverseMock(t)
	deviceConn := mock.Connect(testReverseToken)
	port := releasedPort(t)
	target := startLocalTargetOn(t, "::1", port)
	openConnectorTo(t, mock, port, testPolicy())

	targetConn := target.awaitAccepted(t)
	go io.Copy(targetConn, targetConn)

	payload := []byte("over the IPv6 loopback")
	_ = deviceConn.SetDeadline(time.Now().Add(testWait))
	_, err := deviceConn.Write(payload)
	require.NoError(t, err)
	echoed := make([]byte, len(payload))
	_, err = io.ReadFull(deviceConn, echoed)
	require.NoError(t, err, "the splice must reach the ::1 server after 127.0.0.1 refused")
	assert.Equal(t, payload, echoed)
}

func TestGivenATargetOnBothLoopbacksWhenAConnectorOpensThenTheIPv4OneIsDialed(t *testing.T) {
	mock := serverReverseMock(t)
	mock.Connect(testReverseToken)
	ipv4Target := startLocalTarget(t)
	ipv6Target := startLocalTargetOn(t, "::1", ipv4Target.port)

	openConnectorTo(t, mock, ipv4Target.port, testPolicy())

	ipv4Target.awaitAccepted(t)
	ipv6Target.requireNoDial(t)
}

func TestGivenNothingOnEitherLoopbackWhenAConnectorOpensThenBothRefusalsAreReported(t *testing.T) {
	mock := serverReverseMock(t)
	deviceConn := mock.Connect(testReverseToken)
	port := releasedPort(t)

	err := await(t, openConnectorTo(t, mock, port, testPolicy()), "the connector to end")

	require.ErrorIs(t, err, syscall.ECONNREFUSED, "the IPv4 refusal is the wrapped error")
	var firstAttempt *net.OpError
	require.True(t, errors.As(err, &firstAttempt))
	assert.Equal(t, fmt.Sprintf("127.0.0.1:%d", port), firstAttempt.Addr.String(), "the IPv4 attempt leads")
	assert.ErrorContains(t, err, fmt.Sprintf("[::1]:%d", port), "the IPv6 attempt is named too")
	assert.Equal(t, 1, mock.ConnectorsAccepted(), "the hello is admitted before the local dials")
	assertClosed(t, deviceConn, "the device end must learn the splice failed")
}

// fakeCustomerPort is the port the recording dialer tests name; nothing is dialed for real.
const fakeCustomerPort = 9090

// recordingDialer is a portDialer that fails the addresses in failures, connects the rest through
// a pipe, and records every address in order.
type recordingDialer struct {
	failures map[string]error
	dialed   []string
}

func (dialer *recordingDialer) dial(_ context.Context, _ string, address string) (net.Conn, error) {
	dialer.dialed = append(dialer.dialed, address)
	if err, failed := dialer.failures[address]; failed {
		return nil, err
	}
	clientEnd, serverEnd := net.Pipe()
	_ = serverEnd.Close()
	return clientEnd, nil
}

// connectFailure is the error chain a failed loopback connect returns on Linux and macOS.
func connectFailure(errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: errno}}
}

func TestGivenARefusedIPv4DialWhenTheCustomerPortIsDialedThenTheIPv6LoopbackIsTriedNext(t *testing.T) {
	ipv4Address := net.JoinHostPort("127.0.0.1", strconv.Itoa(fakeCustomerPort))
	dialer := &recordingDialer{failures: map[string]error{ipv4Address: connectFailure(syscall.ECONNREFUSED)}}

	conn, err := dialCustomerPort(context.Background(), dialer.dial, fakeCustomerPort)

	require.NoError(t, err)
	require.NoError(t, conn.Close())
	assert.Equal(t, []string{ipv4Address, net.JoinHostPort("::1", strconv.Itoa(fakeCustomerPort))}, dialer.dialed)
}

func TestGivenAnIPv4DialThatFailsWithoutARefusalWhenTheCustomerPortIsDialedThenTheIPv6LoopbackIsNotTried(t *testing.T) {
	ipv4Address := net.JoinHostPort("127.0.0.1", strconv.Itoa(fakeCustomerPort))
	failures := map[string]error{
		"timed out":        &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded},
		"host unreachable": connectFailure(syscall.EHOSTUNREACH),
		"no IPv4 address":  connectFailure(syscall.EADDRNOTAVAIL),
	}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			dialer := &recordingDialer{failures: map[string]error{ipv4Address: failure}}

			conn, err := dialCustomerPort(context.Background(), dialer.dial, fakeCustomerPort)

			require.Nil(t, conn)
			require.ErrorIs(t, err, failure)
			assert.Equal(t, []string{ipv4Address}, dialer.dialed, "only a refused connect earns the second dial")
		})
	}
}

func TestGivenARefusedConnectorHelloWhenDialedThenNoLocalDialIsAttemptedAndTheReasonCarriesNoToken(t *testing.T) {
	mock := serverReverseMock(t)
	mock.Connect(testReverseToken)
	// A server writes its own refusal text and could echo the token back in it.
	mock.RefuseConnector(fmt.Sprintf("the connector pool is at its cap for token %s", testReverseToken))
	target := startLocalTarget(t)

	err := await(t, openConnectorTo(t, mock, target.port, testPolicy()),
		"the connector to end")

	require.ErrorContains(t, err, "the connector pool is at its cap", "the reason must surface")
	assert.NotContains(t, err.Error(), testReverseToken, "a live token may not travel to a log")
	assert.Equal(t, 1, mock.ConnectorsRefused())
	target.requireNoDial(t)
}

func TestGivenAnAlreadyRedeemedTokenWhenAConnectorRedeemsItAgainThenItIsRefused(t *testing.T) {
	mock := serverReverseMock(t)
	mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	openConnectorTo(t, mock, target.port, testPolicy())
	target.awaitAccepted(t)

	err := await(t, openConnectorTo(t, mock, target.port, testPolicy()),
		"the second connector to end")

	require.Error(t, err, "one token redeems exactly one parked connection")
	assert.Equal(t, 1, mock.ConnectorsAccepted())
	assert.Equal(t, 1, mock.ConnectorsRefused())
	target.requireNoDial(t)
}

func TestGivenAnOpenConnectorWhenItsDeviceEndClosesThenTheConnectorEndsWithoutAReattach(t *testing.T) {
	mock := serverReverseMock(t)
	deviceConn := mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	ended := openConnectorTo(t, mock, target.port, testPolicy())
	targetConn := target.awaitAccepted(t)

	// Closing the device end ends the connector's session. The server closes the websocket with
	// the session-ended code.
	require.NoError(t, deviceConn.Close())

	_ = await(t, ended, "the connector to end")
	assertClosed(t, targetConn, "the local target must see the splice close")
	assert.NotContains(t, mock.Refusals(), "unknown or expired session", "an ended session must not be resumed")
}

func TestGivenARefusedReattachOnAConnectorWhenRefreshRunsThenTheConnectionEndsInsteadOfMintingAFreshSession(t *testing.T) {
	mock := serverReverseMock(t)
	mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	// The mock keeps no session across websockets, so the first refresh is refused as gone.
	ended := openConnectorTo(t, mock, target.port, fastPolicy())
	targetConn := target.awaitAccepted(t)

	// A fresh session would have kept the splice open, so the end is the property.
	_ = await(t, ended, "the connector to end")
	assertClosed(t, targetConn, "the local target must see the splice close")
	assert.Contains(t, mock.Refusals(), "unknown or expired session")
}

func TestGivenADetachedDeviceWhenAConnectorOpensThenTheAnnouncementEndsAsAClosedSession(t *testing.T) {
	target := startLocalTarget(t)

	err := openConnectorOnce(t, "", target.port)

	require.ErrorIs(t, err, errAdbConnClosed, "an announcement for a device that detached is moot")
}

func TestGivenAnUnreachableEndpointWhenAConnectorOpensThenNothingIsDialedLocally(t *testing.T) {
	target := startLocalTarget(t)

	// Port 1 on loopback: nothing serves it, so the websocket dial fails before any hello.
	err := openConnectorOnce(t, "wss://127.0.0.1:1", target.port)

	require.ErrorContains(t, err, "dial the connector websocket")
	target.requireNoDial(t)
}
