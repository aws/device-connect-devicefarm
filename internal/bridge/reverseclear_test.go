package bridge

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// clearingDevice is an attachment at a serverMocks endpoint.
type clearingDevice struct {
	device   *ActiveDevice
	sessions *testsupport.SessionMockWSS
	reverse  *testsupport.ReverseMockWSS
}

// attachClearingDevice attaches a device with reverse on, a parked heartbeat and backoff as the
// redial wait, at serverMocks(answer), and returns at once: the attach clear lands at sessions while
// the notifier comes up.
func attachClearingDevice(t *testing.T, answer func(*testsupport.MockSession, []byte) []byte,
	backoff time.Duration, logger *slog.Logger) *clearingDevice {
	t.Helper()
	sessions, reverse := serverMocks(t, answer)
	return attachDeviceAt(t, sessions, reverse, backoff, logger)
}

// attachDeviceAt is attachClearingDevice at mocks the test already holds.
func attachDeviceAt(t *testing.T, sessions *testsupport.SessionMockWSS, reverse *testsupport.ReverseMockWSS,
	backoff time.Duration, logger *slog.Logger) *clearingDevice {
	t.Helper()
	params := notifierParams(parkedKeepAlive, backoff)
	params.MaxForwardRules = testMaxForwardRules
	params.MaxReverseRules = testMaxReverseRules
	device := NewActiveDevice(WithAttachmentFeatures(newTestDialer(testWait), testPolicy(), params, logger))
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: reverse.URL, Label: "test-device"}))
	return &clearingDevice{device: device, sessions: sessions, reverse: reverse}
}

// startClearingDevice is attachClearingDevice with the notifier ready, its attach clear answered as
// the server does, and the endpoint renewed to a fresh session mock answering with behavior, so the
// detach's clear alone meets behavior. The redial backoff is parked, so a dropped notifier stays down.
func startClearingDevice(t *testing.T, behavior reverseMockBehavior, logger *slog.Logger) *clearingDevice {
	t.Helper()
	clearing := attachClearingDevice(t, reverseMockBehavior{}.answer, parkedKeepAlive, logger)
	clearing.awaitReady(t)
	clearing.sessions = testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{OnMessage: behavior.answer})
	require.True(t, clearing.device.UpdateEndpoint(testDeviceARN, clearing.sessions.URL, "test-device"))
	return clearing
}

func (clearing *clearingDevice) notifier(t *testing.T) *notifierSupervisor {
	t.Helper()
	return attachedFeatures(t, clearing.device).notifier
}

// awaitReady waits for the notifier to report ready, which is after the attach clear settled.
func (clearing *clearingDevice) awaitReady(t *testing.T) {
	t.Helper()
	waitFor(t, clearing.notifier(t).notifierReady, "the notifier never became ready")
}

// holdRule records one reverse rule as the server's OKAY to an install would.
func (clearing *clearingDevice) holdRule(t *testing.T, deviceSpec string) {
	t.Helper()
	request, full := attachedFeatures(t, clearing.device).reverseRules.begin(adbwire.AdbActionForward, deviceSpec, 0)
	require.False(t, full, "the table must have room for the rule")
	request.settle(true)
}

// remove is what the control handler does for `device remove`: the clear, then the detach.
func (clearing *clearingDevice) remove(t *testing.T) {
	t.Helper()
	clearing.device.ClearReverseRulesAtServer(context.Background())
	_, cleared := clearing.device.Clear()
	require.True(t, cleared, "the device must have been attached")
}

// killforwardAllOnFreshSession is what a clear sends: the daemon's select, then the kill.
var killforwardAllOnFreshSession = canonicalSelect + framedKillforwardAll

func TestGivenReverseOnWhenTheNotifierFirstBecomesReadyThenOneKillforwardAllHasReachedTheServer(t *testing.T) {
	clearing := attachClearingDevice(t, reverseMockBehavior{}.answer, notifierPoll, noopLogger())

	clearing.awaitReady(t)

	assert.Equal(t, 1, clearing.sessions.SessionsOpened(),
		"the attach clear is one fresh session, answered before the attachment reports ready")
	assert.Equal(t, killforwardAllOnFreshSession, string(soleSessionInbound(clearing.sessions)))
	assert.Equal(t, 1, clearing.reverse.NotifiersAccepted())
	assert.Empty(t, clearing.reverse.Refusals(), "the clear rides the session route, not the notifier's")
}

