package bridge

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/adbwire"
	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

const (
	// testTimeout bounds every blocking wait so a hung test fails instead of stalling the run.
	testTimeout = 5 * time.Second
	// silentReadTimeout is short: a read that waits on a silent server fails fast.
	silentReadTimeout = 100 * time.Millisecond
)

// dialSending dials a mock that sends each of messages as its own binary websocket
// message and then holds the connection open, so the test controls exactly how a
// status word is split or coalesced on the wire.
func dialSending(t *testing.T, messages ...string) WSConn {
	t.Helper()
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	wsURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		serverConn.PayloadType = websocket.BinaryFrame
		for _, message := range messages {
			if err := websocket.Message.Send(serverConn, []byte(message)); err != nil {
				return
			}
		}
		<-hold
	})
	return dialMock(t, wsURL)
}

func TestGivenAnOkayArrivingWholeWhenStatusReadThenNoLeftover(t *testing.T) {
	ws := dialSending(t, "OKAY")

	leftover, err := readStatusWord(ws, testTimeout, nil)

	require.NoError(t, err)
	assert.Empty(t, leftover)
}

func TestGivenAnOkaySplitAcrossMessagesWhenStatusReadThenReassembled(t *testing.T) {
	// The server relays reads, not words, so a 4-byte word can land in two
	// messages split at any point. The byte after the word proves nothing is lost.
	for splitAt := 1; splitAt < len("OKAY"); splitAt++ {
		t.Run(fmt.Sprintf("split at %d", splitAt), func(t *testing.T) {
			word := "OKAYx"
			ws := dialSending(t, word[:splitAt], word[splitAt:])

			leftover, err := readStatusWord(ws, testTimeout, nil)

			require.NoError(t, err)
			assert.Equal(t, []byte("x"), leftover)
		})
	}
}

func TestGivenTwoStatusWordsCoalescedInOneMessageWhenStatusReadThenTheSecondIsLeftover(t *testing.T) {
	ws := dialSending(t, "OKAYOKAY")

	leftover, err := readStatusWord(ws, testTimeout, nil)
	require.NoError(t, err)
	assert.Equal(t, []byte("OKAY"), leftover)

	// The second read must be satisfied from the leftover alone: the server sends
	// nothing more, so a read that touched the wire would time out here.
	leftover, err = readStatusWord(ws, silentReadTimeout, leftover)
	require.NoError(t, err)
	assert.Empty(t, leftover)
}

func TestGivenAnOkayCoalescedWithPayloadWhenStatusReadThenThePayloadIsLeftover(t *testing.T) {
	ws := dialSending(t, "OKAYhello, app")

	leftover, err := readStatusWord(ws, testTimeout, nil)

	require.NoError(t, err)
	assert.Equal(t, []byte("hello, app"), leftover, "every byte after the word belongs to the app")
}

func TestGivenAFailWithReasonWhenStatusReadThenTheReasonSurfaces(t *testing.T) {
	tests := []struct {
		name string
		// buffered is what an earlier read left over; messages is what the wire adds.
		buffered   string
		messages   []string
		wantReason string
	}{
		{name: "whole", messages: []string{"FAIL0006closed"}, wantReason: "closed"},
		{name: "split between FAIL and the length", messages: []string{"FAIL", "0006closed"}, wantReason: "closed"},
		{name: "split inside the length", messages: []string{"FAIL00", "06closed"}, wantReason: "closed"},
		{name: "split inside the reason", messages: []string{"FAIL0006clo", "sed"}, wantReason: "closed"},
		{name: "started by an earlier read's leftover", buffered: "FAI", messages: []string{"L0006closed"}, wantReason: "closed"},
		{name: "empty reason", messages: []string{"FAIL0000"}, wantReason: ""},
		{name: "bytes after the reason are dropped", messages: []string{"FAIL0006closedXYZ"}, wantReason: "closed"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ws := dialSending(t, testCase.messages...)

			leftover, err := readStatusWord(ws, testTimeout, []byte(testCase.buffered))

			var failure *statusFailure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, testCase.wantReason, failure.reason)
			assert.Contains(t, err.Error(), testCase.wantReason, "the reason must be loggable from the error")
			assert.Nil(t, leftover)
		})
	}
}

