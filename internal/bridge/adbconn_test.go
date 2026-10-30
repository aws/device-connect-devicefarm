package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// neverCleanCutDelay outlives every test, so the mock keeps a predecessor open and writable.
const neverCleanCutDelay = time.Minute

// fastPolicy has production's shape with millisecond durations, so a test drives
// several full cycles in well under a second.
func fastPolicy() WSRefreshPolicy {
	return WSRefreshPolicy{
		Interval:        50 * time.Millisecond,
		FreeSecondWait:  300 * time.Millisecond,
		QuietWindow:     20 * time.Millisecond,
		PollInterval:    5 * time.Millisecond,
		MaxFailures:     3,
		ReattachTimeout: 2 * time.Second,
	}
}

// testDeviceARN is the session ARN every test connection binds to.
const testDeviceARN = "arn:aws:devicefarm:us-west-2:111122223333:session:11111111-2222-3333-4444-555555555555"

// newTestAdbConn binds endpoint to a real ActiveDevice, dials it on the adb route, and
// wraps the websocket as an adbConn, as AdbProxyServer.handleConn does. Closed via
// t.Cleanup.
func newTestAdbConn(t *testing.T, endpoint string, policy WSRefreshPolicy) *adbConn {
	t.Helper()
	return newTestAdbConnOnRoute(t, endpoint, policy, RouteAdb, noopLogger())
}

// newTestAdbConnOnRoute is newTestAdbConn on a caller-chosen route, logging to logger.
func newTestAdbConnOnRoute(t *testing.T, endpoint string, policy WSRefreshPolicy, route Route,
	logger *slog.Logger) *adbConn {
	t.Helper()
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "test-device"}))
	dialer := newTestDialer(testTimeout)
	ws, err := dialer.Dial(context.Background(), endpoint, route)
	require.NoError(t, err)

	conn := newAdbConn(dialer, device, testDeviceARN, route, ws, policy, newActivityTracker(nil), logger)
	t.Cleanup(func() { conn.Close() })
	return conn
}

// redirect points the bound device at endpoint, as the daemon's endpoint refresh
// does, so the connection's next dial uses it.
func redirect(t *testing.T, conn *adbConn, endpoint string) {
	t.Helper()
	require.True(t, conn.device.UpdateEndpoint(conn.arn, endpoint, "test-device"))
}

// handshake drives the opening exchange. Receive consumes the control frame, so the
// first message the caller sees is real ADB data.
func handshake(t *testing.T, conn *adbConn) {
	t.Helper()
	require.NoError(t, conn.Send([]byte("000chost:version")))
	message, err := conn.Receive()
	require.NoError(t, err)
	require.Equal(t, "000chost:version", string(message), "the first delivered message must be ADB data, not a control frame")
	waitFor(t, func() bool { return conn.sessionUUID() != "" }, "session uuid was never latched")
}

// waitFor polls until cond holds, failing with msg on timeout. Only for assertions
// genuinely about a concurrent goroutine making progress.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestGivenNewSessionWhenControlFrameArrivesThenItIsConsumedAndUUIDLatched(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})

	handshake(t, conn)

	assert.NotEmpty(t, conn.sessionUUID(), "the session uuid must be latched from the control frame")
	assert.Equal(t, 1, mock.SessionsOpened())
}

func TestGivenAReverseRouteStatusAsTheFirstAdbFrameWhenReceivedThenTheSessionIsRefused(t *testing.T) {
	// The codec parses 0x10..0x15 for the reverse route, so /adb's refusal of them rests on
	// consumeControlFrame's default arm alone.
	wsURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		serverConn.PayloadType = websocket.BinaryFrame
		websocket.Message.Send(serverConn, newSessionControlFrame(controlStatusNotifierReady, ""))
		io.Copy(io.Discard, serverConn) // stay open, so the failure is the refusal and not an EOF
	})
	conn := newTestAdbConn(t, wsURL, fastPolicy())

	// Bounded: a connection that swallowed the frame would park in Receive forever.
	failed := make(chan error, 1)
	go func() {
		_, receiveErr := conn.Receive()
		failed <- receiveErr
	}()

	var err error
	select {
	case err = <-failed:
	case <-time.After(testTimeout):
		t.Fatal("the notifier status was neither refused nor delivered")
	}

	require.Error(t, err, "a notifier status must not open an adb session")
	assert.ErrorContains(t, err, "opening a new session",
		"the refusal must come from the status switch, not from the codec")
	assert.Empty(t, conn.sessionUUID(), "no session may be latched from a reverse-route frame")
}

func TestGivenControlFrameWhenPipedToLocalAdbThenFrameNeverReachesTheTCPSide(t *testing.T) {
	// Same guard one layer up, through the real pipe.
	mock := testsupport.NewSessionEchoWSS(t)
	appSide, pipeSide := localTCPPair(t)
	conn := newTestAdbConn(t, mock.URL, WSRefreshPolicy{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- pipeConn(ctx, pipeSide, conn, nil) }()

	request := []byte("000chost:version")
	writeAll(t, appSide, request)

	got := readN(t, appSide, len(request))
	assert.Equal(t, request, got)
	assert.NotContains(t, string(got), magicPrefix, "the control frame must never reach the local adb client")
}

func TestGivenIdleConnectionWhenRefreshDueThenSwapsOnTheQuietWindow(t *testing.T) {
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{CleanCutDelay: 20 * time.Millisecond})
	policy := fastPolicy()
	// An unreachable free-second budget forces the quiet-window path.
	policy.FreeSecondWait = time.Hour
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)
	sessionUUID := conn.sessionUUID()

	// The reader drains the predecessor and commits the swap.
	go drainReceives(conn)
	go runRefresher(context.Background(), conn, policy, make(chan struct{}))

	waitFor(t, func() bool { return mock.Reattaches() >= 1 }, "no refresh happened on an idle connection")

	assert.Equal(t, sessionUUID, conn.sessionUUID(), "a refresh must resume the SAME session, not open a new one")
	assert.Equal(t, 1, mock.SessionsOpened(), "a refresh must not mint a second session")
}

func TestGivenConstantTrafficWhenRefreshDueThenSwapsOnlyAfterTheFreeSecondWait(t *testing.T) {
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{CleanCutDelay: 20 * time.Millisecond})
	policy := fastPolicy()
	policy.QuietWindow = time.Hour // unreachable, so only the expiry path can fire
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)

	stop := make(chan struct{})
	defer close(stop)
	go drainReceives(conn)
	// Keep both directions busy, so no quiet window ever appears.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				conn.Send([]byte("000chost:version"))
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	go runRefresher(context.Background(), conn, policy, stop)

	waitFor(t, func() bool { return mock.Reattaches() >= 1 },
		"a busy connection never refreshed; the free-second fallthrough is broken")
}

// startQuietHuntedRefresh wires a non-echoing session mock to a connection whose only swap
// trigger is the quiet window, then starts the reader and the refresher. Nothing marks
// activity except the traffic the caller drives, so mock.Reattaches() reports whether the
// activity timestamp held the swap off.
func startQuietHuntedRefresh(t *testing.T) (*testsupport.SessionMockWSS, *adbConn) {
	t.Helper()
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		// No echo, so inbound bytes appear only when the test pushes them.
		OnMessage: func(*testsupport.MockSession, []byte) []byte { return nil },
	})
	policy := fastPolicy()
	policy.Interval = 20 * time.Millisecond
	policy.QuietWindow = 150 * time.Millisecond
	policy.FreeSecondWait = time.Hour // unreachable, so only the quiet path can swap
	conn := newTestAdbConn(t, mock.URL, policy)

	go drainReceives(conn)
	require.NoError(t, conn.Send([]byte("000chost:version")))
	waitFor(t, func() bool { return conn.sessionUUID() != "" }, "session uuid was never latched")

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go runRefresher(context.Background(), conn, policy, stop)
	return mock, conn
}

// trafficWindow is how long the marking tests drive traffic: several times the quiet window
// they must keep out of reach, so a missing mark has many chances to let the hunt succeed.
const trafficWindow = 500 * time.Millisecond

func TestGivenOutboundOnlyTrafficWhenRefreshDueThenTheSwapWaitsForAQuietWindow(t *testing.T) {
	// Only Send can mark activity: the mock never echoes, so nothing arrives inbound.
	mock, conn := startQuietHuntedRefresh(t)

	deadline := time.Now().Add(trafficWindow)
	for time.Now().Before(deadline) {
		require.NoError(t, conn.Send([]byte("000chost:version")))
		time.Sleep(5 * time.Millisecond)
	}

	assert.Equal(t, 0, mock.Reattaches(),
		"outbound traffic must defer the swap; Send is not marking activity")
}

