package bridge

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// assertDialLoggedWithoutCredential asserts logs hold a dial failure against the presigned
// endpoint and none of its query.
func assertDialLoggedWithoutCredential(t *testing.T, logs *lockedBuffer) {
	t.Helper()
	text := logs.String()
	assert.Contains(t, text, testsupport.ScrubbedDialText, "the dial must have run")
	assert.NotContains(t, text, testsupport.SignatureMarker)
	assert.NotContains(t, text, testsupport.SecurityTokenMarker)
	assert.NotContains(t, text, "X-Amz-")
}

// attachPresigned binds a device with attachment features at the presigned endpoint, so its
// opener, notifier and connectors log to logger.
func attachPresigned(t *testing.T, params AttachmentParams, logger *slog.Logger) *ActiveDevice {
	t.Helper()
	device := NewActiveDevice(WithAttachmentFeatures(newTestDialer(testWait), testPolicy(), params, logger))
	t.Cleanup(func() { device.Clear() })
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: testsupport.PresignedUnreachableEndpoint,
		Label: "test-device"}))
	return device
}

func TestGivenAPresignedEndpointThatRefusesTheDialWhenEachSiteFailsThenTheLogCarriesNoCredential(t *testing.T) {
	testCases := []struct {
		name string
		// failureLine is the site's own log line for the failed dial: proof the site ran and
		// logged into the capture.
		failureLine string
		run         func(t *testing.T, logger *slog.Logger)
	}{
		{name: "local adb connection", failureLine: `msg="failed to dial remote websocket; closing client"`,
			run: func(t *testing.T, logger *slog.Logger) {
				server, err := NewAdbProxyServer("127.0.0.1", 0, testPolicy(), newTestDialer(testWait),
					deviceFor(testsupport.PresignedUnreachableEndpoint), logger)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(context.Background())
				stopped := make(chan error, 1)
				go func() { stopped <- server.Run(ctx) }()
				t.Cleanup(func() { cancel(); <-stopped })
				client, err := net.Dial("tcp", waitForAddr(t, server))
				require.NoError(t, err)
				t.Cleanup(func() { client.Close() })
				_, err = client.Write([]byte("000chost:version"))
				require.NoError(t, err)
				assertClosed(t, client, "a failed dial must close the client with nothing written")
			}},
		{name: "session resume", failureLine: `msg="could not resume the adb session"`,
			run: func(t *testing.T, logger *slog.Logger) {
				// A resume re-reads the endpoint, so the redirect makes its dial the one that fails.
				conn := newTestAdbConnOnRoute(t, testsupport.NewSessionEchoWSS(t).URL, fastPolicy(), RouteAdb, logger)
				handshake(t, conn)
				redirect(t, conn, testsupport.PresignedUnreachableEndpoint)
				assert.False(t, conn.reconnectReactively(io.EOF), "a refused dial must not resume the session")
			}},
		{name: "forwarded device stream", failureLine: `msg="failed to open a forwarded device stream"`,
			run: func(t *testing.T, logger *slog.Logger) {
				device := attachPresigned(t, forwardingParams(), logger)
				port := installRule(t, attachedFeatures(t, device), "tcp:8080")
				assertClosed(t, dialRule(t, port), "a failed dial must close the forwarded connection")
			}},
		{name: "reverse notifier", failureLine: `msg="the reverse notifier ended"`,
			run: func(t *testing.T, logger *slog.Logger) {
				// Set opens the attachment's notifier; a parked backoff keeps it to one failed dial.
				attachPresigned(t, notifierParams(parkedKeepAlive, parkedBackoff), logger)
			}},
		{name: "reverse connector", failureLine: `msg="a reverse connection failed"`,
			run: func(t *testing.T, logger *slog.Logger) {
				params := notifierParams(parkedKeepAlive, parkedBackoff)
				params.MaxReverseRules = testMaxReverseRules
				device := attachPresigned(t, params, logger)
				target := startLocalTarget(t)
				features := attachedFeatures(t, device)
				// A rule naming the port, so the screen admits the open and the dial runs.
				install, full := features.reverseRules.begin(adbwire.AdbActionForward, "tcp:1", target.port)
				require.False(t, full)
				install.settle(true)
				features.notifier.announce(context.Background(), testReverseToken, target.port)
				target.requireNoDial(t)
			}},
		{name: "warm socket keeper", failureLine: `msg="could not dial a warm websocket; retrying"`,
			run: func(t *testing.T, logger *slog.Logger) {
				device := NewActiveDevice(WithAttachmentFeatures(newTestDialer(testWait), warmPolicy(warmInterval), warmParams(),
					logger))
				t.Cleanup(func() { device.Clear() })
				require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN,
					Endpoint: testsupport.PresignedUnreachableEndpoint, Label: "test-device"}))
			}},
		{name: "reverse rule clear", failureLine: `msg="could not clear the server's reverse rules before the detach"`,
			run: func(t *testing.T, logger *slog.Logger) {
				// A clear needs a ready notifier and a held rule; the renewed endpoint then refuses its dial.
				clearing := startClearingDevice(t, reverseMockBehavior{}, logger)
				clearing.holdRule(t, "tcp:1")
				require.True(t, clearing.device.UpdateEndpoint(testDeviceARN, testsupport.PresignedUnreachableEndpoint,
					"test-device"))
				clearing.device.ClearReverseRulesAtServer(context.Background())
			}},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// Debug, so a site that logs there cannot pass unrecorded.
			logs := &lockedBuffer{}
			logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

			testCase.run(t, logger)

			// Eventually: some sites log from their own goroutine.
			require.Eventually(t, func() bool { return strings.Contains(logs.String(), testCase.failureLine) },
				testWait, notifierPoll, "the site's own failure line %s never appeared", testCase.failureLine)
			assertDialLoggedWithoutCredential(t, logs)
		})
	}
}
