// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

const (
	trackDevicesRequest     = "0012host:track-devices"
	trackDevicesLongRequest = "0014host:track-devices-l"
	serverVersionReply      = "OKAY00040029"
	testSerial              = "R3CN30ABCDE"
	// anotherVersionReply and thirdVersionReply are servers on adb versions other than
	// adbwire.AdbServerVersion.
	anotherVersionReply = "OKAY00040027"
	thirdVersionReply   = "OKAY00040028"
	// fallbackVersionReply is the answer before any attachment has kept a version:
	// adbwire.AdbServerVersion.
	fallbackVersionReply = "OKAY00040029"
	// emptyDeviceListReply is the answer to a device list with no device.
	emptyDeviceListReply = "OKAY0000"
)

var (
	serverDevicesReply     = string(okayDevicesReply(testSerial + "\tdevice\n"))
	serverDevicesLongReply = string(okayDevicesReply(
		testSerial + "            device product:a1 model:Pixel device:a1 transport_id:3\n"))
	// listedDevicesLongReply is serverDevicesLongReply as a client reads it: under the daemon's id.
	listedDevicesLongReply = string(okayDevicesReply(
		testSerial + "            device product:a1 model:Pixel device:a1 transport_id:1\n"))
	serverFeaturesReply = "OKAY" + string(framed("shell_v2,cmd,stat_v2,ls_v2,fixed_push_mkdir,apex,abb,abb_exec"))
)

// cannedHostReplies is what the server answers each host request with.
var cannedHostReplies = map[string]string{
	hostVersionRequest:     serverVersionReply,
	hostDevicesRequest:     serverDevicesReply,
	hostDevicesLongRequest: serverDevicesLongReply,
	hostFeaturesRequest:    serverFeaturesReply,
}

// listedReply is what a client reads for a canned reply: the server's bytes, a long list under the
// daemon's transport id.
func listedReply(request, reply string) string {
	if request == hostDevicesLongRequest {
		return listedDevicesLongReply
	}
	return reply
}

// hostReplyMock answers each message in canned with its reply and echoes any other.
func hostReplyMock(t *testing.T, canned map[string]string) *testsupport.SessionMockWSS {
	t.Helper()
	return testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
			if reply, found := canned[string(data)]; found {
				return []byte(reply)
			}
			return data
		},
	})
}

// startHostReplyProxy runs a proxy in front of a mock answering host requests from canned. The
// attach prefetch is off, so every reply is fetched on first use.
func startHostReplyProxy(t *testing.T, canned map[string]string) (proxyAddress string,
	mock *testsupport.SessionMockWSS, device *ActiveDevice) {
	t.Helper()
	mock = hostReplyMock(t, canned)
	proxyAddress, device = attachProxyTo(t, mock, testPolicy(), testSerial)
	return proxyAddress, mock, device
}

// attachPrefetching binds a device at endpoint with the attach prefetch on. Reverse is off and
// testPolicy turns the keeper off, so the prefetch is the only dial.
func attachPrefetching(t *testing.T, dialer *WSDialer, endpoint string, logger *slog.Logger) *ActiveDevice {
	t.Helper()
	device := NewActiveDevice(WithAttachmentFeatures(dialer, testPolicy(), warmParams(), logger))
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "test-device"}))
	return device
}

// sendOnly sends request on a fresh connection and returns it, for a reply the test reads itself.
func sendOnly(t *testing.T, proxyAddress, request string) net.Conn {
	t.Helper()
	client := dialClient(t, proxyAddress)
	writeAll(t, client, []byte(request))
	return client
}

// askHost sends request on a fresh connection and returns every byte up to the close.
func askHost(t *testing.T, proxyAddress, request string) string {
	t.Helper()
	reply, err := io.ReadAll(sendOnly(t, proxyAddress, request))
	require.NoError(t, err, "the daemon must close after a host reply")
	return string(reply)
}

// openTracker sends a tracker request and reads its first message, returning the open connection.
func openTracker(t *testing.T, proxyAddress, request, firstMessage string) net.Conn {
	t.Helper()
	client := sendOnly(t, proxyAddress, request)
	require.Equal(t, firstMessage, string(readN(t, client, len(firstMessage))), "a tracker reads the list first")
	return client
}

// pushedList is the server reply with its status word cut: what a held tracker gets pushed.
func pushedList(serverReply string) string {
	return serverReply[statusWordLen:]
}

// assertHeldQuiet checks that tracker stays open and sends nothing for a while.
func assertHeldQuiet(t *testing.T, tracker net.Conn) {
	t.Helper()
	require.NoError(t, tracker.SetReadDeadline(time.Now().Add(notifierSettle)))
	_, err := tracker.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the tracker must stay open and quiet")
	require.NoError(t, tracker.SetReadDeadline(time.Now().Add(testTimeout)))
}

// assertPushed reads the next tracker message and checks it is list.
func assertPushed(t *testing.T, tracker net.Conn, list string) {
	t.Helper()
	require.Equal(t, list, string(readN(t, tracker, len(list))), "the tracker reads the new list")
}

// startEmptyProxy runs a proxy over an empty device slot; mock is ready for a Set.
func startEmptyProxy(t *testing.T) (proxyAddress string, mock *testsupport.SessionMockWSS, device *ActiveDevice) {
	t.Helper()
	mock = hostReplyMock(t, cannedHostReplies)
	device = newForwardingDevice(t)
	proxyAddress, _ = runProxy(t, device, testPolicy())
	return proxyAddress, mock, device
}

// attachTo binds the test device at endpoint.
func attachTo(t *testing.T, device *ActiveDevice, endpoint string) {
	t.Helper()
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "test-device", Serial: testSerial}))
}

// sortedSessionInbounds returns what each session the mock minted received, in byte order.
func sortedSessionInbounds(mock *testsupport.SessionMockWSS) []string {
	var inbounds []string
	for _, session := range mock.Sessions() {
		inbounds = append(inbounds, string(session.Inbound()))
	}
	slices.Sort(inbounds)
	return inbounds
}

// sortedPrefetches is prefetchedHostRequests in byte order.
func sortedPrefetches() []string {
	return slices.Sorted(slices.Values(prefetchedHostRequests))
}

