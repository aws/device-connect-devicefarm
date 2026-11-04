package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// otherSessionUUID is a second canonical UUIDv4, for an ack that names the wrong session.
const otherSessionUUID = "33333333-4444-4555-8666-777777777777"

// silentProbeTimeout bounds a read from a server that never answers. It is also the dial
// timeout, so it must leave room for a loopback TLS handshake.
const silentProbeTimeout = 500 * time.Millisecond

// unreachedTimeout is a dial and read bound far past every test wait, so only a cancel can end
// a read under it.
const unreachedTimeout = time.Minute

// eachProbe runs one probe, for the tests that treat every probe alike; acked is the server's
// answer that completes its exchange.
var eachProbe = []struct {
	name  string
	run   func(ctx context.Context, dialer *WSDialer, endpoint string) error
	acked [][]byte
}{
	{"session", func(ctx context.Context, dialer *WSDialer, endpoint string) error {
		_, err := dialer.ProbeSession(ctx, endpoint)
		return err
	}, [][]byte{testsupport.FrameNewSession(testSessionUUID), []byte("OKAY")}},
	{"reattach", func(ctx context.Context, dialer *WSDialer, endpoint string) error {
		return dialer.ProbeReattach(ctx, endpoint, testSessionUUID)
	}, [][]byte{testsupport.FrameReattach(testSessionUUID)}},
	{"notifier", func(ctx context.Context, dialer *WSDialer, endpoint string) error {
		return dialer.ProbeNotifier(ctx, endpoint)
	}, [][]byte{testsupport.FrameEmptyArm("notifierReady")}},
}

func TestGivenAServerThatAcksWhenEachProbeSucceedsThenTheSocketCloses(t *testing.T) {
	for _, probe := range eachProbe {
		t.Run(probe.name, func(t *testing.T) {
			server := newNotifierServer(t, probe.acked...)

			require.NoError(t, probe.run(context.Background(), newTestDialer(testWait), server.url))

			await(t, server.closed, "the probe must release its websocket on success")
		})
	}
}

func TestGivenASessionServerWhenTheSessionIsProbedThenTheMintedUUIDIsReturnedAndTheSelectReachedIt(t *testing.T) {
	mock := switchReplyMock(t, "OKAY")

	sessionUUID, err := newTestDialer(testWait).ProbeSession(boundedContext(t), mock.URL)

	require.NoError(t, err)
	require.Len(t, mock.Sessions(), 1)
	assert.Equal(t, mock.Sessions()[0].UUID, sessionUUID, "the probe reports the session the server minted")
	assert.Equal(t, canonicalSelect, string(soleSessionInbound(mock)), "the select is the session's first frame")
	assert.Equal(t, []string{"adb"}, mock.RoutesDialed())
}

func TestGivenABadSessionAnswerWhenTheSessionIsProbedThenTheErrorNamesIt(t *testing.T) {
	cases := []struct {
		name      string
		endpoint  string
		wantError string
		wantIs    error
	}{
		{"a refused session", probeReplyServer(t, testsupport.FrameError("failed to connect to ADB server")),
			"the remote access service refused the session: failed to connect to ADB server", ErrSessionRefused},
		{"an arm that is not newSession", probeReplyServer(t, testsupport.FrameReattach(testSessionUUID)),
			"no session was minted: unexpected control frame reattach", nil},
		{"raw adb bytes before any control frame", probeReplyServer(t, []byte("OKAY")), errNotControlFrame.Error(), errNotControlFrame},
		{"a refused transport select", switchReplyMock(t, "FAIL000edevice offline").URL,
			"the device adb server refused the transport select: device offline", nil},
		{"no status word after the control frame", probeReplyServer(t, controlFrameNew()),
			"read the transport select's status word: " + errReceiveTimeout.Error(), errReceiveTimeout},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sessionUUID, err := newTestDialer(silentProbeTimeout).ProbeSession(context.Background(), testCase.endpoint)

			assert.Empty(t, sessionUUID)
			assert.ErrorContains(t, err, testCase.wantError)
			if testCase.wantIs != nil {
				assert.ErrorIs(t, err, testCase.wantIs)
			}
		})
	}
}

