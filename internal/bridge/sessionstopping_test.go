// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// relayStoppingReason is not the relay's real reason: the client must never echo the wire's text.
const relayStoppingReason = "some words from the relay"

// defaultStopping is the detach reason with no WithSessionStoppingReason.
const defaultStopping = "remote access session " + testDeviceARN + " is stopping"

// stoppingSessionMock closes every new websocket with the session-stopping code.
func stoppingSessionMock(t *testing.T) *testsupport.SessionMockWSS {
	t.Helper()
	mock := testsupport.NewSessionEchoWSS(t)
	mock.CloseNewConnections(sessionStoppingCloseCode, relayStoppingReason)
	return mock
}

// assertStoppingDetach checks the device is gone and its record carries reason.
func assertStoppingDetach(t *testing.T, device *ActiveDevice, reason string) {
	t.Helper()
	waitFor(t, func() bool { _, present := device.Current(); return !present }, "the stopping close never detached the device")
	last, detached := device.LastDetach()
	require.True(t, detached)
	assert.Equal(t, reason, last.Reason)
}

func TestGivenAStoppingCloseAtTheOpeningWhenAnAdbConnectionOpensThenTheDeviceDetachesWithTheStoppingReason(t *testing.T) {
	mock := stoppingSessionMock(t)
	var lookups atomic.Int32
	device := newForwardingDevice(t,
		WithRefusalHandler(func(string) SessionVerdict { lookups.Add(1); return SessionVerdict{} }),
		WithSessionStoppingReason(func(arn string) string { return "stopping: " + arn }))
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: mock.URL, Label: "Stopping Device"}))
	proxyAddress, _ := runProxy(t, device, testPolicy())

	reply := askHost(t, proxyAddress, string(framed("host:get-state")))

	assert.Equal(t, "FAIL"+string(framed("stopping: "+testDeviceARN)), reply,
		"the client reads the daemon's own reason, never the relay's")
	assertStoppingDetach(t, device, "stopping: "+testDeviceARN)
	assert.Zero(t, lookups.Load(), "the session still reads RUNNING, so a stopping close asks for no lookup")
	assert.Equal(t, 1, mock.ConnectionsAccepted(), "a stopping close earns no second dial")
}

func TestGivenAStoppingCloseWhenAForwardedStreamOpensThenTheDeviceDetaches(t *testing.T) {
	mock := stoppingSessionMock(t)
	device := newTestDevice(t, testDeviceARN, mock.URL)

	_, _, err := openStream(t, device, adbwire.AdbSpec{Kind: "tcp", Value: "9000"}, testWait)

	require.True(t, IsSessionStopping(err), "the stream fails on the stopping close: %v", err)
	assertStoppingDetach(t, device, defaultStopping)
	assert.Equal(t, 1, mock.ConnectionsAccepted())
}

func TestGivenAStoppingCloseOnThePreSelectWhenTheKeeperParksThenTheDeviceDetachesAndTheKeeperStops(t *testing.T) {
	mock := stoppingSessionMock(t)

	device, _ := startKeeper(t, mock.URL, time.Hour, 10*time.Millisecond)

	assertStoppingDetach(t, device, defaultStopping)
	assert.Never(t, func() bool { return mock.ConnectionsAccepted() > 1 }, 200*time.Millisecond, 10*time.Millisecond,
		"the keeper parks no socket after a stopping close")
}

func TestGivenAStoppingCloseOnAReattachWhenALiveConnectionSwapsThenTheDeviceDetachesAndNoRetryIsDialed(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	logs := &lockedBuffer{}
	conn := newTestAdbConnOnRoute(t, mock.URL, fastPolicy(), RouteAdb, slog.New(slog.NewTextHandler(logs, nil)))
	handshake(t, conn)
	mock.CloseNewConnections(sessionStoppingCloseCode, relayStoppingReason)

	err := conn.refresh(context.Background(), nil)

	require.True(t, IsSessionStopping(err), "the swap fails on the stopping close: %v", err)
	assertStoppingDetach(t, conn.device, defaultStopping)
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "the first socket and one successor; a stopping close is not retried")
	assert.NotContains(t, logs.String(), "reattach outcome unknown", "a stopping close is a known outcome")
}

func TestGivenAStoppingCloseOnAReactiveResumeWhenAReadFailsThenTheDeviceDetachesAndNoResumeWarnFollows(t *testing.T) {
	mock := testsupport.NewSessionEchoWSS(t)
	logs := &lockedBuffer{}
	conn := newTestAdbConnOnRoute(t, mock.URL, fastPolicy(), RouteAdb, slog.New(slog.NewTextHandler(logs, nil)))
	handshake(t, conn)
	mock.CloseNewConnections(sessionStoppingCloseCode, relayStoppingReason)

	resumed := conn.reconnectReactively(errors.New("the websocket failed"))

	assert.False(t, resumed)
	assertStoppingDetach(t, conn.device, defaultStopping)
	assert.Equal(t, 2, mock.ConnectionsAccepted(), "the first socket and one successor; a stopping close is not retried")
	assert.NotContains(t, logs.String(), "could not resume the adb session", "the detach already says why")
}