func TestGivenInboundOnlyTrafficWhenRefreshDueThenTheSwapWaitsForAQuietWindow(t *testing.T) {
	// Mirror of the outbound case: only Receive can mark while the test pushes server bytes.
	mock, _ := startQuietHuntedRefresh(t)
	sessions := mock.Sessions()
	require.Len(t, sessions, 1)
	// The mock announces the session before it attaches the websocket, so the first push can
	// land nowhere. Wait for it rather than racing the mock's own goroutine.
	waitFor(t, func() bool { return sessions[0].Send([]byte("OKAY")) },
		"the mock session never attached its websocket")

	deadline := time.Now().Add(trafficWindow)
	for time.Now().Before(deadline) {
		require.True(t, sessions[0].Send([]byte("OKAY")), "the mock session lost its websocket")
		time.Sleep(5 * time.Millisecond)
	}

	assert.Equal(t, 0, mock.Reattaches(),
		"inbound traffic must defer the swap; Receive is not marking activity")
}

func TestGivenSuccessorPublishedBeforeCutWhenRefreshedThenOldMessagesDrainFirst(t *testing.T) {
	// Ack arrives FIRST, the server's own code order.
	assertSequenceIsByteExactAcrossRefreshes(t, 40*time.Millisecond, 0)
}

func TestGivenCutBeforeSuccessorPublishedWhenRefreshedThenReaderWaitsForSuccessor(t *testing.T) {
	// Same invariant with the ordering reversed: predecessor cut before the ack lands.
	// The close and the ack use different TCP connections, so the close can arrive first.
	assertSequenceIsByteExactAcrossRefreshes(t, 0, 25*time.Millisecond)
}

// assertSequenceIsByteExactAcrossRefreshes drives repeated refreshes against a
// monotonic stream resuming at the exact next index, asserting both directions are
// gap-free, dupe-free, and ordered.
//
// reattachAckDelay > 0 gives the cut-before-ack interleaving; zero keeps the server's
// ack-then-cut.
func assertSequenceIsByteExactAcrossRefreshes(t *testing.T, cleanCutDelay, reattachAckDelay time.Duration) {
	t.Helper()
	const (
		chunkSize = 64
		// Loop on refreshes, not a fixed request count: requests per cycle depends on load.
		targetRefreshes = 3
		maxRequests     = 400
		// Slow enough that the gap between requests clears the quiet window.
		requestPace = 5 * time.Millisecond
	)
	mock := testsupport.NewSequenceWSS(t, testsupport.SequenceConfig{
		ChunkSize:        chunkSize,
		CleanCutDelay:    cleanCutDelay,
		ReattachAckDelay: reattachAckDelay,
	})
	policy := fastPolicy()
	policy.Interval = 10 * time.Millisecond
	policy.QuietWindow = 2 * time.Millisecond
	policy.FreeSecondWait = 50 * time.Millisecond
	policy.PollInterval = 2 * time.Millisecond
	conn := newTestAdbConn(t, mock.URL, policy)

	// The sequence server answers each inbound message with one chunk, so this request
	// both opens the session and yields chunk one.
	sent := []byte("000chost:version")
	require.NoError(t, conn.Send(sent))

	received := make([]byte, 0, maxRequests*chunkSize)
	message, err := conn.Receive()
	require.NoError(t, err)
	received = append(received, message...)

	stop := make(chan struct{})
	go runRefresher(context.Background(), conn, policy, stop)

	requests := 1
	for mock.Reattaches() < targetRefreshes && requests < maxRequests {
		time.Sleep(requestPace)
		request := []byte("000chost:version")
		require.NoError(t, conn.Send(request))
		sent = append(sent, request...)

		message, err := conn.Receive()
		require.NoError(t, err)
		received = append(received, message...)
		requests++
	}
	close(stop)

	require.GreaterOrEqual(t, mock.Reattaches(), targetRefreshes,
		"the test must actually exercise several refreshes; saw only %d after %d requests",
		mock.Reattaches(), requests)

	assert.Equal(t, testsupport.ExpectedSequence(len(received)), received,
		"the inbound stream must be gap-free, duplicate-free, and in order across refreshes")

	// The loop is lockstep, so this checks two-pointer ordering, not the outbound fence.
	sessions := mock.Sessions()
	require.Len(t, sessions, 1, "every refresh must resume the same session")
	assert.Equal(t, sent, sessions[0].Inbound(),
		"every outbound byte must reach the session exactly once, in order")

	// The silent drop has no error to observe, so assert it directly. Only the quiet-window
	// path promises zero loss.
	assert.Empty(t, sessions[0].Dropped(),
		"no outbound byte may land in a websocket the server has already superseded")
}

func TestGivenRefusedReattachWhenRefreshedThenFallsBackToAFreshSession(t *testing.T) {
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		RejectReattach: true,
		CleanCutDelay:  20 * time.Millisecond,
	})
	policy := fastPolicy()
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)
	originalUUID := conn.sessionUUID()

	stop := make(chan struct{})
	defer close(stop)
	go drainReceives(conn)
	go runRefresher(context.Background(), conn, policy, stop)
	// The fresh session is minted only on the successor's first client frame, so keep
	// offering traffic.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				conn.Send([]byte("000chost:version"))
				time.Sleep(5 * time.Millisecond)
			}
		}
	}()

	waitFor(t, func() bool { return mock.SessionsOpened() >= 2 },
		"a refused reattach must fall back to a fresh session")

	waitFor(t, func() bool {
		current := conn.sessionUUID()
		return current != "" && current != originalUUID
	}, "the fresh session's control frame was never consumed into a new uuid")

	require.NoError(t, conn.Send([]byte("000chost:version")), "the connection must still be usable")
}

func TestGivenRenewedEndpointWhenRefreshedThenTheSwapDialsTheNewEndpoint(t *testing.T) {
	// The daemon re-resolves the endpoint every EndpointRefreshInterval while a
	// connection stays open, so a swap must dial the CURRENT endpoint. A connection that
	// kept the endpoint it was accepted with would go on dialing a URL the daemon has
	// already replaced, and eventually an expired one.
	original := testsupport.NewSessionEchoWSS(t)
	renewed := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, original.URL, fastPolicy())
	handshake(t, conn)

	redirect(t, conn, renewed.URL)
	// The renewed server does not know this session, so it refuses the reattach and the
	// connection falls back to a fresh session -- on the renewed endpoint either way.
	require.NoError(t, conn.refresh(context.Background(), nil))

	request := []byte("000chost:devices")
	require.NoError(t, conn.Send(request), "the connection must still be usable after the swap")
	message, err := conn.Receive()
	require.NoError(t, err)
	assert.Equal(t, request, message)

	assert.Positive(t, renewed.ConnectionsAccepted(), "the swap must dial the renewed endpoint")
	assert.Equal(t, 1, original.ConnectionsAccepted(),
		"the swap must not dial the endpoint the connection was accepted with")
}

func TestGivenDetachedDeviceWhenRefreshedThenNoDialIsAttempted(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	conn.device.Clear()

	err := conn.refresh(context.Background(), nil)

	require.ErrorIs(t, err, errAdbConnClosed, "a detached device leaves nothing to dial")
	assert.Equal(t, 1, mock.ConnectionsAccepted(), "no successor may be dialed for a detached device")
	assert.Equal(t, 1, conn.failureCount(), "the failed refresh must be counted like any other")
}

func TestGivenDialFailureWhenRefreshedThenOldWebsocketStaysInService(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	// Successor dial goes to a dead port; the current websocket is untouched.
	redirect(t, conn, "wss://127.0.0.1:1/adb")

	err := conn.refresh(context.Background(), nil)

	require.Error(t, err, "a failed dial must surface as an error")
	assert.Equal(t, 1, conn.failureCount(), "the failure must be counted")

	request := []byte("000chost:devices")
	require.NoError(t, conn.Send(request))
	message, receiveErr := conn.Receive()
	require.NoError(t, receiveErr)
	assert.Equal(t, request, message, "the old websocket must stay in service after a failed refresh")
}

