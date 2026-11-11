// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/forward"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// notifierPoll is how often a supervisor test samples the mock's counters.
const notifierPoll = 2 * time.Millisecond

// notifierKeepAlive is the heartbeat interval when a test wants pings: wide enough to answer under
// -race.
const notifierKeepAlive = 20 * time.Millisecond

// notifierSettle is how long a test watches for a redial that must never come.
const notifierSettle = 100 * time.Millisecond

// parkedKeepAlive outlasts every test, so no ping fires.
const parkedKeepAlive = time.Hour

// parkedBackoff outlasts every test, so no backoff redial fires.
const parkedBackoff = 10 * time.Second

// fastBackoff redials almost at once.
const fastBackoff = time.Millisecond

// notifierListenPort stands in for the daemon's own adb port. Non-zero, or every announcement
// meets the unknown-port refusal; outside the ephemeral range, so no test target holds it.
const notifierListenPort = 5037

// secondReverseToken is a second parked connection's token, for two-connector tests.
const secondReverseToken = "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"

// notifierParams is one supervisor test's timing.
func notifierParams(keepAlive, backoff time.Duration) AttachmentParams {
	return AttachmentParams{NotifierKeepAlive: keepAlive, NotifierBackoffInitial: backoff,
		NotifierBackoffMax: 4 * backoff, ReverseEnabled: true, ListenPort: notifierListenPort,
		MaxDeviceStreams: testMaxDeviceStreams}
}

// announcingSupervisor builds a supervisor that never runs, for tests that call announce directly:
// the port screen and the budget live there, so no notifier need be open. A reverse rule names
// each of admittedPorts; every other port is refused.
func announcingSupervisor(device *ActiveDevice, params AttachmentParams,
	table *forward.ForwardRulesTable, budget *streamBudget, admittedPorts ...int) *notifierSupervisor {
	cfg := &featureConfig{dialer: newTestDialer(testWait), policy: testPolicy(), params: params, logger: noopLogger()}
	return newNotifierSupervisor(device, cfg, testDeviceARN, table, installedReverseRules(admittedPorts...), budget)
}

// announceOnce drives one announcement the port screen refuses, so it returns at once, and returns
// the screen's reason. A reverse rule names customerPort, so only the class under test can refuse it.
func announceOnce(t *testing.T, device *ActiveDevice, params AttachmentParams,
	table *forward.ForwardRulesTable, customerPort int) string {
	t.Helper()
	supervisor := announcingSupervisor(device, params, table, &streamBudget{limit: testMaxDeviceStreams}, customerPort)
	supervisor.announce(context.Background(), testReverseToken, customerPort)
	return supervisor.refuseCustomerPort(customerPort)
}

// installedReverseRules builds a table holding one confirmed rule per customer port.
func installedReverseRules(customerPorts ...int) *reverseRules {
	rules := newReverseRules(testMaxReverseRules)
	for index, customerPort := range customerPorts {
		rules.rules[tcpSpec(index+1)] = &ruleSlot{installed: true, customerPort: customerPort}
	}
	return rules
}

// ruledSupervisor builds a supervisor that never runs, screening announcements against rules.
func ruledSupervisor(t *testing.T, mock *testsupport.ReverseMockWSS, rules *reverseRules) *notifierSupervisor {
	t.Helper()
	supervisor := announcingSupervisor(deviceAt(t, mock.URL), notifierParams(parkedKeepAlive, notifierPoll),
		nil, &streamBudget{limit: testMaxDeviceStreams})
	supervisor.rules = rules
	return supervisor
}

// serverMocks is one endpoint serving both routes, as the server does: the reverse mock for the
// notifier, a session mock answering with answer for the clears. A test whose notifier comes up
// needs both: the attach clear dials the session route.
func serverMocks(t *testing.T, answer func(*testsupport.MockSession, []byte) []byte) (
	*testsupport.SessionMockWSS, *testsupport.ReverseMockWSS) {
	t.Helper()
	return testsupport.NewServerMockWSS(t, testsupport.SessionMockConfig{OnMessage: answer})
}