func TestGivenAFailWhollyBufferedWhenStatusReadThenNothingIsReadFromTheWire(t *testing.T) {
	// Satisfied from the leftover alone: the server stays silent, so a read that touched
	// the wire would time out here.
	ws := dialSending(t)

	leftover, err := readStatusWord(ws, silentReadTimeout, []byte("FAIL0006closed"))

	var failure *statusFailure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "closed", failure.reason)
	assert.Nil(t, leftover)
}

func TestGivenAnUnknownStatusWordWhenStatusReadThenFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		message string
	}{
		{name: "not a status word", message: "WHAT"},
		{name: "lowercase", message: "okay"},
		{name: "a control frame where a word belongs", message: string(testsupport.FrameEmptyArm("ping"))},
		{name: "FAIL with a non-hex length", message: "FAILzzzzclosed"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ws := dialSending(t, testCase.message)

			leftover, err := readStatusWord(ws, testTimeout, nil)

			assert.ErrorIs(t, err, errStatusWordMalformed)
			assert.Nil(t, leftover)
		})
	}
}

func TestGivenASilentServerWhenStatusReadThenTimesOutAndClosesTheConnection(t *testing.T) {
	ws := dialSending(t, "OK") // half a word, then silence

	leftover, err := readStatusWord(ws, silentReadTimeout, nil)

	assert.ErrorIs(t, err, errReceiveTimeout)
	assert.Nil(t, leftover)
	// The timeout closes the websocket, which is what unblocks the parked Receive.
	_, receiveErr := ws.Receive()
	assert.Error(t, receiveErr, "the timed-out connection must be closed")
}

func TestGivenAClosedConnectionWhenStatusReadThenTheReceiveErrorSurfaces(t *testing.T) {
	wsURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		serverConn.Close()
	})
	ws := dialMock(t, wsURL)

	leftover, err := readStatusWord(ws, testTimeout, nil)

	require.Error(t, err)
	assert.NotErrorIs(t, err, errReceiveTimeout, "a peer close is not a timeout")
	assert.Nil(t, leftover)
}

// prologueMock is a session mock that answers the fabricated prologue requests with
// scripted status words, one per request in order, and records each request.
type prologueMock struct {
	*testsupport.SessionMockWSS

	framesMutex sync.Mutex
	frames      []string
}

func newPrologueMock(t *testing.T, replies ...string) *prologueMock {
	t.Helper()
	return newPrologueMockWith(t, testsupport.SessionMockConfig{}, replies...)
}

// newPrologueMockWith is newPrologueMock over a caller's session behavior. It owns
// cfg.OnMessage.
func newPrologueMockWith(t *testing.T, cfg testsupport.SessionMockConfig, replies ...string) *prologueMock {
	t.Helper()
	require.Nil(t, cfg.OnMessage, "this helper owns OnMessage; a caller's would be dropped")
	mock := &prologueMock{}
	cfg.OnMessage = func(_ *testsupport.MockSession, data []byte) []byte {
		mock.framesMutex.Lock()
		defer mock.framesMutex.Unlock()
		mock.frames = append(mock.frames, string(data))
		if len(mock.frames) > len(replies) {
			return nil
		}
		return []byte(replies[len(mock.frames)-1])
	}
	mock.SessionMockWSS = testsupport.NewSessionMockWSS(t, cfg)
	return mock
}

// Frames returns each request received, one entry per websocket message, so a prologue
// pipelined into one message is visible. MockSession.Inbound concatenates and is not.
func (mock *prologueMock) Frames() []string {
	mock.framesMutex.Lock()
	defer mock.framesMutex.Unlock()
	return append([]string(nil), mock.frames...)
}

// newTestDevice binds one device at arn to endpoint.
func newTestDevice(t *testing.T, arn, endpoint string) *ActiveDevice {
	t.Helper()
	device := NewActiveDevice()
	require.True(t, device.Set(DeviceInfo{ARN: arn, Endpoint: endpoint, Label: "test-device"}))
	return device
}

// openStream opens a forwarded device stream for target, as the stream opener does. It
// always asks for testDeviceARN, so a caller can bind a different one.
func openStream(t *testing.T, device *ActiveDevice, target adbwire.AdbSpec, within time.Duration) (*adbConn, []byte, error) {
	t.Helper()
	conn, leftover, err := openDeviceStream(context.Background(), newTestDialer(within), device, testDeviceARN, nil,
		target, fastPolicy(), within, newActivityTracker(nil), noopLogger())
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, leftover, err
}