func TestGivenAProbedSessionWhenReattachedThenTheServerAcksThatSession(t *testing.T) {
	mock := switchReplyMock(t, "OKAY")
	dialer := newTestDialer(testWait)
	sessionUUID, err := dialer.ProbeSession(boundedContext(t), mock.URL)
	require.NoError(t, err)

	err = dialer.ProbeReattach(boundedContext(t), mock.URL, sessionUUID)

	require.NoError(t, err)
	assert.Equal(t, 1, mock.Reattaches(), "the probe reattached the session the first probe minted")
	assert.Equal(t, 1, mock.SessionsOpened(), "a reattach mints no session")
}

func TestGivenAMalformedSessionUUIDWhenReattachedThenNothingIsDialed(t *testing.T) {
	cases := []struct {
		name        string
		sessionUUID string
	}{
		{"far too short", "not-a-session-uuid"},
		{"one character too short", testSessionUUID[:35]},
		{"one character too long", testSessionUUID + "0"},
		{"version nibble 3, not 4", "11111111-2222-3333-8444-555555555555"},
		{"variant nibble c, not 8 to b", "11111111-2222-4333-c444-555555555555"},
		{"right length, wrong shape", "not-a-uuid-but-exactly-36-chars-long"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mock := testsupport.NewSessionEchoWSS(t)

			err := newTestDialer(testWait).ProbeReattach(context.Background(), mock.URL, testCase.sessionUUID)

			assert.ErrorContains(t, err, fmt.Sprintf("%q is not a session uuid", testCase.sessionUUID))
			assert.Zero(t, mock.ConnectionsAccepted(), "a malformed uuid must not reach the server")
		})
	}
}

func TestGivenACanonicalUUIDv4InEitherCaseWhenReattachedThenTheServerReadsAReconnect(t *testing.T) {
	cases := []struct {
		name        string
		sessionUUID string
	}{
		{"lowercase", "01234567-89ab-4cde-bf01-23456789abcd"},
		{"uppercase", "01234567-89AB-4CDE-BF01-23456789ABCD"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mock := testsupport.NewSessionEchoWSS(t)

			err := newTestDialer(testWait).ProbeReattach(context.Background(), mock.URL, testCase.sessionUUID)

			// Empty mock: refuses a reconnect, mints a session for any other first frame.
			assert.ErrorIs(t, err, ErrSessionGone)
			assert.ErrorContains(t, err, "unknown or expired session")
			assert.Equal(t, 1, mock.ConnectionsAccepted(), "a canonical uuid must be dialed")
			assert.Zero(t, mock.SessionsOpened(), "a reconnect must not mint a session")
		})
	}
}

func TestGivenTheServerRefusesTheReattachWhenReattachedThenTheErrorSaysTheSessionIsGone(t *testing.T) {
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RejectReattach: true})

	err := newTestDialer(testWait).ProbeReattach(context.Background(), mock.URL, testSessionUUID)

	assert.ErrorIs(t, err, ErrSessionGone)
	assert.ErrorContains(t, err, "remote session is gone: unknown or expired session")
}

func TestGivenAnAckWithAnUnexpectedArmWhenReattachedThenTheErrorNamesTheArm(t *testing.T) {
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		ReattachAckArm: armPong.String(), OnMessage: answerSelectWith("OKAY")})
	dialer := newTestDialer(testWait)
	sessionUUID, err := dialer.ProbeSession(context.Background(), mock.URL)
	require.NoError(t, err)

	err = dialer.ProbeReattach(context.Background(), mock.URL, sessionUUID)

	assert.ErrorContains(t, err, "the reattach ack carried pong, not reattach")
}

func TestGivenABadReattachAnswerWhenReattachedThenTheErrorNamesIt(t *testing.T) {
	cases := []struct {
		name      string
		endpoint  string
		wantError string
	}{
		{"an ack naming another session", probeReplyServer(t, testsupport.FrameReattach(otherSessionUUID)),
			"names session " + otherSessionUUID + ", not " + testSessionUUID},
		{"raw adb bytes before any control frame", probeReplyServer(t, []byte("OKAY")), errNotControlFrame.Error()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := newTestDialer(testWait).ProbeReattach(context.Background(), testCase.endpoint, testSessionUUID)

			assert.ErrorContains(t, err, testCase.wantError)
		})
	}
}