// serverReverseMock is the reverse half of serverMocks answering clears as the server does, for a
// test that brings a notifier up and reads nothing on the session side.
func serverReverseMock(t *testing.T) *testsupport.ReverseMockWSS {
	t.Helper()
	_, reverse := serverMocks(t, reverseMockBehavior{}.answer)
	return reverse
}

// deviceAt binds a device at endpoint.
func deviceAt(t *testing.T, endpoint string) *ActiveDevice {
	t.Helper()
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "test-device"}))
	return device
}

// announceInBackground runs announce on its own goroutine, as notifierConn.run does: an admitted
// announcement blocks for the connector's life. The channel closes when announce returns, which
// is after its slot is released. Cleanup ends the connector.
func announceInBackground(t *testing.T, supervisor *notifierSupervisor, token string,
	customerPort int) (ended <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		supervisor.announce(ctx, token, customerPort)
	}()
	return done
}

// lockedBuffer collects log lines a supervisor goroutine writes while the test reads them.
type lockedBuffer struct {
	contentsMutex sync.Mutex
	contents      bytes.Buffer
}

func (buffer *lockedBuffer) Write(p []byte) (int, error) {
	buffer.contentsMutex.Lock()
	defer buffer.contentsMutex.Unlock()
	return buffer.contents.Write(p)
}

func (buffer *lockedBuffer) String() string {
	buffer.contentsMutex.Lock()
	defer buffer.contentsMutex.Unlock()
	return buffer.contents.String()
}

// boundedContext ends an announcement that wrongly admits, so the test fails instead of hanging
// for the connector's life.
func boundedContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testWait)
	t.Cleanup(cancel)
	return ctx
}

// assertNothingRedeemed asserts no connector reached the server: a dial before the refusal would
// show as a redemption, or as a server refusal once the token is unparked.
func assertNothingRedeemed(t *testing.T, mock *testsupport.ReverseMockWSS) {
	t.Helper()
	assert.Zero(t, mock.ConnectorsAccepted(), "a refused port must cost no connector")
	assert.Empty(t, mock.Refusals(), "a refused port must not even open a websocket")
}

// runSupervisor starts a supervisor against mock for a bound device, and returns it with the cancel
// that stands in for a detach. The cleanup cancels, so no test leaves a redial loop running. A
// reverse rule names each of admittedPorts; every other announced port is refused.
func runSupervisor(t *testing.T, mock *testsupport.ReverseMockWSS, params AttachmentParams,
	admittedPorts ...int) (*ActiveDevice, *notifierSupervisor, context.CancelFunc) {
	t.Helper()
	return runSupervisorAt(t, mock.URL, params, noopLogger(), admittedPorts...)
}

// runSupervisorAt is runSupervisor at any endpoint, logging to logger.
func runSupervisorAt(t *testing.T, endpoint string, params AttachmentParams, logger *slog.Logger,
	admittedPorts ...int) (*ActiveDevice, *notifierSupervisor, context.CancelFunc) {
	t.Helper()
	return runSettlingSupervisorAt(t, endpoint, params, logger, notifierSettledAfter, admittedPorts...)
}

// runSettlingSupervisorAt is runSupervisorAt with settledAfter in place of notifierSettledAfter.
func runSettlingSupervisorAt(t *testing.T, endpoint string, params AttachmentParams, logger *slog.Logger,
	settledAfter time.Duration, admittedPorts ...int) (*ActiveDevice, *notifierSupervisor, context.CancelFunc) {
	t.Helper()
	device := deviceAt(t, endpoint)
	cfg := &featureConfig{dialer: newTestDialer(testWait), params: params, logger: logger}
	supervisor := newNotifierSupervisor(device, cfg, testDeviceARN, nil, installedReverseRules(admittedPorts...),
		&streamBudget{limit: params.MaxDeviceStreams})
	supervisor.settledAfter = settledAfter
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go supervisor.run(ctx)
	return device, supervisor, cancel
}

// rivalNotifier plays another client: its notifier holds mock's slot, so the supervisor under test
// is refused with notifierSlotTakenReason. Cleanup closes it.
func rivalNotifier(t *testing.T, mock *testsupport.ReverseMockWSS) *notifierConn {
	t.Helper()
	rival, err := dialTestNotifier(t, mock.URL, testWait)
	require.NoError(t, err)
	t.Cleanup(func() { rival.Close() })
	return rival
}

