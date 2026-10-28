package bridge

import (
	"context"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// reverseNotReadyFail is msgReverseNotReady as the client reads it, framed.
const reverseNotReadyFail = "FAIL0043the device's reverse rules are not ready yet; run the command again"

// reverseSlotHeldFail is msgReverseSlotHeld as the client reads it, framed.
const reverseSlotHeldFail = "FAIL0099adb reverse on this device is held by another adb client, or by this client's previous connection for a short while; stop the other client or retry later"

// framedKillforwardAll is the attach clear's kill as one websocket message.
var framedKillforwardAll = string(framed("reverse:killforward-all"))

// killAnswered marks, in a recordingAnswer's record, the moment the held kill was answered.
const killAnswered = "<kill answered>"

// recordingAnswer answers as reverseMockBehavior{} does, records requests in arrival order, and
// holds the `reverse:killforward-all` reply until release, so the test decides when the attach
// clear settles. Cleanup releases it.
type recordingAnswer struct {
	requestsMutex sync.Mutex
	requests      []string
	released      chan struct{}
	releaseOnce   sync.Once
}

func newRecordingAnswer(t *testing.T) *recordingAnswer {
	t.Helper()
	recorder := &recordingAnswer{released: make(chan struct{})}
	t.Cleanup(recorder.release)
	return recorder
}

func (recorder *recordingAnswer) answer(session *testsupport.MockSession, data []byte) []byte {
	recorder.record(string(data))
	if string(data) == framedKillforwardAll {
		<-recorder.released
		recorder.record(killAnswered)
	}
	return reverseMockBehavior{}.answer(session, data)
}

func (recorder *recordingAnswer) record(entry string) {
	recorder.requestsMutex.Lock()
	defer recorder.requestsMutex.Unlock()
	recorder.requests = append(recorder.requests, entry)
}

// release lets the held kill be answered. Idempotent.
func (recorder *recordingAnswer) release() {
	recorder.releaseOnce.Do(func() { close(recorder.released) })
}

// recorded is a copy of the record so far, in arrival order.
func (recorder *recordingAnswer) recorded() []string {
	recorder.requestsMutex.Lock()
	defer recorder.requestsMutex.Unlock()
	return slices.Clone(recorder.requests)
}

// sawRequest reports whether service, framed as a client sends it behind a select, reached the server.
func (recorder *recordingAnswer) sawRequest(service string) bool {
	return slices.Contains(recorder.recorded(), string(framed(service)))
}

// awaitKill waits until the server holds the attach clear's kill.
func (recorder *recordingAnswer) awaitKill(t *testing.T) {
	t.Helper()
	waitFor(t, func() bool { return recorder.sawRequest("reverse:killforward-all") }, "the attach clear never reached the server")
}

// gatedProxy is a proxy on an attachment at serverMocks whose reverse requests meet a shut gate:
// the server holds the attach clear, a rival holds the notifier slot, or every notifier dial fails.
type gatedProxy struct {
	address  string
	recorder *recordingAnswer
	*clearingDevice
}

func startGatedProxy(t *testing.T, logger *slog.Logger) *gatedProxy {
	t.Helper()
	recorder := newRecordingAnswer(t)
	clearing := attachClearingDevice(t, recorder.answer, notifierPoll, logger)
	address, _ := runProxy(t, clearing.device, testPolicy())
	recorder.awaitKill(t)
	return &gatedProxy{address: address, recorder: recorder, clearingDevice: clearing}
}

// startSlotHeldProxy runs a proxy on an attachment whose notifier slot a rival holds, and waits
// for the mark. Once the rival leaves, the server holds the attach clear's kill until the test
// releases it. It returns the rival so a test can close it.
func startSlotHeldProxy(t *testing.T) (*gatedProxy, *notifierConn) {
	t.Helper()
	recorder := newRecordingAnswer(t)
	sessions, reverse := serverMocks(t, recorder.answer)
	rival := rivalNotifier(t, reverse)
	clearing := attachDeviceAt(t, sessions, reverse, reverse.URL, fastBackoff, noopLogger())
	address, _ := runProxy(t, clearing.device, testPolicy())
	waitFor(t, clearing.notifier(t).notifierSlotHeld, "the attachment's hello was never refused with the slot taken")
	return &gatedProxy{address: address, recorder: recorder, clearingDevice: clearing}, rival
}

// sendBehindSelect dials a client, has its select answered and sends service, as adb does.
func (gated *gatedProxy) sendBehindSelect(t *testing.T, service string) (client net.Conn) {
	t.Helper()
	client = dialClient(t, gated.address)
	writeAll(t, client, []byte("000ehost:tport:any"))
	require.Equal(t, "OKAY"+transportIDReply, string(readN(t, client, 12)))
	writeAll(t, client, framed(service))
	return client
}

// assertHeld asserts that, for a settle window, the server opens no second session and never sees
// service, and the request holds no slot and counts no rule.
func (gated *gatedProxy) assertHeld(t *testing.T, service string) {
	t.Helper()
	features := attachedFeatures(t, gated.device)
	assert.Never(t, func() bool {
		return gated.sessions.SessionsOpened() > 1 || gated.recorder.sawRequest(service) ||
			features.budget.inUse.Load() > 0 || features.reverseRules.holdsAny()
	}, notifierSettle, notifierPoll, "a held reverse request must reach no server, slot or rule")
}

func TestGivenAReverseRequestBeforeTheAttachClearSettlesWhenTheClearIsAnsweredThenTheRequestReachesTheServerAfterTheClearsVerdict(t *testing.T) {
	testCases := []struct {
		name    string
		service string
		reply   string
	}{
		{name: "an install", service: reverseInstall(1), reply: "OKAYOKAY"},
		{name: "a listing", service: "reverse:list-forward", reply: "OKAY0000"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			gated := startGatedProxy(t, noopLogger())
			client := gated.sendBehindSelect(t, testCase.service)
			gated.assertHeld(t, testCase.service)

			gated.recorder.release()

			assert.Equal(t, testCase.reply, string(readN(t, client, 8)), "the held request is relayed and answered")
			requests := gated.recorder.recorded()
			assert.Greater(t, slices.Index(requests, string(framed(testCase.service))), slices.Index(requests, killAnswered),
				"the request must reach the server after the kill's verdict: %q", requests)
			assert.Equal(t, 2, gated.sessions.SessionsOpened(), "the clear's session, then the request's")
		})
	}
}

