//go:build unix

package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/bridge"
	"github.com/aws/device-connect-devicefarm/internal/config"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// manualClock is an advanceable clock for deterministic idle-time tests. Guarded because
// a test that holds a proxied connection open has pipe goroutines stamping activity.
type manualClock struct {
	currentTimeMutex sync.Mutex
	currentTime      time.Time
}

func (clock *manualClock) now() time.Time {
	clock.currentTimeMutex.Lock()
	defer clock.currentTimeMutex.Unlock()
	return clock.currentTime
}

func (clock *manualClock) advance(elapsed time.Duration) {
	clock.currentTimeMutex.Lock()
	defer clock.currentTimeMutex.Unlock()
	clock.currentTime = clock.currentTime.Add(elapsed)
}

// keepAliveFixture wires a Manager, an ActiveDevice on an injected clock, a
// dialer, and a mock WSS endpoint that records the first message it receives.
type keepAliveFixture struct {
	mgr    *DaemonManager
	device *bridge.ActiveDevice
	dialer *bridge.WSDialer
	clock  *manualClock
	gotCh  chan []byte
}

func newKeepAliveFixture(t *testing.T, idleThreshold time.Duration) *keepAliveFixture {
	t.Helper()
	cfg := config.Default()
	cfg.KeepAliveIdle = idleThreshold
	mgr := newTestManagerWithConfig(t, cfg)

	gotCh := make(chan []byte, 1)
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err == nil {
			select {
			case gotCh <- data:
			default:
			}
		}
		ws.Close()
	})

	clock := &manualClock{currentTime: time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)}
	device := bridge.NewActiveDevice(bridge.WithClock(clock.now))
	device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: wsURL, Label: "Pixel - OS 16"})

	return &keepAliveFixture{
		mgr:    mgr,
		device: device,
		dialer: bridge.NewWSDialer(2 * time.Second).TrustRoots(testsupport.TestRoots()),
		clock:  clock,
		gotCh:  gotCh,
	}
}

func TestGivenDeviceIdlePastThresholdWhenKeepAliveCheckedThenHostDevicesProbeSent(t *testing.T) {
	f := newKeepAliveFixture(t, 60*time.Second)
	f.clock.advance(90 * time.Second) // well past the 60s threshold

	sent := f.mgr.keepAliveOnce(context.Background(), f.device, f.dialer)

	require.True(t, sent, "an idle device must trigger a keep-alive probe")
	select {
	case got := <-f.gotCh:
		assert.Equal(t, "000chost:devices", string(got), "the probe must be an adb host:devices request")
	case <-time.After(5 * time.Second):
		t.Fatal("keep-alive probe never reached the endpoint")
	}
}

func TestGivenProbeReplyNamingSerialWhenKeepAliveCheckedThenSerialCached(t *testing.T) {
	fixture := newKeepAliveFixture(t, 60*time.Second)
	// The session mock announces the session, then answers the probe with an adb
	// host:devices reply naming one device.
	payload := "R3CN30ABCDE\tdevice\n"
	mock := testsupport.NewSessionMockWSS(t, testsupport.SessionMockConfig{
		OnMessage: func(_ *testsupport.MockSession, _ []byte) []byte {
			return []byte(fmt.Sprintf("OKAY%04x%s", len(payload), payload))
		},
	})
	fixture.device.Clear()
	require.True(t, fixture.device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: mock.URL, Label: "Pixel - OS 16"}))
	fixture.clock.advance(90 * time.Second)

	require.True(t, fixture.mgr.keepAliveOnce(context.Background(), fixture.device, fixture.dialer))

	got, ok := fixture.device.Current()
	require.True(t, ok)
	assert.Equal(t, "R3CN30ABCDE", got.Serial, "the probe reply's serial must be cached on the device")
}