// awaitAccepts fails unless the mock accepted at least want notifiers. A floor, never an exact
// count: the mock frees its slot only once the dead notifier's goroutine sees the close.
func awaitAccepts(t *testing.T, mock *testsupport.ReverseMockWSS, want int) {
	t.Helper()
	require.Eventually(t, func() bool { return mock.NotifiersAccepted() >= want }, testWait,
		notifierPoll, "the supervisor must open at least %d notifiers", want)
}

func TestGivenASupervisorWhenItRunsThenExactlyOneNotifierIsOpened(t *testing.T) {
	mock := serverReverseMock(t)
	_, supervisor, _ := runSupervisor(t, mock, notifierParams(parkedKeepAlive, notifierPoll))

	require.Eventually(t, supervisor.notifierReady, testWait, notifierPoll,
		"the supervisor must report its live notifier")

	assert.Equal(t, 1, mock.NotifiersAccepted())
	assert.Empty(t, mock.Refusals(), "the hello must satisfy the server")
	assert.Never(t, func() bool { return mock.NotifiersAccepted() > 1 }, notifierSettle, notifierPoll,
		"one attachment holds one notifier, not a stream of them")
}

func TestGivenARunningSupervisorWhenItsContextEndsThenTheNotifierCloses(t *testing.T) {
	mock := serverReverseMock(t)
	_, supervisor, cancel := runSupervisor(t, mock, notifierParams(parkedKeepAlive, notifierPoll))
	awaitAccepts(t, mock, 1)

	cancel()

	require.Eventually(t, func() bool { return mock.NotifiersClosed() == 1 }, testWait, notifierPoll,
		"a detach must close the notifier's socket, not merely stop reading it")
	assert.False(t, supervisor.notifierReady(), "a closed notifier is not ready")
}

func TestGivenAnOpenNotifierWhenTheHeartbeatIntervalElapsesThenAPingIsSentAndAPongClearsIt(t *testing.T) {
	mock := serverReverseMock(t)
	_, supervisor, _ := runSupervisor(t, mock, notifierParams(notifierKeepAlive, notifierPoll))

	require.Eventually(t, func() bool { return mock.PingsSeen() >= 4 }, testWait, notifierPoll,
		"the notifier must keep pinging on its interval")

	// Pings past notifierMissBudget on one notifier prove the pongs land: that many unanswered kill it.
	assert.Equal(t, 1, mock.NotifiersAccepted(), "answered pings must never cost a redial")
	assert.True(t, supervisor.notifierReady())
}

func TestGivenTwoMissedPongsWhenTheDeadlinePassesThenTheNotifierIsKilledAndRedialed(t *testing.T) {
	mock := serverReverseMock(t)
	mock.SwallowPongs(true)
	runSupervisor(t, mock, notifierParams(notifierKeepAlive, notifierPoll))

	awaitAccepts(t, mock, 2)

	assert.GreaterOrEqual(t, mock.PingsSeen(), 2, "the kill must follow two unanswered pings")
	assert.GreaterOrEqual(t, mock.NotifiersClosed(), 1, "a notifier that stopped answering must be closed")
}

func TestGivenAnUnansweredNotifierWhenItIsKilledThenItCostExactlyTheMissBudgetInPings(t *testing.T) {
	mock := serverReverseMock(t)
	mock.SwallowPongs(true)
	device, supervisor, _ := runSupervisor(t, mock, notifierParams(notifierKeepAlive, notifierPoll))
	require.Eventually(t, supervisor.notifierReady, testWait, notifierPoll, "no notifier to count on")

	// A notifier that out-served one keepalive redials at once whatever the backoff, so the device
	// is detached instead: the redial finds no endpoint and the count stays on the one notifier.
	_, cleared := device.Clear()
	require.True(t, cleared)

	require.Eventually(t, func() bool { return mock.NotifiersClosed() == 1 }, testWait, notifierPoll,
		"an unanswered notifier must be killed")

	// The literal, not the constant: this is what pins the budget at two.
	assert.Equal(t, 2, mock.PingsSeen(), "two unanswered pings kill the notifier, and no more")
}