func TestGivenAReverseInstallHeldAtTheGateWhenTheDeviceDetachesThenTheClientReadsEOFAndNothingIsDialedOrCounted(t *testing.T) {
	gated := startGatedProxy(t, noopLogger())
	client := gated.sendBehindSelect(t, reverseInstall(1))
	gated.assertHeld(t, reverseInstall(1))

	_, cleared := gated.device.Clear()
	require.True(t, cleared)

	require.NoError(t, client.SetDeadline(time.Now().Add(detachedReadDeadline)))
	_, readErr := client.Read(make([]byte, 1))
	assert.ErrorIs(t, readErr, io.EOF, "the detach must end the held connection, not answer it")
	assert.Equal(t, 1, gated.sessions.SessionsOpened(), "a request released by a detach dials nothing")
}

// unrunFeatures is an attachment whose supervisor never runs, so only the test opens its gate.
func unrunFeatures(ctx context.Context) *attachmentFeatures {
	return &attachmentFeatures{notifier: newNotifierSupervisor(nil, nil, "", nil, nil, nil), reverseEnabled: true, ctx: ctx}
}

func TestGivenTheGateNeverOpensWhenAReverseRequestWaitsOutTheBoundThenItIsRefusedLocallyAndNothingProceeds(t *testing.T) {
	// An unrun supervisor never opens its gate. Short bound, fast test.
	const bound = 20 * time.Millisecond
	forwarding := unrunFeatures(context.Background())
	testCases := []struct {
		name        string
		service     string
		wantProceed bool
		wantReply   string
	}{
		{name: "a reverse install", service: reverseInstall(1), wantProceed: false, wantReply: reverseNotReadyFail},
		{name: "a device service", service: "shell:ls", wantProceed: true, wantReply: ""},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			client := newScriptedConn("")

			proceed := awaitServerRulesCleared(context.Background(), client, opening{service: testCase.service, selected: true},
				forwarding, bound, noopLogger())

			assert.Equal(t, testCase.wantProceed, proceed)
			assert.Equal(t, testCase.wantReply, client.replies.String())
		})
	}
}