func TestGivenATrackedConnectionWhenTheIdleThresholdPassesThenNoProbeIsSent(t *testing.T) {
	fixture := newKeepAliveFixture(t, 60*time.Second)
	mock := testsupport.NewSessionEchoWSS(t)
	fixture.device.Clear()
	require.True(t, fixture.device.Set(bridge.DeviceInfo{ARN: "arn:1", Endpoint: mock.URL, Label: "Pixel - OS 16"}))

	// Only the bridge registers a connection on the device, so run a real listener
	// and proxy one round trip through it.
	server, err := bridge.NewAdbProxyServer(
		"127.0.0.1", 0, bridge.WSRefreshPolicy{}, fixture.dialer, fixture.device, slog.New(slog.DiscardHandler),
	)
	require.NoError(t, err)
	require.NoError(t, server.Listen())
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = server.Run(ctx) }()

	client, err := net.Dial("tcp", server.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	request := []byte("000chost:version")
	_, err = client.Write(request)
	require.NoError(t, err)
	echoed := make([]byte, len(request))
	_, err = io.ReadFull(client, echoed)
	require.NoError(t, err, "the echoed request proves the connection is registered")

	fixture.clock.advance(90 * time.Second)

	assert.False(t, fixture.mgr.keepAliveOnce(context.Background(), fixture.device, fixture.dialer),
		"an open connection already holds the remote session out of idle")
	assert.Equal(t, 1, mock.SessionsOpened(), "the probe must not mint a second session")
}

func TestGivenDeviceWithinThresholdWhenKeepAliveCheckedThenNoProbeSent(t *testing.T) {
	f := newKeepAliveFixture(t, 60*time.Second)
	f.clock.advance(10 * time.Second) // still within the idle window

	sent := f.mgr.keepAliveOnce(context.Background(), f.device, f.dialer)

	assert.False(t, sent, "a recently-active device must not be probed")
}

func TestGivenNoDeviceAttachedWhenKeepAliveCheckedThenNoProbeSent(t *testing.T) {
	f := newKeepAliveFixture(t, 60*time.Second)
	f.device.Clear()
	f.clock.advance(90 * time.Second)

	sent := f.mgr.keepAliveOnce(context.Background(), f.device, f.dialer)

	assert.False(t, sent, "with no device attached there is nothing to keep alive")
}

func TestGivenIdleThresholdWhenCheckIntervalDerivedThenHalfThresholdCappedAndFloored(t *testing.T) {
	tests := []struct {
		name string
		idle time.Duration
		want time.Duration
	}{
		{name: "default 60s idle -> capped at 15s", idle: 60 * time.Second, want: 15 * time.Second},
		{name: "20s idle -> half is 10s", idle: 20 * time.Second, want: 10 * time.Second},
		{name: "tiny 500ms idle -> floored at 1s", idle: 500 * time.Millisecond, want: time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.KeepAliveIdle = tc.idle
			mgr := newTestManagerWithConfig(t, cfg)

			assert.Equal(t, tc.want, mgr.keepAliveCheckInterval())
		})
	}
}

func TestGivenIdleProbeSentWhenCheckedAgainImmediatelyThenIdleClockReset(t *testing.T) {
	f := newKeepAliveFixture(t, 60*time.Second)
	f.clock.advance(90 * time.Second)
	require.True(t, f.mgr.keepAliveOnce(context.Background(), f.device, f.dialer))
	<-f.gotCh // drain the first probe

	// No time has passed since the probe reset the idle clock, so a second check
	// must not send another probe.
	sent := f.mgr.keepAliveOnce(context.Background(), f.device, f.dialer)

	assert.False(t, sent, "MarkActivity after a probe must reset the idle clock")
}

func TestGivenAPresignedEndpointThatRefusesTheDialWhenTheKeepAliveProbeRunsThenTheLogCarriesNoCredential(t *testing.T) {
	fixture := newKeepAliveFixture(t, 60*time.Second)
	logs := &lockedBuffer{}
	fixture.mgr.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	fixture.device.Clear()
	require.True(t, fixture.device.Set(bridge.DeviceInfo{ARN: "arn:1",
		Endpoint: testsupport.PresignedUnreachableEndpoint, Label: "Pixel - OS 16"}))
	fixture.clock.advance(90 * time.Second)

	sent := fixture.mgr.keepAliveOnce(context.Background(), fixture.device, fixture.dialer)

	assert.False(t, sent, "a refused dial is a failed probe")
	assert.Contains(t, logs.String(), `msg="keep-alive probe failed"`)
	assert.Contains(t, logs.String(), testsupport.ScrubbedDialText, "the dial must have run")
	assert.NotContains(t, logs.String(), testsupport.SignatureMarker)
	assert.NotContains(t, logs.String(), testsupport.SecurityTokenMarker)
	assert.NotContains(t, logs.String(), "X-Amz-")
}