func TestGivenUnackedReattachWhenRefreshFailsThenPoisonedPredecessorIsDropped(t *testing.T) {
	// Once the UUID reached the server a missing ack is ambiguous, so the predecessor
	// cannot go back into service: the server may already drop writes to it.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		SwallowReattachAck: true,
		// Predecessor stays open and writable. A mock that closed it would make the drop
		// happen for the wrong reason and pass against the bug.
		CleanCutDelay: time.Minute,
	})
	policy := fastPolicy()
	// The ack timeout is what makes the outcome indeterminate; short keeps it quick.
	policy.ReattachTimeout = 100 * time.Millisecond
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)

	predecessor := conn.readFrom

	err := conn.refresh(context.Background(), nil)
	require.Error(t, err, "an unacked reattach must surface as an error")
	assert.ErrorIs(t, err, errReattachIndeterminate,
		"the failure must be classified as indeterminate, not as a clean no-op")

	// A send on a closed websocket fails; on a live-but-superseded one it would succeed
	// and vanish, which is the bug.
	waitFor(t, func() bool { return predecessor.Send([]byte("000chost:version")) != nil },
		"the poisoned predecessor must be dropped, not left accepting writes the server discards")
}

func TestGivenAReverseRouteStatusAckingAReattachWhenRefreshedThenTheSwapStaysIndeterminate(t *testing.T) {
	// attachSuccessor's default arm. The ack is payload-free, so it parses and reaches the
	// status switch. The UUID is on the wire by then, so a status the client cannot read
	// leaves the takeover unknown, as a lost ack does.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		ReattachAckStatus: controlStatusNotifierReady,
		// Predecessor stays open and writable, so its drop is provably the client's doing.
		CleanCutDelay: neverCleanCutDelay,
	})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	predecessor := conn.readFrom

	err := conn.refresh(context.Background(), nil)

	require.Error(t, err, "an ack the client cannot read must fail the swap")
	assert.ErrorIs(t, err, errReattachIndeterminate,
		"a status /adb never carries proves nothing about the takeover")
	assert.ErrorContains(t, err, "acking a reattach",
		"the refusal must come from the status switch, not from the codec")
	waitFor(t, func() bool { return predecessor.Send([]byte("000chost:version")) != nil },
		"the poisoned predecessor must be dropped, not left accepting writes the server discards")
}

func TestGivenIndeterminateAttemptThenARefusalWhenRefreshFailsThenTheTaintSurvivesTheRetry(t *testing.T) {
	// Mixed outcome: attempt 1 indeterminate, attempt 2 refused. That refusal is true only
	// of attempt 2; attempt 1 may still have taken over.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		// Only the first ack: unconditional swallowing makes attempt 2 indeterminate too.
		SwallowFirstReattachAcks: 1,
		// The session must be gone before attempt 2 arrives.
		Grace: time.Nanosecond,
		// The predecessor stays open, so a wrongly-trusted refusal shows as a socket still
		// accepting writes.
		CleanCutDelay: time.Minute,
	})
	policy := fastPolicy()
	policy.ReattachTimeout = 100 * time.Millisecond
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)

	predecessor := conn.readFrom
	sessionUUID := conn.sessionUUID()

	err := conn.refresh(context.Background(), nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, errReattachIndeterminate,
		"a refusal following an indeterminate attempt must stay tainted, not pass as determinate")
	waitFor(t, func() bool { return predecessor.Send([]byte("000chost:version")) != nil },
		"the predecessor must be dropped: the refusal does not prove attempt 1 failed to take over")
	// The fallback CLEARS the uuid, so an unchanged uuid is what distinguishes the
	// poisoned path from the fresh-session path.
	assert.Equal(t, sessionUUID, conn.sessionUUID(),
		"the tainted path leaves the session for the reader to resume, not a fresh one")
}

// assertTornDownAfter requires the machine to park in phaseClosed. phaseSteady on a
// connection whose sockets are all closed is a lie the next Receive believes.
func assertTornDownAfter(t *testing.T, conn *adbConn, attempt func()) {
	t.Helper()
	attempt()

	conn.seam.RLock()
	phase := conn.phase
	conn.seam.RUnlock()
	assert.Equal(t, phaseClosed, phase, "a torn-down connection must not be left claiming phaseSteady")
	assert.Zero(t, conn.failureCount(),
		"a teardown is not a refresh failure; counting it can trip the ceiling on a dead connection")
}

// assertNoDialWhileTornDown is assertTornDownAfter plus a check that attempt dialed
// nothing: a latched teardown must refuse before the upgrade, not after.
func assertNoDialWhileTornDown(t *testing.T, conn *adbConn, mock *testsupport.SessionMockWSS, attempt func()) {
	t.Helper()
	upgradesBefore := mock.ConnectionsAccepted()
	assertTornDownAfter(t, conn, attempt)
	assert.Equal(t, upgradesBefore, mock.ConnectionsAccepted(),
		"a torn-down connection must not dial a successor")
}

func TestGivenIndeterminateAttemptThenADialFailureWhenRefreshFailsThenTheTaintSurvives(t *testing.T) {
	// The other exit from the retry loop. Attempt 1 leaves a UUID unacked; attempt 2 fails
	// at the dial, which proves nothing about the takeover, so the predecessor must still
	// be treated as poisoned.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		SwallowReattachAck: true,
		CleanCutDelay:      time.Minute,
	})
	policy := fastPolicy()
	policy.ReattachTimeout = 200 * time.Millisecond
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)

	predecessor := conn.readFrom

	// The context expires while attempt 1 waits for its ack, so attempt 2 cannot get past
	// the dial. The ack wait is bounded by ReattachTimeout, not by the context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := conn.refresh(ctx, nil)

	require.Error(t, err)
	assert.ErrorIs(t, err, errReattachIndeterminate,
		"a failed dial after an unacked attempt must not clear the taint")
	waitFor(t, func() bool { return predecessor.Send([]byte("000chost:version")) != nil },
		"the poisoned predecessor must be dropped, not left accepting writes the server discards")
}

func TestGivenCloseDuringTheAckWaitWhenRefreshFailsThenConnectionIsLeftTornDown(t *testing.T) {
	// A Close inside the ack wait surfaces as errReattachIndeterminate, not
	// errAdbConnClosed. SwallowReattachAck parks the attach in that wait.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{SwallowReattachAck: true})
	policy := fastPolicy()
	policy.ReattachTimeout = 2 * time.Second
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)

	go func() {
		// Close once the mock holds attempt 1's UUID: inside the ack wait, after its dial.
		for mock.Reattaches() == 0 {
			time.Sleep(time.Millisecond)
		}
		conn.Close()
	}()

	assertTornDownAfter(t, conn, func() {
		require.Error(t, conn.refresh(context.Background(), nil))
	})
	// Two upgrades: the handshake and attempt 1. The retry must see the latch before it dials.
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "the retry after a Close must not dial")
}

func TestGivenClosedConnectionWhenRefreshDialIsRefusedThenConnectionIsLeftTornDown(t *testing.T) {
	// Latch sockets.closed directly. Close would also flip the phase via TryLock, so the
	// test would pass with the bug present.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.sockets.mutex.Lock()
	conn.sockets.closed = true
	conn.sockets.mutex.Unlock()

	assertNoDialWhileTornDown(t, conn, mock, func() {
		require.ErrorIs(t, conn.refresh(context.Background(), nil), errAdbConnClosed)
	})
}

func TestGivenClosedConnectionWhenReactiveReconnectIsRefusedThenConnectionIsLeftTornDown(t *testing.T) {
	// Same teardown-first check on the reactive path.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.sockets.mutex.Lock()
	conn.sockets.closed = true
	conn.sockets.mutex.Unlock()

	assertNoDialWhileTornDown(t, conn, mock, func() {
		assert.False(t, conn.reconnectReactively(io.EOF),
			"a torn-down connection must not report a successful resume")
	})
}

func TestGivenClosedConnectionWhenFreshSessionFallbackRunsThenConnectionIsLeftTornDown(t *testing.T) {
	// Teardown-first check on the fresh-session fallback. Driven directly: reaching it
	// through refresh needs a refused reattach and a Close in between.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.sockets.mutex.Lock()
	conn.sockets.closed = true
	conn.sockets.mutex.Unlock()

	assertNoDialWhileTornDown(t, conn, mock, func() {
		conn.seam.Lock()
		_, nextReady, refreshFailed := conn.startRefreshLocked()
		err := conn.restartAsNewSessionLocked(context.Background(), nextReady, refreshFailed)
		require.ErrorIs(t, err, errAdbConnClosed,
			"a latched teardown must surface as a closed connection")
	})
}

func TestGivenCloseDuringTheFreshSessionDialWhenTheDialFailsThenConnectionIsLeftTornDown(t *testing.T) {
	// A Close after the pre-dial check must still win over the dial error. Accept on a bare
	// listener proves the check passed; Close lands in the TLS handshake; the socket close
	// then fails the dial.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	redirect(t, conn, fmt.Sprintf("wss://%s/adb", listener.Addr()))
	go func() {
		acceptedSocket, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		conn.Close()
		acceptedSocket.Close()
	}()

	assertTornDownAfter(t, conn, func() {
		conn.seam.Lock()
		_, nextReady, refreshFailed := conn.startRefreshLocked()
		err := conn.restartAsNewSessionLocked(context.Background(), nextReady, refreshFailed)
		require.ErrorIs(t, err, errAdbConnClosed,
			"a Close during a failing fresh dial must surface as a closed connection, not a dial error")
	})
}