func TestGivenAServerThatAnswersWhenADeviceAttachesThenEachReplyIsFetchedOnceAndServesEveryQuery(t *testing.T) {
	mock := hostReplyMock(t, cannedHostReplies)

	device := attachPrefetching(t, newTestDialer(testWait), mock.URL, noopLogger())

	waitFor(t, func() bool { return slices.Equal(sortedSessionInbounds(mock), sortedPrefetches()) },
		"the attach must open one session per reply, each sending its request alone")
	proxyAddress, _ := runProxy(t, device, testPolicy())
	for request, reply := range cannedHostReplies {
		assert.Equal(t, listedReply(request, reply), askHost(t, proxyAddress, request), "the server's reply, byte for byte")
	}
	openTracker(t, proxyAddress, trackDevicesRequest, serverDevicesReply)
	openTracker(t, proxyAddress, trackDevicesLongRequest, listedDevicesLongReply)
	assert.Never(t, func() bool { return mock.ConnectionsAccepted() > len(prefetchedHostRequests) }, notifierSettle,
		notifierPoll, "the prefetched copies answer with no dial, and nothing retries")
}

func TestGivenAFailedDialWhenADeviceAttachesThenEachReplyIsTriedOnceAndFetchedAgainOnFirstUse(t *testing.T) {
	// A plain listener fails the TLS handshake.
	address, accepted := testsupport.NewAcceptCounter(t)
	logs := &lockedBuffer{}
	device := attachPrefetching(t, newTestDialer(testWait), "wss://"+address,
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	// A prefetch logs its failure after it drops its entry, so a later query fetches anew.
	require.Eventually(t, func() bool {
		return strings.Count(logs.String(), "could not prefetch a host reply") == len(prefetchedHostRequests)
	}, testWait, notifierPoll, "every prefetch must fail")
	assert.Equal(t, len(prefetchedHostRequests), accepted(), "one dial per reply")
	assert.Never(t, func() bool { return accepted() > len(prefetchedHostRequests) }, notifierSettle, notifierPoll,
		"one attempt each, no retry")
	// The endpoint renews, as when the server comes up after the attach.
	serving := hostReplyMock(t, cannedHostReplies)
	require.True(t, device.UpdateEndpoint(testDeviceARN, serving.URL, "test-device"))
	proxyAddress, _ := runProxy(t, device, testPolicy())

	for range 2 {
		assert.Equal(t, serverDevicesReply, askHost(t, proxyAddress, hostDevicesRequest))
	}

	assert.Equal(t, 1, serving.ConnectionsAccepted(), "one fetch on first use, then the copy answers")
}

func TestGivenAPrefetchInFlightWhenTheSameQueryArrivesThenItWaitsForThatFetch(t *testing.T) {
	released := make(chan struct{})
	release := sync.OnceFunc(func() { close(released) })
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
			if string(data) == hostVersionRequest {
				<-released
			}
			return []byte(cannedHostReplies[string(data)])
		},
	})
	t.Cleanup(release) // before the mock's own cleanup, which waits on its handlers
	dialer := newTestDialer(testWait)
	dialer.hostReplyTimeout = time.Hour // only the release may end the prefetch's read
	device := attachPrefetching(t, dialer, mock.URL, noopLogger())
	waitFor(t, func() bool { return mock.ConnectionsAccepted() == len(prefetchedHostRequests) },
		"the prefetch never dialed")
	proxyAddress, _ := runProxy(t, device, testPolicy())
	client := sendOnly(t, proxyAddress, hostVersionRequest)
	require.NoError(t, client.SetReadDeadline(time.Now().Add(notifierSettle)))
	_, err := client.Read(make([]byte, 1))
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the query must wait on the prefetch")
	require.NoError(t, client.SetReadDeadline(time.Now().Add(testTimeout)))

	release()

	reply, err := io.ReadAll(client)
	require.NoError(t, err)
	assert.Equal(t, serverVersionReply, string(reply))
	assert.Equal(t, len(prefetchedHostRequests), mock.ConnectionsAccepted(), "the query shares the prefetch's dial")
}

func TestGivenAPrefetchAwaitingItsRepliesWhenTheDeviceDetachesThenEachFetchEndsAtOnce(t *testing.T) {
	requested := make(chan struct{}, len(prefetchedHostRequests))
	closed := make(chan struct{}, len(prefetchedHostRequests))
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		var request []byte
		if websocket.Message.Receive(ws, &request) != nil {
			return
		}
		requested <- struct{}{}
		// Never answer; wait for the client to close.
		_ = websocket.Message.Receive(ws, &request)
		closed <- struct{}{}
	})
	logs := &lockedBuffer{}
	device := NewActiveDevice(WithAttachmentFeatures(newTestDialer(testWait), testPolicy(), warmParams(),
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	device.featureConfig.dialer.hostReplyTimeout = time.Hour // only the detach may end these fetches
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: wsURL, Label: "test-device"}))
	for range prefetchedHostRequests {
		select {
		case <-requested:
		case <-time.After(testWait):
			t.Fatal("a fetch never sent its request")
		}
	}

	_, cleared := device.Clear()

	require.True(t, cleared)
	for range prefetchedHostRequests {
		select {
		case <-closed:
		case <-time.After(testWait):
			t.Fatal("the detach must close every fetch's websocket")
		}
	}
	assert.Never(t, func() bool { return strings.Contains(logs.String(), "level=WARN") }, notifierSettle,
		notifierPoll, "a fetch the detach ended is no failure")
	assert.NotContains(t, logs.String(), "could not prefetch a host reply")
}

func TestGivenADetachedDeviceWhenItsFetcherRunsThenItRefusesWithNoDial(t *testing.T) {
	mock := hostReplyMock(t, cannedHostReplies)
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL}))
	fetcher := &hostReplyFetcher{device: device, dialer: newTestDialer(testWait), arn: testDeviceARN}
	_, cleared := device.Clear()
	require.True(t, cleared)

	reply, err := fetcher.fetch(context.Background(), hostVersionRequest)

	require.ErrorIs(t, err, errAdbConnClosed)
	assert.Nil(t, reply)
	assert.Zero(t, mock.ConnectionsAccepted())
}