func TestGivenAStoppingCloseOnTheNotifierDialWhenTheSupervisorRunsThenTheDeviceDetachesAndNoRedialFollows(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.CloseNewConnections(sessionStoppingCloseCode, relayStoppingReason)

	device, supervisor, _ := runSupervisor(t, mock, notifierParams(time.Hour, 10*time.Millisecond))

	assertStoppingDetach(t, device, defaultStopping)
	select {
	case <-supervisor.serverRulesCleared:
	case <-time.After(testWait):
		t.Fatal("the supervisor kept running after the stopping close")
	}
	assert.Zero(t, mock.NotifiersAccepted())
}

func TestGivenAStoppingCloseOnAConnectorDialWhenAnAnnounceArrivesThenTheDeviceDetaches(t *testing.T) {
	mock := testsupport.NewReverseMockWSS(t)
	mock.CloseNewConnections(sessionStoppingCloseCode, relayStoppingReason)
	device := deviceAt(t, mock.URL)
	supervisor := announcingSupervisor(device, notifierParams(time.Hour, 10*time.Millisecond), nil,
		&streamBudget{limit: testMaxDeviceStreams}, notifierListenPort+1)

	supervisor.announce(context.Background(), testReverseToken, notifierListenPort+1)

	assertStoppingDetach(t, device, defaultStopping)
	assert.Zero(t, mock.ConnectorsAccepted())
}

func TestGivenAStoppingCloseOnAHostReplyFetchWhenAListIsComposedThenTheDeviceDetaches(t *testing.T) {
	mock := stoppingSessionMock(t)
	device := newTestDevice(t, testDeviceARN, mock.URL)
	fetcher := &hostReplyFetcher{device: device, dialer: newTestDialer(testWait), arn: testDeviceARN}

	reply, err := fetcher.fetch(context.Background(), hostDevicesRequest)

	assert.Nil(t, reply)
	require.True(t, IsSessionStopping(err), "the fetch reports the stopping close, not a reply to relay: %v", err)
	assertStoppingDetach(t, device, defaultStopping)
}

func TestGivenTwoSitesReadingTheStoppingCloseWhenBothDetachThenOneRecordAndOneWarnResult(t *testing.T) {
	logs := &lockedBuffer{}
	device := NewActiveDevice(testFeatures(newTestDialer(testWait), testPolicy(), warmParams(),
		slog.New(slog.NewTextHandler(logs, nil))))
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: "wss://127.0.0.1:1", Label: "Stopping Device"}))
	stoppingClose := &wsCloseError{code: sessionStoppingCloseCode, reason: relayStoppingReason}

	firstReason, firstStopping := device.detachIfSessionStopping(testDeviceARN, stoppingClose)
	secondReason, secondStopping := device.detachIfSessionStopping(testDeviceARN, stoppingClose)

	assert.True(t, firstStopping)
	assert.True(t, secondStopping, "a site that lost the race still stops")
	assert.Equal(t, defaultStopping, firstReason)
	assert.Equal(t, firstReason, secondReason, "every site answers its client with the same reason")
	assert.Equal(t, 1, strings.Count(logs.String(), "the session is stopping; detached the device"))
}

func TestGivenAnotherCloseCodeWhenCheckedForStoppingThenNothingIsDetached(t *testing.T) {
	device := newTestDevice(t, testDeviceARN, "wss://127.0.0.1:1")

	for _, err := range []error{nil, &wsCloseError{code: sessionEndedCloseCode}, &wsCloseError{code: 1011}} {
		_, stopping := device.detachIfSessionStopping(testDeviceARN, err)
		assert.False(t, stopping, "%v", err)
	}
	_, present := device.Current()
	assert.True(t, present)
}

func TestGivenAStoppingCloseOnAConnectionsReadWhenReceivedThenTheDeviceDetachesAndTheConnectionCloses(t *testing.T) {
	// Also the first read of a fresh-session fallback, which takes this path.
	mock := stoppingSessionMock(t)
	conn := newTestAdbConn(t, mock.URL, fastPolicy())
	require.NoError(t, conn.Send([]byte("000chost:version")))

	_, err := conn.Receive()

	require.True(t, IsSessionStopping(err), "the read ends on the stopping close: %v", err)
	assertStoppingDetach(t, conn.device, defaultStopping)
	assert.Equal(t, 1, mock.ConnectionsAccepted(), "a stopping close is not resumed")
}

func TestGivenAStoppingCloseOnTheAttachClearWhenTheNotifierIsReadyThenTheDeviceDetaches(t *testing.T) {
	sessions, reverse := serverMocks(t, reverseMockBehavior{}.answer)
	sessions.CloseNewConnections(sessionStoppingCloseCode, relayStoppingReason)

	clearing := attachDeviceAt(t, sessions, reverse, reverse.URL, parkedKeepAlive, noopLogger())

	assertStoppingDetach(t, clearing.device, defaultStopping)
}