func TestGivenAFreeNotifierSlotWhenProbedThenTheHelloIsAckedAndTheSlotIsFreeAgain(t *testing.T) {
	reverse := testsupport.NewReverseMockWSS(t)
	dialer := newTestDialer(testWait)

	require.NoError(t, dialer.ProbeNotifier(boundedContext(t), reverse.URL))

	require.Eventually(t, func() bool { return reverse.NotifiersClosed() == 1 }, testWait, notifierPoll,
		"the probe must close the one notifier it registered")
	require.NoError(t, dialer.ProbeNotifier(boundedContext(t), reverse.URL), "a second probe takes the freed slot")
	assert.Equal(t, 2, reverse.NotifiersAccepted(), "each probe registers one notifier")
	assert.Empty(t, reverse.Refusals())
}

func TestGivenTheNotifierSlotHeldWhenProbedThenErrNotifierSlotTakenIsReturned(t *testing.T) {
	reverse := testsupport.NewReverseMockWSS(t)
	rivalNotifier(t, reverse)

	err := newTestDialer(testWait).ProbeNotifier(context.Background(), reverse.URL)

	assert.ErrorIs(t, err, ErrNotifierSlotTaken)
	assert.Equal(t, []string{"notifier already registered"}, reverse.Refusals())
	assert.Zero(t, reverse.NotifiersClosed(), "the rival keeps the slot")
}

func TestGivenAnotherHelloRefusalWhenTheNotifierIsProbedThenTheErrorIsARefusalButNotTheHeldSlot(t *testing.T) {
	server := newNotifierServer(t, testsupport.FrameError("notifier evicted"))

	err := newTestDialer(testWait).ProbeNotifier(context.Background(), server.url)

	assert.ErrorIs(t, err, errNotifierRefused)
	assert.NotErrorIs(t, err, ErrNotifierSlotTaken)
	assert.ErrorContains(t, err, "notifier evicted")
	await(t, server.closed, "the probe websocket to close")
}

func TestGivenASilentServerWhenTheContextIsCancelledThenEachProbeReturnsAtOnceAndClosesTheSocket(t *testing.T) {
	for _, probe := range eachProbe {
		t.Run(probe.name, func(t *testing.T) {
			// An empty script never answers, so only the cancel can end the read. It fires after the
			// server holds the first frame, so it lands on the read, never the dial.
			server := newNotifierServer(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			ended := make(chan error, 1)
			go func() { ended <- probe.run(ctx, newTestDialer(unreachedTimeout), server.url) }()
			await(t, server.frames, "the probe's first frame")

			cancel()

			// The close surfaces as the closed socket or, when the server answers the close frame
			// first, as the peer's EOF.
			err := await(t, ended, "the probe to return")
			assert.True(t, errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF), "the cancel's close must end the read, got %v", err)
			await(t, server.closed, "the probe websocket to close")
		})
	}
}

func TestGivenAWriteDeadlineAlreadyPassedWhenEachProbeSendsThenTheErrorNamesTheSendAndTheSocketCloses(t *testing.T) {
	for _, probe := range eachProbe {
		t.Run(probe.name, func(t *testing.T) {
			server := newNotifierServer(t, probe.acked...)
			dialer := newTestDialer(testWait)
			dialer.writeDeadline = -time.Second // every write times out before it starts

			err := probe.run(context.Background(), dialer, server.url)

			assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
			assert.ErrorContains(t, err, "send the ")
			await(t, server.closed, "the probe websocket to close")
		})
	}
}

func TestGivenAPresignedEndpointThatRefusesTheDialWhenEachProbeRunsThenTheErrorCarriesNoCredential(t *testing.T) {
	for _, probe := range eachProbe {
		t.Run(probe.name, func(t *testing.T) {
			err := probe.run(context.Background(), newTestDialer(testWait), testsupport.PresignedUnreachableEndpoint)

			require.Error(t, err)
			assert.Contains(t, err.Error(), testsupport.ScrubbedDialText, "the dial must have run")
			assert.NotContains(t, err.Error(), testsupport.SignatureMarker)
			assert.NotContains(t, err.Error(), testsupport.SecurityTokenMarker)
			assert.NotContains(t, err.Error(), "X-Amz-")
		})
	}
}