func TestGivenRepeatedHostQueriesWhenAnsweredThenEachIsFetchedOnceAndRepliedVerbatim(t *testing.T) {
	for request, reply := range cannedHostReplies {
		t.Run(request, func(t *testing.T) {
			proxyAddress, mock, _ := startHostReplyProxy(t, cannedHostReplies)

			for range 3 {
				assert.Equal(t, listedReply(request, reply), askHost(t, proxyAddress, request), "the server's reply, byte for byte")
			}

			assert.Equal(t, 1, mock.ConnectionsAccepted(), "one fetch serves every later request")
			require.Len(t, mock.Sessions(), 1)
			assert.Equal(t, request, string(mock.Sessions()[0].Inbound()), "the fetch sends the request alone")
		})
	}
}

func TestGivenADetachWhenTheDeviceAttachesAgainThenTheRepliesAreFetchedAgain(t *testing.T) {
	// Remote adb servers differ in adb version, so nothing cached may outlive its attachment.
	proxyAddress, mock, device := startHostReplyProxy(t, cannedHostReplies)
	require.Equal(t, serverVersionReply, askHost(t, proxyAddress, hostVersionRequest))
	_, ok := device.Clear()
	require.True(t, ok)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: mock.URL, Label: "Test Device - OS 1"}))

	assert.Equal(t, serverVersionReply, askHost(t, proxyAddress, hostVersionRequest))

	assert.Equal(t, 2, mock.ConnectionsAccepted(), "the new attachment must fetch its own reply")
}

func TestGivenRowsWhenTheTransportIDIsRewrittenThenOnlyTheTrailingFieldChanges(t *testing.T) {
	tests := []struct {
		name string
		row  string
		want string
	}{
		{name: "the field present", row: "R3CN30ABCDE            device product:a1 model:Pixel device:a1 transport_id:3",
			want: "R3CN30ABCDE            device product:a1 model:Pixel device:a1 transport_id:7"},
		{name: "the field absent", row: "R3CN30ABCDE            device product:a1",
			want: "R3CN30ABCDE            device product:a1 transport_id:7"},
		{name: "a product naming the field", row: "R3CN30ABCDE            device product:x transport_id:9 model:P transport_id:3",
			want: "R3CN30ABCDE            device product:x transport_id:9 model:P transport_id:7"},
		{name: "the word last with no digits", row: "R3CN30ABCDE            device product:x transport_id:none",
			want: "R3CN30ABCDE            device product:x transport_id:none transport_id:7"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.want, rewriteTransportID(testCase.row, 7))
		})
	}
}

func TestGivenAReplyThatIsNoWholeListWhenTheListedIDsAreRewrittenThenItComesBackAsItIs(t *testing.T) {
	for _, reply := range []string{"", "OKAY0000", "FAIL000bunsupported"} {
		assert.Equal(t, reply, string(rewriteListedTransportIDs([]byte(reply), 1)))
	}
}

func TestGivenAServerListingItsOwnTransportIDWhenTheLongListIsAskedThenTheRowCarriesTheDaemonsIDAndThatIDSelects(t *testing.T) {
	// `adb devices -l`, then `adb -t <listed id>`: the listed id must be one the daemon holds.
	proxyAddress, _, _ := startHostReplyProxy(t, cannedHostReplies)

	assert.Equal(t, listedDevicesLongReply, askHost(t, proxyAddress, hostDevicesLongRequest))

	client := sendOnly(t, proxyAddress, "0013host:transport-id:1")
	assert.Equal(t, "OKAY", string(readN(t, client, 4)), "the listed id selects the device")
}

func TestGivenAServerOnAnotherAdbVersionWhenHostVersionIsAskedThenTheServersVersionAnswersFromOneFetch(t *testing.T) {
	// An adb client kills a server whose version differs from its own, so the client must meet
	// the remote server's version: the one the doctor's version check compares with.
	proxyAddress, mock, _ := startHostReplyProxy(t, map[string]string{hostVersionRequest: anotherVersionReply})

	for range 2 {
		assert.Equal(t, anotherVersionReply, askHost(t, proxyAddress, hostVersionRequest), "the server's own version")
	}

	assert.Equal(t, 1, mock.ConnectionsAccepted(), "one fetch, then the copy answers")
}

func TestGivenADetachedDeviceWhenHostVersionIsAskedThenTheLastServersVersionAnswersWithNoDial(t *testing.T) {
	proxyAddress, mock, device := startHostReplyProxy(t, map[string]string{hostVersionRequest: anotherVersionReply})
	require.Equal(t, anotherVersionReply, askHost(t, proxyAddress, hostVersionRequest))
	_, ok := device.Clear()
	require.True(t, ok)

	for range 2 {
		assert.Equal(t, anotherVersionReply, askHost(t, proxyAddress, hostVersionRequest),
			"the version the last attachment's server reported")
	}

	assert.Equal(t, 1, mock.ConnectionsAccepted(), "nothing is dialed with no device")
}

func TestGivenABridgeThatNeverHeldADeviceWhenHostVersionIsAskedThenTheFallbackVersionAnswers(t *testing.T) {
	proxyAddress, _ := runProxy(t, NewActiveDevice(), testPolicy())

	assert.Equal(t, fallbackVersionReply, askHost(t, proxyAddress, hostVersionRequest))
}

func TestGivenAnAttachPrefetchAloneWhenTheDeviceDetachesThenTheVersionItKeptAnswers(t *testing.T) {
	// No client asks while attached: the copy the attach itself fetched is what the detach keeps.
	mock := hostReplyMock(t, map[string]string{hostVersionRequest: anotherVersionReply})
	device := attachPrefetching(t, newTestDialer(testWait), mock.URL, noopLogger())
	waitFor(t, func() bool {
		features, _, _ := device.snapshot()
		return features != nil && features.hostReplies.kept(hostVersionRequest) != nil
	}, "the attach never kept the server's version")
	proxyAddress, _ := runProxy(t, device, testPolicy())
	_, ok := device.Clear()
	require.True(t, ok)

	assert.Equal(t, anotherVersionReply, askHost(t, proxyAddress, hostVersionRequest))
}

