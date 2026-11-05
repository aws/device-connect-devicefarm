package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/forward"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// fakeClock is a manually-advanced clock so quiet-time math is deterministic.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)}
}

func TestGivenEmptyDeviceWhenSetThenBecomesCurrent(t *testing.T) {
	device := NewActiveDevice()
	info := DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/ws", Label: "Pixel - OS 16"}

	require.True(t, device.Set(info), "first Set must succeed")

	got, ok := device.Current()
	require.True(t, ok)
	assert.Equal(t, info, got)
}

func TestGivenDeviceAlreadySetWhenSetAgainThenRejectedAndUnchanged(t *testing.T) {
	device := NewActiveDevice()
	first := DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1", Label: "First"}
	require.True(t, device.Set(first))

	accepted := device.Set(DeviceInfo{ARN: "arn:2", Endpoint: "wss://h/2", Label: "Second"})

	assert.False(t, accepted, "the one-device limit must reject a second Set")
	got, _ := device.Current()
	assert.Equal(t, first, got, "the original device must remain bound")
}

func TestGivenSetDeviceWhenClearedThenReturnsItAndBecomesEmpty(t *testing.T) {
	device := NewActiveDevice()
	info := DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1", Label: "Pixel"}
	require.True(t, device.Set(info))

	cleared, ok := device.Clear()

	require.True(t, ok)
	assert.Equal(t, info, cleared)
	_, present := device.Current()
	assert.False(t, present)
	// After clearing, the slot is free for a new device.
	assert.True(t, device.Set(DeviceInfo{ARN: "arn:2"}), "Set must succeed after Clear")
}

func TestGivenEmptyDeviceWhenClearedThenReportsNothing(t *testing.T) {
	_, ok := NewActiveDevice().Clear()
	assert.False(t, ok)
}

func TestGivenMatchingARNWhenEndpointUpdatedThenEndpointAndLabelReplaced(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://old", Label: "Old"}))

	updated := device.UpdateEndpoint("arn:1", "wss://new", "New")

	require.True(t, updated)
	got, _ := device.Current()
	assert.Equal(t, "wss://new", got.Endpoint)
	assert.Equal(t, "New", got.Label)
	assert.Equal(t, "arn:1", got.ARN, "the ARN must not change on an endpoint refresh")
}

func TestGivenMismatchedARNWhenEndpointUpdatedThenIgnored(t *testing.T) {
	device := NewActiveDevice()
	original := DeviceInfo{ARN: "arn:1", Endpoint: "wss://old", Label: "Old"}
	require.True(t, device.Set(original))

	updated := device.UpdateEndpoint("arn:other", "wss://new", "New")

	assert.False(t, updated, "a refresh for a different ARN must not overwrite the bound device")
	got, _ := device.Current()
	assert.Equal(t, original, got)
}

func TestGivenNoDeviceWhenEndpointUpdatedThenIgnored(t *testing.T) {
	updated := NewActiveDevice().UpdateEndpoint("arn:1", "wss://new", "New")
	assert.False(t, updated)
}

func TestGivenABoundSerialWhenEndpointUpdatedThenSerialSurvives(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://old", Label: "Old", Serial: "R3CN30ABCDE"}))

	require.True(t, device.UpdateEndpoint("arn:1", "wss://new", "New"))

	got, _ := device.Current()
	assert.Equal(t, "R3CN30ABCDE", got.Serial, "an endpoint refresh must not forget the serial")
}

func TestGivenMatchingARNWhenEndpointReadThenCurrentEndpointReturned(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://old", Label: "Old"}))
	require.True(t, device.UpdateEndpoint("arn:1", "wss://new", "New"))

	endpoint, ok := device.EndpointFor("arn:1")

	require.True(t, ok)
	assert.Equal(t, "wss://new", endpoint, "a reader must see the renewed endpoint, not the one bound by Set")
}

func TestGivenMismatchedARNWhenEndpointReadThenRefused(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://old"}))

	endpoint, ok := device.EndpointFor("arn:other")

	assert.False(t, ok, "a connection must not dial a device it is not bound to")
	assert.Empty(t, endpoint)
}