func TestGivenUnsetTimingsWhenTheSupervisorRunsThenItStaysClosed(t *testing.T) {
	mock := serverReverseMock(t)
	// Reverse on, so only the unset cadences can keep the notifier closed.
	runSupervisor(t, mock, AttachmentParams{ReverseEnabled: true})

	assert.Never(t, func() bool { return mock.NotifiersAccepted() > 0 }, notifierSettle, notifierPoll,
		"a supervisor with no timings must not dial")
}

func TestGivenReverseOffWhenTheSupervisorRunsThenNeitherANotifierNorAClearIsDialed(t *testing.T) {
	sessions, mock := serverMocks(t, reverseMockBehavior{}.answer)
	// Cadences a running supervisor would accept, so the closed notifier can only be reverse off.
	params := notifierParams(notifierKeepAlive, notifierPoll)
	params.ReverseEnabled = false
	runSupervisor(t, mock, params)

	assert.Never(t, func() bool { return mock.NotifiersAccepted() > 0 || sessions.ConnectionsAccepted() > 0 },
		notifierSettle, notifierPoll, "a supervisor with reverse off must dial nothing: no notifier, no clear")
}

func TestGivenAnOpenNamingTheDaemonsOwnPortWhenAnnouncedThenItIsRefusedBeforeAnyDial(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	// A live listener on the refused port, so the refusal stops the dial, not a dead port.
	daemonPortTarget := startLocalTarget(t)
	device := deviceAt(t, mock.URL)

	reason := announceOnce(t, device, AttachmentParams{ListenPort: daemonPortTarget.port}, nil, daemonPortTarget.port)

	assert.Equal(t, "it is the daemon's own adb port", reason)
	daemonPortTarget.requireNoDial(t)
	assertNothingRedeemed(t, mock)
}

func TestGivenAnOpenNamingAForwardListenerPortWhenAnnouncedThenItIsRefusedBeforeAnyDial(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL, Label: "test-device"}))
	features := attachedFeatures(t, device)
	// The rule's own listener holds the port, so a dial would land in a forwarded stream.
	port := installRule(t, features, "tcp:9090")

	reason := announceOnce(t, device, AttachmentParams{ListenPort: notifierListenPort}, features.table, port)

	assert.Equal(t, "a forward rule listens on it", reason)
	assertNothingRedeemed(t, mock)
}

func TestGivenAnAttachmentWithNoDaemonPortWhenAnOpenIsAnnouncedThenItIsRefusedBeforeAnyDial(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	target := startLocalTarget(t)
	device := deviceAt(t, mock.URL)

	reason := announceOnce(t, device, AttachmentParams{}, nil, target.port)

	assert.Equal(t, "the daemon's own adb port is unknown", reason)
	target.requireNoDial(t)
	assertNothingRedeemed(t, mock)
}

func TestGivenAnInstalledReverseRuleWhenAnOpenNamesItsCustomerPortThenItIsAdmitted(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	supervisor := ruledSupervisor(t, mock, installedReverseRules(target.port))

	announceInBackground(t, supervisor, testReverseToken, target.port)

	target.awaitAccepted(t)
	assert.Equal(t, 1, mock.ConnectorsAccepted())
}

func TestGivenNoReverseRuleNamingAPortWhenAnOpenNamesItThenItIsRefusedBeforeAnyDial(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.Connect(testReverseToken)
	// A live listener, so only the screen can stop the dial.
	target := startLocalTarget(t)
	supervisor := ruledSupervisor(t, mock, newReverseRules(testMaxReverseRules))

	supervisor.announce(boundedContext(t), testReverseToken, target.port)

	target.requireNoDial(t)
	assertNothingRedeemed(t, mock)
}

func TestGivenAPendingInstallWhenAnOpenNamesItsPortThenItIsAdmitted(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	rules := newReverseRules(testMaxReverseRules)
	// The server can announce an open the moment adbd listens, before the client reads OKAY.
	_, full := rules.begin(adbwire.AdbActionForward, "tcp:1", target.port)
	require.False(t, full)

	announceInBackground(t, ruledSupervisor(t, mock, rules), testReverseToken, target.port)

	target.awaitAccepted(t)
}