func TestGivenTwoAttachmentsWhenTheSecondDetachesThenItsServersVersionAnswers(t *testing.T) {
	proxyAddress, _, device := startHostReplyProxy(t, map[string]string{hostVersionRequest: anotherVersionReply})
	require.Equal(t, anotherVersionReply, askHost(t, proxyAddress, hostVersionRequest))
	_, ok := device.Clear()
	require.True(t, ok)
	second := hostReplyMock(t, map[string]string{hostVersionRequest: thirdVersionReply})
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: second.URL, Label: "Test Device - OS 1"}))
	require.Equal(t, thirdVersionReply, askHost(t, proxyAddress, hostVersionRequest))
	_, ok = device.Clear()
	require.True(t, ok)

	assert.Equal(t, thirdVersionReply, askHost(t, proxyAddress, hostVersionRequest), "the last attachment's, not the first's")
}

func TestGivenAnAttachmentThatKeptNoVersionWhenItDetachesThenTheFallbackVersionAnswers(t *testing.T) {
	// A refused reply is relayed, never kept, so the detach has no copy to carry over.
	refused := "FAIL0006closed"
	proxyAddress, _, device := startHostReplyProxy(t, map[string]string{hostVersionRequest: refused})
	require.Equal(t, refused, string(readN(t, sendOnly(t, proxyAddress, hostVersionRequest), len(refused))))
	_, ok := device.Clear()
	require.True(t, ok)

	assert.Equal(t, fallbackVersionReply, askHost(t, proxyAddress, hostVersionRequest))
}

func TestGivenNoDeviceWhenADeviceListIsAskedThenTheEmptyListAnswers(t *testing.T) {
	for name, request := range map[string]string{"short": hostDevicesRequest, "long": hostDevicesLongRequest} {
		t.Run(name, func(t *testing.T) {
			proxyAddress, _ := runProxy(t, NewActiveDevice(), testPolicy())

			assert.Equal(t, emptyDeviceListReply, askHost(t, proxyAddress, request))
		})
	}
}

func TestGivenAServerThatRefusesAHostQueryWhenAskedThenTheRequestIsRelayedEveryTime(t *testing.T) {
	refused := "FAIL000bunsupported"
	proxyAddress, mock, _ := startHostReplyProxy(t, map[string]string{hostVersionRequest: refused})

	for range 2 {
		assert.Equal(t, refused, string(readN(t, sendOnly(t, proxyAddress, hostVersionRequest), len(refused))),
			"the client reads the server's own answer")
	}

	assert.Equal(t, 4, mock.ConnectionsAccepted(), "a refusal is not cached: each request fetches, then relays")
}

func TestGivenASpentBudgetWhenHostQueriesArriveThenTheyAreAnsweredAndOnlyARelayIsRefused(t *testing.T) {
	refused := "FAIL000bunsupported"
	proxyAddress, mock, device := startHostReplyProxy(t, map[string]string{
		hostVersionRequest: serverVersionReply,
		hostDevicesRequest: refused,
	})
	spendBudget(t, device)

	assert.Equal(t, serverVersionReply, askHost(t, proxyAddress, hostVersionRequest), "a fetch takes no slot")
	assertRefusedOverBudget(t, sendOnly(t, proxyAddress, hostDevicesRequest))

	assert.Equal(t, 2, mock.ConnectionsAccepted(), "each fetch dials; the relay of the refused one may not")
}

func TestGivenAFetchWhoseDialFailsWhenAHostQueryArrivesThenTheClientClosesAfterOneDial(t *testing.T) {
	// A plain listener fails the TLS handshake: the fetch fails, and a relay would only dial again.
	address, accepted := testsupport.NewAcceptCounter(t)
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: "wss://" + address + "/adb"}))
	proxyAddress, _ := runProxy(t, device, testPolicy())

	assert.Empty(t, drainClosed(t, sendOnly(t, proxyAddress, hostVersionRequest)), "a failed fetch gives zero bytes")

	assert.Equal(t, 1, accepted(), "the failed fetch is the only dial")
}

func TestGivenATrackerWhoseFirstFetchFailsWhenAnsweredThenTheClientClosesAfterOneDial(t *testing.T) {
	// A plain listener fails the TLS handshake: the first list cannot be fetched, so the tracker
	// closes with zero bytes, as a host query does, and is not relayed.
	address, accepted := testsupport.NewAcceptCounter(t)
	device := newForwardingDevice(t)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: "wss://" + address + "/adb"}))
	proxyAddress, _ := runProxy(t, device, testPolicy())

	assert.Empty(t, drainClosed(t, sendOnly(t, proxyAddress, trackDevicesRequest)), "a failed first fetch gives zero bytes")

	assert.Equal(t, 1, accepted(), "the failed fetch is the only dial")
}

func TestGivenAHostRequestNotAnsweredLocallyWhenSentThenItIsRelayed(t *testing.T) {
	for name, service := range map[string]string{
		"a serial prefix":                   "host-serial:R3CN30ABCDE:version",
		"the features of any usb device":    "host-usb:features",
		"the features of any local device":  "host-local:features",
		"the host features":                 "host:host-features",
		"the proto binary tracker":          "host:track-devices-proto-binary",
		"a near miss of devices -l":         "host:devices-ll",
		"a near miss of the features probe": "host:features-l",
	} {
		t.Run(name, func(t *testing.T) {
			proxyAddress, mock, _ := startHostReplyProxy(t, cannedHostReplies)
			request := string(framed(service))

			sendOnly(t, proxyAddress, request)

			assertSessionReceived(t, mock, request)
		})
	}
}