func TestGivenNoDeviceWhenEndpointReadThenRefused(t *testing.T) {
	endpoint, ok := NewActiveDevice().EndpointFor("arn:1")

	assert.False(t, ok)
	assert.Empty(t, endpoint)
}

func TestGivenMatchingARNWhenClearMatchingThenDetachesAndReturnsIt(t *testing.T) {
	device := NewActiveDevice()
	info := DeviceInfo{ARN: "arn:1", Endpoint: "wss://h", Label: "Pixel"}
	require.True(t, device.Set(info))

	cleared, ok := device.ClearMatching("arn:1", "the session ended")

	require.True(t, ok)
	assert.Equal(t, info, cleared)
	_, present := device.Current()
	assert.False(t, present)
}

func TestGivenMismatchedARNWhenClearMatchingThenKeepsDevice(t *testing.T) {
	device := NewActiveDevice()
	info := DeviceInfo{ARN: "arn:1", Endpoint: "wss://h", Label: "Pixel"}
	require.True(t, device.Set(info))

	_, ok := device.ClearMatching("arn:other", "the session ended")

	assert.False(t, ok, "ClearMatching must not detach a device whose ARN does not match")
	got, present := device.Current()
	require.True(t, present)
	assert.Equal(t, info, got)
}

func TestGivenNoDeviceWhenClearMatchingThenReportsNothing(t *testing.T) {
	_, ok := NewActiveDevice().ClearMatching("arn:1", "the session ended")
	assert.False(t, ok)
}

func TestGivenTrackedConnsWhenClearMatchingThenAllCancelled(t *testing.T) {
	// ClearMatching detaches on a dead session, so like Clear it must tear down the
	// in-flight connections bound to the device rather than leave them streaming to
	// an endpoint that can no longer be resolved. It ends the attachment the same
	// way, which is how the refresh loop's detach reaches the forward listeners.
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1"}))
	features := attachedFeatures(t, device)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, _, ok := device.trackConn(cancel)
	require.True(t, ok)

	_, cleared := device.ClearMatching("arn:1", "the session ended")

	require.True(t, cleared)
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "ClearMatching must cancel every tracked connection")
	assert.ErrorIs(t, features.ctx.Err(), context.Canceled, "ClearMatching must end the attachment")
}

func TestGivenNoDeviceWhenConnTrackedThenRefused(t *testing.T) {
	_, _, _, ok := NewActiveDevice().trackConn(func() {})
	assert.False(t, ok, "a connection must not be tracked when no device is bound")
}

func TestGivenTrackedConnsWhenClearedThenAllCancelled(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1"}))

	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	info, _, _, ok := device.trackConn(firstCancel)
	require.True(t, ok)
	assert.Equal(t, "arn:1", info.ARN, "trackConn must return the bound device")
	_, _, _, ok = device.trackConn(secondCancel)
	require.True(t, ok)

	_, cleared := device.Clear()

	require.True(t, cleared)
	assert.ErrorIs(t, firstCtx.Err(), context.Canceled, "Clear must cancel every tracked connection")
	assert.ErrorIs(t, secondCtx.Err(), context.Canceled)
}

func TestGivenConnEndsBeforeClearWhenUntrackedThenNotCancelledByLaterClear(t *testing.T) {
	// A connection that ends normally deregisters via the untrack func; a later
	// Clear (a different device's remove) must not touch it. This also exercises
	// that untrack after the connection is gone is safe to call.
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1"}))

	cancelled := false
	_, _, untrack, ok := device.trackConn(func() { cancelled = true })
	require.True(t, ok)
	untrack()

	_, cleared := device.Clear()

	require.True(t, cleared)
	assert.False(t, cancelled, "an already-ended connection must not be cancelled by a later Clear")
	untrack() // idempotent: safe to call again
}

// tcpSpec renders a port as adb spec text.
func tcpSpec(port int) string { return fmt.Sprintf("tcp:%d", port) }

// testMaxDeviceStreams is the bridge tests' stream cap. Zero refuses every accept, so
// the helper always sets it.
const testMaxDeviceStreams = 2

