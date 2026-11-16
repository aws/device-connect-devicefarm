// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/websocket"

	"github.com/aws/device-connect-devicefarm/internal/testsupport"
)

// probeReplyServer builds a mock endpoint that consumes one request, then sends each of
// frames as its own binary websocket message.
func probeReplyServer(t *testing.T, frames ...[]byte) string {
	t.Helper()
	return testsupport.NewMockWSS(t, func(serverConn *websocket.Conn) {
		var request []byte
		if err := websocket.Message.Receive(serverConn, &request); err != nil {
			return
		}
		for _, frame := range frames {
			if err := websocket.Message.Send(serverConn, frame); err != nil {
				return
			}
		}
		// Hold the connection open so the reader, not a close, ends the parse.
		var discard []byte
		_ = websocket.Message.Receive(serverConn, &discard)
	})
}

// controlFrameNew opens every probe reply: the server's newSession frame.
func controlFrameNew() []byte {
	return testsupport.FrameNewSession(testSessionUUID)
}

// okayDevicesReply renders the adb host:devices reply naming the given lines.
func okayDevicesReply(payload string) []byte {
	return []byte(fmt.Sprintf("OKAY%04x%s", len(payload), payload))
}

func TestGivenReachableEndpointWhenTheSerialIsFetchedThenOneHostDevicesFrameGoesOutOnTheAdbRoute(t *testing.T) {
	gotCh := make(chan []byte, 1)
	routeCh := make(chan string, 1)
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		routeCh <- ws.Request().URL.Query().Get("path")
		var data []byte
		if err := websocket.Message.Receive(ws, &data); err == nil {
			gotCh <- data
		}
		ws.Close()
	})

	_, err := newTestDialer(testWait).FetchDeviceSerial(context.Background(), wsURL)

	require.ErrorContains(t, err, "read the devices reply", "a connection closed with no reply is a failed read")
	select {
	case got := <-gotCh:
		assert.Equal(t, hostDevicesRequest, string(got), "the fetch must be an adb host:devices request")
		assert.Equal(t, "adb", <-routeCh, "the fetch must reach the adb session handler")
	case <-time.After(testWait):
		t.Fatal("the fetch frame never reached the endpoint")
	}
}

func TestGivenAnEndpointThatCannotBeDialedWhenTheSerialIsFetchedThenTheDialErrorIsReturned(t *testing.T) {
	testCases := []struct {
		name     string
		endpoint string
	}{
		// Port 1 is not listening.
		{name: "unreachable", endpoint: "wss://127.0.0.1:1/adb"},
		// Fails at url.Parse, before any dial.
		{name: "malformed", endpoint: "://not-a-url"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := newTestDialer(500*time.Millisecond).FetchDeviceSerial(context.Background(), testCase.endpoint)

			require.Error(t, err)
		})
	}
}

func TestGivenADevicesReplyWhenTheSerialIsFetchedThenTheFirstRowSerialIsReturned(t *testing.T) {
	reply := okayDevicesReply("emulator-5554\tdevice\n")
	testCases := []struct {
		name   string
		frames [][]byte
		want   string
	}{
		{name: "after the control frame", frames: [][]byte{controlFrameNew(), okayDevicesReply("R3CN30ABCDE\tdevice\n")},
			want: "R3CN30ABCDE"},
		{name: "split across messages", frames: [][]byte{controlFrameNew(), reply[:2], reply[2:6], reply[6:]},
			want: "emulator-5554"},
		// Leniency: a first message that is already the adb reply parses anyway.
		{name: "with no control frame", frames: [][]byte{okayDevicesReply("R3CN30ABCDE\tdevice\n")},
			want: "R3CN30ABCDE"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			wsURL := probeReplyServer(t, testCase.frames...)

			serial, err := newTestDialer(testWait).FetchDeviceSerial(context.Background(), wsURL)

			require.NoError(t, err)
			assert.Equal(t, testCase.want, serial)
		})
	}
}

func TestGivenAReplyThatIsNoDeviceListWhenTheSerialIsFetchedThenTheErrorNamesTheProblem(t *testing.T) {
	// A length the server never satisfies must stop accumulating at the byte cap.
	neverCompletes := []byte("OKAYffff")
	filler := bytes.Repeat([]byte("x"), maxHostReplyBytes+1-len(neverCompletes))
	testCases := []struct {
		name     string
		frames   [][]byte
		wantErr  error
		wantText string
	}{
		{name: "a FAIL reply", frames: [][]byte{controlFrameNew(), []byte("FAIL0014something went wrong")},
			wantText: "answered FAIL instead of OKAY"},
		{name: "an unparsable length", frames: [][]byte{controlFrameNew(), []byte("OKAYzzzzR3CN30ABCDE\tdevice\n")},
			wantText: `malformed devices reply: length "zzzz"`},
		{name: "no device row", frames: [][]byte{controlFrameNew(), okayDevicesReply("no devices attached\n")},
			wantErr: errNoDeviceSerial},
		{name: "a length never satisfied", frames: [][]byte{controlFrameNew(), neverCompletes, filler},
			wantText: "the devices reply runs past its byte cap"},
		// Fixture vector v1-magic-new-frame: a byte-protocol server's first frame.
		{name: "a control frame on another wire version", frames: [][]byte{decodeHex(t, frameVector(t, "v1-magic-new-frame").Hex)},
			wantErr: errNotControlFrame, wantText: "wire protocol 0x00"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			wsURL := probeReplyServer(t, testCase.frames...)
			dialer := newTestDialer(testWait)
			dialer.hostReplyTimeout = time.Hour // only the reply may end this read

			_, err := dialer.FetchDeviceSerial(context.Background(), wsURL)

			if testCase.wantErr != nil {
				require.ErrorIs(t, err, testCase.wantErr)
			}
			if testCase.wantText != "" {
				require.ErrorContains(t, err, testCase.wantText)
			}
		})
	}
}

func TestGivenAWriteDeadlineAlreadyPastWhenTheSerialIsFetchedThenTheSendErrorIsReturned(t *testing.T) {
	wsURL := probeReplyServer(t)
	dialer := newTestDialer(testWait)
	dialer.writeDeadline = -time.Second

	_, err := dialer.FetchDeviceSerial(context.Background(), wsURL)

	require.ErrorContains(t, err, "send host:devices")
}

func TestGivenASilentServerWhenTheSerialIsFetchedThenTheReplyTimeoutEndsIt(t *testing.T) {
	wsURL := probeReplyServer(t) // consumes the request, sends nothing back
	dialer := newTestDialer(testWait)
	dialer.hostReplyTimeout = 100 * time.Millisecond

	_, err := dialer.FetchDeviceSerial(context.Background(), wsURL)

	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestGivenAFetchAwaitingItsReplyWhenTheContextEndsThenTheFetchReturnsAndClosesItsWebsocket(t *testing.T) {
	// The add's budget, not the reply timeout, ends this fetch.
	requested := make(chan struct{})
	closed := make(chan struct{})
	wsURL := testsupport.NewMockWSS(t, func(ws *websocket.Conn) {
		var request []byte
		if websocket.Message.Receive(ws, &request) != nil {
			return
		}
		close(requested)
		// Never answer; wait for the client to close.
		_ = websocket.Message.Receive(ws, &request)
		close(closed)
	})
	dialer := newTestDialer(testWait)
	dialer.hostReplyTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-requested
		cancel()
	}()

	_, err := dialer.FetchDeviceSerial(ctx, wsURL)

	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-closed:
	case <-time.After(testWait):
		t.Fatal("an ended fetch must close its websocket")
	}
}