func TestGivenAFeaturesProbeNamingADeviceWhenAskedTwiceThenOneFetchAnswersTheAttachedDeviceAndAnotherIsRefused(t *testing.T) {
	// One fetch of the plain probe answers each form the transport select would accept: the
	// attached serial and the daemon's transport id. Another serial or id gets adb's miss text,
	// with no dial.
	tests := map[string]struct {
		service   string
		wantReply string
		wantDials int
	}{
		"the attached serial":        {service: "host-serial:" + testSerial + ":features", wantReply: serverFeaturesReply, wantDials: 1},
		"the daemon's transport id":  {service: "host-transport-id:1:features", wantReply: serverFeaturesReply, wantDials: 1},
		"another transport id":       {service: "host-transport-id:7:features", wantReply: "FAIL" + string(framed("no device with transport id '7'"))},
		"another serial":             {service: "host-serial:emulator-5554:features", wantReply: "FAIL" + string(framed("device 'emulator-5554' not found"))},
		"another serial with a port": {service: "host-serial:192.168.0.1:5555:features", wantReply: "FAIL" + string(framed("device '192.168.0.1:5555' not found"))},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			proxyAddress, mock, _ := startHostReplyProxy(t, cannedHostReplies)

			for range 2 {
				assert.Equal(t, tc.wantReply, askHost(t, proxyAddress, string(framed(tc.service))))
			}

			assert.Equal(t, tc.wantDials, mock.ConnectionsAccepted(), "one fetch for the attached device, none for another serial")
			for _, session := range mock.Sessions() {
				assert.Equal(t, hostFeaturesRequest, string(session.Inbound()), "the fetch sends the plain probe")
			}
		})
	}
}

func TestGivenAFailingFetchWhenAHostQueryIsAskedThenOnlyAFeaturesProbeOnALiveConnectionIsRelayed(t *testing.T) {
	// A canceled connection is a detach or shutdown: nothing to relay to.
	tests := map[string]struct {
		request      string
		canceled     bool
		wantAnswered bool
		wantFetches  int32
	}{
		"the features probe":                          {request: "host:features", wantFetches: 1},
		"the features probe for the serial":           {request: "host-serial:" + testSerial + ":features", wantFetches: 1},
		"the version":                                 {request: "host:version", wantAnswered: true, wantFetches: 1},
		"the features probe on a canceled connection": {request: "host:features", canceled: true, wantAnswered: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var fetches atomic.Int32
			cache := newHostReplyCache(func(context.Context, string) ([]byte, error) {
				fetches.Add(1)
				return nil, errors.New("dial refused")
			})
			features := &attachmentFeatures{ctx: context.Background(), hostReplies: cache}
			connCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			daemonSide, clientSide := net.Pipe()
			written := make(chan []byte, 1)
			go func() {
				bytesRead, _ := io.ReadAll(clientSide)
				written <- bytesRead
			}()

			answered := answerHostQuery(connCtx, daemonSide, opening{service: tc.request}, features, testSerial, noopLogger())

			assert.Equal(t, tc.wantAnswered, answered, "answered false relays the request")
			assert.Equal(t, tc.wantFetches, fetches.Load(), "a live query fetches before it relays or closes")
			require.NoError(t, daemonSide.Close())
			assert.Empty(t, <-written, "a failed fetch writes nothing to the client")
		})
	}
}

func TestGivenAFeaturesFetchWhoseDialFailsWhenTheSessionRunsAtARenewedEndpointThenTheRelayedProbeIsAnsweredThere(t *testing.T) {
	// A plain listener fails the TLS handshake, so the fetch fails and so does the relay's first dial.
	address, accepted := testsupport.NewAcceptCounter(t)
	serving := hostReplyMock(t, cannedHostReplies)
	lookup := &fakeLookup{verdict: SessionVerdict{Endpoint: serving.URL, Label: renewedLabel}}
	proxyAddress, device := runLookupProxy(t, "wss://"+address, lookup)

	client := sendOnly(t, proxyAddress, hostFeaturesRequest)

	assert.Equal(t, serverFeaturesReply, string(readN(t, client, len(serverFeaturesReply))), "the server's reply, relayed")
	assert.Equal(t, 2, accepted(), "the fetch and the relay's first dial")
	assert.Equal(t, []string{testDeviceARN}, lookup.calls(), "the relay's failed dial asks about the session")
	assertSessionReceived(t, serving, hostFeaturesRequest)
	_, present := device.Current()
	assert.True(t, present)
}

func TestGivenAFeaturesFetchWhoseDialFailsWhenTheSessionEndedThenTheClientReadsTheDetachReason(t *testing.T) {
	address, accepted := testsupport.NewAcceptCounter(t)
	lookup := &fakeLookup{verdict: SessionVerdict{Detach: true, Reason: detachedReason}}
	proxyAddress, device := runLookupProxy(t, "wss://"+address, lookup)

	client := sendOnly(t, proxyAddress, hostFeaturesRequest)

	assert.Equal(t, detachedReason, readFailThenEOF(t, client), "adb prints the verdict after error:")
	assertDetached(t, device, detachedReason)
	assert.Equal(t, 2, accepted(), "the fetch and the relay's one dial; a detach earns no second")
}

func TestGivenATransportSelectWhenAHostQueryFollowsThenItIsRelayedBehindTheDaemonsSwitch(t *testing.T) {
	for name, request := range map[string]string{
		"the bare query":     hostVersionRequest,
		"the features probe": hostFeaturesRequest,
		// Stored bare behind the select, this one reads as a cached query.
		"a doubled host prefix": string(framed("host:host:version")),
	} {
		t.Run(name, func(t *testing.T) {
			proxyAddress, mock, _ := startHostReplyProxy(t, map[string]string{canonicalSelect: "OKAY"})
			client := dialClient(t, proxyAddress)
			selectAnyTransport(t, client)

			writeAll(t, client, []byte(request))

			assertSessionReceived(t, mock, canonicalSelect+request)
		})
	}
}

func TestGivenATrackerWhenTheDeviceDetachesThenItReadsTheEmptyListAndStaysOpen(t *testing.T) {
	for request, firstMessage := range map[string]string{
		trackDevicesRequest:     serverDevicesReply,
		trackDevicesLongRequest: listedDevicesLongReply,
	} {
		t.Run(request, func(t *testing.T) {
			proxyAddress, mock, device := startHostReplyProxy(t, cannedHostReplies)

			tracker := openTracker(t, proxyAddress, request, firstMessage)
			assertHeldQuiet(t, tracker)
			assert.Equal(t, 1, mock.ConnectionsAccepted(), "only the fetch dials; the tracker holds no websocket")

			_, ok := device.Clear()
			require.True(t, ok)

			assertPushed(t, tracker, emptyDeviceList)
			assertHeldQuiet(t, tracker)
		})
	}
}