func TestGivenAGateTheDetachOpenedWhenAHeldReverseRequestWakesThenItEndsWithNoReply(t *testing.T) {
	// A detach cancels the attachment's context, then its supervisor opens the gate.
	endedCtx, endAttachment := context.WithCancel(context.Background())
	endAttachment()
	forwarding := unrunFeatures(endedCtx)
	forwarding.notifier.openGate()
	client := newScriptedConn("")

	proceed := awaitServerRulesCleared(context.Background(), client, opening{service: reverseInstall(1), selected: true},
		forwarding, reverseClearTimeout, noopLogger())

	assert.False(t, proceed, "a gate opened by the detach is no verdict")
	assert.Empty(t, client.replies.String(), "the detach closes the connection; no FAIL is owed")
}

func TestGivenAReverseInstallHeldAtTheGateWhenADeviceServiceArrivesOnAnotherConnectionThenItIsRelayedAtOnce(t *testing.T) {
	gated := startGatedProxy(t, noopLogger())
	gated.sendBehindSelect(t, reverseInstall(1))

	gated.sendBehindSelect(t, "shell:ls")

	// Under the clear's bound, so a gate that wrongly held the shell request would still be holding it.
	require.Eventually(t, func() bool { return gated.recorder.sawRequest("shell:ls") }, testWait, notifierPoll,
		"a device service must not wait on the gate")
	assert.False(t, gated.recorder.sawRequest(reverseInstall(1)), "the gate must still hold the install")
}

func TestGivenAnotherClientHoldsTheNotifierSlotWhenAReverseRequestArrivesThenItIsRefusedAtOnceAndNothingIsDialed(t *testing.T) {
	for _, service := range reverseFamily {
		t.Run(service, func(t *testing.T) {
			held, _ := startSlotHeldProxy(t)
			started := time.Now()

			client := held.sendBehindSelect(t, service)

			assert.Equal(t, reverseSlotHeldFail, string(readN(t, client, len(reverseSlotHeldFail))),
				"the client reads the held slot as its request's FAIL")
			assert.Less(t, time.Since(started), reverseClearTimeout/2, "a held slot must not cost the clear's bound")
			_, readErr := client.Read(make([]byte, 1))
			assert.ErrorIs(t, readErr, io.EOF, "the daemon closes a connection it refused")
			assert.Zero(t, held.sessions.ConnectionsAccepted(), "a refused reverse request must dial nothing")
		})
	}
}

func TestGivenAnotherClientHoldsTheNotifierSlotWhenAForwardRequestArrivesThenItIsStillAnsweredLocally(t *testing.T) {
	held, _ := startSlotHeldProxy(t)
	client := dialClient(t, held.address)

	writeAll(t, client, []byte(installForward))

	assert.NotZero(t, readForwardSuccessPort(t, client), "the forward family is answered with the slot held")
	assert.Zero(t, held.sessions.ConnectionsAccepted(), "a forward install dials nothing")
}

func TestGivenAHeldSlotWhenTheOtherClientLeavesThenAReverseRequestIsRelayedAgain(t *testing.T) {
	held, rival := startSlotHeldProxy(t)
	held.recorder.release()
	require.NoError(t, rival.Close())
	held.awaitReady(t)

	client := held.sendBehindSelect(t, reverseInstall(1))

	assert.Equal(t, "OKAYOKAY", string(readN(t, client, 8)), "the regained slot relays the install")
	assert.Equal(t, 2, held.sessions.SessionsOpened(), "the attach clear's session, then the install's")
}

func TestGivenAnOpenGateWhenAnotherClientHoldsTheSlotThenAReverseRequestIsRefusedWithTheSlotHeldFail(t *testing.T) {
	// Once open, the gate stays open; a held slot still refuses.
	forwarding := unrunFeatures(context.Background())
	forwarding.notifier.openGate()
	forwarding.notifier.slotHeld.Store(true)
	client := newScriptedConn("")

	proceed := awaitServerRulesCleared(context.Background(), client, opening{service: reverseInstall(1), selected: true},
		forwarding, reverseClearTimeout, noopLogger())

	assert.False(t, proceed)
	assert.Equal(t, reverseSlotHeldFail, client.replies.String())
}