func TestGivenAClearedAttachmentWhenItsNotifierRedialsThenNoSecondClearIsSent(t *testing.T) {
	clearing := attachClearingDevice(t, reverseMockBehavior{}.answer, notifierPoll, noopLogger())
	clearing.awaitReady(t)
	require.True(t, clearing.reverse.DropNotifier())

	// The second accept is the redial; ready again means its serveOne passed the clear step.
	awaitAccepts(t, clearing.reverse, 2)
	clearing.awaitReady(t)

	assert.Equal(t, 1, clearing.sessions.SessionsOpened(), "a redial is no attach: the customer's rules stay at the server")
}

func TestGivenTheServerRefusesTheAttachClearWhenTheNotifierIsReadyThenTheAttachmentServesAnyway(t *testing.T) {
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clearing := attachClearingDevice(t, reverseMockBehavior{refuseKillAll: true}.answer, notifierPoll, logger)

	clearing.awaitReady(t)

	assert.Equal(t, killforwardAllOnFreshSession, string(soleSessionInbound(clearing.sessions)))
	assert.Contains(t, logs.String(), `msg="could not clear the server's reverse rules at attach"`)
	assert.Contains(t, logs.String(), "bad port", "the Debug line carries the server's reason")
	assert.Equal(t, 1, clearing.reverse.NotifiersAccepted(), "a refused clear must not cost the notifier")
}

func TestGivenTheServerNeverAnswersTheAttachClearWhenTheBoundPassesThenTheAttachmentServesAndAHeldInstallIsRelayed(t *testing.T) {
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	// The kill stays unanswered: the clear waits out its bound while the seam holds an install.
	gated := startGatedProxy(t, logger)
	started := time.Now()
	// Ready follows the clear. This wait also starts the install's hold a settle window after the
	// clear, so the clear's bound passes first and opens the gate.
	assert.Never(t, gated.notifier(t).notifierReady, notifierSettle, notifierPoll,
		"the attachment must not report ready while its clear is unanswered")
	client := gated.sendBehindSelect(t, reverseInstall(1))
	gated.assertHeld(t, reverseInstall(1))

	require.NoError(t, client.SetDeadline(time.Now().Add(reverseClearTimeout+testWait)))
	assert.Equal(t, "OKAYOKAY", string(readN(t, client, 8)), "the held install is relayed once the bound passes")

	assert.GreaterOrEqual(t, time.Since(started), reverseClearTimeout/2, "the clear must wait for the verdict up to the bound")
	waitFor(t, gated.notifier(t).notifierReady, "the attachment must serve once the bound passes")
	assert.Contains(t, logs.String(), `msg="could not clear the server's reverse rules at attach"`)
	assert.Equal(t, 1, gated.reverse.NotifiersAccepted(), "an unanswered clear must not cost the notifier")
}

func TestGivenReverseRulesWhenTheDeviceIsRemovedThenOneKillforwardAllReachesTheServerWhileTheNotifierIsUp(t *testing.T) {
	clearing := startClearingDevice(t, reverseMockBehavior{}, noopLogger())
	clearing.holdRule(t, "tcp:1")

	clearing.device.ClearReverseRulesAtServer(context.Background())

	assert.Equal(t, 1, clearing.sessions.SessionsOpened(), "the clear is one fresh session at the renewed endpoint")
	assert.Equal(t, killforwardAllOnFreshSession, string(soleSessionInbound(clearing.sessions)))
	assert.Zero(t, clearing.reverse.NotifiersClosed(), "the notifier must still be registered while the server reads the kill")
}

func TestGivenNoReverseRulesWhenTheDeviceIsRemovedThenNothingIsDialed(t *testing.T) {
	clearing := startClearingDevice(t, reverseMockBehavior{}, noopLogger())

	clearing.remove(t)

	assert.Zero(t, clearing.sessions.ConnectionsAccepted(), "an empty table leaves nothing to clear")
}