func TestGivenATrackerOpenedWithNoDeviceWhenADeviceAttachesThenItReadsTheEmptyListThenTheList(t *testing.T) {
	for request, serverReply := range map[string]string{
		trackDevicesRequest:     serverDevicesReply,
		trackDevicesLongRequest: listedDevicesLongReply,
	} {
		t.Run(request, func(t *testing.T) {
			proxyAddress, mock, device := startEmptyProxy(t)
			tracker := openTracker(t, proxyAddress, request, statusOkay+emptyDeviceList)
			assertHeldQuiet(t, tracker)

			attachTo(t, device, mock.URL)

			assertPushed(t, tracker, pushedList(serverReply))
			fresh := openTracker(t, proxyAddress, request, serverReply)
			assertHeldQuiet(t, fresh)
			assert.Equal(t, 1, mock.ConnectionsAccepted(), "one fetch serves the push and the fresh tracker")
		})
	}
}

func TestGivenAHeldTrackerWhenTheDeviceDetachesAndAttachesAgainThenItReadsEachList(t *testing.T) {
	proxyAddress, mock, device := startEmptyProxy(t)
	tracker := openTracker(t, proxyAddress, trackDevicesRequest, statusOkay+emptyDeviceList)
	attachTo(t, device, mock.URL)
	assertPushed(t, tracker, pushedList(serverDevicesReply))

	_, cleared := device.Clear()
	require.True(t, cleared)
	assertPushed(t, tracker, emptyDeviceList)
	assertHeldQuiet(t, tracker)
	attachTo(t, device, mock.URL)

	assertPushed(t, tracker, pushedList(serverDevicesReply))
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "each attachment fetches its own list once")
}

func TestGivenTwoHeldTrackersWhenADeviceAttachesThenBothReadTheList(t *testing.T) {
	proxyAddress, mock, device := startEmptyProxy(t)
	first := openTracker(t, proxyAddress, trackDevicesRequest, statusOkay+emptyDeviceList)
	second := openTracker(t, proxyAddress, trackDevicesLongRequest, statusOkay+emptyDeviceList)

	attachTo(t, device, mock.URL)

	assertPushed(t, first, pushedList(serverDevicesReply))
	assertPushed(t, second, pushedList(listedDevicesLongReply))
}

func TestGivenAHeldTrackerWithNoDeviceWhenTheDaemonStopsThenItEndsAtOnce(t *testing.T) {
	device := newForwardingDevice(t)
	server, err := NewAdbProxyServer("127.0.0.1", 0, testPolicy(), newTestDialer(testWait), device, noopLogger())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- server.Run(ctx) }()
	tracker := openTracker(t, waitForAddr(t, server), trackDevicesRequest, statusOkay+emptyDeviceList)

	cancel()

	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(testWait):
		t.Fatal("a held tracker held the daemon's shutdown")
	}
	rest, err := io.ReadAll(tracker)
	require.NoError(t, err, "the shutdown must close the tracker")
	assert.Empty(t, rest, "nothing follows for a tracker the shutdown ends")
}

func TestGivenAHeldTrackerWhenADeviceWhoseListCannotBeCopiedAttachesThenTheTrackerEnds(t *testing.T) {
	// A plain listener fails the TLS handshake, so a fetch of it fails.
	failingAddress, _ := testsupport.NewAcceptCounter(t)
	notReady := hostReplyMock(t, map[string]string{
		hostDevicesRequest: string(okayDevicesReply(testSerial + "\toffline\n")),
	})
	for name, endpoint := range map[string]string{
		"a device not ready": notReady.URL,
		"a fetch that fails": "wss://" + failingAddress,
	} {
		t.Run(name, func(t *testing.T) {
			device := newForwardingDevice(t)
			proxyAddress, _ := runProxy(t, device, testPolicy())
			tracker := openTracker(t, proxyAddress, trackDevicesRequest, statusOkay+emptyDeviceList)

			attachTo(t, device, endpoint)

			rest, err := io.ReadAll(tracker)
			require.NoError(t, err, "a tracker with no list to push must end")
			assert.Empty(t, rest, "nothing is pushed for a list the daemon cannot copy")
		})
	}
}

func TestGivenAHeldTrackerWhenADeviceWithNoDialerAttachesThenTheTrackerEnds(t *testing.T) {
	// No dialer, no features: nothing to copy a list from.
	device := NewActiveDevice()
	proxyAddress, _ := runProxy(t, device, testPolicy())
	tracker := openTracker(t, proxyAddress, trackDevicesRequest, statusOkay+emptyDeviceList)

	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: "wss://example.invalid"}))

	rest, err := io.ReadAll(tracker)
	require.NoError(t, err, "a tracker with no list to push must end")
	assert.Empty(t, rest)
}

func TestGivenATrackerWhoseFirstListIsNotKeptWhenAnsweredThenItIsRelayed(t *testing.T) {
	// The server's own tracker reports a device that is not ready yet.
	proxyAddress, mock, _ := startHostReplyProxy(t, map[string]string{
		hostDevicesRequest:  string(okayDevicesReply(testSerial + "\toffline\n")),
		trackDevicesRequest: serverDevicesReply,
	})

	tracker := sendOnly(t, proxyAddress, trackDevicesRequest)

	waitFor(t, func() bool { return slices.Contains(sortedSessionInbounds(mock), trackDevicesRequest) },
		"the tracker must reach the server")
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "the list fetch, then the relay")
	assertPushed(t, tracker, serverDevicesReply)
	assertHeldQuiet(t, tracker)
}

// heldBackListMock answers host requests from cannedHostReplies once release is called.
func heldBackListMock(t *testing.T) (mock *testsupport.SessionMockWSS, release func()) {
	t.Helper()
	released := make(chan struct{})
	release = sync.OnceFunc(func() { close(released) })
	mock = testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, data []byte) []byte {
			<-released
			return []byte(cannedHostReplies[string(data)])
		},
	})
	t.Cleanup(release) // before the mock's own cleanup, which waits on its handlers
	return mock, release
}