func TestGivenOpenConnectionWhenFreshSessionDialFailsThenPredecessorStaysInService(t *testing.T) {
	// A failed fresh-session dial is a refresh failure, not a teardown. Driven directly, as
	// the closed-connection sibling is.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	redirect(t, conn, "wss://127.0.0.1:1/adb") // the fresh-session dial must fail

	conn.seam.Lock()
	_, nextReady, refreshFailed := conn.startRefreshLocked()
	err := conn.restartAsNewSessionLocked(context.Background(), nextReady, refreshFailed)

	require.Error(t, err)
	assert.NotErrorIs(t, err, errAdbConnClosed, "a failed dial on an open connection is not a teardown")
	conn.seam.RLock()
	phase := conn.phase
	conn.seam.RUnlock()
	assert.Equal(t, phaseSteady, phase, "the predecessor must return to service")
	assert.Equal(t, 1, conn.failureCount(), "a failed fresh dial counts toward the ceiling")
	assert.NoError(t, conn.Send([]byte("000chost:version")), "the predecessor must still carry writes")
}

func TestGivenAlreadyResumedConnectionWhenStalePredecessorIsDroppedThenTheCurrentSocketSurvives(t *testing.T) {
	// dropPoisonedPredecessor takes the socket as a parameter, not a re-read of readFrom,
	// which can legitimately have advanced by then. Resume first so the outcome is
	// deterministic.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.seam.RLock()
	predecessor := conn.readFrom
	conn.seam.RUnlock()
	// Kill it as a dropped path would, so the reconnect below is legitimate.
	predecessor.Close()
	require.True(t, conn.reconnectReactively(io.EOF), "the session must resume for the test to mean anything")

	conn.seam.RLock()
	resumed := conn.readFrom
	conn.seam.RUnlock()
	require.NotSame(t, predecessor, resumed, "the reconnect must have installed a different socket")

	conn.dropPoisonedPredecessor(predecessor)

	assert.NoError(t, conn.Send([]byte("000chost:version")),
		"dropping the stale predecessor must not close the socket the session resumed on")
}

func TestGivenPeerThatStopsReadingWhenSendingThenTheWriteIsBounded(t *testing.T) {
	// The peer never reads, so writes block. See sendDeadline.
	stopServing := make(chan struct{})
	t.Cleanup(func() { close(stopServing) })
	deafURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		<-stopServing // hold the connection open without reading
	})

	// Shrink the bound without giving production a knob it never sets.
	const writeDeadline = 500 * time.Millisecond
	dialer := newTestDialer(testTimeout)
	dialer.writeDeadline = writeDeadline

	ws, err := dialer.Dial(context.Background(), deafURL, RouteAdb)
	require.NoError(t, err)
	t.Cleanup(func() { ws.Close() })

	// Enough to overrun the socket buffers so the write cannot be absorbed. x/net masks
	// client frames into a second buffer, so keep it modest.
	payload := bytes.Repeat([]byte{0xAB}, 4<<20)
	sendReturned := make(chan error, 1)
	go func() { sendReturned <- ws.Send(payload) }()

	select {
	case sendErr := <-sendReturned:
		assert.Error(t, sendErr,
			"a write to a peer that stopped reading must time out rather than block forever, "+
				"or it holds seam.RLock indefinitely and starves enforceWSDeadline")
	case <-time.After(20 * writeDeadline):
		t.Fatal("Send blocked well past its deadline; a wedged write can still starve the anomaly guard")
	}
}

func TestGivenRepeatedRefreshFailuresWhenPastTheCeilingThenConnectionIsTornDown(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	policy := fastPolicy()
	policy.MaxFailures = 2
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)
	redirect(t, conn, "wss://127.0.0.1:1/adb")

	go runRefresher(context.Background(), conn, policy, make(chan struct{}))

	waitFor(t, conn.isClosed, "repeated refresh failures must tear the connection down")
}

func TestGivenRefreshInFlightWhenClosedThenCloseDoesNotDeadlock(t *testing.T) {
	// A refresh can hold the seam for a dial plus an ack, and SIGTERM cannot wait on it.
	mock := testsupport.NewSessionEchoWSS(t)
	policy := fastPolicy()
	// A slow, unreachable dial keeps the refresh holding the seam for the whole test.
	policy.ReattachTimeout = 10 * time.Second
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)
	redirect(t, conn, "wss://10.255.255.1:9/adb") // unroutable: the dial hangs until its timeout

	refreshReturned := make(chan struct{})
	go func() {
		conn.refresh(context.Background(), nil)
		close(refreshReturned)
	}()
	// Let the refresh take the seam and park in the dial.
	time.Sleep(50 * time.Millisecond)

	closed := make(chan error, 1)
	go func() { closed <- conn.Close() }()

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked behind an in-flight refresh")
	}
	assert.True(t, conn.isClosed())
}

func TestGivenUnexpectedWebsocketFailureWhenReadingThenSessionResumesReactively(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	sessionUUID := conn.sessionUUID()

	// Kill the websocket as a dropped network path would, with no refresh in flight to
	// explain it.
	conn.seam.RLock()
	current := conn.readFrom
	conn.seam.RUnlock()
	current.Close()

	// Receive detects the dead socket and resumes, so the reader must drive recovery.
	received := make(chan []byte, 8)
	go func() {
		for {
			message, err := conn.Receive()
			if err != nil {
				return
			}
			received <- message
		}
	}()

	request := []byte("000chost:devices")
	waitFor(t, func() bool { return conn.Send(request) == nil }, "the connection never became writable again")

	select {
	case message := <-received:
		assert.Equal(t, request, message, "traffic must flow again on the resumed session")
	case <-time.After(5 * time.Second):
		t.Fatal("an unexpected websocket failure was never resumed")
	}
	assert.Equal(t, sessionUUID, conn.sessionUUID(), "the reactive reconnect must resume the same session")
	assert.GreaterOrEqual(t, mock.Reattaches(), 1)
	assert.Equal(t, 1, mock.SessionsOpened(), "resuming must not mint a new session")
}

func TestGivenTransientlyDroppedReconnectWhenResumingThenItRetriesOnceAndSucceeds(t *testing.T) {
	// Without the retry the reader gives up on the first drop and loses the session.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{DropFirstReattaches: 1})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	sessionUUID := conn.sessionUUID()

	conn.seam.RLock()
	current := conn.readFrom
	conn.seam.RUnlock()
	current.Close()

	received := make(chan []byte, 8)
	go func() {
		for {
			message, err := conn.Receive()
			if err != nil {
				return
			}
			received <- message
		}
	}()

	request := []byte("000chost:devices")
	waitFor(t, func() bool { return conn.Send(request) == nil },
		"the connection never recovered; the dropped reconnect was not retried")

	select {
	case message := <-received:
		assert.Equal(t, request, message, "traffic must flow again after the retry")
	case <-time.After(5 * time.Second):
		t.Fatal("a transiently dropped reconnect was never retried")
	}

	assert.Equal(t, sessionUUID, conn.sessionUUID(), "the retry must resume the SAME session")
	assert.Equal(t, 1, mock.SessionsOpened(), "recovering by retry must not mint a new session")
	// More than two would mean the retry budget is not respected.
	assert.Equal(t, 2, mock.Reattaches(), "exactly one retry: the dropped attempt plus one more")
}

func TestGivenTransientlyDroppedReattachWhenRefreshingProactivelyThenItRetriesOnceAndSwaps(t *testing.T) {
	// The interval is minutes in production, so deferring to the next cycle leaves the
	// websocket running well past the age the cadence bounds. Without the retry the
	// refresh fails and Reattaches() stops at the single dropped attempt.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{DropFirstReattaches: 1})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	sessionUUID := conn.sessionUUID()

	go drainReceives(conn) // drains the predecessor and commits the swap

	require.NoError(t, conn.refresh(context.Background(), nil),
		"a refresh whose first attach is transiently dropped must still succeed on the retry")

	assert.Equal(t, 2, mock.Reattaches(), "exactly one retry: the dropped attempt plus one more")
	assert.Equal(t, sessionUUID, conn.sessionUUID(), "the retry must resume the SAME session")
	assert.Equal(t, 1, mock.SessionsOpened(), "recovering by retry must not mint a new session")
	assert.Zero(t, conn.failureCount(), "a refresh that succeeded on its retry is not a failure")

	// The swap must have landed, not merely been reported.
	request := []byte("000chost:devices")
	waitFor(t, func() bool { return conn.Send(request) == nil },
		"the connection never became usable after the retried refresh")
}