func TestGivenAReboundRuleWhenAnOpenNamesTheOldPortThenItIsRefused(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.Connect(testReverseToken)
	oldTarget := startLocalTarget(t)
	newTarget := startLocalTarget(t)
	rules := installedReverseRules(oldTarget.port)
	rebind, full := rules.begin(adbwire.AdbActionForward, "tcp:1", newTarget.port)
	require.False(t, full, "a known spec is a rebind")
	rebind.settle(true)

	ruledSupervisor(t, mock, rules).announce(boundedContext(t), testReverseToken, oldTarget.port)

	oldTarget.requireNoDial(t)
	assertNothingRedeemed(t, mock)
	assert.True(t, rules.allowsCustomerPort(newTarget.port), "the rebind moved the port")
}

func TestGivenKillforwardAllWhenALateOpenArrivesThenItIsRefused(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	rules := installedReverseRules(target.port)
	killAll, _ := rules.begin(adbwire.AdbActionKillForwardAll, "", 0)
	killAll.settle(true)

	ruledSupervisor(t, mock, rules).announce(boundedContext(t), testReverseToken, target.port)

	target.requireNoDial(t)
	assertNothingRedeemed(t, mock)
}

func TestGivenARefusedOpenWhenItIsRejectedThenTheNotifierSurvivesAndALaterOpenSucceeds(t *testing.T) {
	mock := serverReverseMock(t)
	mock.Connect(testReverseToken)
	daemonPortTarget := startLocalTarget(t)
	target := startLocalTarget(t)
	params := notifierParams(parkedKeepAlive, notifierPoll)
	params.ListenPort = daemonPortTarget.port
	// Rules name both ports, so the daemon-port class alone refuses the first open.
	_, supervisor, _ := runSupervisor(t, mock, params, target.port, daemonPortTarget.port)
	require.Eventually(t, supervisor.notifierReady, testWait, notifierPoll, "no notifier to announce on")

	require.True(t, mock.Announce(testReverseToken, daemonPortTarget.port))
	require.True(t, mock.Announce(testReverseToken, target.port))

	target.awaitAccepted(t)
	assert.Equal(t, 1, mock.ConnectorsAccepted(), "only the admitted open may redeem a token")
	// A refused open that dialed anyway would lose the race for the parked token and be refused by
	// the server instead, which is the same redemption count.
	assert.Empty(t, mock.Refusals(), "a refused port must not open a websocket")
	assert.True(t, supervisor.notifierReady(), "a refused open must not cost the notifier")
	daemonPortTarget.requireNoDial(t)
}

func TestGivenTheBudgetSpentWhenAnOpenIsAnnouncedThenItIsRefusedBeforeAnyDial(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	// A parked connection, so a connector that dialed anyway would redeem it.
	mock.Connect(testReverseToken)
	target := startLocalTarget(t)
	budget := &streamBudget{limit: 1}
	require.True(t, budget.admit(), "a forwarded stream holds the one slot")
	supervisor := announcingSupervisor(deviceAt(t, mock.URL), notifierParams(parkedKeepAlive, notifierPoll),
		nil, budget, target.port)
	logs := &lockedBuffer{}
	supervisor.cfg.logger = slog.New(slog.NewTextHandler(logs, nil))

	supervisor.announce(boundedContext(t), testReverseToken, target.port)

	target.requireNoDial(t)
	assertNothingRedeemed(t, mock)
	assert.Contains(t, logs.String(), `level=WARN msg="refused a reverse connection: every device-stream slot is taken"`)
	assert.Contains(t, logs.String(), "maxDeviceStreams=1", "the Warn must name the limit")
	assert.NotContains(t, logs.String(), testReverseToken, "the token must not reach the log")
}