// startHeldBackProxy runs a proxy over an empty device slot and opens one tracker on it. A fetch
// ends only at the test's release or at a detach.
func startHeldBackProxy(t *testing.T) (proxyAddress string, mock *testsupport.SessionMockWSS, release func(),
	device *ActiveDevice, tracker net.Conn) {
	t.Helper()
	mock, release = heldBackListMock(t)
	device = newForwardingDevice(t)
	device.featureConfig.dialer.hostReplyTimeout = time.Hour
	proxyAddress, _ = runProxy(t, device, testPolicy())
	tracker = openTracker(t, proxyAddress, trackDevicesRequest, statusOkay+emptyDeviceList)
	return proxyAddress, mock, release, device, tracker
}

func TestGivenAPushFetchInFlightWhenTheDeviceDetachesThenTheTrackerReadsTheEmptyListNext(t *testing.T) {
	_, mock, _, device, tracker := startHeldBackProxy(t)
	attachTo(t, device, mock.URL)
	waitFor(t, func() bool { return mock.ConnectionsAccepted() == 1 }, "the push never fetched")

	_, cleared := device.Clear()

	require.True(t, cleared)
	assertPushed(t, tracker, emptyDeviceList)
	assertHeldQuiet(t, tracker)
}

func TestGivenATrackerClientThatLeavesDuringAPushFetchWhenTheFetchEndsThenTheCopyStillFills(t *testing.T) {
	// The push's fetch is shared with every waiter on the list, so the client's leave must not fail it.
	proxyAddress, mock, release, device, tracker := startHeldBackProxy(t)
	attachTo(t, device, mock.URL)
	waitFor(t, func() bool { return mock.ConnectionsAccepted() == 1 }, "the push never fetched")
	// A byte ends the hold, and the daemon's close is read back: the hold is gone before the fetch may end.
	writeAll(t, tracker, []byte("x"))
	rest, err := io.ReadAll(tracker)
	require.NoError(t, err, "the daemon must close the tracker")
	assert.Empty(t, rest, "nothing is pushed to a tracker that left")

	release()

	assert.Equal(t, serverDevicesReply, askHost(t, proxyAddress, hostDevicesRequest), "the list query reads the copy")
	assert.Equal(t, 1, mock.ConnectionsAccepted(), "the push's fetch filled the copy; nothing fetched again")
}

// trackerOpening is the opening of a lone tracker request.
func trackerOpening(service string) opening {
	return opening{service: service, pending: framed(service)}
}

// writeEntered wraps a conn and signals each Write before it may block.
type writeEntered struct {
	net.Conn
	entered chan struct{}
}

func (conn *writeEntered) Write(data []byte) (int, error) {
	conn.entered <- struct{}{}
	return conn.Conn.Write(data)
}

// awaitWrite waits for the next Write on conn to start.
func awaitWrite(t *testing.T, conn *writeEntered) {
	t.Helper()
	select {
	case <-conn.entered:
	case <-time.After(testTimeout):
		t.Fatal("the hold wrote nothing")
	}
}

func TestGivenAPushStuckOnAnUnreadClientWhenTheDeviceDetachesThenClearReturnsAtOnce(t *testing.T) {
	// net.Pipe: a write blocks until the other end reads, as a full socket buffer would.
	clientEnd, daemonEnd := net.Pipe()
	t.Cleanup(func() { _ = clientEnd.Close() })
	daemon := &writeEntered{Conn: daemonEnd, entered: make(chan struct{}, 8)}
	mock := hostReplyMock(t, cannedHostReplies)
	device := newForwardingDevice(t)
	ctx, cancel := context.WithCancel(context.Background())
	holdEnded := make(chan struct{})
	go func() {
		serveDeviceTracker(ctx, daemon, trackerOpening("host:track-devices"), device, noopLogger())
		close(holdEnded)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-holdEnded:
		case <-time.After(testWait):
			t.Error("the hold did not end with its context")
		}
	})
	require.NoError(t, clientEnd.SetDeadline(time.Now().Add(testTimeout)))
	awaitWrite(t, daemon)
	assertPushed(t, clientEnd, statusOkay+emptyDeviceList)
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL}))
	awaitWrite(t, daemon) // the push's Write has started and cannot finish: nothing reads the pipe

	cleared := make(chan bool, 1)
	go func() { _, ok := device.Clear(); cleared <- ok }()

	select {
	case ok := <-cleared:
		require.True(t, ok)
	case <-time.After(testWait):
		t.Fatal("Clear waited on a tracker's blocked push")
	}
	assertPushed(t, clientEnd, pushedList(serverDevicesReply))
	assertPushed(t, clientEnd, emptyDeviceList)
}

func TestGivenAHeldTrackerWhenTheClientWritesThenTheDaemonClosesIt(t *testing.T) {
	// adb closes a device tracker its client writes to.
	proxyAddress, _, _ := startHostReplyProxy(t, cannedHostReplies)
	tracker := openTracker(t, proxyAddress, trackDevicesRequest, serverDevicesReply)

	writeAll(t, tracker, []byte("x"))

	rest, err := io.ReadAll(tracker)
	require.NoError(t, err, "the daemon must close the tracker")
	assert.Empty(t, rest, "a tracker the client ended gets no empty list")
}

// blockingFetch is a fetch that counts its calls and returns reply and err once released.
type blockingFetch struct {
	calls    atomic.Int32
	started  chan struct{}
	released chan struct{}
	reply    []byte
	err      error
}

func newBlockingFetch(reply string) *blockingFetch {
	return &blockingFetch{started: make(chan struct{}, 1), released: make(chan struct{}), reply: []byte(reply)}
}

func (fetch *blockingFetch) run(context.Context, string) ([]byte, error) {
	fetch.calls.Add(1)
	fetch.started <- struct{}{}
	<-fetch.released
	return fetch.reply, fetch.err
}

// lookupResult is one lookup's return values.
type lookupResult struct {
	reply []byte
	err   error
}

// lookupAsync runs lookup on its own goroutine; the channel gets its result.
func lookupAsync(ctx context.Context, cache *hostReplyCache) <-chan lookupResult {
	result := make(chan lookupResult, 1)
	go func() {
		reply, err := cache.lookup(ctx, hostVersionRequest)
		result <- lookupResult{reply: reply, err: err}
	}()
	return result
}