func TestGivenRefusedReconnectWhenResumingThenItIsNotRetried(t *testing.T) {
	// The counterpart bound on the retry: a refusal is an explicit answer, so a second
	// attempt only stalls the adb client and buries the one actionable error.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RejectReattach: true})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.seam.RLock()
	current := conn.readFrom
	conn.seam.RUnlock()
	current.Close()

	_, err := conn.Receive()
	require.Error(t, err, "a refused reconnect must end the read rather than resume")

	// Count CONNECTIONS, not reattaches: RejectReattach refuses before the session
	// lookup, so the reattach counter never moves and cannot tell one attempt from two.
	// One opened the session, one carried the refused reattach; a third means retried.
	assert.Equal(t, 2, mock.ConnectionsAccepted(),
		"a refusal is terminal: it must not be retried")
	assert.Equal(t, 1, mock.SessionsOpened(),
		"a refused reattach must not mint a session either")
}

func TestGivenSilentAdbClientAfterFreshSessionFallbackWhenGuardPollsThenTheWedgeIsBroken(t *testing.T) {
	// The guard as the wedge's THIRD-line remedy, for when the server's reaper close never
	// lands. Visible only because phaseFreshHandshake is distinct from phaseHandshake: the
	// guard fires on a parked fresh-session successor, never on the opening handshake.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RejectReattach: true})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	require.NoError(t, conn.refresh(context.Background(), nil))

	// The reader parks on the successor awaiting a frame only a client write provokes.
	// Nothing writes.
	readFailed := make(chan error, 1)
	go func() {
		_, err := conn.Receive()
		readFailed <- err
	}()

	waitFor(t, func() bool {
		conn.seam.RLock()
		defer conn.seam.RUnlock()
		return conn.phase == phaseFreshHandshake
	}, "reads never moved onto the fresh successor")
	assert.Empty(t, conn.sessionUUID(),
		"the fallback clears the uuid, which is why the guard's remedy is teardown rather than a resume")

	require.True(t, conn.enforceWSDeadline(time.Nanosecond),
		"an expired fresh-session handshake must be dropped; without this the reader parks forever")

	select {
	case err := <-readFailed:
		assert.Error(t, err, "dropping the wedged websocket must surface as a read failure")
	case <-time.After(5 * time.Second):
		t.Fatal("the reader is still parked: dropping the websocket did not unblock it")
	}

	// Firing ENDS the connection and no reader is left to advance the phase, so without
	// the flip to phaseClosed every later poll re-fires on the same dead socket.
	assert.False(t, conn.enforceWSDeadline(time.Nanosecond),
		"the guard must not re-fire on a connection it already ended")
	conn.seam.RLock()
	phase := conn.phase
	conn.seam.RUnlock()
	assert.Equal(t, phaseClosed, phase, "a dropped fresh handshake leaves the machine closed")
}

func TestGivenWedgedFreshSessionWhenAdbClientWritesThenSessionRecoversLosslessly(t *testing.T) {
	// The first and best way out of the wedge, and why the guard's allowance stays long:
	// the client's next byte provokes the frame and nothing is lost, so a shorter
	// deadline would kill connections this path was about to recover.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RejectReattach: true})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	require.NoError(t, conn.refresh(context.Background(), nil))

	delivered := make(chan []byte, 1)
	go func() {
		message, err := conn.Receive()
		if err == nil {
			delivered <- message
		}
	}()

	waitFor(t, func() bool {
		conn.seam.RLock()
		defer conn.seam.RUnlock()
		return conn.phase == phaseFreshHandshake
	}, "reads never moved onto the fresh successor")

	require.NoError(t, conn.Send([]byte("000chost:version")))

	select {
	case message := <-delivered:
		assert.Equal(t, "000chost:version", string(message),
			"the control frame must be consumed and the adb bytes delivered intact")
	case <-time.After(5 * time.Second):
		t.Fatal("a client write did not break the wedge")
	}

	assert.NotEmpty(t, conn.sessionUUID(), "the fresh session's uuid must be latched")
	conn.seam.RLock()
	phase := conn.phase
	conn.seam.RUnlock()
	assert.Equal(t, phaseSteady, phase, "a recovered connection returns to steady state")
}

func TestGivenNoSessionUUIDWhenReadFailsThenErrorSurfacesRatherThanReconnecting(t *testing.T) {
	mock := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		ws.Close()
	})
	conn := newTestAdbConn(t, mock, fastPolicy())

	_, err := conn.Receive()

	require.Error(t, err)
	assert.Empty(t, conn.sessionUUID())
}

func TestGivenRefreshBeforeSessionAnnouncedWhenAttemptedThenRefusedWithoutDialing(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())

	// Still in the handshake phase. The refresher's own gate would not have released
	// either, so this asserts the machine refuses independently of it.
	err := conn.refresh(context.Background(), nil)

	assert.NoError(t, err, "a refresh outside steady state is a no-op, not a failure")
	assert.Zero(t, mock.Reattaches())
}

func TestGivenGateThatRejectsWhenRefreshAttemptedThenNoSwapHappens(t *testing.T) {
	// Re-evaluated under the seam lock, so traffic arriving between the refresher's poll
	// and the fence cancels the swap rather than being cut through.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	err := conn.refresh(context.Background(), func() bool { return false })

	require.NoError(t, err, "a rejected gate is a no-op, not a failure")
	assert.Zero(t, mock.Reattaches(), "a rejected gate must not reach the server")
	assert.Zero(t, conn.failureCount(), "a rejected gate is not a failure")
}

func TestGivenExpiredWebsocketDeadlineWhenEnforcedThenSocketDroppedOnlyInSteadyState(t *testing.T) {
	// Must NOT fire mid-refresh: dropping the websocket the reader is still draining
	// discards bytes the server already counted as delivered.
	tests := []struct {
		name      string
		phase     refreshPhase
		wantFired bool
	}{
		{name: "steady state drops the stale socket", phase: phaseSteady, wantFired: true},
		{name: "mid-refresh must not drop the draining socket", phase: phaseRefreshing, wantFired: false},
		{name: "mid-drain must not drop the draining socket", phase: phaseDraining, wantFired: false},
		// Same frame owed, opposite treatment. See phaseFreshHandshake.
		{name: "opening handshake has no session to resume", phase: phaseHandshake, wantFired: false},
		{name: "wedged fresh-session handshake is dropped", phase: phaseFreshHandshake, wantFired: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := testsupport.NewSessionEchoWSS(t)
			conn := newTestAdbConn(t, mock.URL, fastPolicy())

			conn.seam.Lock()
			conn.phase = tc.phase
			conn.readSince = time.Now().Add(-time.Hour) // long past any deadline
			stale := conn.readFrom
			conn.seam.Unlock()

			assert.Equal(t, tc.wantFired, conn.enforceWSDeadline(time.Minute))

			// The return value alone is not the guarantee: firing must actually drop the
			// socket, which is what unblocks the parked reader. Probe with Send, not
			// Receive -- Receive on a live idle socket would block forever.
			sendErr := stale.Send([]byte("000chost:version"))
			if tc.wantFired {
				assert.Error(t, sendErr, "firing must actually close the stale websocket")
			} else {
				assert.NoError(t, sendErr, "a guard that did not fire must leave the websocket usable")
			}
		})
	}
}

func TestGivenDisabledDeadlineWhenEnforcedThenNeverFires(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	conn.seam.Lock()
	conn.phase = phaseSteady
	conn.readSince = time.Now().Add(-time.Hour)
	conn.seam.Unlock()

	assert.False(t, conn.enforceWSDeadline(0), "a zero deadline must disable the guard")
}

func TestGivenExpiredWebsocketDeadlineWhenGuardFiresThenReaderResumesTheSameSession(t *testing.T) {
	// The guard is called directly: refreshCycleHeadroom (30s) is additive, so a
	// refresher-driven version cannot fire inside a test window.
	mock := testsupport.NewSessionEchoWSS(t)
	policy := fastPolicy()
	// No proactive refresh, so only the guard can produce a reattach here.
	policy.Interval = time.Hour
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)
	sessionUUID := conn.sessionUUID()

	go drainReceives(conn) // observes the dropped socket and resumes reactively

	conn.seam.Lock()
	conn.readSince = time.Now().Add(-time.Hour)
	conn.seam.Unlock()
	require.True(t, conn.enforceWSDeadline(time.Minute), "an expired websocket must be dropped")

	waitFor(t, func() bool { return mock.Reattaches() >= 1 }, "the session was never resumed")
	assert.Equal(t, sessionUUID, conn.sessionUUID(), "the same session must be resumed")
	assert.False(t, conn.isClosed(), "the connection itself must survive a websocket swap")
}