func TestGivenTheNotifierDownWhenTheDeviceIsRemovedThenNothingIsDialed(t *testing.T) {
	// The server refuses a reverse request with no notifier registered, so a clear only costs
	// the wait.
	clearing := startClearingDevice(t, reverseMockBehavior{}, noopLogger())
	clearing.holdRule(t, "tcp:1")
	notifier := attachedFeatures(t, clearing.device).notifier
	require.True(t, clearing.reverse.DropNotifier())
	waitFor(t, func() bool { return !notifier.notifierReady() }, "the dropped notifier never went down")

	clearing.remove(t)

	assert.Zero(t, clearing.sessions.ConnectionsAccepted(), "no notifier, no clear")
}

func TestGivenTheServerRefusesTheKillWhenTheDeviceIsRemovedThenTheFailureIsLoggedAndTheDetachStillHappens(t *testing.T) {
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	clearing := startClearingDevice(t, reverseMockBehavior{refuseKillAll: true}, logger)
	clearing.holdRule(t, "tcp:1")

	clearing.remove(t)

	assert.Equal(t, killforwardAllOnFreshSession, string(soleSessionInbound(clearing.sessions)))
	assert.Contains(t, logs.String(), `msg="could not clear the server's reverse rules before the detach"`)
	assert.Contains(t, logs.String(), "bad port", "the Debug line carries the server's reason")
	_, present := clearing.device.Current()
	assert.False(t, present, "a refused clear must not keep the device attached")
}

func TestGivenTheServerBreaksTheExchangeWhenTheDeviceIsRemovedThenTheStepIsLoggedAndTheDetachStillHappens(t *testing.T) {
	// Each row is a renewed endpoint that breaks the exchange at one step. The handler reads
	// the select first, so the break lands on the read it names and not on the send.
	breakAfterSelect := func(reply []byte) func(ws *websocket.Conn) {
		return func(ws *websocket.Conn) {
			ws.PayloadType = websocket.BinaryFrame
			var selectFrame []byte
			_ = websocket.Message.Receive(ws, &selectFrame)
			if reply != nil {
				_ = websocket.Message.Send(ws, reply)
			}
			ws.Close()
		}
	}
	testCases := []struct {
		name   string
		serve  func(ws *websocket.Conn)
		logged string
	}{
		{name: "closes before the control frame", serve: breakAfterSelect(nil), logged: "read the session control frame"},
		{name: "refuses the session", serve: breakAfterSelect(testsupport.FrameError("no slot")),
			logged: "refused the session: no slot"},
		{name: "answers with an arm that mints no session", serve: breakAfterSelect(testsupport.FrameConnectorHello(testReverseToken)),
			logged: "no session was minted: unexpected control frame connectorHello"},
		{name: "closes before the select's status word", serve: breakAfterSelect(testsupport.FrameNewSession(testSessionUUID)),
			logged: "transport select"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			clearing := startClearingDevice(t, reverseMockBehavior{}, logger)
			clearing.holdRule(t, "tcp:1")
			broken := testsupport.NewMockWSS(t, testCase.serve)
			require.True(t, clearing.device.UpdateEndpoint(testDeviceARN, broken, "test-device"))

			clearing.remove(t)

			assert.Contains(t, logs.String(), `msg="could not clear the server's reverse rules before the detach"`)
			assert.Contains(t, logs.String(), testCase.logged, "the Debug line names the step that broke")
			assert.NotContains(t, logs.String(), testReverseToken, "a frame body is never logged")
			_, present := clearing.device.Current()
			assert.False(t, present, "a broken clear must not keep the device attached")
		})
	}
}

func TestGivenTheServerUnreachableWhenTheDeviceIsRemovedThenTheRemoveReturnsWithinTheBound(t *testing.T) {
	// A listener that accepts and never answers parks the dial until its timeout.
	clearing := startClearingDevice(t, reverseMockBehavior{}, noopLogger())
	clearing.holdRule(t, "tcp:1")
	blackHole, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { blackHole.Close() })
	require.True(t, clearing.device.UpdateEndpoint(testDeviceARN, "wss://"+blackHole.Addr().String(), "test-device"))

	started := time.Now()
	clearing.remove(t)

	assert.Less(t, time.Since(started), reverseClearTimeout+testWait, "the remove must not outwait the bound")
	assert.Zero(t, clearing.sessions.ConnectionsAccepted())
}