func TestGivenAConnectorThatEndsWhenAnotherOpenIsAnnouncedThenItIsAdmitted(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.Connect(testReverseToken)
	mock.Connect(secondReverseToken)
	target := startLocalTarget(t)
	budget := &streamBudget{limit: 1}
	supervisor := announcingSupervisor(deviceAt(t, mock.URL), notifierParams(parkedKeepAlive, notifierPoll),
		nil, budget, target.port)
	firstEnded := announceInBackground(t, supervisor, testReverseToken, target.port)
	targetConn := target.awaitAccepted(t)
	// Returns at once: the live connector holds the one slot.
	supervisor.announce(boundedContext(t), secondReverseToken, target.port)
	require.Equal(t, 1, mock.ConnectorsAccepted(), "a second connector must not spend a slot the first holds")

	// The customer's server hangs up, so the splice ends without a reattach attempt.
	require.NoError(t, targetConn.Close())
	awaitStreamEnd(t, firstEnded)
	announceInBackground(t, supervisor, secondReverseToken, target.port)

	target.awaitAccepted(t)
	assert.Equal(t, 2, mock.ConnectorsAccepted(), "the freed slot must admit the next open")
}

// prologueFrames is what one forwarded stream sends before any app byte: the select and the service.
const prologueFrames = 2

func TestGivenAConnectorHoldingASlotWhenForwardedStreamsFillTheBudgetThenTheNextIsRefusedUntilItEnds(t *testing.T) {
	reverseMock := testsupport.NewReverseMockWSS(t)
	reverseMock.Connect(testReverseToken)
	target := startLocalTarget(t)
	prologueMock := newPrologueMock(t, slices.Repeat([]string{"OKAY"}, prologueFrames*testMaxDeviceStreams)...)
	_, opener := openerAt(t, prologueMock.URL)
	supervisor := announcingSupervisor(deviceAt(t, reverseMock.URL), notifierParams(parkedKeepAlive, notifierPoll),
		nil, opener.budget, target.port)
	connectorEnded := announceInBackground(t, supervisor, testReverseToken, target.port)
	targetConn := target.awaitAccepted(t)
	// One connector plus forwarded streams fill the budget.
	for range testMaxDeviceStreams - 1 {
		runStream(t, opener)
	}
	waitFor(t, func() bool { return len(prologueMock.Frames()) == prologueFrames*(testMaxDeviceStreams-1) },
		"the forwarded streams beside the connector never opened")

	appConn, ended := runStream(t, opener)

	awaitStreamEnd(t, ended)
	assert.Empty(t, drainClosed(t, appConn), "a forwarded connection above the shared cap must get zero bytes")
	assert.Equal(t, testMaxDeviceStreams-1, prologueMock.ConnectionsAccepted(),
		"a connector's slot must refuse a forwarded connection without a dial")

	// The connector ending frees its slot to a forwarded stream.
	require.NoError(t, targetConn.Close())
	awaitStreamEnd(t, connectorEnded)
	runStream(t, opener)
	waitFor(t, func() bool { return len(prologueMock.Frames()) == prologueFrames*testMaxDeviceStreams },
		"the freed slot never admitted a forwarded stream")
}

func TestGivenADroppedNotifierWhenItDiesThenItIsRedialedWithBackoff(t *testing.T) {
	mock := serverReverseMock(t)
	const backoff = 50 * time.Millisecond
	// A heartbeat interval longer than the test keeps this on the backoff path, not the
	// immediate-redial one.
	runSupervisor(t, mock, notifierParams(parkedKeepAlive, backoff))
	awaitAccepts(t, mock, 1)

	dropTime := time.Now()
	require.True(t, mock.DropNotifier(), "the mock must hold a notifier to drop")

	awaitAccepts(t, mock, 2)
	// The jitter band can shorten the wait.
	assert.GreaterOrEqual(t, time.Since(dropTime), backoff*9/10,
		"a short-lived notifier must wait out its backoff before redialing")
}

func TestGivenANotifierThatServedPastTheSettledTimeWhenItDropsThenItRedialsWithoutWaitingTheBackoff(t *testing.T) {
	mock := serverReverseMock(t)
	// The backoff outlasts the whole test, so a redial inside it can only be the immediate one a
	// notifier earns by serving longer than the settled time.
	runSettlingSupervisorAt(t, mock.URL, notifierParams(notifierKeepAlive, parkedBackoff), noopLogger(), notifierKeepAlive)
	require.Eventually(t, func() bool { return mock.PingsSeen() >= 2 }, testWait, notifierPoll,
		"the notifier must serve past the settled time")

	require.True(t, mock.DropNotifier())

	awaitAccepts(t, mock, 2)
}