func TestGivenClosedConnectionWhenSentOrReceivedThenBothReportClosed(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	require.NoError(t, conn.Close())

	assert.Error(t, conn.Send([]byte("000chost:version")))
	_, err := conn.Receive()
	assert.Error(t, err)
	assert.NoError(t, conn.Close(), "Close must be idempotent")
}

func TestGivenConcurrentSendersWhenRefreshingThenNoBytesAreLostOrRaced(t *testing.T) {
	// The outbound fence under contention. Run with -race it also covers the seam's
	// locking discipline.
	const (
		senders          = 4
		sendsPerSender   = 25
		messageSizeBytes = 8
	)
	mock := testsupport.NewSessionEchoWSS(t)
	policy := fastPolicy()
	policy.Interval = 15 * time.Millisecond
	policy.QuietWindow = time.Hour // force the expiry path, since senders keep it busy
	policy.FreeSecondWait = 20 * time.Millisecond
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)

	stop := make(chan struct{})
	go drainReceives(conn)
	go runRefresher(context.Background(), conn, policy, stop)

	var senderGroup sync.WaitGroup
	senderGroup.Add(senders)
	for sender := range senders {
		go func() {
			defer senderGroup.Done()
			message := bytes.Repeat([]byte{senderMarker(sender)}, messageSizeBytes)
			for range sendsPerSender {
				if err := conn.Send(message); err != nil {
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	senderGroup.Wait()
	close(stop)

	sessions := mock.Sessions()
	require.Len(t, sessions, 1)
	inbound := sessions[0].Inbound()

	// Per-sender counts: interleaving across senders is expected, loss is not.
	for sender := range senders {
		want := sendsPerSender * messageSizeBytes
		got := bytes.Count(inbound, []byte{senderMarker(sender)})
		assert.Equalf(t, want, got, "sender %d lost or duplicated bytes across refreshes", sender)
	}
}

// senderMarker returns a distinct non-ASCII byte per sender, so a sender's count cannot
// be polluted by the ASCII ADB requests the handshake sends.
func senderMarker(sender int) byte {
	return byte(0xF0 + sender)
}

func TestGivenBenignErrorOutsideRefreshWhenAwaitingSuccessorThenNotTreatedAsCleanCut(t *testing.T) {
	// A cut can come with no close frame, so the phase is the ONLY clean-cut signal.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	assert.False(t, conn.awaitSuccessor(net.ErrClosed),
		"outside a refresh, a benign close is a real end of connection")
}

func TestGivenNonBenignErrorDuringRefreshWhenAwaitingSuccessorThenNotTreatedAsCleanCut(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.seam.Lock()
	conn.phase = phaseRefreshing
	conn.nextReady = make(chan struct{})
	conn.refreshFailed = make(chan struct{})
	conn.seam.Unlock()

	assert.False(t, conn.awaitSuccessor(websocket.ErrFrameTooLarge),
		"a protocol fault is never a clean cut")
}

func TestGivenPeerResetDuringRefreshWhenAwaitingSuccessorThenNotTreatedAsCleanCut(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.seam.Lock()
	conn.phase = phaseDraining
	conn.nextReady = make(chan struct{})
	conn.refreshFailed = make(chan struct{})
	conn.seam.Unlock()

	resetRead := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", errPeerReset)}
	assert.False(t, conn.awaitSuccessor(resetRead),
		"a reset may have cut the old websocket short, so the swap must not go ahead")
}

func TestGivenOversizedInboundMessageWhenReceivedThenTheFaultSurfacesWithoutAResume(t *testing.T) {
	// The reactive path must not swallow a codec fault: resuming the session would skip the
	// over-cap payload and desynchronize the adb stream, with nothing surfaced.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(*testsupport.MockSession, []byte) []byte {
			return make([]byte, maxInboundMessageBytes+1)
		},
	})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	// Not handshake(): the first message after the control frame is the over-cap reply.
	require.NoError(t, conn.Send([]byte("000chost:version")))

	// Bounded, because a swallowed fault parks the reader on the resume instead of returning.
	received := make(chan error, 1)
	go func() {
		_, err := conn.Receive()
		received <- err
	}()

	select {
	case err := <-received:
		assert.ErrorIs(t, err, websocket.ErrFrameTooLarge)
	case <-time.After(5 * time.Second):
		t.Fatal("Receive never returned; the codec fault was recovered as a dead socket")
	}
	assert.Equal(t, 0, mock.Reattaches(), "a codec fault must not resume the session")
}

func TestGivenFailedRefreshWhenReaderIsWaitingThenItIsReleasedWithTheReadError(t *testing.T) {
	// Otherwise the pipe hangs forever on a connection that is already dead.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	conn.seam.Lock()
	conn.phase = phaseRefreshing
	conn.nextReady = make(chan struct{})
	conn.refreshFailed = make(chan struct{})
	refreshFailed := conn.refreshFailed
	conn.seam.Unlock()

	waited := make(chan bool, 1)
	// io.EOF is how x/net surfaces the server's close of a superseded websocket.
	go func() { waited <- conn.awaitSuccessor(io.EOF) }()

	// Nothing may be decided until the refresh reports its outcome.
	select {
	case <-waited:
		t.Fatal("awaitSuccessor returned before the refresh outcome was known")
	case <-time.After(50 * time.Millisecond):
	}

	// failRefreshLocked expects the seam held and releases it.
	conn.seam.Lock()
	conn.failRefreshLocked(refreshFailed)

	select {
	case advanced := <-waited:
		assert.False(t, advanced, "a failed refresh must not advance the read target")
	case <-time.After(5 * time.Second):
		t.Fatal("a reader parked on a failed refresh was never released")
	}
}

func TestGivenAnyPhaseWhenClosedThenNeverDeadlocksAndIsIdempotent(t *testing.T) {
	// Close is reachable from every phase (SIGTERM, device remove, failure ceiling),
	// which is why it deliberately does not take the seam lock.
	phases := []struct {
		name  string
		phase refreshPhase
	}{
		{name: "handshake", phase: phaseHandshake},
		{name: "fresh handshake", phase: phaseFreshHandshake},
		{name: "steady", phase: phaseSteady},
		{name: "refreshing", phase: phaseRefreshing},
		{name: "draining", phase: phaseDraining},
		{name: "already closed", phase: phaseClosed},
	}

	for _, tc := range phases {
		t.Run(tc.name, func(t *testing.T) {
			mock := testsupport.NewSessionEchoWSS(t)
			conn := newTestAdbConn(t, mock.URL, fastPolicy())
			conn.seam.Lock()
			conn.phase = tc.phase
			conn.nextReady = make(chan struct{})
			conn.refreshFailed = make(chan struct{})
			conn.seam.Unlock()

			closed := make(chan error, 1)
			go func() { closed <- conn.Close() }()
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatalf("Close deadlocked in phase %s", tc.name)
			}

			assert.NoError(t, conn.Close(), "Close must be idempotent from any phase")
		})
	}
}

func TestGivenCloseRacingRefreshAndTrafficWhenInterleavedThenNeverPanics(t *testing.T) {
	// A Close can land anywhere in a refresh. A double channel close or nil deref there is
	// a panic, so the assertion is only that the loop completes.
	//
	// -race needs cgo, which the build disables; run with CGO_ENABLED=1.
	const interleavings = 25

	for range interleavings {
		mock := testsupport.NewSessionEchoWSS(t)
		policy := fastPolicy()
		// Aggressive cadence so a refresh is almost always in flight when Close lands.
		policy.Interval = time.Millisecond
		policy.QuietWindow = 0
		policy.PollInterval = time.Millisecond
		conn := newTestAdbConn(t, mock.URL, policy)
		handshake(t, conn)

		var group sync.WaitGroup
		group.Add(4)
		go func() { defer group.Done(); drainReceives(conn) }()
		go func() {
			defer group.Done()
			for range 20 {
				conn.Send([]byte("000chost:devices"))
			}
		}()
		go func() {
			defer group.Done()
			runRefresher(context.Background(), conn, policy, make(chan struct{}))
		}()
		go func() {
			defer group.Done()
			time.Sleep(2 * time.Millisecond)
			conn.Close()
		}()
		group.Wait()

		require.True(t, conn.isClosed())
	}
}