func TestGivenAShutGateWhenTheHoldSeesTheMarkOnlyAtItsBoundThenTheBoundRefusesWithTheSlotHeldFail(t *testing.T) {
	// Store sets the mark without the wake, as when the bound and the wake fire together. The mark
	// lands after the entry check, well before the bound, under -race too.
	const markAt = 20 * time.Millisecond
	const bound = 400 * time.Millisecond
	forwarding := unrunFeatures(context.Background())
	client := newScriptedConn("")
	time.AfterFunc(markAt, func() { forwarding.notifier.slotHeld.Store(true) })

	started := time.Now()
	proceed := awaitServerRulesCleared(context.Background(), client, opening{service: reverseInstall(1), selected: true},
		forwarding, bound, noopLogger())

	assert.False(t, proceed)
	assert.GreaterOrEqual(t, time.Since(started), bound, "only the bound path answers after the bound; an entry refusal would prove nothing")
	assert.Equal(t, reverseSlotHeldFail, client.replies.String(), "the held slot outranks the not-ready text at the bound")
}

func TestGivenAReverseRequestHeldBeforeAnyHelloWhenTheHelloIsRefusedForAHeldSlotThenTheSlotHeldFailArrivesWellBeforeTheBound(t *testing.T) {
	recorder := newRecordingAnswer(t)
	sessions, reverse := serverMocks(t, recorder.answer)
	rivalNotifier(t, reverse)
	// Port 1 is closed: no dial reaches a hello, so the gate stays shut and the slot unmarked.
	clearing := attachDeviceAt(t, sessions, reverse, "wss://127.0.0.1:1", fastBackoff, noopLogger())
	address, _ := runProxy(t, clearing.device, testPolicy())
	gated := &gatedProxy{address: address, recorder: recorder, clearingDevice: clearing}
	client := gated.sendBehindSelect(t, reverseInstall(1))
	gated.assertHeld(t, reverseInstall(1))
	reached := time.Now()

	require.True(t, clearing.device.UpdateEndpoint(testDeviceARN, reverse.URL, "test-device"))

	assert.Equal(t, reverseSlotHeldFail, string(readN(t, client, len(reverseSlotHeldFail))))
	assert.Less(t, time.Since(reached), reverseClearTimeout/2, "the refused hello must end the hold, not its bound")
}

func TestGivenAHeldSlotWhenTheOtherClientLeavesThenAReverseRequestSentDuringTheAttachClearWaitsForItsVerdict(t *testing.T) {
	held, rival := startSlotHeldProxy(t)
	require.NoError(t, rival.Close())
	// READY cleared the mark but slotMarked stays closed; the server holds the attach clear's kill.
	held.recorder.awaitKill(t)

	client := held.sendBehindSelect(t, reverseInstall(1))
	held.assertHeld(t, reverseInstall(1))
	held.recorder.release()

	assert.Equal(t, "OKAYOKAY", string(readN(t, client, 8)), "the install is relayed once the clear settles")
	requests := held.recorder.recorded()
	assert.Greater(t, slices.Index(requests, string(framed(reverseInstall(1)))), slices.Index(requests, killAnswered),
		"the install must reach the server after the kill's verdict: %q", requests)
}

func TestGivenADetachedAttachmentWhenTheSlotReadsHeldThenAReverseRequestEndsWithNoReply(t *testing.T) {
	// A detach cancels the attachment's context and opens the gate; the mark may still be set.
	endedCtx, endAttachment := context.WithCancel(context.Background())
	endAttachment()
	forwarding := unrunFeatures(endedCtx)
	forwarding.notifier.slotHeld.Store(true)
	forwarding.notifier.openGate()
	client := newScriptedConn("")

	proceed := awaitServerRulesCleared(context.Background(), client, opening{service: reverseInstall(1), selected: true},
		forwarding, reverseClearTimeout, noopLogger())

	assert.False(t, proceed, "a detached attachment serves nothing")
	assert.Empty(t, client.replies.String(), "the detach closes the connection; no FAIL is owed")
}

func TestGivenReverseOnWithNoNotifierCadenceWhenAnInstallArrivesThenItIsAnsweredWithoutWaitingForAClear(t *testing.T) {
	// No heartbeat interval or backoff set: the supervisor exits at once, and its exit must open the gate.
	proxy := startReverseProxy(t, reverseMockBehavior{})
	started := time.Now()

	reply := proxy.command(t, reverseInstall(1))

	assert.Equal(t, "OKAYOKAY", reply)
	assert.Less(t, time.Since(started), reverseClearTimeout/2, "a supervisor that never clears must not hold the install")
}