func TestGivenANotifierThatServedLongerThanOnePingIntervalButNotSettledWhenItDropsThenItWaitsTheBackoff(t *testing.T) {
	// A notifier that dies after a few pings each time must not redial in a tight loop.
	mock := serverReverseMock(t)
	const backoff = 300 * time.Millisecond
	runSupervisor(t, mock, notifierParams(notifierKeepAlive, backoff))
	require.Eventually(t, func() bool { return mock.PingsSeen() >= 2 }, testWait, notifierPoll,
		"the notifier must serve past one heartbeat interval")

	dropTime := time.Now()
	require.True(t, mock.DropNotifier())

	awaitAccepts(t, mock, 2)
	// The jitter band can shorten the wait.
	assert.GreaterOrEqual(t, time.Since(dropTime), backoff*9/10,
		"a notifier that served under the settled time waits out its backoff")
}

func TestGivenARefusedNotifierWhenRedialedRepeatedlyThenItRetriesUntilTheSlotFrees(t *testing.T) {
	mock := serverReverseMock(t)
	rival := rivalNotifier(t, mock)
	runSupervisor(t, mock, notifierParams(parkedKeepAlive, fastBackoff))

	require.Eventually(t, func() bool { return len(mock.Refusals()) >= 3 }, testWait, notifierPoll,
		"a refused supervisor must keep redialing")

	assert.Equal(t, "notifier already registered", mock.Refusals()[0])
	assert.Equal(t, 1, mock.NotifiersAccepted(), "a refused dial must not count as a notifier")
	require.NoError(t, rival.Close())
	awaitAccepts(t, mock, 2)
}

// slotTakenWarn and slotRegainedWarn are markSlotHeld's Warns as the text handler renders them.
const (
	slotTakenWarn    = `level=WARN msg="another notifier holds reverse on this device, another client's or this client's dropped one until the server times it out; adb reverse is refused here meanwhile"`
	slotRegainedWarn = `level=WARN msg="this client's notifier holds reverse on the device"`
)

func TestGivenARivalNotifierWhenItRefusesTheHelloAndLaterLeavesThenEachSlotChangeLogsOnce(t *testing.T) {
	mock := serverReverseMock(t)
	rival := rivalNotifier(t, mock)
	logs := &lockedBuffer{}
	_, supervisor, _ := runSupervisorAt(t, mock.URL, notifierParams(parkedKeepAlive, fastBackoff),
		slog.New(slog.NewTextHandler(logs, nil)))
	waitFor(t, supervisor.notifierSlotHeld, "a slot-taken refusal must mark the slot held")
	require.Eventually(t, func() bool { return len(mock.Refusals()) >= 3 }, testWait, notifierPoll,
		"the supervisor must keep redialing while held")

	require.NoError(t, rival.Close())

	waitFor(t, supervisor.notifierReady, "the supervisor must take the freed slot")
	assert.False(t, supervisor.notifierSlotHeld(), "READY clears the mark")
	assert.Equal(t, 1, strings.Count(logs.String(), slotTakenWarn), "one Warn when the slot is taken, none per retry")
	assert.Equal(t, 1, strings.Count(logs.String(), slotRegainedWarn), "one Warn when the slot is regained")
}

func TestGivenARefusedNotifierWhenTheSupervisorRedialsThenTheEndIsLoggedAtWarn(t *testing.T) {
	// A rival holds the slot, so every dial is refused and the supervisor redials on its backoff.
	// The handler's default level is Info, so the line is visible without --debug.
	mock := serverReverseMock(t)
	rivalNotifier(t, mock)
	logs := &lockedBuffer{}
	runSupervisorAt(t, mock.URL, notifierParams(parkedKeepAlive, fastBackoff), slog.New(slog.NewTextHandler(logs, nil)))

	require.Eventually(t, func() bool {
		return strings.Contains(logs.String(), `level=WARN msg="the reverse notifier ended; redialing"`)
	}, testWait, notifierPoll, "a refused notifier's end must be logged at the default level")

	assert.Contains(t, logs.String(), notifierSlotTakenReason, "the line carries the server's reason")
}