func TestGivenRefreshInFlightWhenSendAttemptedThenItBlocksUntilTheSeamCommits(t *testing.T) {
	// The outbound fence, asserted structurally: with a refresh parked mid-round-trip a Send
	// must not complete. A byte-counting version cannot tell "fenced" from "the race was not
	// lost this run".
	mock := testsupport.NewSessionEchoWSS(t)
	policy := fastPolicy()
	policy.ReattachTimeout = 5 * time.Second
	conn := newTestAdbConn(t, mock.URL, policy)
	handshake(t, conn)

	// A black-hole endpoint parks the refresh inside attachSuccessor's dial for a known
	// duration. An unroutable address will not do: connect fails fast enough that the
	// fence's span becomes too short to observe reliably.
	const dialWindow = 800 * time.Millisecond
	redirect(t, conn, fmt.Sprintf("wss://%s/adb", blackHoleListener(t)))
	conn.dialer = newTestDialer(dialWindow)

	refreshReturned := make(chan struct{})
	go func() {
		conn.refresh(context.Background(), nil)
		close(refreshReturned)
	}()

	// The phase cannot be observed through the seam here -- that is the point: the refresh
	// holds it exclusively, so even a reader blocks. Wait for the dial instead.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-refreshReturned:
		t.Fatal("the refresh returned before reaching the dial; the test cannot observe the fence")
	default:
	}

	sendReturned := make(chan error, 1)
	go func() { sendReturned <- conn.Send([]byte("000chost:devices")) }()

	select {
	case <-sendReturned:
		t.Fatal("Send completed while a refresh was mid-round-trip: the outbound " +
			"direction is not fenced, so a send can land in a superseded websocket")
	case <-time.After(300 * time.Millisecond):
		// Correct: queued behind the seam, well inside the dial window.
	}

	// Once the dial gives up the fence lifts, so the send must resolve rather than hang.
	select {
	case <-sendReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Send never resolved after the refresh released the seam")
	}
	<-refreshReturned
}

// blackHoleListener returns a TCP listener that accepts and never replies, so a
// websocket dial hangs in the handshake until the dialer's timeout. Torn down via
// t.Cleanup.
func blackHoleListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			// Hold open without answering the handshake.
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		close(accepted)
		for conn := range accepted {
			conn.Close()
		}
	})
	return listener.Addr().String()
}

func TestGivenStalledDrainWhenDeadlineEnforcedThenPredecessorIsDropped(t *testing.T) {
	// The half-duplex wedge guard: a blackholed close leaves sends on the successor and
	// reads parked on a predecessor that never EOFs, and nothing else can recover since
	// refresh() no-ops outside steady state. Hence the drain's own longer allowance.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	deadline := time.Minute
	conn.seam.Lock()
	conn.phase = phaseDraining
	// Just inside the allowance: a legitimate drain must not be cut short.
	conn.drainingSince = time.Now().Add(-drainStallMultiple*deadline + 5*time.Second)
	conn.seam.Unlock()
	assert.False(t, conn.enforceWSDeadline(deadline),
		"a drain within its allowance must be left alone")

	conn.seam.Lock()
	conn.drainingSince = time.Now().Add(-drainStallMultiple * deadline)
	conn.seam.Unlock()

	assert.True(t, conn.enforceWSDeadline(deadline),
		"a drain that never reaches EOF must be broken so the reader can advance")
}

func TestGivenFiredDeadlineGuardWhenPolledAgainThenItDoesNotRefireOrDoubleClose(t *testing.T) {
	// The guard must re-arm the clock it read, or every later poll re-decides the same
	// expiry. No reader runs here: a reader resetting readSince would mask it.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	deadline := time.Minute
	conn.seam.Lock()
	conn.phase = phaseSteady
	conn.readSince = time.Now().Add(-time.Hour)
	stale := conn.readFrom
	conn.seam.Unlock()

	require.True(t, conn.enforceWSDeadline(deadline), "the guard must fire on an expired websocket")
	for poll := 0; poll < 3; poll++ {
		assert.False(t, conn.enforceWSDeadline(deadline),
			"the guard must not fire again for a websocket it already retired")
	}

	// Retiring through forget is what keeps Close from closing the same socket twice; a
	// bare Close leaves it registered and makes teardown report a spurious error.
	conn.sockets.mutex.Lock()
	_, stillRegistered := conn.sockets.open[stale]
	conn.sockets.mutex.Unlock()
	assert.False(t, stillRegistered, "a retired websocket must leave the teardown set")
	assert.NoError(t, conn.Close(), "teardown must not report closing an already-retired socket")
}

func TestGivenRefreshHoldingTheSeamWhenTheConnectionIsGoneThenSuccessorIsRetiredAndReaderReleased(t *testing.T) {
	// The teardown-during-refresh unwind, shared by all three of refresh's "closed while
	// holding a successor" branches. Driven on the helper directly: reaching them needs a
	// Close inside the dial-and-ack window, which no test hits deterministically.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	successor, err := conn.dialer.Dial(context.Background(), mock.URL, RouteAdb)
	require.NoError(t, err)
	require.True(t, conn.sockets.register(successor), "the successor must start out registered")
	refreshFailed := make(chan struct{})

	// The real precondition: a refresh holds the seam with a successor in hand when
	// teardown lands. Latch closed directly; Close would block on the seam this test
	// deliberately holds.
	conn.sockets.mutex.Lock()
	conn.sockets.closed = true
	conn.sockets.mutex.Unlock()

	conn.seam.Lock()
	conn.discardSuccessorLocked(successor, refreshFailed)

	require.True(t, conn.seam.TryLock(), "the unwind must release the seam")
	assert.Equal(t, phaseClosed, conn.phase,
		"a torn-down connection must not be left mid-refresh, or the next Receive reads a dead socket")
	conn.seam.Unlock()

	conn.sockets.mutex.Lock()
	_, stillRegistered := conn.sockets.open[successor]
	conn.sockets.mutex.Unlock()
	assert.False(t, stillRegistered, "the abandoned successor must leave the teardown set")

	select {
	case <-refreshFailed:
	default:
		t.Fatal("refreshFailed must be closed, or a reader parked in awaitSuccessor never wakes")
	}

	// Retiring through forget (not a bare Close) keeps teardown from closing the same
	// socket twice and reporting a spurious error.
	assert.NoError(t, conn.Close(), "teardown must not report closing an already-retired socket")
}

// drainReceives stands in for pipeConn's inbound pump. adbConn.Receive assumes a single
// reader, and that reader drains a predecessor and commits each swap.
func drainReceives(conn *adbConn) {
	for {
		if _, err := conn.Receive(); err != nil {
			return
		}
	}
}

func TestGivenAConnectionOnTheReverseRouteWhenItRefreshesThenEverySuccessorDialsTheSameRoute(t *testing.T) {
	// Both in-connection dial sites (the reattach and the fresh-session fallback) must
	// name the route the first websocket was dialed on: a connector that swapped onto
	// ?path=adb would land on the wrong server handler. A refused reattach makes one
	// refresh exercise both sites.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RejectReattach: true})
	conn := newTestAdbConnOnRoute(t, mock.URL, fastPolicy(), RouteAdbReverse, noopLogger())
	handshake(t, conn)

	require.NoError(t, conn.refresh(context.Background(), nil))

	// The server records a dial once its handler runs, which can trail the client's
	// return from Dial.
	waitFor(t, func() bool { return len(mock.RoutesDialed()) >= 3 },
		"expected the first dial, one refused reattach and one fresh-session dial")
	assert.Equal(t, []string{"adbreverse", "adbreverse", "adbreverse"}, mock.RoutesDialed(),
		"the reattach and the fresh-session fallback must both dial the connection's own route")
}

func TestGivenAConnectionPastItsHandshakeWhenTheHandshakeIsAwaitedThenNoInboundMessageIsStolen(t *testing.T) {
	// Awaiting outside the opening handshake must never read: the next inbound message
	// belongs to the stream's reader, and the echo below proves it is still there.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	require.NoError(t, conn.Send([]byte("0009host:kill")))

	assert.ErrorIs(t, conn.awaitHandshake(time.Second), errHandshakeConsumed)

	message, err := conn.Receive()
	require.NoError(t, err)
	assert.Equal(t, "0009host:kill", string(message))
}