// testMaxForwardRules is the forwarding tests' rule limit. Zero refuses every new rule,
// so the helper always sets it.
const testMaxForwardRules = 2

// testMaxReverseRules is the bridge tests' reverse rule limit; the pinned FAIL text checks the
// format, not the production default.
const testMaxReverseRules = 2

// forwardingParams are the attachment params most bridge tests use: reverse on, no notifier
// cadence, so no notifier dials.
func forwardingParams() AttachmentParams {
	return AttachmentParams{
		DeviceStreamStatusTimeout: testWait,
		MaxDeviceStreams:          testMaxDeviceStreams,
		MaxForwardRules:           testMaxForwardRules,
		MaxReverseRules:           testMaxReverseRules,
		ReverseEnabled:            true,
	}
}

// testFeatures is WithAttachmentFeatures with the attach prefetch off, so a test's mock
// sees only the dials the test is about.
func testFeatures(dialer *WSDialer, policy WSRefreshPolicy, params AttachmentParams,
	logger *slog.Logger) ActiveDeviceOption {
	withFeatures := WithAttachmentFeatures(dialer, policy, params, logger)
	return func(device *ActiveDevice) {
		withFeatures(device)
		device.featureConfig.skipHostPrefetch = true
	}
}

// newForwardingDevice builds an ActiveDevice whose attachments forward, each through
// a stream opener of its own.
func newForwardingDevice(t *testing.T, opts ...ActiveDeviceOption) *ActiveDevice {
	t.Helper()
	return newAttachedDevice(t, forwardingParams(), opts...)
}

// newAttachedDevice builds an ActiveDevice whose attachments run under params.
func newAttachedDevice(t *testing.T, params AttachmentParams, opts ...ActiveDeviceOption) *ActiveDevice {
	t.Helper()
	featureOption := testFeatures(newTestDialer(testWait), testPolicy(), params, noopLogger())
	device := NewActiveDevice(append([]ActiveDeviceOption{featureOption}, opts...)...)
	t.Cleanup(func() { device.Clear() })
	return device
}

// attachedFeatures reads the bound attachment's features the way a connection does,
// without leaving the connection registered.
func attachedFeatures(t *testing.T, device *ActiveDevice) *attachmentFeatures {
	t.Helper()
	_, features, untrack, ok := device.trackConn(func() {})
	require.True(t, ok, "a device must be bound")
	untrack()
	require.NotNil(t, features, "an attachment must carry its own features")
	return features
}

// installRule installs a rule on an OS-assigned local port and returns that port.
func installRule(t *testing.T, features *attachmentFeatures, remote string) int {
	t.Helper()
	result := features.table.Install("tcp:0", remote, false)
	require.Equal(t, forward.AdbInstallOK, result.Status, "install must succeed, got %q", result.FailReason)
	require.NotZero(t, result.ResolvedPort)
	return result.ResolvedPort
}

// dialRule connects to a rule's local port, failing the test if it cannot.
func dialRule(t *testing.T, port int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestGivenForwardingConfiguredWhenDeviceSetThenAFreshTableServesThroughTheOpener(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL}))

	features := attachedFeatures(t, device)
	require.NoError(t, features.ctx.Err(), "the attachment context must be live while the device is bound")
	dialRule(t, installRule(t, features, "tcp:8080"))

	// The attachment's own opener dialed the bound device and asked it for the rule's
	// destination, which is the whole of the wiring this test owns.
	waitFor(t, func() bool { return mock.SessionsOpened() == 1 }, "the opener never dialed the device")
	waitFor(t, func() bool {
		return string(mock.Sessions()[0].Inbound()) == "0012host:transport-any0008tcp:8080"
	}, "the opener never asked for the rule's destination")
}

func TestGivenInstalledRulesWhenDeviceClearedThenEveryListenerCloses(t *testing.T) {
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1"}))
	features := attachedFeatures(t, device)
	ports := []int{installRule(t, features, "tcp:5555"), installRule(t, features, "tcp:5556")}

	_, cleared := device.Clear()

	require.True(t, cleared)
	assert.ErrorIs(t, features.ctx.Err(), context.Canceled, "a detach must sever the attachment's streams")
	for _, port := range ports {
		conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			conn.Close()
		}
		assert.Error(t, err, "the detach must have closed the forward listener on port %d", port)
	}
}

