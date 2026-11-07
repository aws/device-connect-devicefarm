// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// testWait bounds every wait for something the notifier must do promptly.
const testWait = 2 * time.Second

// The frames a notifier sends, as literal bytes (fixture vectors notifier-hello and ping): the
// oracle for the client's encoder, never its output.
var (
	notifierHelloFrame = []byte("\x00DFADB\x01{\"notifierHello\":{}}")
	pingFrame          = []byte("\x00DFADB\x01{\"ping\":{}}")
)

// notifierServer is the scripted server half of a notifier test: it records every client
// frame and the route the dial named, and plays a fixed script. Scripted rather than
// stateful on purpose, so each test states the exact bytes it puts on the wire.
type notifierServer struct {
	url string

	route     chan string
	frames    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

// newNotifierServer starts a server for one notifier. On the client's first frame it queues
// every entry of script, the hello ack leading, and then echoes each PING with a PONG. An
// empty script never answers the hello, which is the silent server. Anything but a PING
// after the hello is refused with an ERROR, so a client that breaks the direction rules
// fails a test instead of being tolerated.
func newNotifierServer(t *testing.T, script ...[]byte) *notifierServer {
	t.Helper()
	server := &notifierServer{
		route:  make(chan string, 1),
		frames: make(chan []byte, 8),
		closed: make(chan struct{}),
	}
	server.url = testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		serverConn.PayloadType = websocket.BinaryFrame
		defer server.closeOnce.Do(func() { close(server.closed) })
		server.route <- serverConn.Request().URL.Query().Get("path")

		for first := true; ; first = false {
			var message []byte
			if err := websocket.Message.Receive(serverConn, &message); err != nil {
				return
			}
			server.frames <- message

			switch {
			case first:
				// The whole script is queued at once: the ack read and the read loop share
				// one connection, so frames buffered behind the ack still reach the loop.
				for _, reply := range script {
					if websocket.Message.Send(serverConn, reply) != nil {
						return
					}
				}
			case bytes.Equal(message, pingFrame):
				websocket.Message.Send(serverConn, testsupport.FrameEmptyArm("pong"))
			default:
				websocket.Message.Send(serverConn, testsupport.FrameError(
					fmt.Sprintf("a ready notifier sends only ping, got %q", message)))
				return
			}
		}
	})
	return server
}

// await returns the next value on channel, failing the test when nothing arrives in time.
func await[T any](t *testing.T, channel <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(testWait):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// dialTestNotifier binds endpoint to an ActiveDevice and dials a notifier on it, as the
// attachment will.
func dialTestNotifier(t *testing.T, endpoint string, ackWithin time.Duration) (*notifierConn, error) {
	t.Helper()
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: testDeviceARN, Endpoint: endpoint, Label: "test-device"}))
	return dialNotifier(context.Background(), newTestDialer(testTimeout), device, testDeviceARN,
		ackWithin, noopLogger())
}

// readyNotifier dials a notifier against a server that acks the hello and then plays script.
// It asserts the hello exchange on the wire and consumes the hello frame, so a test's next
// frame is its own.
func readyNotifier(t *testing.T, script ...[]byte) (*notifierServer, *notifierConn) {
	t.Helper()
	ready := append([][]byte{testsupport.FrameEmptyArm("notifierReady")}, script...)
	server := newNotifierServer(t, ready...)
	notifier, err := dialTestNotifier(t, server.url, testWait)
	require.NoError(t, err)
	t.Cleanup(func() { notifier.Close() })
	require.Equal(t, notifierHelloFrame, await(t, server.frames, "the notifier hello"),
		"notifierHello must be the notifier's first frame")
	require.Equal(t, "adbreverse", await(t, server.route, "the dialed route"))
	return server, notifier
}

// runNotifier starts the read loop and returns the channel its error lands on.
func runNotifier(notifier *notifierConn, onReverseOpen func(string, int), onPong func()) <-chan error {
	ended := make(chan error, 1)
	go func() { ended <- notifier.run(onReverseOpen, onPong) }()
	return ended
}

func TestGivenABadHelloAnswerWhenTheNotifierIsDialedThenItFailsAndClosesTheSocket(t *testing.T) {
	cases := []struct {
		name      string
		script    [][]byte
		wantError string
		// wantIs pins the sentinel a supervisor branches on; nil where there is none.
		wantIs error
	}{
		{"a refusal", [][]byte{testsupport.FrameError("notifier already registered")},
			"notifier already registered", ErrNotifierSlotTaken},
		{"an arm that is not notifierReady", [][]byte{testsupport.FrameEmptyArm("pong")},
			"answered with pong", nil},
		{"no answer at all", nil, errReceiveTimeout.Error(), errReceiveTimeout},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := newNotifierServer(t, testCase.script...)

			notifier, err := dialTestNotifier(t, server.url, silentReadTimeout)

			require.Nil(t, notifier)
			assert.ErrorContains(t, err, testCase.wantError)
			if testCase.wantIs != nil {
				assert.ErrorIs(t, err, testCase.wantIs)
			}
			await(t, server.closed, "the notifier websocket to close")
		})
	}
}