func TestGivenAnInteractiveConnectionWhenReattachIsRefusedThenTheFreshSessionFallbackStillRuns(t *testing.T) {
	// The terminal option is per connection: a connection built without it must keep
	// falling back, because an interactive adb client re-issues its own requests.
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{RejectReattach: true})
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)

	require.NoError(t, conn.refresh(context.Background(), nil))

	assert.False(t, conn.isClosed(), "a refused reattach must not end an interactive connection")
	// The fallback clears the uuid under the seam before it publishes the successor, so an
	// empty one is a synchronous observable that it ran.
	assert.Empty(t, conn.sessionUUID(), "the fallback must retire the refused session's uuid")
	// The fresh session is minted by the successor's first client frame, so one write is what
	// separates this from the terminal case, where no third websocket is ever dialed.
	require.NoError(t, conn.Send([]byte("000chost:version")))
	waitFor(t, func() bool { return mock.SessionsOpened() == 2 }, "the write must mint a fresh session")
	assert.Equal(t, 3, mock.ConnectionsAccepted(), "one dial, one refused reattach, one fresh session")
}

// blockingWSConn is a WSConn whose Send parks until release closes, standing in for a peer
// that stopped reading.
type blockingWSConn struct {
	entered chan struct{}
	release chan struct{}
}

func (conn *blockingWSConn) Send([]byte) error {
	select {
	case conn.entered <- struct{}{}:
	default:
	}
	<-conn.release
	return nil
}

func (conn *blockingWSConn) Receive() ([]byte, error) {
	<-conn.release
	return nil, io.EOF
}

func (conn *blockingWSConn) Close() error { return nil }

func TestGivenABlockedSendWhenQuietForIsReadThenTheConnectionCountsAsActive(t *testing.T) {
	// The refresher hunts for a quiet moment by polling quietFor. A write blocked in the
	// socket must not read as quiet, or the swap is decided, waits on the outbound fence,
	// and then fires the instant the write lands.
	socket := &blockingWSConn{entered: make(chan struct{}, 1), release: make(chan struct{})}
	clock := newFakeClock()
	// The clock samples the counter. While a write counts, only the mark reads the clock
	// (quietFor returns early), so the assertion below pins the mark INSIDE the counted
	// span. Ending the span first would leave the pre-write gap readable for an instant.
	var activity *activityTracker
	markedInFlight := false
	activity = newActivityTracker(func() time.Time {
		if activity != nil && activity.inFlightWrites.Load() > 0 {
			markedInFlight = true
		}
		return clock.now()
	})
	conn := newAdbConn(newTestDialer(time.Second), NewActiveDevice(), testDeviceARN, RouteAdb, socket,
		fastPolicy(), activity, noopLogger())

	sendReturned := make(chan error, 1)
	go func() { sendReturned <- conn.Send([]byte("000chost:version")) }()
	select {
	case <-socket.entered:
	case <-time.After(testTimeout):
		t.Fatal("Send never reached the socket")
	}
	clock.advance(80 * time.Second)

	assert.Zero(t, activity.quietFor(), "a write still in the socket is traffic")

	close(socket.release)
	select {
	case sendErr := <-sendReturned:
		require.NoError(t, sendErr)
	case <-time.After(testTimeout):
		t.Fatal("Send never returned after the socket was released")
	}
	assert.True(t, markedInFlight, "the mark must be stamped while the write still counts")
	clock.advance(5 * time.Second)
	assert.Equal(t, 5*time.Second, activity.quietFor(), "the finished write must stop counting")
}

// waitUntilLocked waits until tryLock fails, which means another goroutine holds the lock. It
// releases each lock it gets while it waits.
func waitUntilLocked(t *testing.T, tryLock func() bool, unlock func(), msg string) {
	t.Helper()
	waitFor(t, func() bool {
		if tryLock() {
			unlock()
			return false
		}
		return true
	}, msg)
}

// receiveInBackground runs one conn.Receive on its own goroutine and hands back its outcome.
func receiveInBackground(conn *adbConn) <-chan receivedMessage {
	results := make(chan receivedMessage, 1)
	go func() {
		message, err := conn.Receive()
		results <- receivedMessage{message: message, err: err}
	}()
	return results
}

func TestGivenAServerCloseWhenReadingThenOnlyTheSessionEndedCodeSkipsTheResume(t *testing.T) {
	tests := []struct {
		name        string
		closeCode   int
		endsSession bool
		wantResumed bool
		wantDials   int
	}{
		{name: "the server ended the session", closeCode: testsupport.SessionEndedCloseCode, endsSession: true, wantDials: 1},
		{name: "the relay lost its leg to the server", closeCode: testsupport.InternalErrorCloseCode, wantResumed: true, wantDials: 2},
		{
			name:      "a server without the session-ended code ended the session",
			closeCode: testsupport.InternalErrorCloseCode, endsSession: true, wantDials: 2,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			mock := testsupport.NewSessionEchoWSS(t)
			conn := newTestAdbConn(t, mock.URL, fastPolicy())
			handshake(t, conn)
			require.Len(t, mock.Sessions(), 1)
			session := mock.Sessions()[0]
			results := receiveInBackground(conn)

			if testCase.endsSession {
				mock.EndSession(session, testCase.closeCode)
			} else {
				session.DropAttached(testCase.closeCode)
			}

			if testCase.wantResumed {
				// The reader holds the seam from the reattach to the swap, so a Send from here on
				// waits for the resumed websocket.
				waitFor(t, func() bool { return mock.Reattaches() == 1 }, "the session was never resumed")
				request := []byte("000chost:devices")
				require.NoError(t, conn.Send(request))
				result := await(t, results, "the echo on the resumed session")
				require.NoError(t, result.err)
				assert.Equal(t, request, result.message)
			} else {
				result := await(t, results, "the read to end")
				assert.ErrorIs(t, result.err, io.EOF, "the read must end as a clean close")
			}
			assert.Equal(t, testCase.wantDials, mock.ConnectionsAccepted(), "websockets dialed, the first included")
		})
	}
}

func TestGivenASessionEndedCloseWhenReadThenOnlyAWholeUnmaskedOneSkipsTheResume(t *testing.T) {
	sessionEnded := closeFrame(testsupport.SessionEndedCloseCode, "session closing")
	tests := []struct {
		name        string
		closing     []byte
		wantResumes int
	}{
		{name: "a whole close", closing: sessionEnded},
		{name: "a close cut short after its code", closing: sessionEnded[:5], wantResumes: 1},
		{name: "a masked close", closing: maskedFrame(websocket.CloseFrame, sessionEnded[2:]), wantResumes: 1},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			// The scripted server opens a session, then closes. The resume dials the mock, which
			// does not know the session and refuses it.
			mock := testsupport.NewSessionEchoWSS(t)
			device := NewActiveDevice()
			require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL, Label: "test-device"}))
			opening := binaryFrame(newSessionControlFrame(controlStatusNew, testSessionUUID))
			scripted := &wsConn{conn: dialScriptedServer(t, [][]byte{opening, testCase.closing}, maxInboundMessageBytes), writeDeadline: sendDeadline}
			conn := newAdbConn(newTestDialer(testTimeout), device, testDeviceARN, RouteAdb, scripted, fastPolicy(),
				newActivityTracker(nil), noopLogger())
			t.Cleanup(func() { conn.Close() })

			_, err := conn.Receive()

			require.Error(t, err)
			assert.Equal(t, testCase.wantResumes, mock.ConnectionsAccepted(), "resume dials")
		})
	}
}

func TestGivenARefreshInFlightWhenTheServerEndsTheSessionThenTheReadEndsWithoutWaitingForIt(t *testing.T) {
	// The refresh holds the seam until its dial times out. A reader that waited for it would end
	// late, then dial a fresh session for a finished stream.
	mock := testsupport.NewSessionEchoWSS(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	handshake(t, conn)
	conn.seam.RLock()
	first := conn.readFrom.(*wsConn)
	conn.seam.RUnlock()
	results := receiveInBackground(conn)
	// Parked inside the socket read, as the pump is, not at the seam the refresh will hold.
	waitUntilLocked(t, first.receiveMutex.TryLock, first.receiveMutex.Unlock, "the reader never parked in Receive")
	redirect(t, conn, "wss://"+blackHoleListener(t))
	refreshContext, cancelRefresh := context.WithCancel(context.Background())
	t.Cleanup(cancelRefresh)
	refreshed := make(chan error, 1)
	go func() { refreshed <- conn.refresh(refreshContext, nil) }()
	waitUntilLocked(t, conn.seam.TryRLock, conn.seam.RUnlock, "the refresh never took the seam")

	mock.EndSession(mock.Sessions()[0], testsupport.SessionEndedCloseCode)

	result := await(t, results, "the read to end")
	assert.ErrorIs(t, result.err, io.EOF, "the read must end as a clean close")
	assert.Empty(t, refreshed, "the read must end while the refresh still holds the seam")
}