// openTestDeviceStream is openStream for tcp:port against a device freshly bound to endpoint.
func openTestDeviceStream(t *testing.T, endpoint, port string) (*adbConn, []byte, error) {
	t.Helper()
	return openStream(t, newTestDevice(t, testDeviceARN, endpoint), adbwire.AdbSpec{Kind: "tcp", Value: port},
		testWait)
}

func TestGivenAForwardedOpenWhenPrologueSentThenTransportSelectPrecedesTheDeviceService(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")

	conn, leftover, err := openTestDeviceStream(t, mock.URL, "8080")

	require.NoError(t, err)
	require.NotNil(t, conn)
	assert.Empty(t, leftover)
	// Framed by hand, as every wire expectation in this package is: 4 hex digits of payload
	// length then the service. Two entries, so a pipelined prologue fails here too.
	assert.Equal(t, []string{"0012host:transport-any", "0008tcp:8080"}, mock.Frames())
	assert.Equal(t, 1, mock.SessionsOpened(), "one forwarded connection is one session")
}

func TestGivenANonTCPTargetWhenOpenedThenTheDeviceServiceIsTheSpecTheCustomerNamed(t *testing.T) {
	mock := newPrologueMock(t, "OKAY", "OKAY")

	_, _, err := openStream(t, newTestDevice(t, testDeviceARN, mock.URL),
		adbwire.AdbSpec{Kind: "localabstract", Value: "chrome_devtools_remote"}, testWait)

	require.NoError(t, err)
	// A hardcoded "tcp:" prefix would put tcp:chrome_devtools_remote on the wire.
	assert.Equal(t, "0024localabstract:chrome_devtools_remote", mock.Frames()[1])
}

func TestGivenBothOkaysCoalescedWithPayloadWhenOpenedThenLeftoverIsReturnedNotLost(t *testing.T) {
	// The remote pumps the adb server with partial reads, so one message can carry both
	// status words and the app's first bytes. The second read must then touch no wire at
	// all, which the mock proves by never answering the second request.
	mock := newPrologueMock(t, "OKAYOKAYhello, app")

	conn, leftover, err := openTestDeviceStream(t, mock.URL, "8080")

	require.NoError(t, err)
	require.NotNil(t, conn)
	assert.Equal(t, []byte("hello, app"), leftover, "the app's bytes must survive the prologue")
}

func TestGivenTheTransportSelectRefusedWhenOpenedThenTheErrorCarriesAdbsReasonAndNoStreamIsReturned(t *testing.T) {
	tests := []struct {
		name       string
		replies    []string
		wantStage  string
		wantReason string
	}{
		{
			name:       "the transport select fails",
			replies:    []string{"FAIL000edevice offline"},
			wantStage:  "transport select",
			wantReason: "device offline",
		},
		{
			name:       "the device service fails",
			replies:    []string{"OKAY", "FAIL0006closed"},
			wantStage:  "device service",
			wantReason: "closed",
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			mock := newPrologueMock(t, testCase.replies...)

			conn, leftover, err := openTestDeviceStream(t, mock.URL, "8080")

			require.Error(t, err)
			assert.Nil(t, conn, "a refused prologue must hand back no stream")
			assert.Nil(t, leftover)
			var failure *statusFailure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, testCase.wantReason, failure.reason)
			assert.Contains(t, err.Error(), testCase.wantStage, "the error must name which request was refused")
			assert.Equal(t, 1, mock.SessionsOpened(), "one dial, one session")
			waitFor(t, func() bool { return !mock.Sessions()[0].Send([]byte("x")) },
				"a refused open must close the websocket, not leak it")
		})
	}
}