func TestGivenAClearedDeviceWhenAnInstallReachesItsOldTableThenItIsRefused(t *testing.T) {
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1"}))
	features := attachedFeatures(t, device)
	_, cleared := device.Clear()
	require.True(t, cleared)

	result := features.table.Install("tcp:0", "tcp:5555", false)

	assert.Equal(t, forward.AdbInstallCannotBind, result.Status, "the detach must close the table, not just empty it")
}

func TestGivenAReattachAfterAClearWhenForwardingThenAFreshEmptyTableServes(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1"}))
	first := attachedFeatures(t, device)
	port := installRule(t, first, "tcp:5555")
	device.Clear()

	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL}))

	second := attachedFeatures(t, device)
	assert.NotSame(t, first.table, second.table, "a new attachment must get its own table")
	require.NoError(t, second.ctx.Err(), "a new attachment must get a live context")
	// The same local port binds again (the old listener is gone) and its stream reaches
	// the device attached now, so nothing of the first attachment survived the detach.
	result := second.table.Install(tcpSpec(port), "tcp:6000", false)
	require.Equal(t, forward.AdbInstallOK, result.Status, "the released port must bind again, got %q", result.FailReason)
	dialRule(t, port)
	waitFor(t, func() bool {
		return mock.SessionsOpened() == 1 &&
			string(mock.Sessions()[0].Inbound()) == "0012host:transport-any0008tcp:6000"
	}, "the second attachment never opened the rule's stream")
}

func TestGivenTheForwardRuleCapWhenOneMoreRuleIsInstalledThenTheAttachmentsTableRefusesIt(t *testing.T) {
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1"}))
	features := attachedFeatures(t, device)
	for range testMaxForwardRules {
		installRule(t, features, "tcp:5555")
	}

	result := features.table.Install("tcp:0", "tcp:5555", false)

	assert.Equal(t, forward.AdbInstallTooManyRules, result.Status, "the attachment's limit must reach its table")
}

func TestGivenTheBudgetSpentThroughTheSupervisorWhenAForwardedConnectionArrivesThenTheOpenerRefusesIt(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL}))
	features := attachedFeatures(t, device)
	// Spent through the supervisor's handle: the opener behind the table must hold the same budget.
	for range testMaxDeviceStreams {
		require.True(t, features.notifier.budget.admit())
	}

	appConn := dialRule(t, installRule(t, features, "tcp:8080"))

	assert.Empty(t, drainClosed(t, appConn), "a forwarded connection above the shared budget must get zero bytes")
	assert.Zero(t, mock.ConnectionsAccepted(), "a refused forwarded connection must not dial")
}

func TestGivenNoDialerWhenFeaturesAreConfiguredThenTheAttachmentCarriesNone(t *testing.T) {
	device := NewActiveDevice(WithAttachmentFeatures(nil, testPolicy(), AttachmentParams{}, noopLogger()))
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://h/1"}))

	info, features, untrack, ok := device.trackConn(func() {})

	require.True(t, ok, "a device without features must still take connections")
	defer untrack()
	assert.Nil(t, features, "a nil dialer must leave forwarding and the notifier off")
	assert.Equal(t, "wss://h/1", info.Endpoint, "adb must still be proxied")
}

// newNotifierDevice builds a device whose attachment dials mock for its notifier. A keepalive
// of an hour parks the pings: these tests are about the attachment, not the cadence. The
// cleanup detaches, so no test leaves a redial loop running.
func newNotifierDevice(t *testing.T, mock *testsupport.ReverseMockWSS) *ActiveDevice {
	t.Helper()
	device := NewActiveDevice(testFeatures(newTestDialer(testWait), testPolicy(),
		notifierParams(parkedKeepAlive, notifierPoll), noopLogger()))
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL}))
	return device
}