func TestGivenNoBoundDeviceWhenTheNotifierIsDialedThenItRefusesBeforeDialing(t *testing.T) {
	notifier, err := dialNotifier(context.Background(), newTestDialer(testTimeout),
		NewActiveDevice(), testDeviceARN, testWait, noopLogger())

	require.Nil(t, notifier)
	assert.ErrorIs(t, err, errNoNotifierEndpoint)
}

func TestGivenAReverseOpenFrameWhenItArrivesThenTheTokenAndPortReachTheCallback(t *testing.T) {
	_, notifier := readyNotifier(t, testsupport.FrameReverseOpen(testSessionUUID, 8080))
	announced := make(chan string, 1)

	runNotifier(notifier, func(token string, customerPort int) {
		announced <- fmt.Sprintf("%s:%d", token, customerPort)
	}, func() {})

	assert.Equal(t, testSessionUUID+":8080", await(t, announced, "the announced connection"))
}

func TestGivenSeveralReverseOpensWhenTheyArriveThenTheReadLoopIsNotBlockedByASlowCallback(t *testing.T) {
	_, notifier := readyNotifier(t, testsupport.FrameReverseOpen(testSessionUUID, 8080),
		testsupport.FrameReverseOpen(testSessionUUID, 9090))
	// Every callback parks until the test ends, so the second announcement can only arrive
	// if the read loop dispatched the first off its own goroutine.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	announced := make(chan int, 2)

	runNotifier(notifier, func(_ string, customerPort int) {
		announced <- customerPort
		<-release
	}, func() {})

	assert.ElementsMatch(t, []int{8080, 9090}, []int{
		await(t, announced, "the first announced port"),
		await(t, announced, "the second announced port"),
	})
}

func TestGivenAFatalFrameWhenItArrivesThenTheRunLoopEndsWithAnError(t *testing.T) {
	cases := []struct {
		name      string
		frame     []byte
		wantError string
	}{
		{"a reverse open with a malformed token", testsupport.FrameReverseOpen("not-a-uuid", 8080),
			"does not match the schema"},
		// Fixture vector unknown-arm.
		{"an unknown arm", testsupport.FrameEmptyArm("hello"), "does not match the schema"},
		{"an arm the notifier never receives", testsupport.FrameEmptyArm("notifierReady"),
			"unexpected control frame notifierReady"},
		{"a message that is no control frame at all", []byte("OKAY"), errNotControlFrame.Error()},
		{"a frame on wire version 0x00", []byte("\x00DFADB\x00\x14"), errNotControlFrame.Error()},
		{"the server ending the notifier", testsupport.FrameError("notifier evicted"), "notifier evicted"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, notifier := readyNotifier(t, testCase.frame)

			ended := runNotifier(notifier, func(string, int) {}, func() {})

			assert.ErrorContains(t, await(t, ended, "the read loop to end"), testCase.wantError)
			await(t, server.closed, "the notifier websocket to close")
		})
	}
}

func TestGivenAPingWhenSentThenTheServerSeesItAndThePongReachesTheCallback(t *testing.T) {
	server, notifier := readyNotifier(t)
	ponged := make(chan struct{}, 1)
	runNotifier(notifier, func(string, int) {}, func() { ponged <- struct{}{} })

	require.NoError(t, notifier.ping())

	assert.Equal(t, pingFrame, await(t, server.frames, "the ping"),
		"ping must travel client to server, so the server's idle clock sees the notifier")
	await(t, ponged, "the pong to reach the callback")
}

func TestGivenARunningNotifierWhenClosedThenTheReadLoopEndsAndTheSocketIsReleased(t *testing.T) {
	server, notifier := readyNotifier(t)
	ponged := make(chan struct{}, 1)
	ended := runNotifier(notifier, func(string, int) {}, func() { ponged <- struct{}{} })
	// A pong round trip first, so the read loop is provably parked in Receive when Close lands.
	require.NoError(t, notifier.ping())
	await(t, ponged, "the pong that proves the read loop is parked")

	require.NoError(t, notifier.Close())

	assert.Error(t, await(t, ended, "the read loop to end"), "Close must unblock the parked Receive")
	await(t, server.closed, "the notifier websocket to close")
}