func TestGivenAFetchInFlightWhenTheSameReplyIsAskedForThenTheRequestWaitsForThatFetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fetch := newBlockingFetch(serverVersionReply)
		cache := newHostReplyCache(fetch.run)
		filler := lookupAsync(context.Background(), cache)
		<-fetch.started

		waiter := lookupAsync(context.Background(), cache)

		synctest.Wait()
		assert.Empty(t, waiter, "the second request must wait for the fetch in flight")
		close(fetch.released)
		assert.Equal(t, lookupResult{reply: []byte(serverVersionReply)}, <-filler)
		assert.Equal(t, lookupResult{reply: []byte(serverVersionReply)}, <-waiter)
		assert.Equal(t, int32(1), fetch.calls.Load(), "requests during a fetch share it")
	})
}

func TestGivenAFetchThatFailsWhenRequestsWaitOnItThenEachGetsItsError(t *testing.T) {
	// A waiter that saw no error would relay, one more dial per waiter in an outage.
	synctest.Test(t, func(t *testing.T) {
		fetch := newBlockingFetch("")
		fetch.err = errors.New("dial refused")
		cache := newHostReplyCache(fetch.run)
		filler := lookupAsync(context.Background(), cache)
		<-fetch.started
		waiter := lookupAsync(context.Background(), cache)
		synctest.Wait()
		require.Empty(t, waiter, "the second request must wait for the fetch in flight")

		close(fetch.released)

		assert.ErrorIs(t, (<-filler).err, fetch.err)
		assert.ErrorIs(t, (<-waiter).err, fetch.err)
		assert.Equal(t, int32(1), fetch.calls.Load())
	})
}

func TestGivenAKeptCopyWhenTheConnectionIsAlreadyCanceledThenTheCopyIsNotServed(t *testing.T) {
	// A detach cancels the connection; the old device list must not answer it.
	cache := newHostReplyCache(func(context.Context, string) ([]byte, error) { return []byte(serverDevicesReply), nil })
	_, err := cache.lookup(context.Background(), hostDevicesRequest)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reply, err := cache.lookup(ctx, hostDevicesRequest)

	assert.Nil(t, reply)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestGivenAWaiterWhoseConnectionEndsWhenTheFetchStillRunsThenItStopsWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fetch := newBlockingFetch(serverVersionReply)
		cache := newHostReplyCache(fetch.run)
		filler := lookupAsync(context.Background(), cache)
		<-fetch.started
		ctx, cancel := context.WithCancel(context.Background())
		waiter := lookupAsync(ctx, cache)
		synctest.Wait()
		require.Empty(t, waiter, "the waiter must be parked on the fetch in flight")

		cancel()

		assert.Equal(t, lookupResult{err: context.Canceled}, <-waiter, "a waiter that left gets nothing to answer with")
		close(fetch.released)
		assert.Equal(t, lookupResult{reply: []byte(serverVersionReply)}, <-filler, "the fetch itself runs on")
	})
}

func TestGivenAFetchWithNothingToKeepWhenAskedAgainThenItFetchesAgain(t *testing.T) {
	notKept := [][]byte{
		nil,
		okayDevicesReply(""),
		[]byte("FAIL0004gone"),
		okayDevicesReply(testSerial + "\toffline\n"),
		okayDevicesReply(testSerial + "\tdevice\nemulator-5554\tunauthorized\n"),
		okayDevicesReply(testSerial + "\n"),
	}
	kept := okayDevicesReply(testSerial + "\tdevice\nemulator-5554\tdevice\n")
	var calls int
	cache := newHostReplyCache(func(context.Context, string) ([]byte, error) {
		calls++
		if calls <= len(notKept) {
			return notKept[calls-1], nil
		}
		return kept, nil
	})
	for _, fetched := range notKept {
		reply, err := cache.lookup(context.Background(), hostDevicesRequest)
		require.NoError(t, err)
		assert.Nil(t, reply, "%q is not answered from", fetched)
	}
	_, _ = cache.lookup(context.Background(), hostDevicesRequest)

	cached, err := cache.lookup(context.Background(), hostDevicesRequest)

	require.NoError(t, err)
	assert.Equal(t, len(notKept)+1, calls, "every reply not kept leaves the next request to fetch, a kept one none")
	assert.Equal(t, kept, cached)
}

func TestGivenAFetchWaitingOnASilentServerWhenTheDaemonShutsDownThenTheServerStopsAtOnce(t *testing.T) {
	// The attachment outlives the accept loop's drain, so only the server's context can end the fetch.
	silent := hostReplyMock(t, map[string]string{hostVersionRequest: ""})
	device := newForwardingDevice(t)
	device.featureConfig.dialer.hostReplyTimeout = time.Hour
	require.True(t, device.Set(DeviceInfo{ARN: "arn:test", Endpoint: silent.URL}))
	logs := &lockedBuffer{}
	server, err := NewAdbProxyServer("127.0.0.1", 0, testPolicy(), newTestDialer(testWait), device,
		slog.New(slog.NewTextHandler(logs, nil)))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- server.Run(ctx) }()
	sendOnly(t, waitForAddr(t, server), hostVersionRequest)
	waitFor(t, func() bool { return silent.ConnectionsAccepted() == 1 }, "the fetch never dialed")

	cancel()

	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(testWait):
		t.Fatal("a fetch held the daemon's shutdown")
	}
	assert.NotContains(t, logs.String(), "level=ERROR", "a shutdown is a normal end, not a fetch failure")
}

func TestGivenATrackerRequestWithBytesBehindItWhenAnsweredThenTheDaemonClosesIt(t *testing.T) {
	// adb closes a device tracker its client writes to, even in the request's own segment.
	proxyAddress, _, _ := startHostReplyProxy(t, cannedHostReplies)

	tracker := openTracker(t, proxyAddress, trackDevicesRequest+"x", serverDevicesReply)

	rest, err := io.ReadAll(tracker)
	require.NoError(t, err, "the daemon must close the tracker")
	assert.Empty(t, rest)
}