func TestGivenTheSessionRefusedWithAnErrorFrameWhenOpenedThenTheRefusalReasonSurfaces(t *testing.T) {
	// The other refusal shape: the remote will not open the session at all, so the refusal
	// is the control frame itself and no status word ever arrives.
	wsURL := testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		serverConn.PayloadType = websocket.BinaryFrame
		var first []byte
		if websocket.Message.Receive(serverConn, &first) != nil {
			return
		}
		websocket.Message.Send(serverConn, testsupport.FrameError("device is not attached"))
	})

	conn, leftover, err := openTestDeviceStream(t, wsURL, "8080")

	require.Error(t, err)
	assert.Nil(t, conn)
	assert.Nil(t, leftover)
	assert.Contains(t, err.Error(), "device is not attached", "the remote's reason must be loggable")
	assert.NotContains(t, err.Error(), "transport select",
		"a refused session is a session refusal, not a malformed status word")
}

func TestGivenADetachedDeviceWhenOpenedThenRefusedBeforeDialing(t *testing.T) {
	// The device can detach between the accept and the open. The endpoint read is ARN
	// guarded, so a live mock bound under another ARN proves the refusal positively: an
	// opener that skipped the guard would dial it.
	mock := newPrologueMock(t, "OKAY", "OKAY")
	otherDeviceARN := "arn:aws:devicefarm:us-west-2:111122223333:session:99999999-9999-4999-8999-999999999999"

	conn, leftover, err := openStream(t, newTestDevice(t, otherDeviceARN, mock.URL),
		adbwire.AdbSpec{Kind: "tcp", Value: "8080"}, time.Second)

	require.ErrorIs(t, err, errAdbConnClosed)
	assert.Nil(t, conn)
	assert.Nil(t, leftover)
	assert.Zero(t, mock.ConnectionsAccepted(), "another device's endpoint must never be dialed")
}

func TestGivenAnUnreachableOrSilentRemoteWhenOpenedThenTheOpenFailsWithoutAStream(t *testing.T) {
	tests := []struct {
		name     string
		endpoint func(t *testing.T) string
		wantText string
		wantIs   error
	}{
		{
			name: "the dial never connects",
			// Port 1 on loopback needs root to bind, so the dial is refused at once.
			endpoint: func(*testing.T) string { return "wss://127.0.0.1:1/adb" },
			wantText: "dial device stream websocket",
		},
		{
			name: "the remote takes the request and never answers",
			endpoint: func(t *testing.T) string {
				hold := make(chan struct{})
				t.Cleanup(func() { close(hold) })
				return testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
					serverConn.PayloadType = websocket.BinaryFrame
					var first []byte
					if websocket.Message.Receive(serverConn, &first) != nil {
						return
					}
					<-hold
				})
			},
			wantIs: errReceiveTimeout,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			conn, leftover, err := openStream(t, newTestDevice(t, testDeviceARN, testCase.endpoint(t)),
				adbwire.AdbSpec{Kind: "tcp", Value: "8080"}, 200*time.Millisecond)

			require.Error(t, err)
			assert.Nil(t, conn)
			assert.Nil(t, leftover)
			if testCase.wantIs != nil {
				assert.ErrorIs(t, err, testCase.wantIs)
			}
			if testCase.wantText != "" {
				assert.Contains(t, err.Error(), testCase.wantText)
			}
		})
	}
}

func TestGivenARefusedReattachOnADeviceStreamWhenRefreshRunsThenTheConnectionEndsInsteadOfMintingAFreshSession(t *testing.T) {
	mock := newPrologueMockWith(t, testsupport.SessionMockConfig{RejectReattach: true}, "OKAY", "OKAY")
	conn, _, err := openTestDeviceStream(t, mock.URL, "8080")
	require.NoError(t, err)
	// A reader stands in for the pipe's inbound pump: closing every socket is what tells it
	// the stream is over, and a reader left parked would hold the app socket open forever.
	readReturned := make(chan error, 1)
	go func() {
		_, readErr := conn.Receive()
		readReturned <- readErr
	}()

	// The fallback returns nil here, so this alone rules it out.
	require.ErrorIs(t, conn.refresh(context.Background(), nil), ErrSessionGone)

	// Not failRefreshLocked, which would return the predecessor to service instead.
	assert.True(t, conn.isClosed(), "the stream must end rather than restart in host mode")
	assert.Equal(t, 2, mock.ConnectionsAccepted(),
		"one dial opened the stream, one carried the refused reattach; a third is a fresh session")
	select {
	case readErr := <-readReturned:
		assert.Error(t, readErr, "the parked reader must be released with an error")
	case <-time.After(testTimeout):
		t.Fatal("the reader was never released, so the app socket would stay open")
	}
}