func TestGivenAnAttachedDeviceWhenItIsClearedThenItsNotifierOpenedAndClosedWithIt(t *testing.T) {
	mock := serverReverseMock(t)
	device := newNotifierDevice(t, mock)
	awaitAccepts(t, mock, 1)
	assert.Empty(t, mock.Refusals(), "the attachment's hello must satisfy the server")

	device.Clear()

	require.Eventually(t, func() bool { return mock.NotifiersClosed() == 1 }, testWait, notifierPoll,
		"the detach must close the notifier")
	assert.Never(t, func() bool { return mock.NotifiersAccepted() > 1 }, notifierSettle, notifierPoll,
		"a detached attachment must not redial")
}

func TestGivenAClearedDeviceWhenAnotherIsAttachedThenItGetsItsOwnNotifier(t *testing.T) {
	mock := serverReverseMock(t)
	device := newNotifierDevice(t, mock)
	awaitAccepts(t, mock, 1)
	device.Clear()
	require.Eventually(t, func() bool { return mock.NotifiersClosed() == 1 }, testWait, notifierPoll,
		"the first notifier must be gone before the next attachment dials")

	require.True(t, device.Set(DeviceInfo{ARN: "arn:2", Endpoint: mock.URL}))

	// The mock holds one notifier at a time, so a second accept proves the first
	// supervisor was gone rather than doubled up. Waiting out the close above is what
	// makes that deterministic: a detach cancels the old supervisor without joining it.
	awaitAccepts(t, mock, 2)
	assert.Empty(t, mock.Refusals(), "the new attachment's notifier must not collide with the old one")
}

func TestGivenAnAlternateLoopbackAddressInUseWhenRuleInstalledThenTheBindIsLoopbackOnly(t *testing.T) {
	// The daemon's --listen-addr never reaches a forward listener: a rule binds
	// 127.0.0.1 alone, so a daemon listening on a wildcard still keeps forwards off
	// the network. Holding the port on another loopback address proves it: a
	// wildcard bind would collide, a 127.0.0.1 bind does not. This needs a second
	// bindable loopback address, which is Linux in practice.
	squatter, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("127.0.0.2 cannot be bound here: %v", err)
	}
	defer squatter.Close()
	port := squatter.Addr().(*net.TCPAddr).Port

	mock := newPrologueMock(t, "OKAY", "OKAY")
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL}))
	features := attachedFeatures(t, device)

	result := features.table.Install(tcpSpec(port), "tcp:8080", false)

	require.Equal(t, forward.AdbInstallOK, result.Status, "a rule must bind 127.0.0.1, not the wildcard, got %q", result.FailReason)
	dialRule(t, port)
	waitFor(t, func() bool { return mock.SessionsOpened() == 1 },
		"the loopback listener never served the rule")
}

func TestGivenAnARNGuardedDetachWhenRecordedThenLastDetachHoldsTheReasonUntilTheNextSet(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://127.0.0.1:0/a", Label: "One"}))
	_, detached := device.LastDetach()
	assert.False(t, detached, "an attached device has no last detach")

	_, cleared := device.ClearMatching("arn:1", "the session ended")
	require.True(t, cleared)

	last, detached := device.LastDetach()
	require.True(t, detached)
	assert.Equal(t, "the session ended", last.Reason)
	assert.WithinDuration(t, time.Now(), last.At, time.Minute)

	require.True(t, device.Set(DeviceInfo{ARN: "arn:2", Endpoint: "wss://127.0.0.1:0/b", Label: "Two"}))
	_, detached = device.LastDetach()
	assert.False(t, detached, "the next attach clears the record")
}

func TestGivenAUserRemoveWhenClearedThenNoLastDetachIsRecorded(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://127.0.0.1:0/a", Label: "One"}))

	_, cleared := device.Clear()
	require.True(t, cleared)

	_, detached := device.LastDetach()
	assert.False(t, detached, "a remove by the user is not a detach the status reports")
}

func TestGivenAMismatchedARNWhenClearMatchingRunsThenNoLastDetachIsRecorded(t *testing.T) {
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: "arn:1", Endpoint: "wss://127.0.0.1:0/a", Label: "One"}))

	_, cleared := device.ClearMatching("arn:other", "the session ended")

	assert.False(t, cleared)
	_, detached := device.LastDetach()
	assert.False(t, detached, "a refused detach records nothing")
}