func TestGivenARefusalWithAnotherReasonWhenTheHelloIsRefusedThenTheSlotDoesNotReadHeld(t *testing.T) {
	// A scripted server refuses the hello with another reason; the parked backoff stops a second dial.
	server := newNotifierServer(t, testsupport.FrameError("no reverse interceptor"))
	_, supervisor, _ := runSupervisorAt(t, server.url, notifierParams(parkedKeepAlive, parkedBackoff), noopLogger())

	await(t, server.closed, "the refused notifier to close")

	assert.Never(t, supervisor.notifierSlotHeld, notifierSettle, notifierPoll,
		"only the server's slot-taken reason marks the slot held")
}

func TestGivenAHeldSlotWhenTheNextDialFailsThenTheSlotStillReadsHeld(t *testing.T) {
	mock := serverReverseMock(t)
	rivalNotifier(t, mock)
	logs := &lockedBuffer{}
	device, supervisor, _ := runSupervisorAt(t, mock.URL, notifierParams(parkedKeepAlive, fastBackoff),
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	waitFor(t, supervisor.notifierSlotHeld, "a slot-taken refusal must mark the slot held")

	// A closed port: every redial fails before any hello, which says nothing about the slot.
	require.True(t, device.UpdateEndpoint(testDeviceARN, "wss://127.0.0.1:1", "test-device"))

	require.Eventually(t, func() bool { return failedDialLogged(logs.String()) }, testWait, notifierPoll,
		"a redial must fail against the closed port")
	assert.True(t, supervisor.notifierSlotHeld(), "only READY clears the mark")
}

// failedDialLogged reports a notifier end that was not the server's refusal: the dial itself failed.
func failedDialLogged(logs string) bool {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "the reverse notifier ended") && !strings.Contains(line, notifierSlotTakenReason) {
			return true
		}
	}
	return false
}

func TestGivenACancelledSupervisorWhenTheNotifierWouldRedialThenItStops(t *testing.T) {
	mock := serverReverseMock(t)
	_, _, cancel := runSupervisor(t, mock, notifierParams(parkedKeepAlive, fastBackoff))
	awaitAccepts(t, mock, 1)
	// Prove the loop redials before asking it to stop, so a pass cannot be a loop that never did.
	require.True(t, mock.DropNotifier())
	awaitAccepts(t, mock, 2)

	cancel()

	assert.Never(t, func() bool { return mock.NotifiersAccepted() > 2 }, notifierSettle, notifierPoll,
		"a stopped supervisor must not redial again")
}

func TestGivenARenewedEndpointWhenTheNotifierRedialsThenItDialsTheNewOne(t *testing.T) {
	lapsed := testsupport.NewReverseMockWSS(t)
	renewed := testsupport.NewReverseMockWSS(t)
	device, _, _ := runSupervisor(t, lapsed, notifierParams(parkedKeepAlive, fastBackoff))
	awaitAccepts(t, lapsed, 1)

	require.True(t, device.UpdateEndpoint(testDeviceARN, renewed.URL, "renewed"))
	require.True(t, lapsed.DropNotifier())

	awaitAccepts(t, renewed, 1)
	assert.Equal(t, 1, lapsed.NotifiersAccepted(), "a redial must not return to the lapsed endpoint")
}

func TestGivenARedialDelayWhenAdvancedThenItDoublesUntilItsCapAndNeverShrinks(t *testing.T) {
	assert.Equal(t, 2*time.Second, nextNotifierDelay(time.Second, 30*time.Second))
	assert.Equal(t, 30*time.Second, nextNotifierDelay(20*time.Second, 30*time.Second))
	assert.Equal(t, 30*time.Second, nextNotifierDelay(30*time.Second, 30*time.Second))
	assert.Equal(t, time.Second, nextNotifierDelay(time.Second, time.Millisecond),
		"a cap under the delay must not shrink it into a hot redial loop")
}

func TestGivenARedialDelayWhenJitteredThenItSpreadsInsideItsBand(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 20 {
		delay := jitteredDelay(time.Second)
		assert.InDelta(t, float64(time.Second), float64(delay), float64(100*time.Millisecond))
		seen[delay] = true
	}
	assert.Greater(t, len(seen), 1, "one repeated value is no jitter")
	assert.Zero(t, jitteredDelay(0), "a zero delay has no band to be shifted inside")
}
